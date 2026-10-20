//go:build !windows

package projectcoord

import "os"

func syncOperationArchiveDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
