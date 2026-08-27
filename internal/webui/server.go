// Package webui provides the local Codex-style desktop interface for Metis.
package webui

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/agent"
	transcriptpkg "github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/artifact"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/desktop"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/pluginmarket"
	"github.com/Ricardo-M-L/metis/internal/processutil"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	tui "github.com/Ricardo-M-L/metis/internal/tui"
	metisversion "github.com/Ricardo-M-L/metis/internal/version"
	"github.com/google/uuid"
)

//go:embed static/*
var staticFS embed.FS

// Server owns one agent loop. Turns are serialized because Loop contains the
// active transcript and is not safe for simultaneous conversations.
type Server struct {
	addr         string
	loop         *agent.Loop
	store        *session.Store
	runMu        sync.Mutex
	hub          *eventHub
	prefsMu      sync.Mutex
	workspacesMu sync.Mutex
	providersMu  sync.Mutex
	effortMu     sync.Mutex

	// cancelMu guards the identity, cancel func, and completion signal for the
	// in-flight turn. Keeping the session identity separate from the session
	// being viewed lets Desktop browse other transcripts while work continues.
	cancelMu       sync.Mutex
	cancelTurn     context.CancelFunc
	runningSession string
	turnDone       chan struct{}

	roster *agent.Roster

	// buildVersion is stamped at NewServer and injected into index.html so
	// every server start serves unique asset URLs - the browser can never
	// keep serving a stale embedded script/CSS across a rebuild.
	buildVersion string

	// pendingPerms holds in-flight tool permission requests surfaced to the
	// browser as approval cards. The agent loop blocks on the reply channel
	// until /api/permission (or the timeout goroutine) resolves it.
	permMu       sync.Mutex
	pendingPerms map[string]*permissionPending
	askMu        sync.Mutex
	pendingAsks  map[string]*askPending

	stateMu            sync.RWMutex
	activeSessionID    string
	activeProviderName string
	activeModel        string
	activePreset       string

	freshProviderName   string
	freshModel          string
	freshSystem         string
	freshSystemSections []llm.SystemSection
	freshPermissionMode permission.Mode
	freshPreset         string

	buildProvider   func(providerName, model string) (*rtpkg.ProviderBuild, error)
	sessionBoundary func()
	sessionSwitch   func(sessionID string)
	openWorkspace   func(path string) error
	openPath        func(path string) error
	clipboardFiles  func() ([]desktop.ClipboardFile, error)
	plugins         *rtpkg.PluginRegistry
	pluginMarket    *pluginmarket.Manager
	artifactStore   *artifact.Store
}

// eventHub fans agent events out to zero or more SSE subscribers. The agent
// loop is serialized (runMu guards turns), but multiple browser tabs /
// EventSource clients may be connected at once - each gets its own buffered
// channel. Slow subscribers drop events instead of stalling the loop.
//
// Events carry the session they belong to so a tab viewing session A can
// ignore the live stream of a turn running in session B.
// permissionPending is one unresolved tool-approval request.
type permissionPending struct {
	reply chan agent.PermissionDecision
	tool  string
}

// askPending is one unresolved AskUser question from the model.
type askPending struct {
	reply chan string
}

type hubEvent struct {
	sequence uint64
	session  string
	ev       agent.Event
	extra    map[string]any // merged into the SSE payload (e.g. permId)
}

type eventHub struct {
	mu        sync.RWMutex
	subs      map[chan hubEvent]struct{}
	nextID    uint64
	replay    []hubEvent
	replayCap int
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan hubEvent]struct{}), replayCap: 2048}
}

func (h *eventHub) subscribe() chan hubEvent {
	ch, _, _ := h.subscribeFrom(0)
	return ch
}

func (h *eventHub) subscribeFrom(afterID uint64) (chan hubEvent, []hubEvent, bool) {
	ch := make(chan hubEvent, 256)
	h.mu.Lock()
	var replay []hubEvent
	reset := false
	if afterID > 0 {
		if afterID > h.nextID {
			reset = true
		} else if len(h.replay) > 0 && afterID+1 < h.replay[0].sequence {
			reset = true
		} else {
			for _, event := range h.replay {
				if event.sequence > afterID {
					replay = append(replay, event)
				}
			}
		}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, replay, reset
}

func (h *eventHub) unsubscribe(ch chan hubEvent) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *eventHub) publish(sessionID string, ev agent.Event, extra ...map[string]any) {
	he := hubEvent{session: sessionID, ev: ev}
	if len(extra) > 0 {
		he.extra = extra[0]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	he.sequence = h.nextID
	he.ev.PermissionReply = nil
	he.ev.AskUserReply = nil
	h.replay = append(h.replay, he)
	if len(h.replay) > h.replayCap {
		copy(h.replay, h.replay[len(h.replay)-h.replayCap:])
		h.replay = h.replay[:h.replayCap]
	}
	for ch := range h.subs {
		select {
		case ch <- he:
		default: // slow subscriber - drop, don't block the turn
		}
	}
}

// forgetSession removes replay-only browser state after a durable session is
// deleted. Live subscribers keep their channels, but reconnecting clients must
// not be able to replay events that no longer have an on-disk owner.
func (h *eventHub) forgetSession(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	h.mu.Lock()
	kept := h.replay[:0]
	for _, event := range h.replay {
		if event.session != sessionID {
			kept = append(kept, event)
		}
	}
	h.replay = kept
	h.mu.Unlock()
}

// RuntimeBindings supplies the process-owned pieces needed to cross a real
// top-level session boundary. The web server owns transcript serialization,
// while the command composition layer still owns provider construction and
// global session routers (task storage, prompt dumps and checkpoints).
//
// NewServer keeps this optional for reduced embedders and read-only API tests.
// A server without BuildProvider can continue a session only when its stored
// provider/model already match the live loop; it fails closed on a mismatch.
type RuntimeBindings struct {
	InitialSessionID string
	ProviderName     string
	// PresetName is "standard" or the agent profile loaded before the tool
	// registry was built. It is displayed and persisted with new sessions.
	PresetName          string
	FreshPermissionMode permission.Mode
	BuildProvider       func(providerName, model string) (*rtpkg.ProviderBuild, error)
	SessionBoundary     func()
	SessionSwitch       func(sessionID string)
	// OpenWorkspace starts an independent Desktop window rooted at path.
	// A separate process avoids process-wide cwd races with active jobs.
	OpenWorkspace func(path string) error
	// OpenPath reveals a local directory in the platform file manager.
	OpenPath func(path string) error
	Plugins  *rtpkg.PluginRegistry
	// Roster exposes the live sub-agent registry so the status bar can
	// show "N sub-agents ~ M background tasks" like the harness GUI.
	Roster *agent.Roster
}

func NewServer(addr string, loop *agent.Loop, store *session.Store, bindings ...RuntimeBindings) *Server {
	binding := RuntimeBindings{}
	if len(bindings) > 0 {
		binding = bindings[0]
	}

	server := &Server{
		addr:                addr,
		loop:                loop,
		store:               store,
		hub:                 newEventHub(),
		pendingPerms:        make(map[string]*permissionPending),
		buildVersion:        strconv.FormatInt(time.Now().UnixNano(), 36),
		roster:              binding.Roster,
		pendingAsks:         make(map[string]*askPending),
		activeSessionID:     binding.InitialSessionID,
		activeProviderName:  binding.ProviderName,
		activePreset:        binding.PresetName,
		freshProviderName:   binding.ProviderName,
		freshPermissionMode: binding.FreshPermissionMode,
		freshPreset:         binding.PresetName,
		buildProvider:       binding.BuildProvider,
		sessionBoundary:     binding.SessionBoundary,
		sessionSwitch:       binding.SessionSwitch,
		openWorkspace:       binding.OpenWorkspace,
		openPath:            binding.OpenPath,
		clipboardFiles:      desktop.ClipboardFiles,
		plugins:             binding.Plugins,
		pluginMarket:        pluginmarket.NewManager(),
	}
	if loop != nil {
		server.activeModel = loop.Model
		server.freshModel = loop.Model
		server.freshSystem = loop.System
		server.freshSystemSections = append([]llm.SystemSection(nil), loop.SystemSections...)
		if loop.Provider != nil {
			if server.activeProviderName == "" {
				server.activeProviderName = loop.Provider.Name()
				server.freshProviderName = server.activeProviderName
			}
			if server.activeModel == "" {
				server.activeModel = loop.Provider.ModelID()
				server.freshModel = server.activeModel
			}
		}
		if server.freshPermissionMode == "" && loop.Gate != nil {
			server.freshPermissionMode = loop.Gate.Mode()
		}
	}
	if server.freshPermissionMode == "" {
		server.freshPermissionMode = permission.ModeAsk
	}
	if server.activePreset == "" {
		server.activePreset = "standard"
		server.freshPreset = "standard"
	}
	return server
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	staticSub, _ := fs.Sub(staticFS, "static")
	staticHandler := http.FileServer(http.FS(staticSub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			data, err := staticFS.ReadFile("static/index.html")
			if err != nil {
				writeError(w, http.StatusInternalServerError, "index unavailable")
				return
			}
			_, _ = w.Write([]byte(strings.ReplaceAll(string(data), "__BUILD__", s.buildVersion)))
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/activate", s.handleSessionActivate)
	mux.HandleFunc("/api/sessions/", s.handleSession)
	mux.HandleFunc("/api/artifacts", s.handleArtifacts)
	mux.HandleFunc("/api/artifacts/", s.handleArtifact)
	mux.HandleFunc("/api/turns", s.handleTurn)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/commands/session", s.handleSessionCommand)
	mux.HandleFunc("/api/compact", s.handleCompact)
	mux.HandleFunc("/api/goals", s.handleGoals)
	mux.HandleFunc("/api/preferences", s.handlePreferences)
	mux.HandleFunc("/api/workspaces", s.handleWorkspaces)
	mux.HandleFunc("/api/workspaces/rename", s.handleWorkspaceRename)
	mux.HandleFunc("/api/workspaces/remove", s.handleWorkspaceRemove)
	mux.HandleFunc("/api/workspaces/reorder", s.handleWorkspaceReorder)
	mux.HandleFunc("/api/workspaces/open", s.handleWorkspaceOpen)
	mux.HandleFunc("/api/providers", s.handleProviders)
	mux.HandleFunc("/api/providers/default", s.handleProviderDefault)
	mux.HandleFunc("/api/providers/validate", s.handleProviderValidate)
	mux.HandleFunc("/api/providers/probe", s.handleProviderProbe)
	mux.HandleFunc("/api/effort", s.handleEffort)
	mux.HandleFunc("/api/presets", s.handlePresets)
	mux.HandleFunc("/api/presets/default", s.handlePresetDefault)
	mux.HandleFunc("/api/presets/open", s.handlePresetDirectory)
	mux.HandleFunc("/api/plugins", s.handlePlugins)
	mux.HandleFunc("/api/plugins/catalog", s.handlePluginCatalog)
	mux.HandleFunc("/api/plugins/icon", s.handlePluginIcon)
	mux.HandleFunc("/api/plugins/catalog/refresh", s.handlePluginCatalogRefresh)
	mux.HandleFunc("/api/plugins/install", s.handlePluginInstall)
	mux.HandleFunc("/api/plugins/remove", s.handlePluginRemove)
	mux.HandleFunc("/api/routing", s.handleRouting)
	mux.HandleFunc("/api/trace", s.handleTrace)
	mux.HandleFunc("/api/trace/export", s.handleTraceExport)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/exports/open", s.handleExportsOpen)
	mux.HandleFunc("/api/permission", s.handlePermission)
	mux.HandleFunc("/api/ask", s.handleAsk)
	mux.HandleFunc("/api/fork", s.handleFork)
	mux.HandleFunc("/api/sessions/rename", s.handleRename)
	mux.HandleFunc("/api/sessions/archive", s.handleArchive)
	mux.HandleFunc("/api/feedback", s.handleFeedback)
	mux.HandleFunc("/api/config/file", s.handleConfigFile)
	mux.HandleFunc("/api/clipboard/files", s.handleClipboardFiles)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/api/steer", s.handleSteer)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/events", s.handleEvents)
	return s.securityHeaders(s.sameOriginOnly(mux))
}

func (s *Server) Run(ctx context.Context) error {
	server := &http.Server{Addr: s.addr, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("metis webui: listening on %s", s.addr)
	fmt.Fprintf(os.Stderr, "Metis Desktop: http://%s\n", s.addr)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	lastID := uint64(0)
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			lastID = parsed
		}
	}
	ch, replay, replayReset := s.hub.subscribeFrom(lastID)
	defer s.hub.unsubscribe(ch)

	// Write the initial ready event so the client knows the stream is live;
	// an idle turn would otherwise produce a silent blank page.
	s.stateMu.RLock()
	activeSession := s.activeSessionID
	s.stateMu.RUnlock()
	ready, _ := json.Marshal(map[string]any{"kind": "ready", "session": activeSession, "replayReset": replayReset})
	fmt.Fprintf(w, "event: ready\ndata: %s\n\n", ready)
	flusher.Flush()
	for _, event := range replay {
		s.writeHubEvent(w, event)
	}
	if len(replay) > 0 {
		flusher.Flush()
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// Comment-only keepalive — ignored by EventSource but keeps the
			// connection open through idle proxies.
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case he := <-ch:
			s.writeHubEvent(w, he)
			flusher.Flush()
		}
	}
}

