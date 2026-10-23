//go:build unix

package checkpoint

import "syscall"

const regularFileOpenFlags = syscall.O_NONBLOCK | syscall.O_NOFOLLOW
