package checkpoint

import (
	"errors"
	"fmt"
)

// ErrDisabledByPolicy means this workspace is intentionally excluded from
// code checkpoints: the user's home directory, a filesystem root, or an empty
// workspace. It does not indicate failed initialization or damaged storage.
// Callers may skip automatic checkpoint hooks for this error while continuing
// to expose the reason when the user explicitly asks for a code rewind.
var ErrDisabledByPolicy = errors.New("checkpoint: disabled by workspace policy; choose a project directory instead of the home directory or filesystem root")

// DisabledByPolicy reports an intentionally excluded workspace without
// waiting for a snapshot. Workspace controls refresh it at an idle boundary
// under a cross-process lease. Automatic hooks read it atomically so a long
// scan cannot turn a status check into an unbounded mutex wait.
func (m *Manager) DisabledByPolicy() bool { return m.disabledByPolicy.Load() }

// DisabledReason returns nil while checkpointing is enabled, ErrDisabledByPolicy
// for an intentionally excluded workspace, or the original wrapped fatal
// initialization cause. Context cancellation and snapshot scan failures do not
// disable the manager. The returned fatal error supports errors.Is/errors.As.
// Like Disabled, this method serializes with checkpoint operations.
func (m *Manager) DisabledReason() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disabledReasonLocked()
}

// The caller owns m.mu (or has not yet published a newly created Manager).
func (m *Manager) disabledReasonLocked() error {
	if m.controlErr != nil {
		return m.controlErr
	}
	if !m.disabled {
		return nil
	}
	if m.disabledByPolicy.Load() {
		return ErrDisabledByPolicy
	}
	if m.initErr != nil {
		return fmt.Errorf("checkpoint: initialization failed: %w", m.initErr)
	}
	return errors.New("checkpoint: disabled")
}