func (s *Server) writeHubEvent(w http.ResponseWriter, he hubEvent) {
	eventName := eventKindName(he.ev.Kind)
	payload := map[string]any{
		"kind":      eventName,
		"session":   he.session,
		"tool":      he.ev.ToolName,
		"id":        he.ev.ToolUseID,
		"isError":   he.ev.ToolResult != nil && he.ev.ToolResult.IsError,
		"elapsedMs": he.ev.Elapsed.Milliseconds(),
	}
	for k, v := range he.extra {
		payload[k] = v
	}
	switch he.ev.Kind {
	case agent.EventTokens:
		payload["inputTokens"] = he.ev.InputTokens
		payload["outputTokens"] = he.ev.OutputTokens
	case agent.EventContextWarn, agent.EventCompactionStart:
		payload["info"] = he.ev.Info
	case agent.EventContextCompacted:
		payload["info"] = he.ev.Info
		payload["previousContextTokens"] = he.ev.PreviousContextTokens
		payload["contextTokens"] = he.ev.ContextTokens
	case agent.EventCompactionProgress:
		payload["info"] = he.ev.Info
		payload["progressBytes"] = he.ev.InputTokens
	case agent.EventCompactionEnd:
		payload["info"] = he.ev.Info
		if he.ev.Err != nil {
			payload["error"] = truncateSSE(he.ev.Err.Error(), 600)
		}
	case agent.EventToolStart:
		if raw, err := json.Marshal(he.ev.ToolInput); err == nil && len(raw) > 0 {
			payload["input"] = truncateSSE(string(raw), toolInputSSELimit(he.ev.ToolName))
		}
	case agent.EventToolResult:
		if he.ev.ToolResult != nil {
			payload["output"] = truncateSSE(he.ev.ToolResult.Output, 600)
			if he.ev.ToolResult.Display != "" {
				payload["display"] = he.ev.ToolResult.Display
			}
			if len(he.ev.ToolResult.Presentation) > 0 {
				payload["presentation"] = he.ev.ToolResult.Presentation
			}
		}
	case agent.EventPermissionRequest:
		payload["reason"] = he.ev.PermissionReason
		if raw, err := json.Marshal(he.ev.ToolInput); err == nil && len(raw) > 0 {
			payload["input"] = truncateSSE(string(raw), 300)
		}
	case agent.EventAskUser:
		payload["question"] = he.ev.AskUserQuestion
		payload["options"] = he.ev.AskUserOptions
		payload["allowFreeform"] = he.ev.AskUserAllowFreeform
	case agent.EventError:
		if he.ev.Err != nil {
			payload["message"] = truncateSSE(he.ev.Err.Error(), 600)
		}
	}
	if he.ev.TextDelta != "" {
		payload["delta"] = he.ev.TextDelta
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", he.sequence, eventName, data)
}

// eventKindNames maps agent event kinds to stable SSE event names.
// Built once: the SSE loop consults it for every streamed event.
var eventKindNames = map[agent.EventKind]string{
	agent.EventTextDelta:          "text_delta",
	agent.EventToolStart:          "tool_start",
	agent.EventToolResult:         "tool_result",
	agent.EventPermissionRequest:  "permission_request",
	agent.EventTurnEnd:            "turn_end",
	agent.EventLoopDone:           "loop_done",
	agent.EventError:              "agent_error",
	agent.EventTokens:             "tokens",
	agent.EventInfo:               "info",
	agent.EventPlan:               "plan",
	agent.EventStreamStart:        "stream_start",
	agent.EventStreamEnd:          "stream_end",
	agent.EventSubAgentStart:      "subagent_start",
	agent.EventSubAgentProgress:   "subagent_progress",
	agent.EventSubAgentEnd:        "subagent_end",
	agent.EventContextWarn:        "context_warn",
	agent.EventContextCompacted:   "context_compacted",
	agent.EventCompactionStart:    "compaction_start",
	agent.EventCompactionProgress: "compaction_progress",
	agent.EventRateLimitHit:       "rate_limit_hit",
	agent.EventModelFallback:      "model_fallback",
	agent.EventThinkingDelta:      "thinking_delta",
	agent.EventRedactedThinking:   "redacted_thinking",
	agent.EventAskUser:            "ask_user",
	agent.EventToolArgsDelta:      "tool_args_delta",
	agent.EventChannelInbound:     "channel_inbound",
	agent.EventChannelSent:        "channel_sent",
	agent.EventHookFired:          "hook_fired",
	agent.EventDreamingStart:      "dreaming_start",
	agent.EventDreamingProgress:   "dreaming_progress",
	agent.EventDreamingEnd:        "dreaming_end",
	agent.EventCompactionEnd:      "compaction_end",
}

func eventKindName(kind agent.EventKind) string {
	if name, ok := eventKindNames[kind]; ok {
		return name
	}
	return fmt.Sprintf("event_%d", int(kind))
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets are embedded in the binary: force revalidation so a
		// rebuilt server is never masked by browser HTTP caching.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The browser build remains non-embeddable. The Wails shell receives a
		// fresh high-entropy token on every launch and may frame only the root
		// document carrying that token; subresources do not need an exception.
		frameToken := os.Getenv("METIS_DESKTOP_FRAME_TOKEN")
		nativeFrame := (r.URL.Path == "/" || r.URL.Path == "/index.html") && frameToken != "" && r.URL.Query().Get("desktop-frame") == frameToken
		if !nativeFrame {
			w.Header().Set("X-Frame-Options", "DENY")
		}
		// Artifact previews run on a capability-bearing, short-lived loopback
		// origin. Allow only that exact host class to be framed; leaving it out
		// would make the browser reject the intentionally cross-origin preview,
		// while a broad http: source would let arbitrary pages enter the app.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; font-src 'self' data:; img-src 'self' data:; connect-src 'self'; frame-src http://127.0.0.1:*")
		next.ServeHTTP(w, r)
	})
}

