package agent

import (
	"errors"
	"os"
	"path/filepath"
)

// withCronStorageLock serializes durable cron transactions across Metis
// processes. The JSON files are individually published with atomic renames,
// but a scheduler fire is a read-modify-write operation and must not race a
// sibling CLI/Desktop pause, remove, update, or manual run.
func withCronStorageLock(root string, fn func() error) (err error) {
	lockFile, err := os.OpenFile(filepath.Join(root, ".cron.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := lockCronFile(lockFile); err != nil {
		_ = lockFile.Close()
		return err
	}
	defer func() {
		unlockErr := unlockCronFile(lockFile)
		closeErr := lockFile.Close()
		if err == nil {
			err = errors.Join(unlockErr, closeErr)
		}
	}()
	return fn()
}
