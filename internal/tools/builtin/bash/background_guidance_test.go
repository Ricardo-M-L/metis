package bash

import (
	"context"
	"strings"
	"testing"
)

func assertBackgroundCompletionScope(t *testing.T, text string) {
	t.Helper()
	for _, marker := range []string{"TUI/Desktop", "readline", "one-shot", "await_completion=true", "required_for_completion=true", "persistent", "servers"} {
		if !strings.Contains(text, marker) {
			t.Errorf("background guidance missing %q: %s", marker, text)
		}
	}
}

func TestBashDescriptionsExplainBackgroundContinuationScope(t *testing.T) {
	assertBackgroundCompletionScope(t, (Bash{}).ShortDescription())
	assertBackgroundCompletionScope(t, (Bash{}).Description())
}

func TestExplicitBackgroundOutputExplainsContinuationScope(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	t.Cleanup(func() { pool.ResetAndWait(0) })
	res, err := (Bash{gate: gate, Jobs: pool}).Execute(context.Background(), map[string]any{
		"command": "printf completion-scope", "description": "check background completion guidance", "run_in_background": true,
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("Execute: result=%+v err=%v", res, err)
	}
	assertBackgroundCompletionScope(t, res.Output)
	if res.Presentation["await_completion"] == true || res.Presentation["required_for_completion"] == true {
		t.Fatalf("guidance changed default completion semantics: %#v", res.Presentation)
	}
}