// sameOriginOnly prevents an unrelated web page from driving the local agent
// through a cross-origin POST. Browsers do not require a CORS preflight for a
// text/plain JSON body, so response-side CORS defaults alone are insufficient.
// Requests from non-browser clients generally omit Origin and remain allowed.
func (s *Server) sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browser-driven cross-site fetches are rejected for BOTH safe and
		// unsafe methods. A DNS-rebinding page reading a GET endpoint sends
		// Sec-Fetch-Site: cross-site, so safe-method reads are covered too.
		// (Non-browser clients - curl, tests - send no Sec-Fetch-Site and are
		// unaffected; a local non-browser attacker already owns the host.)
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			writeError(w, http.StatusForbidden, "cross-origin request denied")
			return
		}
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			writeError(w, http.StatusForbidden, "cross-origin request denied")
			return
		}
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
			if !isLoopbackHost(r.Host) {
				writeError(w, http.StatusForbidden, "non-local request denied")
				return
			}
			u, err := url.Parse(origin)
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if err != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) {
				writeError(w, http.StatusForbidden, "cross-origin request denied")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

type sessionItem struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Model       string    `json:"model"`
	WorkDir     string    `json:"workDir,omitempty"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Effort      string    `json:"effort,omitempty"`
	Preset      string    `json:"preset,omitempty"`
	Status      string    `json:"status,omitempty"`
	Archived    bool      `json:"archived,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "session store unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		archivedOnly := r.URL.Query().Get("archived_only") == "true"
		includeArchived := r.URL.Query().Get("include_archived") == "true" || archivedOnly
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 200 {
				writeError(w, http.StatusBadRequest, "session limit must be between 1 and 200")
				return
			}
			limit = parsed
		}
		cursor := 0
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			decoded, err := base64.RawURLEncoding.DecodeString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid session cursor")
				return
			}
			parsed, err := strconv.Atoi(string(decoded))
			if err != nil || parsed < 0 {
				writeError(w, http.StatusBadRequest, "invalid session cursor")
				return
			}
			cursor = parsed
		}
		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		// The web client is a global session browser: list EVERY session
		// regardless of which directory created it. Filtering by this
		// process's cwd (the CLI resume picker's behavior) hid all
		// sessions started elsewhere and made the client look out of
		// sync with `metis -r`.
		entries, err := s.store.ListResumable(session.ResumeListOptions{})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list sessions")
			return
		}
		items := make([]sessionItem, 0, len(entries))
		for _, e := range entries {
			archived := s.store.IsArchived(e.ID)
			if (!includeArchived && archived) || (archivedOnly && !archived) {
				continue
			}
			hdr, _, _ := s.store.LoadHeader(e.ID)
			item := sessionItem{ID: e.ID, Title: e.Title, Model: e.Model, Archived: archived, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
			if hdr != nil {
				item.WorkDir = hdr.WorkDir
				item.WorkspaceID = workspaceIDForPath(hdr.WorkDir)
				item.Mode = hdr.Mode
				item.Effort = hdr.Effort
				item.Preset = hdr.Preset
				item.Status = hdr.Status
			}
			if query != "" {
				haystack := strings.ToLower(strings.Join([]string{item.ID, item.Title, item.Model, item.WorkDir, item.Mode, item.Preset, item.Status}, "\n"))
				if !strings.Contains(haystack, query) {
					continue
				}
			}
			items = append(items, item)
		}
		if cursor > len(items) {
			writeError(w, http.StatusBadRequest, "invalid session cursor")
			return
		}
		end := min(len(items), cursor+limit)
		nextCursor := ""
		if end < len(items) {
			nextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": items[cursor:end], "nextCursor": nextCursor, "total": len(items)})
	case http.MethodPost:
		model := s.freshModel
		var body struct {
			Model string `json:"model"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
		}
		if body.Model != "" {
			model = body.Model
		}
		id := s.store.NewSessionID()
		createdAt := time.Now()
		cwd, _ := os.Getwd()
		if err := s.store.WriteHeaderFull(session.Header{
			ID:        id,
			CreatedAt: createdAt,
			Provider:  s.freshProviderName,
			Model:     model,
			System:    s.freshSystem,
			WorkDir:   cwd,
			Mode:      string(s.freshPermissionMode),
			Effort:    effortHeaderValue(s.loop.EffortValue()),
			Preset:    s.freshPreset,
			Status:    "idle",
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		writeJSON(w, http.StatusCreated, sessionItem{ID: id, Title: "Untitled", Model: model, Status: "idle", CreatedAt: createdAt, UpdatedAt: createdAt})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if !validSessionID(id) || s.store == nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if r.Method == http.MethodDelete {
		s.handleSessionDelete(w, id)
		return
	}
	hdr, messages, err := s.store.Load(id)
	if err != nil || hdr == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sessionItem{
		ID: id, Title: hdr.Title, Model: hdr.Model, WorkDir: hdr.WorkDir, WorkspaceID: workspaceIDForPath(hdr.WorkDir), Mode: hdr.Mode,
		Effort: hdr.Effort, Preset: hdr.Preset, Status: hdr.Status,
		Archived: s.store.IsArchived(id), CreatedAt: hdr.CreatedAt,
	}, "messages": messages})
}

// handleSessionDelete permanently removes one session and every Metis-owned
// sidecar. A running turn owns runMu, so deletion cannot race a transcript,
// trace, task, dump, or checkpoint write. Deleting the process's active
// session first crosses to a fresh empty session; this keeps every global
// session router valid and prevents a late write from recreating the old data.
func (s *Server) handleSessionDelete(w http.ResponseWriter, id string) {
	if !s.runMu.TryLock() {
		writeError(w, http.StatusConflict, "a turn is running; stop it before deleting sessions")
		return
	}
	defer s.runMu.Unlock()

	hdr, _, err := s.store.Load(id)
	if err != nil || hdr == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	s.stateMu.RLock()
	isActive := s.activeSessionID == id
	s.stateMu.RUnlock()
	if isActive && s.roster != nil && s.roster.Count() > 0 {
		writeError(w, http.StatusConflict, "background sub-agents are still running; stop them before deleting this session")
		return
	}

	// runMu only protects this Desktop server. A recovery pointer owned by a
	// different live PID means a CLI or another Desktop can still append to
	// this transcript, so deleting it would let that process recreate a
	// headerless session file after the response.
	if hdr.WorkDir != "" {
		pointer, pointerErr := session.ReadPointer(hdr.WorkDir)
		if pointerErr != nil {
			writeError(w, http.StatusConflict, "cannot verify whether the session is active in another process")
			return
		}
		if pointer != nil && pointer.SessionID == id && pointer.PID != os.Getpid() && processutil.Alive(pointer.PID) {
			writeError(w, http.StatusConflict, "session is active in another Metis process; stop it before deleting")
			return
		}
	}
	if pid, exists, pidErr := readSessionRunPID(id); pidErr != nil {
		writeError(w, http.StatusConflict, "cannot verify the session run pid")
		return
	} else if exists && pid != os.Getpid() && processutil.Alive(pid) {
		writeError(w, http.StatusConflict, "session is active in another Metis process; stop it before deleting")
		return
	}

	var ownedJobOutputs []string
	if isActive && s.loop != nil && s.loop.Jobs != nil {
		for _, job := range s.loop.Jobs.List() {
			if job.Status == jobs.StatusRunning {
				writeError(w, http.StatusConflict, "background shell jobs are still running; stop them before deleting this session")
				return
			}
			if job.OutputPath != "" {
				ownedJobOutputs = append(ownedJobOutputs, job.OutputPath)
			}
		}
	}
	activeID := ""
	if isActive {
		if s.loop == nil {
			writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
			return
		}
		replacement, err := s.createFreshSessionForDelete()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create a replacement session")
			return
		}
		if err := s.activateSession(replacement.ID, replacement, nil); err != nil {
			if cleanupErr := s.store.Delete(replacement.ID); cleanupErr != nil {
				log.Printf("session delete: clean up replacement %s: %v", replacement.ID, cleanupErr)
			}
			writeError(w, http.StatusConflict, "failed to reset the active session: "+err.Error())
			return
		}
		activeID = replacement.ID
	} else {
		s.stateMu.RLock()
		activeID = s.activeSessionID
		s.stateMu.RUnlock()
	}

	if err := s.deleteSessionData(id, hdr.WorkDir, ownedJobOutputs); err != nil {
		log.Printf("session delete %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":           "failed to delete all session data; the session remains visible so deletion can be retried",
			"partial":         true,
			"activeSessionId": activeID,
		})
		return
	}
	s.hub.forgetSession(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":         true,
		"id":              id,
		"activeSessionId": activeID,
	})
}

func (s *Server) createFreshSessionForDelete() (*session.Header, error) {
	id := s.store.NewSessionID()
	cwd, _ := os.Getwd()
	hdr := &session.Header{
		ID:        id,
		CreatedAt: time.Now(),
		Provider:  s.freshProviderName,
		Model:     s.freshModel,
		System:    s.freshSystem,
		WorkDir:   cwd,
		Mode:      string(s.freshPermissionMode),
		Effort:    effortHeaderValue(s.loop.EffortValue()),
		Preset:    s.freshPreset,
		Status:    "idle",
	}
	if err := s.store.WriteHeaderFull(*hdr); err != nil {
		return nil, err
	}
	return hdr, nil
}

func (s *Server) deleteSessionData(id, workDir string, ownedJobOutputs []string) error {
	var err error
	if workDir != "" {
		pointer, pointerErr := session.ReadPointer(workDir)
		err = errors.Join(err, pointerErr)
		if pointerErr == nil && pointer != nil && pointer.SessionID == id {
			err = errors.Join(err, session.ClearPointer(workDir))
		}
	}
	if traceStore := rtpkg.CurrentTraceStore(); traceStore != nil {
		err = errors.Join(err, traceStore.Delete(id))
	}
	err = errors.Join(err, transport.DeleteSessionDump(id))
	if artifacts, artifactErr := s.artifacts(); artifactErr != nil {
		err = errors.Join(err, artifactErr)
	} else {
		err = errors.Join(err, artifacts.DeleteSession(id))
	}
	err = errors.Join(err, tasks.Delete(id))
	err = errors.Join(err, checkpoint.DeleteSession(id, ""))
	err = errors.Join(err, rtpkg.DeleteSnapshot(id))
	err = errors.Join(err, rtpkg.DeleteSessionPlans(id))
	err = errors.Join(err, rtpkg.DeleteSessionHistory(id))
	err = errors.Join(err, rtpkg.DeleteSessionLearned(id))
	err = errors.Join(err, deleteSessionRunPID(id))
	for _, path := range ownedJobOutputs {
		err = errors.Join(err, deleteOwnedJobOutput(path))
	}
	if err != nil {
		// Keep the canonical transcript until every ancillary store has been
		// detached. The visible session remains retryable after a partial I/O
		// failure instead of disappearing while private sidecars survive.
		return err
	}
	return s.store.Delete(id)
}

func readSessionRunPID(id string) (int, bool, error) {
	path := filepath.Join(config.Home(), "run", id+".pid")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, true, errors.New("refusing non-regular session pid file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, true, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0, true, errors.New("malformed session pid file")
	}
	return pid, true, nil
}

func deleteSessionRunPID(id string) error {
	path := filepath.Join(config.Home(), "run", id+".pid")
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func deleteOwnedJobOutput(path string) error {
	jobsDir, err := filepath.Abs(filepath.Join(config.Home(), "jobs"))
	if err != nil {
		return err
	}
	candidate, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	base := filepath.Base(candidate)
	if filepath.Dir(candidate) != jobsDir || !regexp.MustCompile(`^bg_[0-9a-f]{8}\.out$`).MatchString(base) {
		return fmt.Errorf("refusing job output outside the Metis jobs store: %q", path)
	}
	if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// handleSessionActivate makes the user's navigation choice the live runtime
// boundary before composer controls (model/effort/permissions) can mutate it.
// A second tab cannot switch the loop while a turn is running.
func (s *Server) handleSessionActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.loop == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || !validSessionID(body.ID) {
		writeError(w, http.StatusBadRequest, "valid session id is required")
		return
	}
	if !s.runMu.TryLock() {
		s.cancelMu.Lock()
		runningSessionID := s.runningSession
		s.cancelMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":            "a turn is running; opening the requested session read-only",
			"turnRunning":      true,
			"runningSessionId": runningSessionID,
		})
		return
	}
	defer s.runMu.Unlock()
	hdr, history, err := s.store.Load(body.ID)
	if err != nil || hdr == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := s.activateSession(body.ID, hdr, history); err != nil {
		writeError(w, http.StatusConflict, "failed to activate session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sessionItem{
		ID: body.ID, Title: hdr.Title, Model: hdr.Model, WorkDir: hdr.WorkDir, WorkspaceID: workspaceIDForPath(hdr.WorkDir),
		Mode: hdr.Mode, Effort: hdr.Effort, Preset: hdr.Preset, Status: hdr.Status, Archived: s.store.IsArchived(body.ID), CreatedAt: hdr.CreatedAt,
	}, "messages": history})
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.loop == nil || s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Input     string `json:"input"`
		Images    []struct {
			MediaType string `json:"mediaType"`
			Data      string `json:"data"`
		} `json:"images"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}
	// Image attachments: base64-decoded, sanity-capped, and only
	// image/* media types are accepted. At least text OR one image is
	// required to start a turn.
	imgBlocks := make([]llm.ContentBlock, 0, len(body.Images))
	for _, img := range body.Images {
		if !strings.HasPrefix(img.MediaType, "image/") || img.Data == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil || len(raw) > 8<<20 {
			continue
		}
		imgBlocks = append(imgBlocks, llm.ContentBlock{Type: "image", MediaType: img.MediaType, Data: img.Data})
	}
	if len(imgBlocks) > 6 {
		imgBlocks = imgBlocks[:6]
	}
	if strings.TrimSpace(body.Input) == "" && len(imgBlocks) == 0 {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}
	if body.SessionID != "" && !validSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	s.runMu.Lock()
	defer s.runMu.Unlock()
	if body.SessionID == "" {
		body.SessionID = s.store.NewSessionID()
		cwd, _ := os.Getwd()
		if err := s.store.WriteHeaderFull(session.Header{
			ID:       body.SessionID,
			Provider: s.freshProviderName,
			Model:    s.freshModel,
			System:   s.freshSystem,
			WorkDir:  cwd,
			Mode:     string(s.freshPermissionMode),
			Effort:   effortHeaderValue(s.loop.EffortValue()),
			Preset:   s.freshPreset,
			Status:   "idle",
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
	}
	t0 := time.Now()
	hdr, history, err := s.store.Load(body.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	loadMs := time.Since(t0).Milliseconds()
	t0 = time.Now()
	if err := s.activateSession(body.SessionID, hdr, history); err != nil {
		writeError(w, http.StatusConflict, "failed to activate session: "+err.Error())
		return
	}
	activateMs := time.Since(t0).Milliseconds()
	if loadMs > 100 || activateMs > 100 {
		log.Printf("turn pre-request: history load=%dms activate=%dms (session %s, %d msgs)", loadMs, activateMs, body.SessionID, len(history))
	}
	input := strings.TrimSpace(body.Input)
	messageMetric := session.MessageMetric{
		Turn:      s.nextMessageMetricTurn(body.SessionID, history),
		StartedAt: time.Now(),
	}
	// Trajectory anchor: USER row + per-turn TTFT (first assistant text
	// minus this timestamp). No-op when tracing is disabled.
	rtpkg.RecordUserMessage(body.SessionID, input)
	user := llm.Message{Role: llm.RoleUser}
	if input != "" {
		user.Content = append(user.Content, llm.ContentBlock{Type: "text", Text: input})
	}
	user.Content = append(user.Content, imgBlocks...)
	if len(imgBlocks) > 0 {
		s.loop.AppendUserBlocks(user.Content)
	} else {
		s.loop.AppendUser(input)
	}
	if err := s.store.AppendMessage(body.SessionID, user); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist input")
		return
	}
	// The user-facing prompt written above can intentionally differ from the
	// loop-only message (for example a synthetic plan reminder). Anchor the
	// durable cursor to the live boundary so the normal append path preserves
	// that display-safe on-disk prompt, while still detecting a later history
	// replacement caused by automatic compaction.
	persistedBoundary := s.loop.History()
	historyCursor := session.NewHistoryCursor(persistedBoundary)
	previousCheckpoint := s.loop.CompactionCheckpoint
	s.loop.CompactionCheckpoint = func(before, after []llm.Message) error {
		return s.store.CheckpointCompaction(body.SessionID, before, after, &historyCursor)
	}
	defer func() { s.loop.CompactionCheckpoint = previousCheckpoint }()
	if len(history) == 0 {
		title := []rune(input)
		if len(title) > 60 {
			title = title[:60]
		}
		_ = s.store.SetTitle(body.SessionID, string(title))
	}
	if err := s.store.WriteHeaderFull(session.Header{ID: body.SessionID, Status: "running"}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist running status")
		return
	}
	finalStatus := "failed"
	defer func() { _ = s.store.WriteHeaderFull(session.Header{ID: body.SessionID, Status: finalStatus}) }()

	// Run under a cancellable context so the stop button can interrupt a
	// long turn (the model may stream for minutes before first token).
	turnCtx, cancel := context.WithCancel(r.Context())
	turnDone := make(chan struct{})
	s.cancelMu.Lock()
	s.cancelTurn = cancel
	s.runningSession = body.SessionID
	s.turnDone = turnDone
	s.cancelMu.Unlock()
	defer func() {
		s.cancelMu.Lock()
		if s.turnDone == turnDone {
			s.cancelTurn = nil
			s.runningSession = ""
			s.turnDone = nil
		}
		s.cancelMu.Unlock()
		close(turnDone)
		cancel()
	}()
	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() { done <- s.loop.Run(turnCtx, events); close(events) }()
	var text strings.Builder
	var firstTokenAt time.Time
	var outputTokens int64
	for ev := range events {
		// Permission requests are published by their case below (with the
		// request id); everything else broadcasts here.
		if ev.Kind != agent.EventPermissionRequest && ev.Kind != agent.EventAskUser {
			s.hub.publish(body.SessionID, ev)
		}
		switch ev.Kind {
		case agent.EventTextDelta:
			if firstTokenAt.IsZero() && ev.TextDelta != "" {
				firstTokenAt = time.Now()
			}
			text.WriteString(ev.TextDelta)
		case agent.EventThinkingDelta, agent.EventRedactedThinking:
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
		case agent.EventTokens:
			outputTokens += int64(ev.OutputTokens)
		case agent.EventPermissionRequest:
			if ev.PermissionReply != nil {
				id := uuid.NewString()
				pending := &permissionPending{reply: ev.PermissionReply, tool: ev.PermissionTool}
				s.permMu.Lock()
				if len(s.pendingPerms) >= 32 {
					// Safety valve: never let an abandoned browser accumulate
					// unbounded in-flight approvals.
					pending.reply <- agent.PermissionDecisionDeny
					s.permMu.Unlock()
					break
				}
				s.pendingPerms[id] = pending
				s.permMu.Unlock()
				// Broadcast the approval card; the reply arrives via /api/permission.
				s.hub.publish(body.SessionID, ev, map[string]any{"permId": id})
				go func() {
					time.Sleep(120 * time.Second)
					s.permMu.Lock()
					cur, ok := s.pendingPerms[id]
					if ok {
						delete(s.pendingPerms, id)
					}
					s.permMu.Unlock()
					if ok {
						cur.reply <- agent.PermissionDecisionDeny // timeout = deny
					}
				}()
			}
		case agent.EventAskUser:
			if ev.AskUserReply != nil {
				id := uuid.NewString()
				s.askMu.Lock()
				s.pendingAsks[id] = &askPending{reply: ev.AskUserReply}
				s.askMu.Unlock()
				s.hub.publish(body.SessionID, ev, map[string]any{"askId": id})
				go func() {
					time.Sleep(120 * time.Second)
					s.timeoutAsk(id)
				}()
			}
		}
	}
	runErr := <-done
	messageMetric.CompletedAt = time.Now()
	messageMetric.DurationMS = messageMetric.CompletedAt.Sub(messageMetric.StartedAt).Milliseconds()
	messageMetric.OutputTokens = outputTokens
	if !firstTokenAt.IsZero() && firstTokenAt.After(messageMetric.StartedAt) {
		messageMetric.TTFTMS = firstTokenAt.Sub(messageMetric.StartedAt).Milliseconds()
		if outputTokens > 0 && messageMetric.CompletedAt.After(firstTokenAt) {
			messageMetric.TokPerSec = float64(outputTokens) / messageMetric.CompletedAt.Sub(firstTokenAt).Seconds()
		}
	}
	if err := s.store.AppendMessageMetric(body.SessionID, messageMetric); err != nil {
		log.Printf("persist message metrics for %s turn %d: %v", body.SessionID, messageMetric.Turn, err)
	}
	if errors.Is(runErr, context.Canceled) {
		finalStatus = "stopped"
	}
	updated := s.loop.History()
	// The cursor already points at the latest durable compaction checkpoint
	// when one occurred. AppendHistoryTail therefore writes only messages
	// produced after it, while still falling back to history_replace for any
	// other same-length prefix rewrite. Persist before handling Run errors so
	// completed tool rounds and orphan repairs survive cancellation/failure.
	persistErr := s.store.AppendHistoryTail(body.SessionID, updated, &historyCursor)
	if persistErr != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist turn history")
		return
	}
	if errors.Is(runErr, context.Canceled) {
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": body.SessionID, "text": text.String(), "stopped": true})
		return
	}
	if runErr != nil {
		writeError(w, http.StatusBadGateway, runErr.Error())
		return
	}
	finalStatus = "completed"
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": body.SessionID, "text": text.String()})
}

func displayTurnCount(history []llm.Message) int {
	turns := 0
	for _, message := range history {
		if message.Role != llm.RoleUser {
			continue
		}
		for _, block := range message.Content {
			if block.Type != "text" {
				continue
			}
			visible := transcriptpkg.VisibleUserText(block.Text)
			if visible == "" || strings.HasPrefix(visible, "[user steer mid-turn] ") {
				continue
			}
			turns++
			break
		}
	}
	return turns
}

func (s *Server) nextMessageMetricTurn(sessionID string, history []llm.Message) int {
	maxTurn := displayTurnCount(history)
	if persisted, err := s.store.ReadMessageMetrics(sessionID); err == nil {
		for _, metric := range persisted {
			if metric.Turn > maxTurn {
				maxTurn = metric.Turn
			}
		}
	}
	if traceStore := rtpkg.CurrentTraceStore(); traceStore != nil {
		if traceTurn := traceStore.CurrentTurn(sessionID); traceTurn > maxTurn {
			maxTurn = traceTurn
		}
	}
	return maxTurn + 1
}

// activateSession moves the long-lived web runtime to id. A transcript-only
// Restore is insufficient here: provider/model, system prompt, permissions and
// every session-keyed sidecar must cross the boundary together. Provider
// construction and source-session persistence are the only fallible steps and
// happen before any live state is changed.
func (s *Server) activateSession(id string, hdr *session.Header, history []llm.Message) error {
	if s == nil || s.loop == nil || s.store == nil {
		return errors.New("agent runtime unavailable")
	}
	if !validSessionID(id) || hdr == nil {
		return errors.New("invalid session")
	}

	s.stateMu.RLock()
	activeID := s.activeSessionID
	activeProviderName := s.activeProviderName
	activeModel := s.activeModel
	s.stateMu.RUnlock()

	// Re-reading the active transcript is not a top-level boundary. It repairs
	// the in-memory view after a prior failed turn without clearing permission
	// state or session-scoped loop guards that still belong to this session.
	if activeID == id {
		s.loop.Restore(history)
		s.loop.SetEffort(effortFromHeader(hdr.Effort))
		s.loop.TimingSink = s.store.NewTimingRecorder(id).Record
		s.stateMu.Lock()
		if hdr.Preset != "" {
			s.activePreset = hdr.Preset
		}
		s.stateMu.Unlock()
		provider, _, _ := s.loop.ProviderModelSnapshot()
		rtpkg.RebindLoopRuntime(s.loop, provider, activeModel, s.loop.System, id)
		return nil
	}

	targetProviderName := hdr.Provider
	if targetProviderName == "" {
		targetProviderName = s.freshProviderName
	}
	if targetProviderName == "" {
		targetProviderName = activeProviderName
	}
	targetModel := hdr.Model
	if targetModel == "" {
		targetModel = s.freshModel
	}
	if targetModel == "" {
		targetModel = activeModel
	}
	targetSystem := s.freshSystem
	if hdr.System != "" {
		targetSystem = hdr.System
	}
	targetEffort := effortFromHeader(hdr.Effort)
	targetPreset := hdr.Preset
	if targetPreset == "" {
		targetPreset = "standard"
	}

	runtimeState := s.loop.ProviderRuntimeState()
	provider := runtimeState.Provider
	targetMaxOutputTokens := runtimeState.MaxOutputTokens
	needsProviderBuild := provider == nil || targetProviderName != activeProviderName || targetModel != activeModel
	if !needsProviderBuild && provider != nil && targetModel != "" {
		// ModelID is the transport truth. An empty value is permitted for
		// embedders/test providers that cannot report it.
		if wireModel := provider.ModelID(); wireModel != "" && wireModel != targetModel {
			needsProviderBuild = true
		}
	}
	if needsProviderBuild {
		if s.buildProvider == nil {
			return fmt.Errorf("stored provider/model %q/%q does not match live runtime %q/%q",
				targetProviderName, targetModel, activeProviderName, activeModel)
		}
		built, err := s.buildProvider(targetProviderName, targetModel)
		if err != nil {
			return fmt.Errorf("provider/model preflight: %w", err)
		}
		if built == nil || built.Provider == nil {
			return errors.New("provider/model preflight returned no provider")
		}
		provider = built.Provider
		targetMaxOutputTokens = built.MaxOutputTokens
		if built.Model != "" {
			targetModel = built.Model
		}
	}

	if err := s.persistActiveSessionState(); err != nil {
		return err
	}
	if s.sessionBoundary != nil {
		s.sessionBoundary()
	}

	mode := s.freshPermissionMode
	if hdr.Mode != "" {
		mode = permission.Mode(hdr.Mode)
	}
	resumedRules := make([]permission.Rule, 0, len(hdr.AlwaysAllow))
	for _, rule := range hdr.AlwaysAllow {
		resumedRules = append(resumedRules, permission.Rule{
			Tool:   rule.Tool,
			Match:  rule.Match,
			Verb:   permission.Decision(rule.Verb),
			Source: permission.ResumedSessionSource(rule.Source),
		})
	}

	canonicalTargetSystem, canonicalTargetSections := rtpkg.RebindProviderPrompt(
		s.freshSystem, s.freshSystemSections, targetProviderName, targetModel,
	)
	targetSections := []llm.SystemSection(nil)
	if targetSystem == s.freshSystem || targetSystem == canonicalTargetSystem {
		// Model switches persist the flattened, provider-rebound prompt for
		// backwards-compatible session files. Recognize that canonical form on
		// resume and restore the typed sections too; otherwise the next switch
		// cannot remove the old provider_hint without flattening custom text.
		targetSystem = canonicalTargetSystem
		targetSections = canonicalTargetSections
	}
	s.loop.RebindProviderRuntime(provider, targetModel, targetMaxOutputTokens, targetSystem, targetSections)
	s.loop.SetEffort(targetEffort)
	if s.loop.Gate != nil {
		s.loop.Gate.ResetSessionState(mode, resumedRules)
	}
	s.loop.SetPrePlanMode("")
	s.loop.ResetSession(history)
	s.loop.TimingSink = s.store.NewTimingRecorder(id).Record
	rtpkg.RebindLoopRuntime(s.loop, provider, targetModel, targetSystem, id)

	s.stateMu.Lock()
	s.activeSessionID = id
	s.activeProviderName = targetProviderName
	s.activeModel = targetModel
	s.activePreset = targetPreset
	s.stateMu.Unlock()
	if s.sessionSwitch != nil {
		s.sessionSwitch(id)
	}
	return nil
}

// persistActiveSessionState preserves only state whose lifetime is the chat
// being left. Process/config/CLI rules remain in Gate and must not be copied
// into a user-editable session header with elevated authority.
func (s *Server) persistActiveSessionState() error {
	s.stateMu.RLock()
	id := s.activeSessionID
	providerName := s.activeProviderName
	model := s.activeModel
	preset := s.activePreset
	s.stateMu.RUnlock()
	runtimeState := s.loop.ProviderRuntimeState()
	return s.writeActiveSessionState(
		id, providerName, model, preset, runtimeState.System, s.loop.EffortValue(),
	)
}

func (s *Server) writeActiveSessionState(id, providerName, model, preset, system string, effort llm.Effort) error {
	if id == "" {
		return nil
	}

	hdr := session.Header{
		ID:       id,
		Provider: providerName,
		Model:    model,
		System:   system,
		Effort:   effortHeaderValue(effort),
		Preset:   preset,
	}
	if s.loop.Gate != nil {
		hdr.Mode = string(s.loop.Gate.Mode())
		for _, rule := range s.loop.Gate.Snapshot() {
			if rule.Source != "interactive" && !strings.HasPrefix(rule.Source, "session:") {
				continue
			}
			hdr.AlwaysAllow = append(hdr.AlwaysAllow, session.SavedRule{
				Tool: rule.Tool, Match: rule.Match, Verb: int(rule.Verb), Source: rule.Source,
			})
		}
		hdr.ClearAlwaysAllow = len(hdr.AlwaysAllow) == 0
	}
	if err := s.store.WriteHeaderFull(hdr); err != nil {
		return fmt.Errorf("persist active session %s: %w", id, err)
	}
	return nil
}

// validSessionID keeps transcript lookup and every session-keyed sidecar on
// the same storage identity. Store.Load defensively applies filepath.Base,
// but task/checkpoint/prompt routers receive the original ID; accepting a path
// alias here would therefore validate one session and bind another path.
func validSessionID(id string) bool {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." ||
		filepath.Base(id) != id || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// secretConfigLine matches toml lines that carry credentials.
var secretConfigLine = regexp.MustCompile(`(?i)^(\s*[\w.]*(?:api_key|apikey|token|secret|password)[\w.]*\s*=\s*).+$`)

// redactSecrets masks credential values before the browser sees a config file.
func redactSecrets(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if m := secretConfigLine.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + `"***redacted***"`
		}
	}
	return strings.Join(lines, "\n")
}

// redactTraceText removes credential-shaped JSON fields before a trajectory
// payload crosses into the browser. Trace data intentionally remains useful
// for debugging, but values under authentication keys are never needed there.
// Non-JSON tool output is preserved verbatim because broad regex replacement
// would corrupt source code, logs, and ordinary prose.
func redactTraceText(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return content
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return content
	}
	redactTraceJSON(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return content
	}
	return string(encoded)
}

