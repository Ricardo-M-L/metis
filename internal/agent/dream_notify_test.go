package agent

// dream_notify_test.go — Phase G.5 (2026-05-12) locks the DreamTask
// phase model + notification envelope.
//
// Coverage:
//   1. ExtractorStats.Phase starts at Idle and transitions through
//      Done after a successful fork.
//   2. DreamNotify channel receives a notification with files
//      touched + duration + session count after a successful run.
//   3. injectDreamNotifications consumes pending notifications and
//      appends one <memory_consolidation_done> envelope to the loop.
//   4. nil DreamNotify is a no-op for the inject path (no message
//      written, no panic).
//   5. formatDreamNotifications collapses N items into one envelope
//      and renders both success + failure rows clearly.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestAutoMemoryExtractor_PhaseTransitionsToDone verifies that
// after a fork completes, Stats().Phase reports Done and the
// extractor records duration + counts the run.
func TestAutoMemoryExtractor_PhaseTransitionsToDone(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "nothing new"}}, StopReason: "end_turn"},
		},
	}
	loop, ext := newTestExtractor(t, prov, t.TempDir())
	loop.AppendUser("hi")
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{{Type: "text", Text: "ok"}},
	})
	loop.mu.Unlock()

	// Before any fork: idle.
	if p := ext.Stats().Phase; p != DreamPhaseIdle {
		t.Errorf("initial Phase = %q, want %q", p, DreamPhaseIdle)
	}

	ext.OnLoopEnd(context.Background(), "end_turn")
	waitFor(t, time.Second, func() bool {
		return ext.Stats().Phase == DreamPhaseDone
	})
	st := ext.Stats()
	if st.Phase != DreamPhaseDone {
		t.Errorf("final Phase = %q, want %q", st.Phase, DreamPhaseDone)
	}
	if st.TotalExtractions != 1 {
		t.Errorf("TotalExtractions = %d, want 1", st.TotalExtractions)
	}
	if st.LastDuration <= 0 {
		t.Errorf("LastDuration not recorded: %v", st.LastDuration)
	}
	if st.InProgress {
		t.Errorf("InProgress should be false after Done")
	}
}

// TestAutoMemoryExtractor_DreamNotifyChannelFires sends a fork
// through the extractor while watching a buffered notify channel
// and verifies a DreamNotification arrives.
func TestAutoMemoryExtractor_DreamNotifyChannelFires(t *testing.T) {
	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "no changes"}}, StopReason: "end_turn"},
		},
	}
	loop, ext := newTestExtractor(t, prov, t.TempDir())
	loop.AppendUser("hi")
	loop.mu.Lock()
	loop.Messages = append(loop.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{{Type: "text", Text: "ok"}},
	})
	loop.mu.Unlock()

	ch := make(chan DreamNotification, 4)
	ext.SetDreamNotify(ch)

	ext.OnLoopEnd(context.Background(), "end_turn")

	select {
	case n := <-ch:
		if n.Phase != DreamPhaseDone {
			t.Errorf("Notification.Phase = %q, want %q", n.Phase, DreamPhaseDone)
		}
		if n.Duration <= 0 {
			t.Errorf("Notification.Duration not set: %v", n.Duration)
		}
		// fork wrote nothing here; FilesTouched should be empty.
		if len(n.FilesTouched) != 0 {
			t.Errorf("FilesTouched should be empty for no-op fork; got %v", n.FilesTouched)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("DreamNotification didn't arrive within 2s")
	}
}

