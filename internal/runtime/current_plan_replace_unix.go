//go:build !windows

package runtime

import "os"

func replaceCurrentPlanFile(src, dst string) error {
	return os.Rename(src, dst)
}
