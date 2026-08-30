//go:build !windows

package mcpoauth

import "os"

func secureTokenStoreDirectory(path string) error {
	return os.Chmod(path, 0o700)
}

func secureTokenStoreFile(path string) error {
	return os.Chmod(path, 0o600)
}
