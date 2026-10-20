//go:build windows

package projectcoord

// Windows does not expose a portable directory fsync through os.File. Each
// segment and run file is flushed before its atomic replacement. Filesystem
// corruption or missing committed segments still fails closed on the next read.
func syncOperationArchiveDirectory(string) error { return nil }
