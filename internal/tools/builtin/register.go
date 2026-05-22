package builtin

import (
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func Register(r *tools.Registry, cfg *config.Config, gate *permission.Gate) {
	RegisterWithDirs(r, cfg, gate, cfg.Session.SkillDir, cfg.Session.Dir)
}

// sessionReadState is a package-level pointer so the same store is
// shared across Read/Write/Edit registrations within one session.
// Re-initialised on every RegisterWithDirs call (one per session
// boot), so concurrent test sessions get fresh state.
//
// Exposed via SessionReadState() for runtime code (e.g. /clear) that
// needs to reset it independently of re-registering the tools.
var sessionReadState *ReadFileState

// SessionReadState returns the active session's ReadFileState. May be
// nil before the first RegisterWithDirs call.
func SessionReadState() *ReadFileState { return sessionReadState }

func RegisterWithDirs(r *tools.Registry, cfg *config.Config, gate *permission.Gate, skillDir, memoryDir string) {
	disabled := make(map[string]bool, len(cfg.Tools.Disabled))
	for _, n := range cfg.Tools.Disabled {
		disabled[n] = true
	}
	os.MkdirAll(skillDir, 0o755)
	os.MkdirAll(memoryDir, 0o755)

	// Memory dir for Memory tool
	memDir := filepath.Join(memoryDir, "..", "memories")
	os.MkdirAll(memDir, 0o755)

	// Per-session ReadFileState shared by Read/Write/Edit so
	// stale-write detection works across them.
	sessionReadState = NewReadFileState()

	// NOTE: Skill + Memory are registered in internal/runtime/BuildToolRegistry,
	// not here:
	//   - Skill needs a multi-source loader that includes plugin contributions
	//   - Memory needs a *memory.MemoryManager so writes flow into the same
	//     store BuildContext reads from (otherwise the LLM's writes never
	//     appear in the next turn's system prompt — bug audit 2026-04-30).
	// `metis tools` and the chat REPL both go through BuildToolRegistry
	// after this Register call, so both tools always show up in the end.
	all := []tools.Tool{
		Read{gate: gate, state: sessionReadState},
		Write{gate: gate, state: sessionReadState},
		Edit{gate: gate, state: sessionReadState},
		bash.New(gate, cfg.Tools.Bash),
		LS{gate: gate},
		Glob{gate: gate},
		Grep{gate: gate},
		WebFetch{gate: gate},
		WebBrowse{gate: gate},
		ViewImage{gate: gate},
		Git{gate: gate},
		WebSearch{gate: gate},
		Todo{gate: gate},
		TodoRead{gate: gate},
		AskUser{gate: gate},
		NotebookEdit{gate: gate},
		TaskCreate{gate: gate},
		TaskGet{gate: gate},
		TaskList{gate: gate},
		TaskUpdate{gate: gate},
		TaskOutput{gate: gate},
		TaskStop{gate: gate},
		LSP{gate: gate},
	}
	_ = skillDir // referenced by BuildToolRegistry, kept here for symmetry
	for _, t := range all {
		if disabled[t.Name()] {
			continue
		}
		// Self-aware availability check (claude-code Tool.isEnabled
		// parity): a tool that knows it can't run in this environment
		// (LSP w/o gopls, hypothetical WebBrowse w/o chromium) returns
		// false and is filtered out before the model ever sees it. Most
		// tools embed tools.BaseTool which returns true unconditionally.
		if !t.IsEnabled() {
			continue
		}
		r.Register(t)
	}
}
