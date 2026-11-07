package tasks

import "os"

// A plan envelope is shared by live roots and late child processes. Rename
// prevents torn JSON; this lock also prevents a stale read/modify/write from
// overwriting a newer active plan or a sibling's archived updates.
func lockTaskMutation(sessionID string) (func(), error) {
	mu.Lock()
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		mu.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(path(sessionID)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err = lockTaskFile(f); err != nil {
		f.Close()
		mu.Unlock()
		return nil, err
	}
	return func() { _ = unlockTaskFile(f); _ = f.Close(); mu.Unlock() }, nil
}
