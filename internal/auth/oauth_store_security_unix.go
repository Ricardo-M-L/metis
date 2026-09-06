//go:build !windows && !darwin

package auth

import "os"

func secureOAuthStoreDirectory(path string) error { return os.Chmod(path, 0o700) }
func secureOAuthStoreFile(path string) error      { return os.Chmod(path, 0o600) }

func secureOpenedOAuthStoreFile(file *os.File) error { return file.Chmod(0o600) }
