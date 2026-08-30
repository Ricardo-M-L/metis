package shellguard

import "testing"

func TestCheckAllowsWorkflowEnvironmentProbe(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		`[ -n "${OPENAI_API_KEY:-}" ]`,
		`if true; then echo leaked; exit 9; fi`,
		`if [ -n "${OPENAI_API_KEY:-}" ]; then echo leaked; exit 9; fi`,
		`printf %s "$TMPDIR"`,
		`if [ -n "${OPENAI_API_KEY:-}" ]; then echo leaked; exit 9; fi; printf %s "$TMPDIR"`,
	} {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) = %v, want safe shell control flow to remain allowed", command, err)
			}
		})
	}
}
