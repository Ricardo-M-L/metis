package webui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceImageProjectionUsesUniqueContentAnchors(t *testing.T) {
	hash := strings.Repeat("a", 64)
	user := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "which image?"}, {Type: "image", AttachmentHash: hash, MediaType: "image/png"}}}
	answer := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "unique anchored answer"}}}
	at := time.Now()
	nodes := []session.TracedNode{
		{Event: session.TraceEvent{ID: "old-user", SessionID: "images", Turn: 17, Sequence: 99, TS: at, Kind: "user", Text: historyTraceUserInput(user)}},
		{Event: session.TraceEvent{Kind: "text", SessionID: "images", Turn: 17, Sequence: 100, Text: "unique anchored answer"}},
	}
	out := restoreMissingTraceUserInputs("images", nodes, []llm.Message{user, answer})
	got := out[0].Event
	if len(got.Images) != 1 || got.Images[0].AttachmentHash != hash || got.ID != "old-user" || got.Turn != 17 || got.Sequence != 99 || !got.TS.Equal(at) {
		t.Fatalf("image projection changed event identity: %+v", got)
	}
	if len(nodes[0].Event.Images) != 0 {
		t.Fatal("display projection mutated persisted nodes")
	}

	// Identical prompts and responses in two compacted turns cannot establish
	// which history image belongs to either occurrence.
	ambiguous := append([]session.TracedNode(nil), nodes...)
	for _, node := range nodes {
		cp := node
		cp.Event.Turn = 23
		ambiguous = append(ambiguous, cp)
	}
	out = restoreMissingTraceUserInputs("images", ambiguous, []llm.Message{user, answer})
	for _, node := range out {
		if len(node.Event.Images) != 0 {
			t.Fatalf("ambiguous history attached image to turn %d", node.Event.Turn)
		}
	}
}
