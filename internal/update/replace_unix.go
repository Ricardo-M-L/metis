//go:build !windows

package update

import "os"

func replaceFileAtomic(src, dst string) error { return os.Rename(src, dst) }