func redactTraceJSON(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch normalized {
			case "apikey", "accesstoken", "refreshtoken", "token", "secret", "clientsecret", "password", "authorization", "proxyauthorization", "cookie", "setcookie":
				typed[key] = "***redacted***"
			default:
				redactTraceJSON(child)
			}
		}
	case []any:
		for _, child := range typed {
			redactTraceJSON(child)
		}
	}
}

const fileEditInputSSELimit = 64 * 1024

func toolInputSSELimit(tool string) int {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "edit", "write":
		// File-edit rows need the structured old/new or content fields to
		// produce a useful live diff. Saved history already retains the same
		// input. Bound the live payload so a pathological Write cannot turn one
		// SSE frame into a multi-megabyte response.
		return fileEditInputSSELimit
	default:
		return 400
	}
}

// truncateSSE clips text to n runes without splitting multibyte chars.
func truncateSSE(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n]) + "...(truncated)"
}

// handlePermission resolves one queued tool-approval card from the browser.
func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.permMu.Lock()
	pending, ok := s.pendingPerms[body.ID]
	if ok {
		delete(s.pendingPerms, body.ID)
	}
	s.permMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "permission request expired or already resolved")
		return
	}
	decision := agent.PermissionDecisionDeny
	if body.Approve {
		decision = agent.PermissionDecisionAllow
	}
	pending.reply <- decision
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true})
}

