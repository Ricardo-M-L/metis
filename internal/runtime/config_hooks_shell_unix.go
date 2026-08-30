//go:build !windows

package runtime

// hookShellCommand returns the native command shell invocation for Unix-like
// platforms. Keep this seam separate from process creation so the hook runner
// and its process-group cancellation logic remain identical across platforms.
func hookShellCommand(command string) (string, []string) {
	return "sh", []string{"-c", command}
}
