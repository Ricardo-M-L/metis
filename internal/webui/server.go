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
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
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
}

func NewServer(addr string, loop *agent.Loop, store *session.Store) *Server {
	return &Server{addr: addr, loop: loop, store: store}
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
		model := ""
		if s.loop != nil {
			model = s.loop.Model
		}
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
		if err := s.store.WriteHeader(id, model, ""); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		writeJSON(w, http.StatusCreated, sessionItem{ID: id, Title: "Untitled", Model: model, CreatedAt: time.Now()})
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
	if id == "" || strings.Contains(id, "/") || s.store == nil {
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

	s.runMu.Lock()
	defer s.runMu.Unlock()
	if body.SessionID == "" {
		body.SessionID = s.store.NewSessionID()
		if err := s.store.WriteHeader(body.SessionID, s.loop.Model, ""); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
	}
	_, history, err := s.store.Load(body.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	s.loop.Restore(history)
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

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	model := ""
	if s.loop != nil {
		model = s.loop.Model
	}
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
