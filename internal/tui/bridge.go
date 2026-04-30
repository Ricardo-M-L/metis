package tui

// bridge.go is metis's lightweight "remote session" bridge — a
// localhost HTTP+SSE server that lets external clients (IDE
// extensions, browser, mobile companion app) follow along with the
// active chat session in real time. claude-code calls this
// `teleport` and runs it over a private wire protocol; we use plain
// HTTP+SSE so any client with curl can connect.
//
// Endpoints:
//
//	GET  /transcript        full snapshot as JSON (one shot)
//	GET  /events            SSE stream of new messages (live)
//	POST /message           inject a user prompt (body: {"text":"..."})
//	GET  /health            "ok\n" (for readiness probes)
//
// Lifecycle: `/share` slash command starts/stops it. Address is
// 127.0.0.1:<auto> chosen at start; cmdShare returns the URL so
// the user can copy-paste it into their browser / IDE config.
//
// Reference: claude-code's `bridge/bridgeMain.ts` (~3k lines, full
// duplex JSON-RPC). We trade richness for simplicity — SSE + JSON
// covers the 80% case of "show me what claude is doing" without
// the protocol complexity.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	bridgePortStart = 7300
	bridgePortEnd   = 7350 // try 50 ports before giving up
)

var (
	bridgeMu       sync.RWMutex
	bridgeServer   *http.Server
	bridgeAddr     string
	bridgeSnapshot bridgeData
)

// bridgeData is the wire format for /transcript and /events. Mirrors
// the structure of the chat surface (messages + tool events) without
// leaking bubbletea internals.
type bridgeData struct {
	Messages   []bridgeMessage   `json:"messages"`
	ToolEvents []bridgeToolEvent `json:"tool_events"`
	Spinner    bool              `json:"spinner"`
	Tokens     int               `json:"tokens"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type bridgeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type bridgeToolEvent struct {
	Tool    string `json:"tool"`
	Kind    string `json:"kind"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// publishBridgeSnapshot is called from the chat surface every render
// to keep the remote view in sync. Cheap — just locks + struct copy.
// Real-time SSE clients are notified via the embedded Updated stamp.
func publishBridgeSnapshot(m *Model) {
	bridgeMu.RLock()
	if bridgeServer == nil {
		bridgeMu.RUnlock()
		return
	}
	bridgeMu.RUnlock()
	d := bridgeData{
		Messages:   make([]bridgeMessage, 0, len(m.messages)),
		ToolEvents: make([]bridgeToolEvent, 0, len(m.toolEvents)),
		Spinner:    m.spinnerActive,
		Tokens:     m.totalTokens.Total(),
		UpdatedAt:  time.Now(),
	}
	for _, msg := range m.messages {
		d.Messages = append(d.Messages, bridgeMessage{Role: msg.Role, Content: msg.Content})
	}
	for _, te := range m.toolEvents {
		d.ToolEvents = append(d.ToolEvents, bridgeToolEvent{
			Tool: te.ToolName, Kind: te.Kind, Output: te.Output, IsError: te.IsError,
		})
	}
	bridgeMu.Lock()
	bridgeSnapshot = d
	bridgeMu.Unlock()
}

// startBridge spawns the HTTP server on 127.0.0.1:<auto>. Returns
// the resolved address ("127.0.0.1:7301") or an error if no port
// in [bridgePortStart, bridgePortEnd) was available.
func startBridge(injectCh chan string) (string, error) {
	bridgeMu.Lock()
	if bridgeServer != nil {
		bridgeMu.Unlock()
		return bridgeAddr, fmt.Errorf("already running on %s", bridgeAddr)
	}
	bridgeMu.Unlock()

	var listener net.Listener
	var addr string
	for port := bridgePortStart; port < bridgePortEnd; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			listener = l
			addr = l.Addr().String()
			break
		}
	}
	if listener == nil {
		return "", fmt.Errorf("no free port in %d-%d", bridgePortStart, bridgePortEnd-1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/transcript", handleTranscript)
	mux.HandleFunc("/events", handleEvents)
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		handleMessage(w, r, injectCh)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.Serve(listener) // blocks until Close
	}()

	bridgeMu.Lock()
	bridgeServer = srv
	bridgeAddr = addr
	bridgeMu.Unlock()
	return addr, nil
}

// stopBridge halts the server. Idempotent — calling when already
// stopped is a no-op.
func stopBridge() {
	bridgeMu.Lock()
	srv := bridgeServer
	bridgeServer = nil
	bridgeAddr = ""
	bridgeMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// bridgeCurrentAddr returns the current listen address ("" when off).
// Used by the status-bar pill renderer.
func bridgeCurrentAddr() string {
	bridgeMu.RLock()
	defer bridgeMu.RUnlock()
	return bridgeAddr
}

func handleTranscript(w http.ResponseWriter, r *http.Request) {
	bridgeMu.RLock()
	d := bridgeSnapshot
	bridgeMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

// handleEvents is the SSE poll endpoint. We diff against the last
// UpdatedAt the client sent (Last-Event-ID header) and emit one
// event per snapshot change. Polling at 250ms gives smooth-enough
// updates without overwhelming the bubbletea event loop.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		http.Error(w, "streaming unsupported", 500)
		return
	}

	var lastUpdated time.Time
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			bridgeMu.RLock()
			d := bridgeSnapshot
			bridgeMu.RUnlock()
			if d.UpdatedAt.Equal(lastUpdated) {
				continue
			}
			lastUpdated = d.UpdatedAt
			payload, _ := json.Marshal(d)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// handleMessage accepts {"text": "..."} via POST and forwards to
// the chat surface's input channel. Returns 503 if no chat is
// listening (shouldn't happen since the bridge only starts from
// inside an active chat session).
func handleMessage(w http.ResponseWriter, r *http.Request, ch chan string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Text == "" {
		http.Error(w, "text field required", http.StatusBadRequest)
		return
	}
	if ch == nil {
		http.Error(w, "no chat listening", http.StatusServiceUnavailable)
		return
	}
	select {
	case ch <- body.Text:
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued\n"))
	default:
		http.Error(w, "chat input buffer full", http.StatusServiceUnavailable)
	}
}
