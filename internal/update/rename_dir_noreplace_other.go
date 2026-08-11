//go:build !darwin && !linux && !windows

package update

import "fmt"

func renameDirNoReplace(oldPath, newPath string) error {
	return fmt.Errorf("atomic no-replace directory rename is unsupported on this platform: %s -> %s", oldPath, newPath)
}
