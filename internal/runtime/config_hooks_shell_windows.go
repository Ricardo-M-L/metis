//go:build windows

package runtime

// cmd.exe is available on every supported Windows installation. /S gives /C
// consistent quote handling for the complete user-configured hook command.
func hookShellCommand(command string) (string, []string) {
	return "cmd.exe", []string{"/S", "/C", command}
}
