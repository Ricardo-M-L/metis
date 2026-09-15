package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestRecordedHumanInputRedactsBeforeTracePersistence(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	previous := CurrentTraceAdapter()
	SetTraceAdapter(NewTraceAdapter(store))
	t.Cleanup(func() { SetTraceAdapter(previous) })
	canary := "ghp_" + strings.Repeat("A", 36)
	RecordUserMessage("human-redaction", "please inspect "+canary)
	events := store.Events("human-redaction")
	if len(events) != 1 {
		t.Fatalf("human events = %d, want 1", len(events))
	}
	if strings.Contains(events[0].Text, canary) || !strings.Contains(events[0].Text, "[REDACTED]") {
		t.Fatal("human input was persisted without the shared secret redaction")
	}
}

func TestRecordUserInputKeepsAttachmentsWithoutOpaquePayloads(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	previous := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(previous) })
	adapter.SetSession("selected-elsewhere")
	RecordUserInput("input-owner", []llm.ContentBlock{
		{Type: "text", Text: "   "},
		{Type: "text", Text: "not human", Synthetic: true},
		{Type: "image", Data: "opaque-base64-payload", MediaType: "secret-media-type", ProviderHint: map[string]string{"secret": "opaque-hint"}},
	})
	events := store.Events("input-owner")
	if len(events) != 1 || events[0].Kind != "user" || events[0].Text != "[image attachment]" || events[0].Turn != 1 {
		t.Fatalf("image-only user trace = %+v", events)
	}
	RecordUserInput("input-owner", []llm.ContentBlock{{Type: "text", Text: "   "}})
	if len(store.Events("input-owner")) != 1 {
		t.Fatal("blank input created a USER event")
	}
	if len(store.Events("selected-elsewhere")) != 0 {
		t.Fatal("explicit input was assigned to selected session")
	}
	ctx, origin := BindTraceTurn(context.Background(), "input-owner")
	defer EndTraceTurn(ctx)
	if origin.Turn != events[0].Turn {
		t.Fatal("USER anchor and bound execution use different turns")
	}
}

func TestContextTracePersistsFullEnvelopeAndSourceWithoutOpeningHumanTurn(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	adapter := NewTraceAdapter(store)
	adapter.SetSession("context-session")
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "previous response"})
	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone})
	body := "<peer_message>\n" + strings.Repeat("complete detail ", 250) + "\napi_key=example-secret\n</peer_message>"
	adapter.OnEvent(agent.Event{Kind: agent.EventContextInjected, Source: "peer", ContextText: body})
	adapter.OnEvent(agent.Event{Kind: agent.EventContextInjected, Source: "credential-source-" + strings.Repeat("x", 100), ContextText: "another injection"})
	if err := adapter.Flush(); err != nil {
		t.Fatal(err)
	}
	events := store.Events("context-session")
	if len(events) != 4 || events[2].Kind != "context" || events[2].Source != "peer" || events[3].Source != "context" {
		t.Fatalf("context trace rows = %+v", events)
	}
	if events[2].Turn != 1 || events[3].Turn != 1 || store.CurrentTurn("context-session") != 1 {
		t.Fatal("context injection opened a new human turn")
	}
	if !strings.HasSuffix(events[2].Text, "</peer_message>") || len(events[2].Text) < 2000 {
		t.Fatal("full context envelope was truncated")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "context-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "example-secret") || strings.Contains(string(raw), "credential-source-") {
		t.Fatal("context persistence leaked content or arbitrary source")
	}
	reloaded, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })
	if got := reloaded.Events("context-session"); len(got) != 4 || got[2].Source != "peer" {
		t.Fatal("context source failed durable round-trip")
	}
}

func TestContextTraceFollowsBoundOriginAcrossSessionSwitch(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	previous := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(previous) })
	RecordUserMessage("owner", "inspect")
	ctx, origin := BindTraceTurn(context.Background(), "owner")
	defer EndTraceTurn(ctx)
	adapter.SetSession("elsewhere")
	adapter.OnEvent(agent.Event{Kind: agent.EventContextInjected, Source: "job", ContextText: "completed", TraceInvocationID: origin.InvocationID})
	if rows := store.Events("owner"); len(rows) != 2 || rows[1].Turn != origin.Turn || rows[1].Kind != "context" {
		t.Fatal("bound context lost original session/turn")
	}
	if len(store.Events("elsewhere")) != 0 {
		t.Fatal("bound context leaked into selected session")
	}
}

func TestTraceRedactsJoinedStreamingSecretBeforeTruncation(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	adapter := NewTraceAdapter(store)
	adapter.SetSession("redacted-stream")
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "ghp_"})
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: strings.Repeat("A", 36)})
	adapter.OnEvent(agent.Event{Kind: agent.EventInfo, Info: "api_key=info-secret"})
	for _, row := range store.Events("redacted-stream") {
		if !strings.Contains(row.Text, "[REDACTED]") {
			t.Fatalf("trace text not redacted for %s", row.Kind)
		}
	}
}

func TestRecordUserInputConcurrentSessionSwitchAndContext(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	previous := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(previous) })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			RecordUserMessage("input-owner", "accepted input")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			adapter.SetSession("other")
			adapter.OnEvent(agent.Event{Kind: agent.EventContextInjected, Source: "job", ContextText: "completed"})
		}
	}()
	wg.Wait()
	if len(store.Events("input-owner")) != 100 {
		t.Fatal("concurrent session switch lost human input")
	}
}

func TestLegacyTraceEventSourceRemainsOptional(t *testing.T) {
	var row session.TraceEvent
	if err := json.Unmarshal([]byte(`{"id":"legacy","session_id":"sid","turn":1,"kind":"text","text":"answer"}`), &row); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(row)
	if err != nil || row.Source != "" || strings.Contains(string(encoded), `"source"`) {
		t.Fatal("optional source changed legacy trace format")
	}
}

func TestRecordContextInputAnchorsAutomaticExecutionWithoutHumanEvent(t *testing.T) {
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	previous := CurrentTraceAdapter()
	adapter := NewTraceAdapter(store)
	SetTraceAdapter(adapter)
	t.Cleanup(func() { SetTraceAdapter(previous) })
	RecordContextInput("cron-owner", "cron", "scheduled poll api_key=cron-secret")
	ctx, origin := BindTraceTurn(context.Background(), "cron-owner")
	defer EndTraceTurn(ctx)
	rows := store.Events("cron-owner")
	if len(rows) != 1 || rows[0].Kind != "context" || rows[0].Source != "cron" || rows[0].Turn != origin.Turn || strings.Contains(rows[0].Text, "cron-secret") {
		t.Fatal("automatic submission lost context provenance, ownership, or redaction")
	}
}