func (s *Server) takePendingAsk(id string) (*askPending, bool) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	pending, ok := s.pendingAsks[id]
	if ok {
		delete(s.pendingAsks, id)
	}
	return pending, ok
}

func (s *Server) timeoutAsk(id string) {
	if pending, ok := s.takePendingAsk(id); ok {
		pending.reply <- "" // timeout: empty fallback
	}
}

// handleAsk resolves one queued AskUser question with the user's answer.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID     string `json:"id"`
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	pending, ok := s.takePendingAsk(body.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "question expired or already answered")
		return
	}
	pending.reply <- body.Answer
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true})
}

// handleRename updates a session title from the sidebar action menu.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !validSessionID(body.ID) || s.store == nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}
	if err := s.store.SetTitle(body.ID, title); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"title": title})
}

// handleArchive toggles recoverable session archive metadata. The transcript,
// trace, feedback and exports remain untouched.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID       string `json:"id"`
		Archived bool   `json:"archived"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || !validSessionID(body.ID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "session store unavailable")
		return
	}
	var err error
	if body.Archived {
		err = s.store.Archive(body.ID)
	} else {
		err = s.store.Unarchive(body.ID)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": body.ID, "archived": body.Archived})
}

// handleFeedback records a log-only human remark on a session (DSH
// command-feedback parity). The entry never enters model context; it is
// appended to the JSONL as a "feedback" entry the resume path ignores.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Kind      string `json:"kind"`
		Text      string `json:"text"`
		Rating    string `json:"rating"`
		MsgIdx    string `json:"msgIdx"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !validSessionID(body.SessionID) || s.store == nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if body.Kind == "rating" {
		// Message-level 👍/👎: rating must be up/down; msgIdx optional.
		if body.Rating != "up" && body.Rating != "down" {
			writeError(w, http.StatusBadRequest, "rating must be up or down")
			return
		}
		if err := s.store.AppendRating(body.SessionID, body.Rating, body.MsgIdx); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rating": body.Rating})
		return
	}
	kind := body.Kind
	if kind != "remark" && kind != "note" {
		kind = "remark"
	}
	text := strings.TrimSpace(body.Text)
	if len([]rune(text)) > 2000 {
		text = string([]rune(text)[:2000])
	}
	if err := s.store.AppendFeedback(body.SessionID, kind, text); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleFork branches the conversation at a message: the new session keeps
// the header and the transcript up to (and including) messageIndex.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID    string `json:"sessionId"`
		MessageIndex int    `json:"messageIndex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !validSessionID(body.SessionID) || s.store == nil || body.MessageIndex < -1 {
		writeError(w, http.StatusBadRequest, "invalid session or index")
		return
	}
	hdr, messages, err := s.store.Load(body.SessionID)
	if err != nil || hdr == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if body.MessageIndex == -1 {
		body.MessageIndex = len(messages) - 1
	}
	if body.MessageIndex >= len(messages) {
		writeError(w, http.StatusBadRequest, "message index out of range")
		return
	}
	newID := s.store.NewSessionID()
	// WriteHeaderFull stores the entry under h.ID, so the copied header must
	// be re-pointed at the new session - otherwise the branch writes a
	// duplicate header into the ORIGINAL session's file and the new session
	// is left headerless (invisible to resume).
	newHeader := *hdr
	newHeader.ID = newID
	newHeader.CreatedAt = time.Now()
	if err := s.store.WriteHeaderFull(newHeader); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.SetTitle(newID, hdr.Title+" (branch)")
	for i := 0; i <= body.MessageIndex; i++ {
		if err := s.store.AppendMessage(newID, messages[i]); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": newID})
}

// handleConfigFile serves the user/project config.toml contents so the
// settings Configuration tab can show the real files.
func (s *Server) handleConfigFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userPath := filepath.Join(config.Home(), "config.toml")
	userContent, _ := os.ReadFile(userPath)
	projectPath := filepath.Join(".metis", "config.toml")
	projectContent, _ := os.ReadFile(projectPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"userPath":       userPath,
		"userContent":    redactSecrets(string(userContent)),
		"projectPath":    projectPath,
		"projectContent": redactSecrets(string(projectContent)),
	})
}

// handleClipboardFiles resolves the native file URLs represented by a
// browser paste event. WKWebView intentionally exposes Finder directories as
// zero-byte File objects with only a basename, so the absolute path must come
// from the platform pasteboard. The browser-provided names are matched again
// after the native read to fail closed if the clipboard changed meanwhile.
func (s *Server) handleClipboardFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.clipboardFiles == nil {
		writeError(w, http.StatusNotImplemented, "native clipboard file paths are unavailable")
		return
	}
	var body struct {
		Names []string `json:"names"`
		All   bool     `json:"all"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := decoder.Decode(&body); err != nil || (!body.All && len(body.Names) == 0) || len(body.Names) > 64 {
		writeError(w, http.StatusBadRequest, "one to 64 clipboard item names are required")
		return
	}
	expected := make(map[string]struct{}, len(body.Names))
	for _, name := range body.Names {
		if !validClipboardItemName(name) {
			writeError(w, http.StatusBadRequest, "invalid clipboard item name")
			return
		}
		expected[name] = struct{}{}
	}
	items, err := s.clipboardFiles()
	if errors.Is(err, desktop.ErrClipboardFilesUnsupported) {
		writeError(w, http.StatusNotImplemented, "native clipboard file paths are unavailable")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "unable to read native clipboard file paths")
		return
	}
	if body.All {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"files": items})
		return
	}
	matched := make([]desktop.ClipboardFile, 0, len(expected))
	seen := make(map[string]struct{}, len(expected))
	for _, item := range items {
		if _, wanted := expected[item.Name]; !wanted {
			continue
		}
		if _, duplicate := seen[item.Name]; duplicate {
			continue
		}
		seen[item.Name] = struct{}{}
		matched = append(matched, item)
	}
	if len(seen) != len(expected) {
		writeError(w, http.StatusConflict, "clipboard changed before its file paths could be resolved")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"files": matched})
}

func validClipboardItemName(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

const traceRedactedThinkingPlaceholder = "Reasoning redacted by provider"

// traceFromHistory synthesizes a trajectory from the stored transcript
// when no live trace exists. User messages open turns; assistant thinking
// and text remain separate rows; tool_use/tool_result blocks become nested
// rows the same way the live trace renders them.
func traceFromHistory(s *Server, sid string) []session.TracedNode {
	if s.store == nil {
		return nil
	}
	_, messages, err := s.store.Load(sid)
	if err != nil {
		return nil
	}
	var nodes []session.TracedNode
	turn := 0
	seq := int64(0)
	ts := time.Now()
	add := func(depth int, ev session.TraceEvent) {
		seq++
		ev.Sequence = seq
		ev.Turn = turn
		if ev.TS.IsZero() {
			ev.TS = ts
		}
		ev.SessionID = sid
		nodes = append(nodes, session.TracedNode{Event: ev, Depth: depth})
	}
	for _, msg := range messages {
		if msg.Role == llm.RoleUser {
			turn++
		}
		for _, block := range msg.Content {
			switch block.Type {
			case "thinking":
				if msg.Role != llm.RoleAssistant || strings.TrimSpace(block.Text) == "" {
					continue
				}
				add(0, session.TraceEvent{Kind: "thinking", Text: block.Text})
			case "redacted_thinking":
				if msg.Role != llm.RoleAssistant {
					continue
				}
				// Data is opaque provider ciphertext needed for round-tripping.
				// Never copy it into the user-visible trajectory.
				add(0, session.TraceEvent{Kind: "thinking_redacted", Text: traceRedactedThinkingPlaceholder})
			case "text":
				if strings.TrimSpace(block.Text) == "" {
					continue
				}
				kind := "text"
				if msg.Role == llm.RoleUser {
					kind = "user"
				}
				add(0, session.TraceEvent{Kind: kind, Text: block.Text})
			case "tool_use":
				input, _ := json.Marshal(block.ToolInput)
				add(0, session.TraceEvent{
					Kind:      "tool_start",
					ToolName:  block.ToolName,
					ToolUseID: block.ToolUseID,
					Text:      truncateSSE(string(input), 800),
				})
			case "tool_result":
				add(1, session.TraceEvent{
					Kind:      "tool_result",
					ToolName:  block.ToolName,
					ToolUseID: block.ToolUseID,
					ParentID:  block.ToolUseID,
					Text:      truncateSSE(block.ToolResult, 800),
					IsError:   block.IsError,
				})
			}
		}
	}
	return nodes
}

// webModel is one switchable model/profile from config.
type webModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Label    string `json:"label"`
}

