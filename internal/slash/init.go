package slash

import "strings"

// RegisterInitCommand installs the model-directed /init workflow. The handler
// intentionally performs no filesystem writes: repository inspection and the
// resulting CLAUDE.md edit run through the ordinary agent/tool permission path.
func RegisterInitCommand(r *Registry) {
	r.Register(Cmd{
		Name:         "init",
		Description:  "analyze this repository and create or improve CLAUDE.md",
		ArgumentHint: "[focus]",
		Category:     "project",
		Handler: func(args string) (string, Signal) {
			return buildInitPrompt(strings.TrimSpace(args)), SignalCustomPrompt
		},
	})
}

func buildInitPrompt(userFocus string) string {
	var b strings.Builder
	b.WriteString("# Initialize repository guidance\n\n")
	b.WriteString("Analyze this repository and create or improve `CLAUDE.md` for future coding-agent sessions. ")
	b.WriteString("This is a repository-analysis task, not a generic template-writing task.\n\n")
	b.WriteString("## Workflow\n\n")
	b.WriteString("1. Inspect the repository before editing. Read the existing `CLAUDE.md`, `AGENTS.md`, or equivalent instruction files, plus the README and the smallest useful set of manifests, build scripts, CI configuration, entry points, and tests.\n")
	b.WriteString("2. Infer only facts supported by those files: repository purpose and layout, build/test/lint commands, local conventions, generated-code boundaries, and any important safety or release constraints.\n")
	b.WriteString("3. Create `CLAUDE.md` when it is absent. If it already exists, improve it in place and preserve useful project-specific instructions instead of replacing them wholesale.\n")
	b.WriteString("4. Keep the result concise and operational. Prefer exact commands and paths over prose. Do not invent commands, architecture, policies, or conventions that you cannot verify from the repository.\n")
	b.WriteString("5. Re-read the finished file and report what you added or changed. Do not modify unrelated files.\n")
	if userFocus != "" {
		b.WriteString("\n## User focus\n\n")
		b.WriteString(userFocus)
		b.WriteString("\n")
	}
	return b.String()
}
