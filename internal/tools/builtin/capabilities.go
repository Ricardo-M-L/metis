package builtin

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// This file collects the optional capability methods (IsReadOnly,
// IsDestructive, RequiresUserInteraction, InterruptBehavior) for the
// built-in tool family. Centralising them here keeps the per-tool
// .go files focused on their core logic; the runtime reads these via
// type assertion in pkg/tool helpers (tool.IsReadOnly et al.).
//
// Read.IsReadOnly and Edit.IsDestructive live in their own files so
// they're co-located with the staleness logic that depends on the
// same flags. Bash's IsReadOnly is in bash.go because it's input-
// dependent (looks at argv).

// --- read-only tools ----------------------------------------------------

// LS lists directory contents — pure read.
func (LS) IsReadOnly(map[string]any) bool { return true }

// Glob walks the filesystem to find paths — pure read.
func (Glob) IsReadOnly(map[string]any) bool { return true }

// WebFetch downloads a URL but doesn't mutate local state. Treated as
// read-only for Snip purposes; the network is a side effect but not
// one whose evidence we need to retain in full detail.
func (WebFetch) IsReadOnly(map[string]any) bool { return true }

// WebBrowse same rationale as WebFetch.
func (WebBrowse) IsReadOnly(map[string]any) bool { return true }

// WebSearch's IsReadOnly lives in websearch.go alongside the rest of
// the type's methods.

// TodoRead just lists the in-memory todo list.
func (TodoRead) IsReadOnly(map[string]any) bool { return true }

// TaskGet / TaskList / TaskOutput are pure reads on the task store.
func (TaskGet) IsReadOnly(map[string]any) bool    { return true }
func (TaskList) IsReadOnly(map[string]any) bool   { return true }
func (TaskOutput) IsReadOnly(map[string]any) bool { return true }

// SubAgentList / SubAgentOutput inspect the in-process Roster + the
// sub-agent transcript on disk respectively — no mutation, no I/O the
// parent needs to roll back. Mirrors TaskList/TaskOutput which
// claude-code also marks isReadOnly=true. (SubAgentStop kills a
// running sub-agent and is NOT read-only.)
func (SubAgentList) IsReadOnly(map[string]any) bool   { return true }
func (SubAgentOutput) IsReadOnly(map[string]any) bool { return true }

// bash.List / bash.Output are the same shape: enumerate the bash-job
// pool / tail a background command's tee. Read-only. (bash.Kill is
// the destructive counterpart and is NOT marked here.)

// LSP queries language-server diagnostics — read-only.
func (LSP) IsReadOnly(map[string]any) bool { return true }

// Skill list/get are read-only at the user-content boundary. Invoke is not:
// it records usage and trusted skills may execute inline shell expansion.
// The runtime passes the structured input through its read-only hook so Plan
// mode can still use list/get while refusing invoke.
func (Skill) IsReadOnly(in map[string]any) bool {
	action, _ := in["action"].(string)
	return action != "invoke"
}

func (Skill) IsDestructive(in map[string]any) bool {
	action, _ := in["action"].(string)
	return action == "invoke"
}

// --- mutating but reversible -------------------------------------------

// Todo writes the in-memory todo list — mutating but trivially undone.
// Not destructive (we never delete work the user can't recover).
func (Todo) IsReadOnly(map[string]any) bool { return false }

// TaskCreate / TaskUpdate / TaskStop mutate the task store. Not
// destructive — old state is recoverable from JSONL history.
func (TaskCreate) IsReadOnly(map[string]any) bool { return false }
func (TaskUpdate) IsReadOnly(map[string]any) bool { return false }
func (TaskStop) IsReadOnly(map[string]any) bool   { return false }

// Memory writes user-facing memory entries. Mutating but recoverable
// (the file content stays on disk, no rm).
func (Memory) IsReadOnly(map[string]any) bool { return false }

// NotebookEdit modifies a Jupyter notebook cell. Same as Edit —
// mutating + destructive (overwrites cell content).
func (NotebookEdit) IsReadOnly(map[string]any) bool    { return false }
func (NotebookEdit) IsDestructive(map[string]any) bool { return true }

// --- destructive (irreversible) ----------------------------------------

// SendMessage publishes to a real channel (Slack, WeChat, …). Once
// out, the message is unrecallable.
func (SendMessage) IsDestructive(map[string]any) bool { return true }

// ScheduleWakeup schedules a future cron firing. Not destructive
// (cron entries are easy to delete) but worth surfacing.
func (ScheduleWakeup) IsReadOnly(map[string]any) bool { return false }

// Git is input-dependent: `git status` and `git log` are read, but
// `git push`, `git reset --hard`, `git rebase` are destructive. The
// tool already has Concurrency=Exclusive; we treat it as not-readOnly
// for Snip purposes (write-side side effects on the index/refs need
// retained context for the model's next turn).
func (Git) IsReadOnly(map[string]any) bool { return false }

// Agent delegates permission checks to the child's gate-bound tool registry,
// matching Claude Code's normal no-extra-prompt spawn behavior. An explicit
// permission override is still a parent-boundary concern: a more permissive
// child posture is not read-only and must go through the parent's normal
// approval flow. Worktree isolation is never read-only because its setup
// changes git metadata before the child runs.
func (a Agent) IsReadOnly(in map[string]any) bool {
	if isolation, _ := in["isolation"].(string); strings.TrimSpace(isolation) == "worktree" {
		return false
	}

	parentMode := permission.ModeDefault
	if a.gate != nil {
		parentMode = a.gate.Mode()
	}
	requested, hasRequested, err := requestedAgentPermissionMode(in)
	if err != nil {
		return false
	}
	if !hasRequested {
		return true
	}
	return !agentPermissionModeEscalates(parentMode, requested)
}

// Fork is the warm-start variant of Agent — same caveat applies:
// can't statically know what the child did, so not read-only.
func (Fork) IsReadOnly(map[string]any) bool { return false }

// --- interaction-required ----------------------------------------------

// AskUser exists specifically to wait for human input. Even mode
// bypass cannot satisfy this — bypass is "skip user prompts on stuff
// the model proposed", not "answer the model's questions for the user".
// Mirrors Tool.ts:435 requiresUserInteraction.
func (AskUser) RequiresUserInteraction() bool { return true }

// --- interrupt behaviour -----------------------------------------------

// Bash defaults to InterruptBlock: a half-finished `make install`
// is worse than a fully-finished one. The user asks for cancel via
// ^C^C double-tap if they really mean it.

// SendMessage is unrecallable; if it's already in-flight to the
// channel adapter, we can't yank it back. Block for completion so
// the result event correctly reflects "did it actually send?".
func (SendMessage) InterruptBehavior() tools.InterruptBehavior { return tools.InterruptBlock }

// Edit / Write hold an open file handle briefly; an interrupt
// during the write byte stream could leave a half-written file.
// Block to completion so the file is whole-or-untouched.
func (Edit) InterruptBehavior() tools.InterruptBehavior  { return tools.InterruptBlock }
func (Write) InterruptBehavior() tools.InterruptBehavior { return tools.InterruptBlock }

// All read-only tools default to InterruptCancel via the helper
// fallback in pkg/tool — no need to enumerate.
