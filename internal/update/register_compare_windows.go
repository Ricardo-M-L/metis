//go:build windows

package update

import (
	"os"
	"path/filepath"
	"strings"
)

func runningExecutableMatchesVersion(executable string, layout installLayout, versioned string) bool {
	if sameFileContents(executable, versioned) {
		return true
	}
	info, err := os.Lstat(versioned)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	executable, err = filepath.Abs(executable)
	if err != nil || !strings.EqualFold(filepath.Dir(executable), filepath.Dir(layout.launcher)) {
		return false
	}
	name := filepath.Base(executable)
	// QueryFullProcessImageName may retain the original stable name after the
	// launcher is switched. A renamed backup, however, must pass the content
	// comparison above; its filename alone is never sufficient evidence.
	return strings.EqualFold(name, executableName())
}
