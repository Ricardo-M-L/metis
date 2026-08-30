package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

const (
	repositoryLockName = ".repository.lock"
	tombstoneDirName   = "tombstones"
	// invalidTombstoneSentinel replaces a corrupt or symlinked tombstone after
	// its original bytes or target have been quarantined. It intentionally has
	// no session_id, so sessionDeletedLocked keeps the hashed session deleted
	// with an identity-mismatch error instead of permitting late resurrection.
	invalidTombstoneSentinel = "{\"invalid\":true}\n"
)

var repositoryProcessLocks sync.Map // canonical root -> *sync.Mutex

// withRepositoryLock serializes every read-modify-write sequence for one
// memory repository. The process mutex makes the contract unambiguous even on
// platforms where two file descriptors in one process can share advisory-lock
// state; the file lock extends the same guarantee to Desktop/CLI processes.
func withRepositoryLock(root string, fn func() error) (err error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" {
		return errors.New("memory: empty repository root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	value, _ := repositoryProcessLocks.LoadOrStore(root, &sync.Mutex{})
	processLock := value.(*sync.Mutex)
	processLock.Lock()
	defer processLock.Unlock()

	file, err := memdir.OpenPrivateRegularFile(root, repositoryLockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if err := lockRepositoryFile(file); err != nil {
		return err
	}
	defer func() {
		if unlockErr := unlockRepositoryFile(file); err == nil && unlockErr != nil {
			err = unlockErr
		}
	}()
	return fn()
}

func repositoryRootForTier(root, tier string) string {
	root = filepath.Clean(root)
	if filepath.Base(root) == tier {
		return filepath.Dir(root)
	}
	return root
}

type sessionTombstone struct {
	SessionID string `json:"session_id"`
	DeletedAt string `json:"deleted_at"`
}

func tombstonePath(root, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(root, tombstoneDirName, hex.EncodeToString(sum[:])+".json")
}

// sessionDeletedLocked checks a tombstone while the caller holds the root
// repository lock. Existence is fail-closed: an unreadable or malformed file
// still blocks late writes for the hashed session ID.
func sessionDeletedLocked(root, sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, nil
	}
	path := tombstonePath(root, sessionID)
	relative, err := memdir.RootRelativePath(root, path)
	if err != nil {
		return true, fmt.Errorf("resolve session tombstone: %w", err)
	}
	raw, err := memdir.ReadPrivateRegularFile(root, relative, 1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("read session tombstone: %w", err)
	}
	var tombstone sessionTombstone
	if err := json.Unmarshal(raw, &tombstone); err != nil {
		return true, fmt.Errorf("parse session tombstone: %w", err)
	}
	if tombstone.SessionID != sessionID {
		return true, errors.New("session tombstone identity mismatch")
	}
	return true, nil
}

func markSessionDeletedLocked(root, sessionID string) error {
	deleted, err := sessionDeletedLocked(root, sessionID)
	if deleted {
		return err
	}
	data, err := json.Marshal(sessionTombstone{
		SessionID: sessionID,
		DeletedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	path := tombstonePath(root, sessionID)
	relative, err := memdir.RootRelativePath(root, path)
	if err != nil {
		return err
	}
	return memdir.AtomicWritePrivateFile(root, relative, append(data, '\n'), 0o600)
}

func rejectDeletedSessionLocked(root, sessionID string) error {
	deleted, err := sessionDeletedLocked(root, sessionID)
	if !deleted {
		return err
	}
	if err != nil {
		return errors.Join(ErrSessionDeleted, err)
	}
	return ErrSessionDeleted
}

// sanitizePersistedText redacts a small number of credentials and rejects an
// env/key dump. It is deliberately shared by Recall and Daily so neither tier
// can become a plaintext credential log.
func sanitizePersistedText(content string) (string, error) {
	redacted := memdir.Redact(content)
	if redacted.Reject {
		return "", ErrSensitiveMemory
	}
	return redacted.Redacted, nil
}

func validatePersistedMetadata(values ...string) error {
	for _, value := range values {
		redacted, err := sanitizePersistedText(value)
		if err != nil || redacted != value {
			return ErrSensitiveMemory
		}
	}
	return nil
}
