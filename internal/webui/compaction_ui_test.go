package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type activeContextStatusProvider struct {
	calls int
}

func (*activeContextStatusProvider) Name() string          { return "active-context-status" }
func (*activeContextStatusProvider) ModelID() string       { return "active-context-status-model" }
func (*activeContextStatusProvider) MaxContextTokens() int { return 128_000 }
func (*activeContextStatusProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("active-context status test expects streaming")
}
func (p *activeContextStatusProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	p.calls++
	input, cacheCreate, cacheRead, output, text := 600, 100, 200, 50, "first response"
	if p.calls == 2 {
		input, cacheCreate, cacheRead, output, text = 900, 150, 300, 75, "second response"
	}
	return &composerSummaryStream{events: []llm.StreamEvent{
		{Type: "message_start", InputTokens: input, CacheCreationInputTokens: cacheCreate, CacheReadInputTokens: cacheRead},
		{Type: "text_delta", TextDelta: text},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: output},
		{Type: "message_stop"},
	}}, nil
}

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

func TestStatusContextUsedReportsActiveRatherThanCumulativeUsage(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activeContextStatusProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "system", 2)
	loop.Model = provider.ModelID()
	loop.ContextWindow = provider.MaxContextTokens()
	for _, prompt := range []string{"first question", "second question"} {
		loop.AppendUser(prompt)
		out := make(chan agent.Event, 32)
		if err := loop.Run(context.Background(), out); err != nil {
			t.Fatalf("Run(%q): %v", prompt, err)
		}
	}

	s := NewServer("127.0.0.1:0", loop, store)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		ContextUsed int `json:"contextUsed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	first := 600 + 100 + 200 + 50
	second := 900 + 150 + 300 + 75
	if payload.ContextUsed != second {
		t.Fatalf("contextUsed = %d, want latest active context %d (not cumulative %d)", payload.ContextUsed, second, first+second)
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
