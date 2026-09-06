//go:build windows

package mcpoauth

import (
	"errors"
	"fmt"
	"os"
)

func openTokenStoreFile(path string, maxSize int64) (*os.File, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("refusing symlink token store")
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
		return nil, false, errors.Join(errors.New("token store changed while opening"), file.Close())
	}
	if opened.Size() > maxSize {
		return nil, false, errors.Join(errors.New("token store is too large"), file.Close())
	}
	if err := secureTokenStoreFile(path); err != nil {
		return nil, false, errors.Join(fmt.Errorf("secure token store for current user: %w", err), file.Close())
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		if err == nil {
			err = errors.New("linked token store identity changed")
		}
		return nil, false, errors.Join(fmt.Errorf("token store changed while securing: %w", err), file.Close())
	}
	return file, true, nil
}
