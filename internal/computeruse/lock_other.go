//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package computeruse

import (
	"errors"
	"os"
)

func openInstallLock(string) (*os.File, error) {
	return nil, errors.New("computer-use installation advisory locking is unsupported on this platform")
}

func tryInstallLock(*os.File) (bool, error) {
	return false, errors.New("computer-use installation advisory locking is unsupported on this platform")
}
