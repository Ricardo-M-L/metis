package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func decodeHubEventPayload(t *testing.T, body string) map[string]any {
	t.Helper()
	const marker = "data: "
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("SSE data line missing: %q", body)
	}
	line := body[start+len(marker):]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("decode SSE payload: %v\n%s", err, body)
	}
	return payload
}

func TestWriteHubEventIncludesCompactionMetricsAndFailure(t *testing.T) {
	t.Run("successful replacement", func(t *testing.T) {
		w := httptest.NewRecorder()
		(&Server{}).writeHubEvent(w, hubEvent{
			sequence: 11,
			session:  "session-1",
			ev: agent.Event{
				Kind:                  agent.EventContextCompacted,
				Info:                  "auto",
				PreviousContextTokens: 108_800,
				ContextTokens:         6_240,
			},
		})
		payload := decodeHubEventPayload(t, w.Body.String())
		if payload["previousContextTokens"] != float64(108_800) || payload["contextTokens"] != float64(6_240) {
			t.Fatalf("missing compaction token delta: %#v", payload)
		}
	})

	t.Run("failed lifecycle", func(t *testing.T) {
		w := httptest.NewRecorder()
		(&Server{}).writeHubEvent(w, hubEvent{
			sequence: 12,
			session:  "session-1",
			ev: agent.Event{
				Kind: agent.EventCompactionEnd,
				Info: "auto",
				Err:  errors.New("summary stopped at max_tokens"),
			},
		})
		payload := decodeHubEventPayload(t, w.Body.String())
		if payload["error"] != "summary stopped at max_tokens" {
			t.Fatalf("compaction end error = %#v", payload["error"])
		}
	})
}

func TestStatusIncludesAuthoritativeCompactionTriggerTokens(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "wire", model: "model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 2)
	loop.Model = provider.ModelID()
	loop.ContextWindow = provider.MaxContextTokens()
	loop.Compactor = agent.NewCompactor(agent.DefaultCompactionConfig(), loop.Model, loop.ContextWindow, provider)
	loop.Compactor.MaxOutputTokens = 20_000

	s := NewServer("127.0.0.1:0", loop, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		CompactAtTokens int `json:"compactAtTokens"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if want := loop.Compactor.TriggerTokens(); payload.CompactAtTokens != want {
		t.Fatalf("compactAtTokens = %d, want authoritative trigger %d", payload.CompactAtTokens, want)
	}
}

func TestDesktopCompactionLifecycleContract(t *testing.T) {
	s, _ := testServer(t)
	get := func(path string) string {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	chat := get("/chat.js")
	for _, want := range []string{
		"onLive('compaction_start', handleCompactionStart);",
		"onLive('compaction_progress', handleCompactionProgress);",
		"onLive('compaction_end', handleCompactionEnd);",
		"d.previousContextTokens",
		"d.contextTokens",
		"fmtTokens(before) + ' → ' + fmtTokens(after) + ' tokens'",
		"Conversation history compacted",
	} {
		if !strings.Contains(chat, want) {
			t.Fatalf("chat.js missing compaction lifecycle contract %q", want)
		}
	}
	if strings.Contains(chat, "contextUsed: after") {
		t.Fatal("chat.js must not replace full request pressure with history-only compaction tokens")
	}
	app := get("/app.js")
	for _, want := range []string{"d.compactAtTokens", "fmtTokens(compactAtTokens)"} {
		if !strings.Contains(app, want) {
			t.Fatalf("app.js missing authoritative compaction status contract %q", want)
		}
	}
}
