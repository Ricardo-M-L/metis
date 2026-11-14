package memory

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

const distillationQueueDir = "distillation-queue"

// CheckSourceSessionAtRoot is the queue worker's pre-resolution revocation
// guard. It requires neither workspace configuration nor provider credentials.
func CheckSourceSessionAtRoot(root, sessionID string) error {
	return withRepositoryLock(root, func() error { return rejectDeletedSessionLocked(root, sessionID) })
}

// The queue is owned by agent, so deletion edits only its stable provenance,
// payload and status fields and preserves the remaining envelope verbatim.
// state.lock is shared with agent's enqueue/claim/finish transitions.
func revokeSessionDistillationQueue(root, sessionID string) error {
	entries, err := os.ReadDir(filepath.Join(root, distillationQueueDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lock, err := memdir.OpenPrivateRegularFile(root, filepath.Join(distillationQueueDir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockRepositoryFile(lock); err != nil {
		return err
	}
	defer unlockRepositoryFile(lock)
	entries, err = os.ReadDir(filepath.Join(root, distillationQueueDir))
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if len(id) != 64 {
			errs = append(errs, errors.New("invalid distillation queue identity"))
			continue
		}
		if _, err := hex.DecodeString(id); err != nil {
			errs = append(errs, errors.New("invalid distillation queue identity"))
			continue
		}
		relative := filepath.Join(distillationQueueDir, entry.Name())
		raw, err := memdir.ReadPrivateRegularFile(root, relative, 16<<20)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var job map[string]json.RawMessage
		if err := json.Unmarshal(raw, &job); err != nil {
			errs = append(errs, fmt.Errorf("read distillation queue envelope: %w", err))
			continue
		}
		var source string
		if err := json.Unmarshal(job["session_id"], &source); err != nil {
			errs = append(errs, errors.New("invalid distillation queue source"))
			continue
		}
		if source != sessionID {
			continue
		}
		job["status"] = json.RawMessage(`"revoked"`)
		job["completed_at"], _ = json.Marshal(time.Now().UTC())
		delete(job, "user_message")
		delete(job, "assistant_message")
		delete(job, "last_failure")
		delete(job, "lease_until")
		delete(job, "next_attempt_at")
		data, err := json.Marshal(job)
		if err == nil {
			err = memdir.AtomicWritePrivateFile(root, relative, append(data, '\n'), 0o600)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