// handleModels lists the configured models (GET) and switches the active
// model (POST) by rebuilding the provider the same way activateSession does.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg, _, err := config.Load()
	if err != nil || cfg == nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		models := listConfiguredModels(cfg)
		s.stateMu.RLock()
		curProvider, curModel := s.activeProviderName, s.activeModel
		s.stateMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"models":  models,
			"current": map[string]string{"provider": curProvider, "model": curModel},
		})
	case http.MethodPost:
		if !s.runMu.TryLock() {
			writeError(w, http.StatusConflict, "cannot switch model while a turn is running")
			return
		}
		defer s.runMu.Unlock()
		if s.loop == nil {
			writeError(w, http.StatusServiceUnavailable, "agent loop unavailable")
			return
		}
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
			writeError(w, http.StatusBadRequest, "model is required")
			return
		}
		// Resolve provider from config if not supplied.
		if body.Provider == "" {
			for _, m := range listConfiguredModels(cfg) {
				if m.Model == body.Model {
					body.Provider = m.Provider
					break
				}
			}
		}
		if body.Provider == "" {
			body.Provider = cfg.Provider.Default
		}
		if s.buildProvider == nil {
			writeError(w, http.StatusServiceUnavailable, "provider construction unavailable")
			return
		}
		built, err := s.buildProvider(body.Provider, body.Model)
		if err != nil || built == nil || built.Provider == nil {
			if err == nil {
				err = errors.New("provider construction returned no provider")
			}
			writeError(w, http.StatusBadRequest, "provider build failed: "+err.Error())
			return
		}
		selectedModel := body.Model
		if built.Model != "" {
			selectedModel = built.Model
		}
		previousRuntime := s.loop.ProviderRuntimeState()
		newSystem, newSections := rtpkg.RebindProviderPrompt(
			previousRuntime.System, previousRuntime.SystemSections, body.Provider, selectedModel,
		)
		capability := reasoningEffortCapability(cfg, body.Provider, selectedModel)
		selectedEffort := s.loop.EffortValue()
		if !capability.Supported {
			selectedEffort = llm.EffortDefault
		}
		// Persist the desired metadata before mutating the live runtime. A disk
		// failure therefore leaves provider, prompt, effort and selector exactly
		// as they were instead of requiring a lossy provider reconstruction.
		if err := s.commitActiveModelSelectionState(
			body.Provider, selectedModel, newSystem, selectedEffort,
		); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.loop.RebindProviderRuntime(
			built.Provider, selectedModel, built.MaxOutputTokens, newSystem, newSections,
		)
		s.loop.SetEffort(selectedEffort)
		s.stateMu.RLock()
		activeSessionID := s.activeSessionID
		s.stateMu.RUnlock()
		// Built-in Agent/Fork tools and the lazy pricing resolver capture the
		// active provider. Keep them on the same atomic model boundary as the
		// main loop instead of leaving child work on the old transport.
		rtpkg.RebindLoopRuntime(s.loop, built.Provider, selectedModel, newSystem, activeSessionID)
		writeJSON(w, http.StatusOK, map[string]any{
			"provider": body.Provider, "model": selectedModel,
			"effortSupported": capability.Supported, "effortReason": capability.Reason,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// commitActiveModelSelection keeps the live selector and durable resume
// metadata in sync after a successful provider rebuild.
func (s *Server) commitActiveModelSelection(providerName, model string) error {
	runtimeState := s.loop.ProviderRuntimeState()
	return s.commitActiveModelSelectionState(
		providerName, model, runtimeState.System, s.loop.EffortValue(),
	)
}

func (s *Server) commitActiveModelSelectionState(providerName, model, system string, effort llm.Effort) error {
	s.stateMu.RLock()
	id := s.activeSessionID
	preset := s.activePreset
	s.stateMu.RUnlock()
	if err := s.writeActiveSessionState(id, providerName, model, preset, system, effort); err != nil {
		return fmt.Errorf("persist model selection: %w", err)
	}
	s.stateMu.Lock()
	s.activeProviderName = providerName
	s.activeModel = model
	s.stateMu.Unlock()
	return nil
}

// listConfiguredModels enumerates switchable models: built-in providers with
// a configured model, plus every custom profile.
func listConfiguredModels(cfg *config.Config) []webModel {
	var out []webModel
	add := func(provider, model string) {
		if provider == "" || model == "" {
			return
		}
		out = append(out, webModel{Provider: provider, Model: model, Label: provider + " · " + model})
	}
	add("anthropic", cfg.Provider.Anthropic.Model)
	add("openai", cfg.Provider.OpenAI.Model)
	add("gemini", cfg.Provider.Gemini.Model)
	for name, raw := range cfg.Provider.Custom {
		add(name, raw.Model)
	}
	return out
}

// handleStatus reports live sub-agent and background-task counts for the
// status bar chip ("N sub-agents ~ M background tasks").
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	subAgents := 0
	agentDetails := make([]map[string]any, 0)
	if s.roster != nil {
		subAgents = s.roster.Summary().Total
		for _, teammate := range s.roster.List() {
			if teammate == nil {
				continue
			}
			snap := teammate.Snapshot()
			agentDetails = append(agentDetails, map[string]any{
				"name": snap.Name, "agentId": snap.AgentID,
				"status": snap.Status.String(), "background": snap.Background,
				"startedAt": snap.Started,
			})
			if len(agentDetails) >= 12 {
				break
			}
		}
	}
	backgroundTasks := 0
	jobDetails := make([]map[string]any, 0)
	if s.loop != nil && s.loop.Jobs != nil {
		for _, j := range s.loop.Jobs.List() {
			if j.Status == jobs.StatusRunning {
				backgroundTasks++
			}
			if len(jobDetails) < 12 {
				jobDetails = append(jobDetails, map[string]any{
					"id": j.ID, "description": j.Description,
					"status": j.Status.String(), "startedAt": j.StartTime,
				})
			}
		}
	}
	workspace := "metis"
	if wd, err := os.Getwd(); err == nil {
		workspace = filepath.Base(wd)
	}
	contextUsed, contextWindow := 0, 0
	compactThreshold := 0.0
	compactAtTokens := 0
	toolNames := make([]string, 0)
	if s.loop != nil {
		contextUsed = s.loop.EstimateContextTokens()
		contextWindow, compactThreshold, compactAtTokens = s.loop.ContextStatusSnapshot()
		if s.loop.Registry != nil {
			for _, tool := range s.loop.Registry.All() {
				if tool != nil {
					toolNames = append(toolNames, tool.Name())
				}
			}
		}
	}
	s.cancelMu.Lock()
	runningSessionID := s.runningSession
	turnRunning := s.turnDone != nil && runningSessionID != ""
	s.cancelMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"subAgents": subAgents, "backgroundTasks": backgroundTasks, "workspace": workspace,
		"agents": agentDetails, "jobs": jobDetails,
		"toolCount": len(toolNames), "tools": toolNames,
		"contextUsed": contextUsed, "contextWindow": contextWindow, "compactThreshold": compactThreshold,
		"compactAtTokens": compactAtTokens,
		"turnRunning":     turnRunning, "runningSessionId": runningSessionID,
		"build": s.buildVersion,
	})
}

