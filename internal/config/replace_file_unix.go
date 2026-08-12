//go:build !windows

package config

import "os"

func replaceUserConfigFile(src, dst string) error {
	return os.Rename(src, dst)
}