// TestAutoMemoryExtractor_FilesTouchedTracked plants a memory file
// AFTER snapshot baseline and confirms the next fork's
// FilesTouched lists it (we don't actually rely on the fork writing
// — we drop a file ourselves between scans, but the diff logic is
// the same).
func TestAutoMemoryExtractor_FilesTouchedTracked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pre := map[string]struct{}{"old_user_role.md": {}}
	post := map[string]struct{}{"old_user_role.md": {}, "new_feedback.md": {}, "new_project.md": {}}
	got := diffMemdirNames(pre, post)
	want := []string{"new_feedback.md", "new_project.md"}
	if len(got) != len(want) {
		t.Fatalf("diff = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("diff[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Also exercise snapshotMemdirNames against a real directory.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("---\nname: a\ndescription: d\ntype: user\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := snapshotMemdirNames(context.Background(), root)
	if err != nil {
		t.Fatalf("snapshotMemdirNames: %v", err)
	}
	if _, ok := names["a.md"]; !ok {
		t.Errorf("snapshot missing a.md; got %v", names)
	}
}

// TestInjectDreamNotifications_NoOpWhenNilChannel verifies the
// Loop.injectDreamNotifications inject path is safe when the
// DreamNotify channel hasn't been wired.
func TestInjectDreamNotifications_NoOpWhenNilChannel(t *testing.T) {
	loop := &Loop{}
	before := len(loop.Messages)
	loop.injectDreamNotifications(context.Background(), nil)
	if len(loop.Messages) != before {
		t.Errorf("nil DreamNotify must not append messages; got %d (was %d)",
			len(loop.Messages), before)
	}
}

// TestInjectDreamNotifications_AppendsEnvelope sends a notification
// onto a wired DreamNotify channel and confirms the loop appends a
// single <memory_consolidation_done> message containing the file
// list + duration.
func TestInjectDreamNotifications_AppendsEnvelope(t *testing.T) {
	ch := make(chan DreamNotification, 2)
	ch <- DreamNotification{
		Phase:        DreamPhaseDone,
		FilesTouched: []string{"feedback_x.md", "project_y.md"},
		Duration:     1234 * time.Millisecond,
		SessionCount: 5,
	}
	ch <- DreamNotification{
		Phase:    DreamPhaseDone,
		Err:      errors.New("provider 429"),
		Duration: 800 * time.Millisecond,
	}
	loop := &Loop{DreamNotify: ch}
	before := len(loop.Messages)

	out := make(chan Event, 4) // captures EventInfo so emit() doesn't block
	loop.injectDreamNotifications(context.Background(), out)

	if got := len(loop.Messages); got != before+1 {
		t.Fatalf("expected 1 new message; got %d (was %d)", got, before)
	}
	msg := loop.Messages[len(loop.Messages)-1]
	if msg.Role != llm.RoleUser {
		t.Errorf("envelope must be RoleUser; got %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("envelope must have 1 content block; got %d", len(msg.Content))
	}
	body := msg.Content[0].Text
	if !strings.Contains(body, "<memory_consolidation_done>") {
		t.Errorf("envelope tag missing in body:\n%s", body)
	}
	if !strings.Contains(body, "feedback_x.md") || !strings.Contains(body, "project_y.md") {
		t.Errorf("file list not rendered:\n%s", body)
	}
	if !strings.Contains(body, "provider 429") {
		t.Errorf("error row not rendered:\n%s", body)
	}
	if !strings.Contains(body, "</memory_consolidation_done>") {
		t.Errorf("envelope close-tag missing:\n%s", body)
	}
}

// TestFormatDreamNotifications_SingleVsMultiHeader checks the
// "Long-term memory was just refreshed:" vs "...refreshed N times"
// phrasing branches without re-entering the inject path.
func TestFormatDreamNotifications_SingleVsMultiHeader(t *testing.T) {
	t.Parallel()
	one := formatDreamNotifications([]DreamNotification{
		{Phase: DreamPhaseDone, Duration: time.Second, FilesTouched: []string{"a.md"}, SessionCount: 1},
	})
	if !strings.Contains(one, "Long-term memory was just refreshed:") {
		t.Errorf("singular header missing:\n%s", one)
	}
	if strings.Contains(one, "refreshed 1 times") {
		t.Errorf("singular path should not use 'N times' phrasing:\n%s", one)
	}
	multi := formatDreamNotifications([]DreamNotification{
		{Phase: DreamPhaseDone, Duration: time.Second, FilesTouched: []string{"a.md"}},
		{Phase: DreamPhaseDone, Duration: 2 * time.Second, FilesTouched: []string{"b.md"}},
	})
	if !strings.Contains(multi, "refreshed 2 times") {
		t.Errorf("multi-header missing 'refreshed 2 times':\n%s", multi)
	}
}
