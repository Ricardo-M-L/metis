package memdir

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// AtomicWritePrivateFile writes one root-relative regular file without ever
// resolving the destination through a process-global path after the memory
// root has been opened. The root and each parent directory are pinned by an
// os.Root handle, the temporary name is unpredictable and opened O_EXCL, and
// permissions are changed only through opened file descriptors.
func AtomicWritePrivateFile(root, relative string, content []byte, perm os.FileMode) error {
	parent, base, err := openPinnedParent(root, relative, true)
	if err != nil {
		return err
	}
	defer parent.Close()

	if info, statErr := parent.Lstat(base); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("memdir: destination is not a regular file: %s", relative)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	tmpName, tmp, err := createExclusiveTemp(parent, base, perm)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		_ = tmp.Close()
		if !renamed {
			_ = parent.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	writtenInfo, err := tmp.Stat()
	if err != nil {
		return err
	}
	if !writtenInfo.Mode().IsRegular() {
		return fmt.Errorf("memdir: temporary inode is not a regular file: %s", tmpName)
	}
	// Keep tmp open through Rename. This lets us verify that the inode now at
	// the destination is the exact private inode that was written above.
	if err := parent.Rename(tmpName, base); err != nil {
		return err
	}
	renamed = true
	committedInfo, err := parent.Lstat(base)
	if err != nil {
		return err
	}
	if committedInfo.Mode()&os.ModeSymlink != 0 || !committedInfo.Mode().IsRegular() || !os.SameFile(writtenInfo, committedInfo) {
		// Fail closed if a same-user adversary managed to replace the random
		// source name during the commit. Never leave attacker-controlled bytes
		// under the trusted memory filename.
		_ = parent.Remove(base)
		return fmt.Errorf("memdir: destination changed during atomic commit: %s", relative)
	}
	if err := syncPinnedDirectory(parent); err != nil {
		return err
	}
	return nil
}

// ReadPrivateRegularFile performs a bounded read through a pinned memory root.
// Symlink components are rejected and the opened leaf must match the inode
// observed by Lstat.
func ReadPrivateRegularFile(root, relative string, maxBytes int64) ([]byte, error) {
	parent, base, err := openPinnedParent(root, relative, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()

	leafInfo, err := parent.Lstat(base)
	if err != nil {
		return nil, err
	}
	if leafInfo.Mode()&os.ModeSymlink != 0 || !leafInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("memdir: source is not a regular file: %s", relative)
	}
	if maxBytes > 0 && leafInfo.Size() > maxBytes {
		return nil, fmt.Errorf("memdir: source exceeds %d byte limit: %s", maxBytes, relative)
	}
	file, err := parent.Open(base)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(leafInfo, openedInfo) {
		return nil, fmt.Errorf("memdir: source changed while opening: %s", relative)
	}
	var reader io.Reader = file
	if maxBytes > 0 {
		reader = io.LimitReader(file, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("memdir: source exceeds %d byte limit: %s", maxBytes, relative)
	}
	return data, nil
}

// OpenPrivateRegularFile opens (or exclusively creates) a regular file through
// a pinned root. It is intended for durable lock files that must stay open
// across a critical section.
func OpenPrivateRegularFile(root, relative string, flag int, perm os.FileMode) (*os.File, error) {
	parent, base, err := openPinnedParent(root, relative, flag&os.O_CREATE != 0)
	if err != nil {
		return nil, err
	}
	defer parent.Close()

	for attempt := 0; attempt < 2; attempt++ {
		before, statErr := parent.Lstat(base)
		if errors.Is(statErr, os.ErrNotExist) && flag&os.O_CREATE != 0 {
			file, openErr := parent.OpenFile(base, flag|os.O_EXCL, perm.Perm())
			if errors.Is(openErr, os.ErrExist) {
				continue
			}
			if openErr != nil {
				return nil, openErr
			}
			if err := file.Chmod(perm.Perm()); err != nil {
				file.Close()
				_ = parent.Remove(base)
				return nil, err
			}
			return file, nil
		}
		if statErr != nil {
			return nil, statErr
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
			return nil, fmt.Errorf("memdir: file is not regular: %s", relative)
		}
		file, openErr := parent.OpenFile(base, flag&^os.O_CREATE, perm.Perm())
		if openErr != nil {
			return nil, openErr
		}
		openedInfo, statOpenErr := file.Stat()
		if statOpenErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(before, openedInfo) {
			file.Close()
			if statOpenErr != nil {
				return nil, statOpenErr
			}
			return nil, fmt.Errorf("memdir: file changed while opening: %s", relative)
		}
		if err := file.Chmod(perm.Perm()); err != nil {
			file.Close()
			return nil, err
		}
		return file, nil
	}
	return nil, fmt.Errorf("memdir: file changed while creating: %s", relative)
}

// RemovePrivateRegularFile removes a root-relative regular file. Root.Remove
// unlinks the directory entry itself and therefore never follows a leaf
// symlink; rejecting non-regular leaves keeps repository semantics explicit.
func RemovePrivateRegularFile(root, relative string) error {
	parent, base, err := openPinnedParent(root, relative, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Lstat(base)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("memdir: destination is not a regular file: %s", relative)
	}
	if err := parent.Remove(base); err != nil {
		return err
	}
	return syncPinnedDirectory(parent)
}

// RootRelativePath converts an absolute candidate to a clean path beneath the
// configured root without resolving symlinks. The subsequent pinned operation
// is authoritative and rejects any symlink component.
func RootRelativePath(root, candidate string) (string, error) {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	candidateAbs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		if err == nil {
			err = fmt.Errorf("memdir: path escapes root: %s", candidate)
		}
		return "", err
	}
	return filepath.Clean(rel), nil
}

func openPinnedParent(root, relative string, create bool) (*os.Root, string, error) {
	rel, err := cleanRootRelative(relative)
	if err != nil {
		return nil, "", err
	}
	rootHandle, err := openPinnedRoot(root)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	current := rootHandle
	for _, part := range parts[:len(parts)-1] {
		info, statErr := current.Lstat(part)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if err := current.Mkdir(part, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				current.Close()
				return nil, "", err
			}
			info, statErr = current.Lstat(part)
		}
		if statErr != nil {
			current.Close()
			return nil, "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			current.Close()
			return nil, "", fmt.Errorf("memdir: parent is not a directory: %s", part)
		}
		next, openErr := current.OpenRoot(part)
		if openErr != nil {
			current.Close()
			return nil, "", openErr
		}
		openedInfo, openErr := next.Lstat(".")
		if openErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			next.Close()
			current.Close()
			if openErr != nil {
				return nil, "", openErr
			}
			return nil, "", fmt.Errorf("memdir: parent changed while opening: %s", part)
		}
		if err := chmodOpenedDirectory(next, 0o700); err != nil {
			next.Close()
			current.Close()
			return nil, "", err
		}
		current.Close()
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func openPinnedRoot(root string) (*os.Root, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("memdir: empty root")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(absRoot)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("memdir: root is not a real directory: %s", absRoot)
	}
	handle, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, err
	}
	after, err := handle.Lstat(".")
	if err != nil || !after.IsDir() || !os.SameFile(before, after) {
		handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("memdir: root changed while opening: %s", absRoot)
	}
	if err := chmodOpenedDirectory(handle, 0o700); err != nil {
		handle.Close()
		return nil, err
	}
	return handle, nil
}

func cleanRootRelative(relative string) (string, error) {
	relative = filepath.Clean(strings.TrimSpace(relative))
	if relative == "." || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("memdir: invalid root-relative path %q", relative)
	}
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("memdir: invalid root-relative path component %q", part)
		}
	}
	return relative, nil
}

func chmodOpenedDirectory(root *os.Root, mode os.FileMode) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Chmod(mode)
}

func createExclusiveTemp(root *os.Root, base string, perm os.FileMode) (string, *os.File, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", nil, err
		}
		name := "." + base + "." + hex.EncodeToString(token[:]) + ".tmp"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm.Perm())
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if err := file.Chmod(perm.Perm()); err != nil {
			file.Close()
			_ = root.Remove(name)
			return "", nil, err
		}
		return name, file, nil
	}
	return "", nil, errors.New("memdir: could not allocate exclusive temporary file")
}

func syncPinnedDirectory(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
