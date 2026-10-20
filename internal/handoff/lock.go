package handoff

import (
	"context"
	"errors"
	"os"
	"time"
)

type fileLock struct{ file *os.File }

func acquireLock(ctx context.Context, p string) (*fileLock, error) {
	if info, err := os.Lstat(p); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("refusing symlinked handoff lock")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := openLock(p)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*fileLock, error) { return nil, errors.Join(err, f.Close()) }
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		ok, err := tryLock(f)
		if err != nil {
			return fail(err)
		}
		if ok {
			opened, err := f.Stat()
			if err != nil {
				_ = unlock(f)
				return fail(err)
			}
			linked, err := os.Lstat(p)
			if err != nil {
				_ = unlock(f)
				return fail(err)
			}
			if !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, linked) {
				_ = unlock(f)
				return fail(errors.New("handoff lock inode changed"))
			}
			return &fileLock{file: f}, nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail(ctx.Err())
		case <-timer.C:
		}
	}
}
func (l *fileLock) release() { _ = unlock(l.file); _ = l.file.Close() }
