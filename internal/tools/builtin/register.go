package builtin

import (
	"os"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

func Register(r *tools.Registry, cfg *config.Config, gate *permission.Gate) {
	RegisterWithSandbox(r, cfg, gate, nil)
}

// RegisterWithSandbox wires every process-launching builtin to the same
// per-runtime Manager. Register remains available for informational/test
// registries that intentionally have no runtime sandbox.
func RegisterWithSandbox(r *tools.Registry, cfg *config.Config, gate *permission.Gate, manager *sandbox.Manager) {
	RegisterWithDirsAndSandbox(r, cfg, gate, cfg.Session.SkillDir, cfg.Session.Dir, manager)
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
	RegisterWithDirsAndSandbox(r, cfg, gate, skillDir, memoryDir, nil)
}

func RegisterWithDirsAndSandbox(r *tools.Registry, cfg *config.Config, gate *permission.Gate, skillDir, memoryDir string, manager *sandbox.Manager) {
	disabled := make(map[string]bool, len(cfg.Tools.Disabled))
	for _, n := range cfg.Tools.Disabled {
		disabled[n] = true
	}
	os.MkdirAll(skillDir, 0o755)
	// memoryDir is the private session store root, not a second memory
	// repository. Older builds also created ../memories here even though no
	// production writer used it; the canonical Repository now owns all memory
	// paths, so boot must not recreate that abandoned tree.
	os.MkdirAll(memoryDir, 0o700)

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
	bashTool := bash.New(gate, cfg.Tools.Bash)
	gitTool := NewGit(gate)
	runCode := NewRunCode(gate)
	lspTool := NewLSP(gate)
	lsTool := NewLS(gate)
	globTool := NewGlob(gate)
	grepTool := NewGrep(gate)
	if manager != nil {
		bashTool = bash.NewWithSandbox(gate, cfg.Tools.Bash, manager)
		gitTool = NewGitWithSandbox(gate, manager)
		runCode = NewRunCodeWithSandbox(gate, manager)
		lspTool = NewLSPWithSandbox(gate, manager)
		lsTool = NewLSWithSandbox(gate, manager)
		globTool = NewGlobWithSandbox(gate, manager)
		grepTool = NewGrepWithSandbox(gate, manager)
	}
	goalCreate := NewGoalCreate(gate)
	goalUpdate := NewGoalUpdate(gate)
	goalList := NewGoalList(gate)
	goalDelete := NewGoalDelete(gate)
	eventRead := NewSessionEventRead(gate)
	eventSearch := NewSessionEventSearch(gate)
	sessionTrace := NewSessionTrace(gate)

	all := []tools.Tool{
		NewArtifact(gate),
		Read{gate: gate, state: sessionReadState, authorizer: newReadPathAuthorizer()},
		Write{gate: gate, state: sessionReadState, authorizer: newInvocationAuthorizer[approvedWriteTarget]()},
		Edit{gate: gate, state: sessionReadState, authorizer: newInvocationAuthorizer[approvedExistingPath]()},
		bashTool,
		lsTool,
		globTool,
		grepTool,
		WebFetch{gate: gate},
		WebBrowse{gate: gate},
		ViewImage{gate: gate},
		gitTool,
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
		lspTool,
		&runCode,
		&goalCreate,
		&goalUpdate,
		&goalList,
		&goalDelete,
		&eventRead,
		&eventSearch,
		&sessionTrace,
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
