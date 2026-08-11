//go:build !windows

package update

func runningExecutableMatchesVersion(executable string, _ installLayout, versioned string) bool {
	return sameFile(executable, versioned)
}
