package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// fakeSessions backs the Sessions tool with an in-memory catalog.
type fakeSessions struct {
	infos []SessionInfo
	msgs  map[string][]llm.Message
}

func (f *fakeSessions) list(limit int) ([]SessionInfo, error) {
	if limit > 0 && len(f.infos) > limit {
		return f.infos[:limit], nil
	}
	return f.infos, nil
}

func (f *fakeSessions) load(id string) ([]llm.Message, error) {
	return f.msgs[id], nil
}

func newSessionsFixture() *fakeSessions {
	mk := func(minAgo int) time.Time { return time.Now().Add(-time.Duration(minAgo) * time.Minute) }
	return &fakeSessions{
		infos: []SessionInfo{
			{ID: "aaaa1111", Title: "kafka consumer lag", Model: "m1", UpdatedAt: mk(5), MessageCount: 3},
			{ID: "bbbb2222", Title: "react rerender bug", Model: "m2", UpdatedAt: mk(60), MessageCount: 3},
			{ID: "cccc3333", Title: "grocery list", Model: "m1", UpdatedAt: mk(120), MessageCount: 1},
		},
		msgs: map[string][]llm.Message{
			"aaaa1111": {
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "the consumer group keeps rebalancing under load"}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "that is the classic max.poll.interval miss"}}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "fixed it, bumping the interval worked"}}},
			},
			"bbbb2222": {
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "my table rerenders on every keystroke"}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "memoize the row component and check the key prop"}}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "the key was the index, that was it"}}},
			},
			"cccc3333": {
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "buy milk"}}},
			},
		},
	}
}

func TestSessions_List(t *testing.T) {
	f := newSessionsFixture()
	s := NewSessions(f.list, f.load)
	res, err := s.Execute(context.Background(), map[string]any{"operation": "list", "limit": 2})
	if err != nil || res.IsError {
		t.Fatalf("list failed: %v %s", err, res.Output)
	}
	if !strings.Contains(res.Output, "aaaa1111") || !strings.Contains(res.Output, "kafka consumer lag") {
		t.Fatalf("list output missing rows:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "cccc3333") {
		t.Fatalf("limit=2 should cap rows:\n%s", res.Output)
	}
}

func TestSessions_SearchAcrossSessions(t *testing.T) {
	f := newSessionsFixture()
	s := NewSessions(f.list, f.load)
	res, err := s.Execute(context.Background(), map[string]any{"operation": "search", "query": "rerender keystroke memoize"})
	if err != nil || res.IsError {
		t.Fatalf("search failed: %v %s", err, res.Output)
	}
	var parsed struct {
		Matches []struct {
			Session string `json:"session"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(res.Output), &parsed); err != nil {
		t.Fatalf("search output not JSON: %v\n%s", err, res.Output)
	}
	if len(parsed.Matches) == 0 || parsed.Matches[0].Session != "bbbb2222" {
		t.Fatalf("top hit should be the react session:\n%s", res.Output)
	}
}

func TestSessions_SearchTitleOnlyMatch(t *testing.T) {
	f := newSessionsFixture()
	s := NewSessions(f.list, f.load)
	// "grocery" appears only in the TITLE — the title doc must surface it.
	res, _ := s.Execute(context.Background(), map[string]any{"operation": "search", "query": "grocery"})
	if res.IsError || !strings.Contains(res.Output, "cccc3333") {
		t.Fatalf("title-only match should surface the session:\n%s", res.Output)
	}
}

func TestSessions_ReadByIdPrefixAndAround(t *testing.T) {
	f := newSessionsFixture()
	s := NewSessions(f.list, f.load)
	res, _ := s.Execute(context.Background(), map[string]any{"operation": "read", "session": "aaaa"})
	if res.IsError {
		t.Fatalf("prefix resolve failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "aaaa1111") || !strings.Contains(res.Output, "max.poll.interval") {
		t.Fatalf("digest missing content:\n%s", res.Output)
	}
	// around an index
	res2, _ := s.Execute(context.Background(), map[string]any{"operation": "read", "session": "aaaa1111", "index": 1})
	if res2.IsError || !strings.Contains(res2.Output, "#1 assistant") {
		t.Fatalf("around read missing indexed message:\n%s", res2.Output)
	}
}

func TestSessions_ReadAmbiguousRef(t *testing.T) {
	f := newSessionsFixture()
	s := NewSessions(f.list, f.load)
	res, _ := s.Execute(context.Background(), map[string]any{"operation": "read", "session": "zzzz"})
	if !res.IsError || !strings.Contains(res.Output, "uniquely match") {
		t.Fatalf("unmatched ref should error:\n%s", res.Output)
	}
}

func TestResolveSessionRef_TitleSubstring(t *testing.T) {
	f := newSessionsFixture()
	if got := ResolveSessionRef(f.infos, "react"); got != "bbbb2222" {
		t.Fatalf("title substring resolve = %q, want bbbb2222", got)
	}
	if got := ResolveSessionRef(f.infos, "aaaa1111"); got != "aaaa1111" {
		t.Fatalf("exact id resolve = %q", got)
	}
	if got := ResolveSessionRef(f.infos, "e"); got != "" { // ambiguous: appears in multiple titles? "e" is in all → ambiguous
		t.Fatalf("ambiguous resolve should be empty, got %q", got)
	}
}

func TestSessionDigest_Bounds(t *testing.T) {
	f := newSessionsFixture()
	d := SessionDigest(f.infos[0], f.msgs["aaaa1111"], -1, 400)
	if len(d) > 700 { // truncation slack
		t.Fatalf("digest not bounded: %d chars", len(d))
	}
	if !strings.Contains(d, "[session aaaa1111") {
		t.Fatalf("digest missing header:\n%s", d)
	}
}
