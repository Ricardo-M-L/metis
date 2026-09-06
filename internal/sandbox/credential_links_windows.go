//go:build windows

package sandbox

// Windows has no supported METIS process-sandbox backend. Permission mode
// fails closed before command execution, so inode-link inspection is not used.
func rejectCredentialHardLink(string) error { return nil }