// handleSteer folds a busy-composer message into the in-flight agent run.
// Loop.SteerInject owns the final-response race: false means the run already
// closed its steering gate, so the browser can safely fall back to its FIFO
// next-turn queue without losing the user's text.
func (s *Server) handleSteer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.loop == nil {
		writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Input     string `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.Input = strings.TrimSpace(body.Input)
	if body.Input == "" {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}
	s.stateMu.RLock()
	activeID := s.activeSessionID
	s.stateMu.RUnlock()
	if body.SessionID == "" {
		body.SessionID = activeID
	}
	if !validSessionID(body.SessionID) || body.SessionID != activeID {
		writeError(w, http.StatusConflict, "session is not the active turn")
		return
	}
	s.cancelMu.Lock()
	running := s.cancelTurn != nil
	s.cancelMu.Unlock()
	if !running {
		writeError(w, http.StatusConflict, "no turn in progress")
		return
	}
	if !s.loop.SteerInject(body.Input) {
		writeError(w, http.StatusConflict, "turn no longer accepts steering")
		return
	}
	rtpkg.RecordUserMessage(body.SessionID, "[steer] "+body.Input)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "sessionId": body.SessionID})
}

// handleStop cancels the in-flight turn (the composer's stop button). The
// optional session id prevents a stale/background view from stopping the
// wrong turn, and the response only reports "stopped" after the turn handler
// has unwound. A slow tool gets a truthful 202 "stopping" response instead.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid stop request")
			return
		}
		if body.SessionID != "" && !validSessionID(body.SessionID) {
			writeError(w, http.StatusBadRequest, "invalid session id")
			return
		}
	}

	s.cancelMu.Lock()
	cancel := s.cancelTurn
	runningSession := s.runningSession
	done := s.turnDone
	s.cancelMu.Unlock()
	if cancel == nil {
		writeError(w, http.StatusConflict, "no turn in progress")
		return
	}
	if body.SessionID != "" && runningSession != "" && body.SessionID != runningSession {
		writeError(w, http.StatusConflict, "requested session is not the running turn")
		return
	}

	cancel()
	s.cancelPendingInteractions()
	if done != nil {
		select {
		case <-done:
			writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "sessionId": runningSession})
			return
		case <-time.After(2 * time.Second):
			writeJSON(w, http.StatusAccepted, map[string]any{"stopping": true, "sessionId": runningSession})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "sessionId": runningSession})
}

// cancelPendingInteractions releases approval and AskUser waits immediately.
// Context cancellation alone cannot finish a loop that is blocked waiting for
// one of these browser replies, which made the old stop button appear broken.
func (s *Server) cancelPendingInteractions() {
	s.permMu.Lock()
	permissions := s.pendingPerms
	s.pendingPerms = make(map[string]*permissionPending)
	s.permMu.Unlock()
	for _, pending := range permissions {
		select {
		case pending.reply <- agent.PermissionDecisionDeny:
		default:
		}
	}

	s.askMu.Lock()
	asks := s.pendingAsks
	s.pendingAsks = make(map[string]*askPending)
	s.askMu.Unlock()
	for _, pending := range asks {
		select {
		case pending.reply <- "":
		default:
		}
	}
}

// handleExport writes the session transcript as a glyph-led txt export,
// byte-identical to the CLI /export command (same sanitization, same
// ~/.metis/exports directory, 0600 perms). The UI presents the file name and
// keeps the full path behind explicit copy/open-folder actions.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !validSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	_, messages, err := s.store.Load(body.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	path, err := tui.ExportConversation(messages, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path})
}

// handleExportsOpen reveals the trusted exports directory. It deliberately
// accepts no caller-provided path so the Desktop cannot be used as an
// arbitrary local-path opener.
func (s *Server) handleExportsOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.openPath == nil {
		writeError(w, http.StatusServiceUnavailable, "open path unavailable")
		return
	}
	dir := filepath.Join(config.Home(), "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "create exports directory: "+err.Error())
		return
	}
	if err := s.openPath(dir); err != nil {
		writeError(w, http.StatusInternalServerError, "open exports directory: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir})
}

// handleTraceExport writes the session's trajectory as a readable txt file
// (stats header + indented event tree) into ~/.metis/exports and returns the
// path. Mirrors the export convention of handleExport.
func (s *Server) handleTraceExport(w http.ResponseWriter, r *http.Request) {
	// POST (not GET): this endpoint writes a file, and safe methods are
	// exempt from sameOriginOnly's cross-origin checks.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
	sid := body.SessionID
	if sid == "" {
		sid = r.URL.Query().Get("sessionId")
	}
	if sid == "" {
		s.stateMu.RLock()
		sid = s.activeSessionID
		s.stateMu.RUnlock()
	}
	if !validSessionID(sid) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	store := rtpkg.CurrentTraceStore()
	if store == nil {
		writeError(w, http.StatusNotFound, "session tracing is not enabled for this process")
		return
	}
	nodes := store.Trace(sid)
	if len(nodes) == 0 {
		nodes = traceFromHistory(s, sid)
	}
	if len(nodes) == 0 {
		writeError(w, http.StatusNotFound, "no trajectory recorded for this session")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Metis trajectory - session %s\nexported %s\n\n", sid, time.Now().Format(time.RFC3339))
	var calls, errs int
	for _, n := range nodes {
		ev := n.Event
		if ev.Kind == "tool_start" {
			calls++
		}
		if ev.Kind == "error" || ev.IsError {
			errs++
		}
	}
	fmt.Fprintf(&b, "events=%d tool_calls=%d errors=%d\n\n", len(nodes), calls, errs)
	for _, n := range nodes {
		ev := n.Event
		indent := strings.Repeat("  ", n.Depth)
		line := fmt.Sprintf("%s#%d [turn %d] %s", indent, ev.Sequence, ev.Turn, ev.Kind)
		if ev.ToolName != "" {
			line += " " + ev.ToolName
		}
		if ev.ElapsedMs > 0 {
			line += fmt.Sprintf(" (%dms)", ev.ElapsedMs)
		}
		if ev.IsError {
			line += " [ERROR]"
		}
		if text := strings.TrimSpace(ev.Text); text != "" {
			for i, tl := range strings.Split(text, "\n") {
				if i == 0 {
					line += " | " + tl
				} else {
					line += "\n" + indent + "    " + tl
				}
			}
		}
		b.WriteString(line + "\n")
	}

	dir := filepath.Join(config.Home(), "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := time.Now().Format("2006-01-02-150405") + "-" + sanitizeTraceExportName(sid) + "-trace.txt"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path})
}

// sanitizeTraceExportName keeps session ids safe inside export filenames.
func sanitizeTraceExportName(sid string) string {
	var out strings.Builder
	for _, r := range sid {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

// webSetting is one user-editable config entry exposed to the web UI,
// mirroring the TUI /config panel's shape.
type webSetting struct {
	Key             string   `json:"key"`
	Label           string   `json:"label"`
	Description     string   `json:"description"`
	Type            string   `json:"type"` // enum | number | bool
	Value           string   `json:"value"`
	Options         []string `json:"options,omitempty"`
	LockedReason    string   `json:"lockedReason,omitempty"`
	RestartRequired bool     `json:"restartRequired"`
}

// allowedWebSettings is the settings whitelist. Everything else remains
// TUI-only or is not editable from the browser.
var allowedWebSettings = map[string]bool{
	"permission.mode":                     true,
	"ui.theme":                            true,
	"ui.thinking_display":                 true,
	"session.auto_compact_threshold":      true,
	"session.auto_compact_minimum_tokens": true,
	"session.max_iterations":              true,
	"loop_detection.disabled":             true,
}

var allowedWebThemes = map[string]bool{
	"auto": true, "dark": true, "light": true,
	"dark-daltonized": true, "nord": true, "solarized-dark": true,
}

// handleSettings serves the editable settings list (GET) and persists
// validated changes (POST) via the same SaveUserSettingsAndLoad path the
// TUI /config panel uses.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSettings(w)
	case http.MethodPost:
		s.saveSettings(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listSettings(w http.ResponseWriter) {
	cfg, _, err := config.Load()
	if err != nil || cfg == nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	permValue := cfg.Permission.Mode
	if s.loop != nil && s.loop.Gate != nil {
		permValue = string(s.loop.Gate.Mode())
	}
	if _, ok := permission.ParseMode(permValue); !ok {
		permValue = string(permission.ModeDefault)
	}
	items := []webSetting{
		{Key: "permission.mode", Label: "Permission mode", Description: "Default tool approval policy (applies immediately)", Type: "enum",
			Value: permValue, Options: []string{"default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"}},
		{Key: "ui.theme", Label: "Theme", Description: "Web UI color scheme (applies immediately)", Type: "enum",
			Value: cfg.UI.Theme, Options: []string{"auto", "dark", "light", "dark-daltonized", "nord", "solarized-dark"}},
		{Key: "ui.thinking_display", Label: "Thinking display", Description: "Provider reasoning rows: show, hide, or auto (applies after restart)", Type: "enum",
			Value: cfg.UI.ThinkingDisplay, Options: []string{"show", "hide", "auto"}, RestartRequired: true},
		{Key: "session.auto_compact_threshold", Label: "Auto-compact threshold", Description: "Context fraction that triggers compaction", Type: "number",
			Value: strconv.FormatFloat(cfg.Session.AutoCompactThreshold, 'g', -1, 64), RestartRequired: true},
		{Key: "session.auto_compact_minimum_tokens", Label: "Compact minimum tokens", Description: "Absolute auto-compaction floor", Type: "number",
			Value: strconv.Itoa(cfg.Session.AutoCompactMinimumTokens), RestartRequired: true},
		{Key: "session.max_iterations", Label: "Max iterations", Description: "Maximum tool-loop iterations per turn", Type: "number",
			Value: strconv.Itoa(cfg.Session.MaxIterations), RestartRequired: true},
		{Key: "loop_detection.disabled", Label: "Loop detection disabled", Description: "Repeated-tool loop detection", Type: "bool",
			Value: strconv.FormatBool(cfg.LoopDetection.Disabled), RestartRequired: true},
	}
	for i := range items {
		if source, serr := config.UserSettingOverrideSource(items[i].Key); serr != nil {
			items[i].LockedReason = "project config is unreadable"
		} else if source != "" {
			items[i].LockedReason = "controlled by " + source
		}
	}
	s.stateMu.RLock()
	model := s.activeModel
	s.stateMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"settings": items, "model": model})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Changes []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(body.Changes) == 0 {
		writeError(w, http.StatusBadRequest, "no changes")
		return
	}
	var settings []config.UserSetting
	for _, c := range body.Changes {
		if !allowedWebSettings[c.Key] {
			writeError(w, http.StatusBadRequest, "unsupported setting: "+c.Key)
			return
		}
		if source, err := config.UserSettingOverrideSource(c.Key); err != nil {
			writeError(w, http.StatusConflict, "setting source unreadable")
			return
		} else if source != "" {
			writeError(w, http.StatusConflict, c.Key+" is controlled by "+source)
			return
		}
		if !validWebSettingValue(c.Key, c.Value) {
			writeError(w, http.StatusBadRequest, "invalid value for "+c.Key)
			return
		}
		settings = append(settings, config.UserSetting{Key: c.Key, Value: c.Value})
	}
	if _, err := config.SaveUserSettingsAndLoad(settings); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var live, restart []string
	for _, c := range body.Changes {
		switch c.Key {
		case "permission.mode":
			if s.loop != nil && s.loop.Gate != nil {
				s.applyPermissionMode(permission.CanonicalMode(c.Value))
				live = append(live, c.Key)
			} else {
				restart = append(restart, c.Key)
			}
		case "ui.theme":
			// Persisted now; the browser swaps the palette client-side.
			live = append(live, c.Key)
		default:
			restart = append(restart, c.Key)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": len(settings), "liveApplied": live, "restartRequired": restart})
}

func validWebSettingValue(key, value string) bool {
	switch key {
	case "permission.mode":
		_, ok := permission.ParseMode(value)
		return ok
	case "ui.theme":
		return allowedWebThemes[value]
	case "ui.thinking_display":
		return value == "show" || value == "hide" || value == "auto"
	case "loop_detection.disabled":
		_, err := strconv.ParseBool(value)
		return err == nil
	case "session.auto_compact_threshold":
		f, err := strconv.ParseFloat(value, 64)
		return err == nil && f > 0 && f <= 1
	case "session.auto_compact_minimum_tokens":
		n, err := strconv.Atoi(value)
		return err == nil && n >= 0
	case "session.max_iterations":
		n, err := strconv.Atoi(value)
		return err == nil && n >= 1
	}
	return false
}

// traceEventView is the wire shape of one trajectory node for the web UI.
type traceEventView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Turn      int    `json:"turn"`
	Sequence  int64  `json:"sequence"`
	TS        string `json:"ts"`
	ToolName  string `json:"toolName,omitempty"`
	ToolUseID string `json:"toolUseID,omitempty"`
	ParentID  string `json:"parentID,omitempty"`
	Text      string `json:"text,omitempty"`
	IsError   bool   `json:"isError,omitempty"`
	ElapsedMs int64  `json:"elapsedMs,omitempty"`
	Depth     int    `json:"depth"`
}

type traceStats struct {
	DurationMs    int64   `json:"durationMs"`
	Turns         int     `json:"turns"`
	Steps         int     `json:"steps"`
	ToolCalls     int     `json:"toolCalls"`
	Errors        int     `json:"errors"`
	ToolMs        int64   `json:"toolMs"`
	LlmMs         int64   `json:"llmMs"`
	InputTokens   int64   `json:"inputTokens"`
	OutputTokens  int64   `json:"outputTokens"`
	CacheRead     int64   `json:"cacheReadTokens"`
	CacheWrite    int64   `json:"cacheWriteTokens"`
	CacheHitRate  float64 `json:"cacheHitRate"`
	TokPerSec     float64 `json:"tokPerSec"`
	TtftAverageMs int64   `json:"ttftAverageMs"`
}

// traceTurnMetricView is the durable message-footer metadata for one user
// turn. The live chat computes the same values while streaming; exposing the
// persisted trace summary lets a resumed Desktop session reconstruct the
// footer instead of losing everything except a newly fabricated timestamp.
type traceTurnMetricView struct {
	Turn         int     `json:"turn"`
	StartedAt    string  `json:"startedAt"`
	CompletedAt  string  `json:"completedAt"`
	DurationMs   int64   `json:"durationMs"`
	TtftMs       int64   `json:"ttftMs,omitempty"`
	OutputTokens int64   `json:"outputTokens,omitempty"`
	TokPerSec    float64 `json:"tokPerSec,omitempty"`
}

func traceTurnMetrics(nodes []session.TracedNode) []traceTurnMetricView {
	type turnMetric struct {
		first        time.Time
		last         time.Time
		firstToken   time.Time
		outputTokens int64
		completed    bool
		timingExact  bool
	}
	turns := make(map[int]*turnMetric)
	for _, node := range nodes {
		ev := node.Event
		if ev.Turn <= 0 || ev.TS.IsZero() {
			continue
		}
		metric := turns[ev.Turn]
		if metric == nil {
			metric = &turnMetric{}
			turns[ev.Turn] = metric
		}
		if metric.first.IsZero() || ev.TS.Before(metric.first) {
			metric.first = ev.TS
		}
		if metric.last.IsZero() || ev.TS.After(metric.last) {
			metric.last = ev.TS
		}
		if ev.Kind == "text" || ev.Kind == "thinking" {
			if metric.firstToken.IsZero() || ev.TS.Before(metric.firstToken) {
				metric.firstToken = ev.TS
				metric.timingExact = ev.ElapsedMs > 0
			}
		}
		if ev.Kind == "tokens" {
			var in, out, cacheWrite, cacheRead int64
			fmt.Sscanf(ev.Text, "input=%d output=%d cache_write=%d cache_read=%d", &in, &out, &cacheWrite, &cacheRead)
			metric.outputTokens += out
		}
		if ev.Kind == "loop_done" {
			metric.completed = true
		}
	}

	turnNumbers := make([]int, 0, len(turns))
	for turn, metric := range turns {
		if metric.completed && !metric.first.IsZero() && !metric.last.Before(metric.first) {
			turnNumbers = append(turnNumbers, turn)
		}
	}
	sort.Ints(turnNumbers)
	out := make([]traceTurnMetricView, 0, len(turnNumbers))
	for _, turn := range turnNumbers {
		metric := turns[turn]
		view := traceTurnMetricView{
			Turn:         turn,
			StartedAt:    metric.first.Format(time.RFC3339),
			CompletedAt:  metric.last.Format(time.RFC3339),
			DurationMs:   metric.last.Sub(metric.first).Milliseconds(),
			OutputTokens: metric.outputTokens,
		}
		if metric.timingExact && !metric.firstToken.IsZero() && metric.firstToken.After(metric.first) {
			view.TtftMs = metric.firstToken.Sub(metric.first).Milliseconds()
		}
		if metric.timingExact && metric.outputTokens > 0 && !metric.firstToken.IsZero() && metric.last.After(metric.firstToken) {
			view.TokPerSec = float64(metric.outputTokens) / metric.last.Sub(metric.firstToken).Seconds()
		}
		out = append(out, view)
	}
	return out
}

// activeTraceDuration sums the recorded wall time inside each conversation
// turn. Time between turns is user idle time and must not be reported as LLM
// latency when a session is resumed hours or days later.
func activeTraceDuration(nodes []session.TracedNode) int64 {
	type turnRange struct {
		first time.Time
		last  time.Time
	}
	ranges := make(map[int]turnRange)
	for _, node := range nodes {
		ev := node.Event
		if ev.TS.IsZero() {
			continue
		}
		r := ranges[ev.Turn]
		if r.first.IsZero() || ev.TS.Before(r.first) {
			r.first = ev.TS
		}
		if r.last.IsZero() || ev.TS.After(r.last) {
			r.last = ev.TS
		}
		ranges[ev.Turn] = r
	}
	var total int64
	for _, r := range ranges {
		if r.first.IsZero() || r.last.Before(r.first) {
			continue
		}
		total += r.last.Sub(r.first).Milliseconds()
	}
	return total
}

// activeTraceToolDuration returns leaf-tool wall time. Agent spans are
// orchestration containers: their elapsed time includes child LLM and tool
// work, so subtracting them from the session duration would erase the child
// model time. Per-turn interval merging also prevents parallel leaf tools from
// being counted more than once.
func activeTraceToolDuration(nodes []session.TracedNode) int64 {
	type interval struct {
		start time.Time
		end   time.Time
	}
	type turnIntervals struct {
		first time.Time
		last  time.Time
		tools []interval
	}

	turns := make(map[int]*turnIntervals)
	for _, node := range nodes {
		ev := node.Event
		if ev.TS.IsZero() {
			continue
		}
		turn := turns[ev.Turn]
		if turn == nil {
			turn = &turnIntervals{}
			turns[ev.Turn] = turn
		}
		if turn.first.IsZero() || ev.TS.Before(turn.first) {
			turn.first = ev.TS
		}
		if turn.last.IsZero() || ev.TS.After(turn.last) {
			turn.last = ev.TS
		}
		if ev.Kind != "tool_result" || ev.ElapsedMs <= 0 || strings.EqualFold(ev.ToolName, "Agent") {
			continue
		}
		turn.tools = append(turn.tools, interval{
			start: ev.TS.Add(-time.Duration(ev.ElapsedMs) * time.Millisecond),
			end:   ev.TS,
		})
	}

	var total int64
	for _, turn := range turns {
		if turn.first.IsZero() || !turn.last.After(turn.first) || len(turn.tools) == 0 {
			continue
		}
		clamped := make([]interval, 0, len(turn.tools))
		for _, tool := range turn.tools {
			if tool.start.Before(turn.first) {
				tool.start = turn.first
			}
			if tool.end.After(turn.last) {
				tool.end = turn.last
			}
			if tool.end.After(tool.start) {
				clamped = append(clamped, tool)
			}
		}
		if len(clamped) == 0 {
			continue
		}
		sort.Slice(clamped, func(i, j int) bool {
			return clamped[i].start.Before(clamped[j].start)
		})
		current := clamped[0]
		for _, next := range clamped[1:] {
			if !next.start.After(current.end) {
				if next.end.After(current.end) {
					current.end = next.end
				}
				continue
			}
			total += current.end.Sub(current.start).Milliseconds()
			current = next
		}
		total += current.end.Sub(current.start).Milliseconds()
	}
	return total
}

// handleTrace serves the recorded trajectory of a session as a nested
// event tree plus aggregate stats (duration, turns, tool calls, token
// totals). Mirrors the harness GUI's trajectory pane.
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sid := r.URL.Query().Get("sessionId")
	if sid == "" {
		s.stateMu.RLock()
		sid = s.activeSessionID
		s.stateMu.RUnlock()
	}
	// Validate before any store access: a malformed id is a client bug
	// even when tracing is disabled.
	if !validSessionID(sid) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 2000 {
			writeError(w, http.StatusBadRequest, "trace limit must be between 1 and 2000")
			return
		}
		limit = parsed
	}
	cursorEnd := -1
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid trace cursor")
			return
		}
		parsed, err := strconv.Atoi(string(decoded))
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid trace cursor")
			return
		}
		cursorEnd = parsed
	}
	store := rtpkg.CurrentTraceStore()
	if store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "events": []any{}})
		return
	}

	nodes := store.Trace(sid)
	source := "live"
	if len(nodes) == 0 {
		// No live trace (session predates tracing or ran elsewhere):
		// rebuild the trajectory from the persisted transcript so every
		// session has a trajectory view.
		nodes = traceFromHistory(s, sid)
		source = "history"
	}
	nodes = coalesceTraceToolArgs(nodes)
	turnMetrics := []traceTurnMetricView{}
	if source == "live" {
		turnMetrics = traceTurnMetrics(nodes)
	}
	if persisted, err := s.store.ReadMessageMetrics(sid); err == nil && len(persisted) > 0 {
		byTurn := make(map[int]traceTurnMetricView, len(turnMetrics)+len(persisted))
		for _, metric := range turnMetrics {
			byTurn[metric.Turn] = metric
		}
		for _, metric := range persisted {
			byTurn[metric.Turn] = traceTurnMetricView{
				Turn:         metric.Turn,
				StartedAt:    metric.StartedAt.Format(time.RFC3339),
				CompletedAt:  metric.CompletedAt.Format(time.RFC3339),
				DurationMs:   metric.DurationMS,
				TtftMs:       metric.TTFTMS,
				OutputTokens: metric.OutputTokens,
				TokPerSec:    metric.TokPerSec,
			}
		}
		turnNumbers := make([]int, 0, len(byTurn))
		for turn := range byTurn {
			turnNumbers = append(turnNumbers, turn)
		}
		sort.Ints(turnNumbers)
		turnMetrics = turnMetrics[:0]
		for _, turn := range turnNumbers {
			turnMetrics = append(turnMetrics, byTurn[turn])
		}
	}
	pageEnd := len(nodes)
	if cursorEnd >= 0 {
		if cursorEnd > len(nodes) {
			writeError(w, http.StatusBadRequest, "invalid trace cursor")
			return
		}
		pageEnd = cursorEnd
	}
	pageStart := max(0, pageEnd-limit)
	events := make([]traceEventView, 0, len(nodes))
	stats := traceStats{DurationMs: activeTraceDuration(nodes)}
	turnFirst := make(map[int]time.Time)
	turnFirstToken := make(map[int]time.Time)
	for _, n := range nodes {
		ev := n.Event
		if t, ok := turnFirst[ev.Turn]; !ok || ev.TS.Before(t) {
			turnFirst[ev.Turn] = ev.TS
		}
		if ev.Kind == "text" || ev.Kind == "thinking" {
			if t, ok := turnFirstToken[ev.Turn]; !ok || ev.TS.Before(t) {
				turnFirstToken[ev.Turn] = ev.TS
			}
		}
		if ev.Turn > stats.Turns {
			stats.Turns = ev.Turn
		}
		switch ev.Kind {
		case "text", "thinking", "thinking_redacted", "user":
			stats.Steps++
		case "tool_start":
			stats.ToolCalls++
			stats.Steps++
		case "tool_result":
			// Tool wall time is calculated after the scan so overlapping leaf
			// tools and Agent orchestration spans are handled correctly.
		case "error":
			stats.Errors++
		case "tokens":
			var in, out, cw, cr int64
			fmt.Sscanf(ev.Text, "input=%d output=%d cache_write=%d cache_read=%d", &in, &out, &cw, &cr)
			stats.InputTokens += in + cw + cr
			stats.OutputTokens += out
			stats.CacheRead += cr
			stats.CacheWrite += cw
		}
		events = append(events, traceEventView{
			ID:        ev.ID,
			Kind:      ev.Kind,
			Turn:      ev.Turn,
			Sequence:  ev.Sequence,
			TS:        ev.TS.Format(time.RFC3339),
			ToolName:  ev.ToolName,
			ToolUseID: ev.ToolUseID,
			ParentID:  ev.ParentID,
			Text:      redactTraceText(ev.Text),
			IsError:   ev.IsError,
			ElapsedMs: ev.ElapsedMs,
			Depth:     n.Depth,
		})
	}
	stats.ToolMs = activeTraceToolDuration(nodes)
	if stats.DurationMs > stats.ToolMs {
		stats.LlmMs = stats.DurationMs - stats.ToolMs
	}
	if denom := stats.InputTokens; denom > 0 {
		stats.CacheHitRate = float64(stats.CacheRead) / float64(denom) * 100
	}
	if stats.LlmMs > 0 {
		stats.TokPerSec = float64(stats.OutputTokens) / (float64(stats.LlmMs) / 1000)
	}
	// Per-turn time-to-first-token: first provider-visible reasoning or
	// assistant text event minus the turn's first recorded event, averaged
	// over turns that have both.
	var ttftTotal int64
	var ttftCount int
	for turn, firstToken := range turnFirstToken {
		if start, ok := turnFirst[turn]; ok && firstToken.After(start) {
			ttftTotal += firstToken.Sub(start).Milliseconds()
			ttftCount++
		}
	}
	if ttftCount > 0 {
		stats.TtftAverageMs = ttftTotal / int64(ttftCount)
	}
	pageEvents := events[pageStart:pageEnd]
	nextCursor := ""
	if pageStart > 0 {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(pageStart)))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     true,
		"sessionId":   sid,
		"source":      source,
		"events":      pageEvents,
		"stats":       &stats,
		"turnMetrics": turnMetrics,
		"totalEvents": len(events),
		"hasMore":     pageStart > 0,
		"nextCursor":  nextCursor,
	})
}

// coalesceTraceToolArgs removes transport-level parameter fragments from the
// user-facing trajectory. A completed tool_start already carries the provider's
// authoritative full JSON. If a stream ended before tool_start, the fragments
// become one synthetic tool row so evidence is preserved without hundreds of
// one-token rows.
func coalesceTraceToolArgs(nodes []session.TracedNode) []session.TracedNode {
	type partial struct {
		node session.TracedNode
		text strings.Builder
	}
	keyOf := func(ev session.TraceEvent) string {
		if ev.ToolUseID != "" {
			return "id:" + ev.ToolUseID
		}
		return "name:" + ev.ToolName
	}
	partials := make(map[string]*partial)
	order := make([]string, 0)
	out := make([]session.TracedNode, 0, len(nodes))
	for _, node := range nodes {
		ev := node.Event
		if ev.Kind == "tool_args" || ev.Kind == "tool_args_delta" {
			key := keyOf(ev)
			p := partials[key]
			if p == nil {
				p = &partial{node: node}
				partials[key] = p
				order = append(order, key)
			}
			p.text.WriteString(ev.Text)
			continue
		}
		if ev.Kind == "tool_start" {
			key := keyOf(ev)
			p := partials[key]
			partialKey := key
			// Older trace files predate tool identity persistence on args
			// deltas. Fall back to the same-name stream, then the anonymous
			// stream; provider tool calls serialize their input deltas.
			if p == nil && ev.ToolName != "" {
				partialKey = "name:" + ev.ToolName
				p = partials[partialKey]
			}
			if p == nil {
				partialKey = "name:"
				p = partials[partialKey]
			}
			if p != nil {
				current := strings.TrimSpace(ev.Text)
				if current == "" || current == "null" || current == "{}" {
					node.Event.Text = p.text.String()
				}
				delete(partials, partialKey)
			}
		}
		out = append(out, node)
	}
	for _, key := range order {
		p := partials[key]
		if p == nil || p.text.Len() == 0 {
			continue
		}
		p.node.Event.Kind = "tool_start"
		p.node.Event.Text = p.text.String()
		out = append(out, p.node)
	}
	return out
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.stateMu.RLock()
	model := s.activeModel
	preset := s.activePreset
	s.stateMu.RUnlock()
	effort := "default"
	if s.loop != nil {
		effort = effortHeaderValue(s.loop.EffortValue())
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": metisversion.Short(), "name": "Metis", "model": model, "preset": preset, "effort": effort})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "build": s.buildVersion})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
