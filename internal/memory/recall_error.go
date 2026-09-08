package memory

import "encoding/json"

// RecallPersistenceError marks a failure in the synchronous local Recall
// storage path after a completed task. It is not an optional enrichment error.
// Keep the original cause available to errors.Is/As, but never expose paths,
// rejected data, or arbitrary repository error text in user-facing output.
type RecallPersistenceError struct {
	Err error `json:"-"`
}

func (*RecallPersistenceError) Error() string {
	return "task completed, but recall memory could not be saved (recall_persistence_failed)"
}

func (e *RecallPersistenceError) Unwrap() error { return e.Err }

// RecallPersistenceStatus separates the task's completed response from its
// failed memory hand-off. Headless callers serialize this on their error path.
type RecallPersistenceStatus struct {
	Kind         string `json:"kind"`
	TaskStatus   string `json:"task_status"`
	MemoryStatus string `json:"memory_status"`
	Reason       string `json:"reason"`
	Message      string `json:"message"`
}

func (e *RecallPersistenceError) Status() RecallPersistenceStatus {
	return RecallPersistenceStatus{
		Kind: "memory_persistence_error", TaskStatus: "completed", MemoryStatus: "failed",
		Reason: "recall_persistence_failed", Message: e.Error(),
	}
}

func (e *RecallPersistenceError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Status())
}

// IsRecallPolicyRejection only accepts errors whose every leaf is an explicit
// refusal to remember content. A tombstone refusal joined with a filesystem
// failure must not hide that failure merely because errors.Is finds a policy
// sentinel somewhere inside the tree.
func IsRecallPolicyRejection(err error) bool {
	switch err := err.(type) {
	case *RecallPersistenceError:
		return false
	case interface{ Unwrap() []error }:
		children := err.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsRecallPolicyRejection(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsRecallPolicyRejection(err.Unwrap())
	default:
		return err == ErrSensitiveMemory || err == ErrUnsafeMemory || err == ErrSessionDeleted
	}
}
