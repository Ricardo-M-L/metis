package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecallPolicyClassificationPreservesMixedStorageErrors(t *testing.T) {
	storageErr := errors.New("storage failure")
	for _, tc := range []struct {
		name   string
		err    error
		policy bool
	}{
		{"nil", nil, false},
		{"sensitive", ErrSensitiveMemory, true},
		{"unsafe", ErrUnsafeMemory, true},
		{"deleted", ErrSessionDeleted, true},
		{"wrapped policy", fmt.Errorf("refused: %w", ErrSensitiveMemory), true},
		{"joined policies", errors.Join(ErrSensitiveMemory, ErrSessionDeleted), true},
		{"mixed", errors.Join(ErrSessionDeleted, storageErr), false},
		{"nested mixed", fmt.Errorf("refused: %w", errors.Join(ErrSensitiveMemory, errors.Join(ErrSessionDeleted, storageErr))), false},
		{"storage", storageErr, false},
		{"classified storage wrapping policy", &RecallPersistenceError{Err: ErrSessionDeleted}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRecallPolicyRejection(tc.err); got != tc.policy {
				t.Fatalf("policy classification = %v, want %v", got, tc.policy)
			}
		})
	}
}

func TestRecordTurnClassifiesStorageButNotPolicyRejections(t *testing.T) {
	t.Run("storage", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "private-recall-location")
		manager, err := NewMemoryManager(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "recall", "sessions.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		err = manager.RecordTurn(context.Background(), "s", "m", "user fact", "assistant response")
		var persistenceErr *RecallPersistenceError
		if !errors.As(err, &persistenceErr) || persistenceErr.Unwrap() == nil {
			t.Fatalf("missing classified local persistence error: %v", err)
		}
		encoded, marshalErr := json.Marshal(err)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(err.Error(), "private-recall-location") || strings.Contains(string(encoded), "private-recall-location") {
			t.Fatal("public recall error leaked the storage path")
		}
		var status RecallPersistenceStatus
		if err := json.Unmarshal(encoded, &status); err != nil {
			t.Fatal(err)
		}
		if status.TaskStatus != "completed" || status.MemoryStatus != "failed" || status.Reason != "recall_persistence_failed" {
			t.Fatalf("bad structured status: %+v", status)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		manager, err := NewMemoryManager(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.DeleteSession("deleted"); err != nil {
			t.Fatal(err)
		}
		err = manager.RecordTurn(context.Background(), "deleted", "late", "user", "assistant")
		var persistenceErr *RecallPersistenceError
		if !errors.Is(err, ErrSessionDeleted) || errors.As(err, &persistenceErr) {
			t.Fatalf("deletion refusal mislabeled as storage failure: %v", err)
		}
	})
}
