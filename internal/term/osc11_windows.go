//go:build windows

package term

// DetectTerminalBackground falls back to the configured/default theme on
// Windows. The Unix implementation depends on /dev/tty and poll(2), neither of
// which has a compatible Windows equivalent. Keeping the public function
// available lets all callers compile while avoiding a blocking console probe.
func DetectTerminalBackground() (isLight bool, ok bool) {
	return false, false
}
