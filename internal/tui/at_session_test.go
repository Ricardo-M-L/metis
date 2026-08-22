package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// fake list/load for expandAtSession — mirrors the shape runtime injects.
func atSessionFixture() (func(int) ([]builtin.SessionInfo, error), func(string) ([]llm.Message, error)) {
	infos := []builtin.SessionInfo{
		{ID: "sess-alpha", Title: "kafka lag investigation", Model: "m1", UpdatedAt: time.Now(), MessageCount: 2},
		{ID: "sess-beta", Title: "react bug", Model: "m2", UpdatedAt: time.Now(), MessageCount: 1},
	}
	msgs := map[string][]llm.Message{
		"sess-alpha": {
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "why does the consumer lag"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "max.poll.interval exceeded"}}},
		},
		"sess-beta": {
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "rerenders everywhere"}}},
		},
	}
	return func(limit int) ([]builtin.SessionInfo, error) { return infos, nil },
		func(id string) ([]llm.Message, error) { return msgs[id], nil }
}

func TestExpandAtSession_ResolvesAndStrips(t *testing.T) {
	list, load := atSessionFixture()
	text := "@session:sess-alpha what did we conclude there?"
	rewritten, blocks, errs := expandAtSession(text, list, load)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if strings.Contains(rewritten, "@session:") {
		t.Fatalf("resolved ref must be stripped, got %q", rewritten)
	}
	if len(blocks) != 1 {
		t.Fatalf("want 1 digest block, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0].Text, "sess-alpha") || !strings.Contains(blocks[0].Text, "max.poll.interval") {
		t.Fatalf("digest missing content:\n%s", blocks[0].Text)
	}
	if !strings.Contains(rewritten, "what did we conclude there?") {
		t.Fatalf("user text must survive rewrite: %q", rewritten)
	}
}

func TestExpandAtSession_TitleSlug(t *testing.T) {
	list, load := atSessionFixture()
	_, blocks, errs := expandAtSession("recall @session:kafka-lag please", list, load)
	if len(errs) != 0 || len(blocks) != 1 {
		t.Fatalf("title-slug resolution failed: %v (blocks=%d)", errs, len(blocks))
	}
}

func TestExpandAtSession_UnmatchedDegrades(t *testing.T) {
	list, load := atSessionFixture()
	rewritten, blocks, errs := expandAtSession("see @session:nope-xyz", list, load)
	if len(errs) != 1 || !strings.Contains(errs[0], "no unique match") {
		t.Fatalf("want degrade warning, got %v", errs)
	}
	if len(blocks) != 0 {
		t.Fatalf("no blocks on failure, got %d", len(blocks))
	}
	if !strings.Contains(rewritten, "@session:nope-xyz") {
		t.Fatalf("failed ref must stay verbatim: %q", rewritten)
	}
}

func TestExpandAtSession_NoRefsNoWork(t *testing.T) {
	list, load := atSessionFixture()
	rewritten, blocks, errs := expandAtSession("plain message, nothing to expand", list, load)
	if rewritten != "plain message, nothing to expand" || blocks != nil || errs != nil {
		t.Fatalf("no-op path should be identity: %q %v %v", rewritten, blocks, errs)
	}
}

func TestExpandAtSession_EmailNotMangled(t *testing.T) {
	list, load := atSessionFixture()
	rewritten, blocks, errs := expandAtSession("mail me at user@session:never.example", list, load)
	// pattern requires whitespace-or-start before @session: — mid-word must not match
	if len(errs) != 0 || len(blocks) != 0 {
		t.Fatalf("mid-token @session: must not match: %v %v", errs, blocks)
	}
	if rewritten != "mail me at user@session:never.example" {
		t.Fatalf("text must be untouched: %q", rewritten)
	}
}
