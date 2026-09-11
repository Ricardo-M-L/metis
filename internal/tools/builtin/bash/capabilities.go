package bash

import "github.com/Ricardo-M-L/metis/internal/tools"

// Capability methods previously lived in internal/tools/builtin/
// capabilities.go alongside every other tool's caps. Moved here on
// 2026-05-20 along with the bash split — Go won't let you attach
// methods to types defined in another package.

// IsReadOnly: List enumerates the job pool; Output tails an existing
// command's tee. Both are read-only. Kill is the destructive
// counterpart and is NOT marked here.
func (List) IsReadOnly(map[string]any) bool   { return true }
func (Output) IsReadOnly(map[string]any) bool { return true }

// Foreground commands must observe an explicit turn cancellation (Esc/Ctrl+C)
// so the caller can join their owned process group before starting a new turn.
// Finite background jobs are joined by the owning Run; explicitly detached
// services keep their separate job-registry lifecycle after successful spawn.
func (Bash) InterruptBehavior() tools.InterruptBehavior { return tools.InterruptCancel }
