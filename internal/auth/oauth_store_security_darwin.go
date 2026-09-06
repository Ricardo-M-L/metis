//go:build darwin

package auth

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func secureOAuthStoreDirectory(path string) error {
	return secureOAuthStorePath(path, true)
}

func secureOAuthStoreFile(path string) error {
	return secureOAuthStorePath(path, false)
}

func secureOAuthStorePath(path string, directory bool) error {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if directory && !opened.IsDir() || !directory && !opened.Mode().IsRegular() {
		return errors.New("credential path has an unexpected file type")
	}
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	if err := secureOpenedOAuthStoreDarwin(file, mode); err != nil {
		return err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, linked) {
		return errors.New("credential path changed while securing")
	}
	return nil
}

func secureOpenedOAuthStoreFile(file *os.File) error {
	return secureOpenedOAuthStoreDarwin(file, 0o600)
}

func secureOpenedOAuthStoreDarwin(file *os.File, mode os.FileMode) error {
	if err := file.Chmod(mode); err != nil {
		return err
	}
	// chmod alone preserves macOS ACL grants, including inherited Everyone
	// read/search access. Replace the ACL on this descriptor with an empty,
	// non-inheriting ACL; only the owner-only mode above then grants access.
	// The native attrreference + kauth_filesec layout and flag are defined in
	// Darwin's <sys/attr.h> and <sys/kauth.h>. Using fsetattrlist keeps both
	// permission changes on the same inode and also works with CGO disabled.
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := struct {
		Offset     int32
		Length     uint32
		Magic      uint32
		Owner      [16]byte
		Group      [16]byte
		EntryCount uint32
		Flags      uint32
	}{Offset: 8, Length: 44, Magic: 0x012cc16d, Flags: 1 << 17} // KAUTH_ACL_NO_INHERIT
	_, _, errno := unix.Syscall6(unix.SYS_FSETATTRLIST, file.Fd(),
		uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buffer)), unsafe.Sizeof(buffer), 0, 0)
	runtime.KeepAlive(file)
	if errno != 0 {
		return &os.SyscallError{Syscall: "fsetattrlist credential ACL", Err: errno}
	}
	return nil
}
