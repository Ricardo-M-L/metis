// Package webui provides the local Codex-style desktop interface for Metis.
package webui

import (
	"context"
	"embed"
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
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

//go:embed static/*
var staticFS embed.FS

// Server owns one agent loop. Turns are serialized because Loop contains the
// active transcript and is not safe for simultaneous conversations.
type Server struct {
	addr  string
	loop  *agent.Loop
	store *session.Store
	runMu sync.Mutex

	stateMu            sync.RWMutex
	activeSessionID    string
	activeProviderName string
	activeModel        string

	freshProviderName   string
	freshModel          string
	freshSystem         string
	freshSystemSections []llm.SystemSection
	freshPermissionMode permission.Mode

	buildProvider   func(providerName, model string) (*rtpkg.ProviderBuild, error)
	sessionBoundary func()
	sessionSwitch   func(sessionID string)
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
	InitialSessionID    string
	ProviderName        string
	FreshPermissionMode permission.Mode
	BuildProvider       func(providerName, model string) (*rtpkg.ProviderBuild, error)
	SessionBoundary     func()
	SessionSwitch       func(sessionID string)
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
		activeSessionID:     binding.InitialSessionID,
		activeProviderName:  binding.ProviderName,
		freshProviderName:   binding.ProviderName,
		freshPermissionMode: binding.FreshPermissionMode,
		buildProvider:       binding.BuildProvider,
		sessionBoundary:     binding.SessionBoundary,
		sessionSwitch:       binding.SessionSwitch,
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
	return server
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(staticSub)))
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSession)
	mux.HandleFunc("/api/turns", s.handleTurn)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/health", s.handleHealth)
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

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// sameOriginOnly prevents an unrelated web page from driving the local agent
// through a cross-origin POST. Browsers do not require a CORS preflight for a
// text/plain JSON body, so response-side CORS defaults alone are insufficient.
// Requests from non-browser clients generally omit Origin and remain allowed.
func (s *Server) sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "session store unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		entries, err := s.store.List(100)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list sessions")
			return
		}
		items := make([]sessionItem, 0, len(entries))
		for _, e := range entries {
			title := e.Title
			if title == "" {
				title = "Untitled"
			}
			items = append(items, sessionItem{ID: e.ID, Title: title, Model: e.Model, CreatedAt: e.CreatedAt})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
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
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		writeJSON(w, http.StatusCreated, sessionItem{ID: id, Title: "Untitled", Model: model, CreatedAt: createdAt})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if !validSessionID(id) || s.store == nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	hdr, messages, err := s.store.Load(id)
	if err != nil || hdr == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sessionItem{ID: id, Title: hdr.Title, Model: hdr.Model, CreatedAt: hdr.CreatedAt}, "messages": messages})
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
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil || strings.TrimSpace(body.Input) == "" {
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
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
	}
	hdr, history, err := s.store.Load(body.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := s.activateSession(body.SessionID, hdr, history); err != nil {
		writeError(w, http.StatusConflict, "failed to activate session: "+err.Error())
		return
	}
	input := strings.TrimSpace(body.Input)
	s.loop.AppendUser(input)
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: input}}}
	if err := s.store.AppendMessage(body.SessionID, user); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist input")
		return
	}
	if len(history) == 0 {
		title := []rune(input)
		if len(title) > 60 {
			title = title[:60]
		}
		_ = s.store.SetTitle(body.SessionID, string(title))
	}

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() { done <- s.loop.Run(r.Context(), events); close(events) }()
	var text strings.Builder
	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			text.WriteString(ev.TextDelta)
		case agent.EventPermissionRequest:
			if ev.PermissionReply != nil {
				ev.PermissionReply <- agent.PermissionDecisionDeny
			}
		case agent.EventAskUser:
			if ev.AskUserReply != nil {
				ev.AskUserReply <- ""
			}
		}
	}
	if err := <-done; err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	updated := s.loop.History()
	for i := len(history) + 1; i < len(updated); i++ {
		if err := s.store.AppendMessage(body.SessionID, updated[i]); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to persist response")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": body.SessionID, "text": text.String()})
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
		s.loop.TimingSink = s.store.NewTimingRecorder(id).Record
		rtpkg.RebindLoopRuntime(s.loop, s.loop.Provider, activeModel, s.loop.System, id)
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

	provider := s.loop.Provider
	needsProviderBuild := provider == nil || targetProviderName != activeProviderName || targetModel != activeModel
	if !needsProviderBuild && provider != nil && targetModel != "" {
		// ModelID is the transport truth. An empty value is permitted for
		// embedders/test providers that cannot report it.
		if wireModel := provider.ModelID(); wireModel != "" && wireModel != targetModel {
			needsProviderBuild = true
		}
	}
	providerRebuilt := false
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
		if built.Model != "" {
			targetModel = built.Model
		}
		providerRebuilt = true
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

	s.loop.Provider = provider
	s.loop.Model = targetModel
	if provider != nil {
		s.loop.ContextWindow = provider.MaxContextTokens()
	}
	if providerRebuilt && s.loop.Compactor != nil {
		oldCfg := s.loop.Compactor.Config
		oldMaxOut := s.loop.Compactor.MaxOutputTokens
		s.loop.Compactor = agent.NewCompactor(oldCfg, targetModel, provider.MaxContextTokens(), provider)
		s.loop.Compactor.MaxOutputTokens = oldMaxOut
		s.loop.Compactor.ApplyWindowTier(provider.MaxContextTokens() - oldMaxOut)
	}
	s.loop.System = targetSystem
	if targetSystem == s.freshSystem {
		s.loop.SystemSections = append([]llm.SystemSection(nil), s.freshSystemSections...)
	} else {
		// Persisted free-form prompts have no typed-section representation.
		// Clearing prevents sections from the source session overriding it.
		s.loop.SystemSections = nil
	}
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
	s.stateMu.RUnlock()
	if id == "" {
		return nil
	}

	hdr := session.Header{
		ID:       id,
		Provider: providerName,
		Model:    model,
		System:   s.loop.System,
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
		filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.stateMu.RLock()
	model := s.activeModel
	s.stateMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"version": "0.2.8", "name": "Metis", "model": model})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
