//go:build windows

package auth

import (
	"errors"
	"fmt"
	"os"
)

// Windows has no os.OpenFile equivalent of O_NOFOLLOW. Reject reparse points
// before opening, validate the opened identity, apply the current-user-only
// ACL, then validate the linked identity again. This is best effort; the
// protected .credentials directory remains the primary replacement boundary.
func openCredentialStoreFile(path string, maxSize int64, _ bool) (*os.File, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("refusing symlink credential store")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, false, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !before.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, false, errors.Join(errors.New("credential store changed while opening"), file.Close())
	}
	if opened.Size() > maxSize {
		return nil, false, errors.Join(errors.New("credential store is too large"), file.Close())
	}
	if err := secureOAuthStoreFile(path); err != nil {
		return nil, false, errors.Join(fmt.Errorf("secure credential store for current user: %w", err), file.Close())
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		if err == nil {
			err = errors.New("linked credential store identity changed")
		}
		return nil, false, errors.Join(fmt.Errorf("credential store changed while securing: %w", err), file.Close())
	}
	return file, true, nil
}
