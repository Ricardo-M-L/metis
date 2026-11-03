package runtime

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// Desktop's parent reads status while its worker is still waiting for the
// provider. The submitted input must be visible without a token/turn-end sync.
func TestSubmittedTaskInputBoundaryVisibleBeforeProviderEvents(t *testing.T) {
	dir := t.TempDir()
	writer, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	previous := CurrentTraceAdapter()
	SetTraceAdapter(NewTraceAdapter(writer))
	defer SetTraceAdapter(previous)

	RecordUserInput("human-plan", []llm.ContentBlock{{Type: "text", Text: "Start the next task"}})
	RecordContextInput("scheduled-plan", "cron", "Start scheduled task")
	for _, sid := range []string{"human-plan", "scheduled-plan"} {
		if err := reader.Refresh(sid); err != nil {
			t.Fatal(err)
		}
		turn, startedAt := reader.LatestInputBoundary(sid)
		if turn != 1 || startedAt.IsZero() {
			t.Errorf("%s input is hidden until a later provider event: turn=%d startedAt=%s", sid, turn, startedAt)
		}
	}
}
