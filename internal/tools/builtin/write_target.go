package builtin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type approvedExistingPath struct {
	rawPath      string
	resolvedPath string
	lexicalInfo  os.FileInfo
	targetInfo   os.FileInfo
}

type approvedNewPath struct {
	rawPath  string
	ancestor approvedExistingPath
	suffix   []string // missing components at approval time; leaf is last
	stateKey string
}

type approvedWriteTarget struct {
	existing *approvedExistingPath
	newPath  *approvedNewPath
}

func (p approvedWriteTarget) stillPrepared() bool {
	if p.existing != nil {
		return p.existing.matchesCurrent(p.existing.targetInfo)
	}
	if p.newPath == nil || !p.newPath.ancestor.matchesCurrent(p.newPath.ancestor.targetInfo) {
		return false
	}
	cursor := p.newPath.ancestor.rawPath
	for _, component := range p.newPath.suffix {
		cursor = filepath.Join(cursor, component)
		if _, err := os.Lstat(cursor); errors.Is(err, os.ErrNotExist) {
			// Once one component is absent, all descendants are necessarily
			// absent in this namespace too.
			return true
		} else if err != nil {
			return false
		}
		// Any component that existed after approval is a type upgrade/race,
		// even if the final leaf remains missing.
		return false
	}
	return false
}

func prepareExistingPath(path string, wantDirectory bool) (approvedExistingPath, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return approvedExistingPath{}, err
	}
	lexical, err := os.Lstat(absPath)
	if err != nil {
		return approvedExistingPath{}, err
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return approvedExistingPath{}, err
	}
	target, err := os.Stat(absPath)
	if err != nil {
		return approvedExistingPath{}, err
	}
	if wantDirectory {
		if !target.IsDir() {
			return approvedExistingPath{}, fmt.Errorf("target is not a directory: %s", path)
		}
	} else if !target.Mode().IsRegular() {
		return approvedExistingPath{}, fmt.Errorf("target is not a regular file: %s", path)
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return approvedExistingPath{}, err
	}
	return approvedExistingPath{
		rawPath:      absPath,
		resolvedPath: filepath.Clean(resolvedAbs),
		lexicalInfo:  lexical,
		targetInfo:   target,
	}, nil
}

func prepareWriteTarget(path string) (approvedWriteTarget, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return approvedWriteTarget{}, err
	}
	if _, err := os.Lstat(absPath); err == nil {
		existing, prepErr := prepareExistingPath(absPath, false)
		if prepErr != nil {
			return approvedWriteTarget{}, prepErr
		}
		return approvedWriteTarget{existing: &existing}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return approvedWriteTarget{}, err
	}

	cursor := filepath.Dir(absPath)
	suffix := []string{filepath.Base(absPath)}
	for {
		if _, err := os.Lstat(cursor); err == nil {
			ancestor, prepErr := prepareExistingPath(cursor, true)
			if prepErr != nil {
				return approvedWriteTarget{}, prepErr
			}
			stateKey := ancestor.resolvedPath
			for _, component := range suffix {
				stateKey = filepath.Join(stateKey, component)
			}
			return approvedWriteTarget{newPath: &approvedNewPath{
				rawPath: absPath, ancestor: ancestor,
				suffix:   append([]string(nil), suffix...),
				stateKey: filepath.Clean(stateKey),
			}}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return approvedWriteTarget{}, err
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return approvedWriteTarget{}, fmt.Errorf("no existing parent directory for %s", path)
		}
		suffix = append([]string{filepath.Base(cursor)}, suffix...)
		cursor = parent
	}
}

func (p approvedExistingPath) matchesCurrent(opened os.FileInfo) bool {
	if opened == nil || !os.SameFile(p.targetInfo, opened) {
		return false
	}
	lexical, lexicalErr := os.Lstat(p.rawPath)
	resolved, resolvedErr := filepath.EvalSymlinks(p.rawPath)
	target, targetErr := os.Stat(p.rawPath)
	if lexicalErr != nil || resolvedErr != nil || targetErr != nil {
		return false
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return false
	}
	return os.SameFile(p.lexicalInfo, lexical) &&
		os.SameFile(p.targetInfo, target) &&
		filepath.Clean(resolvedAbs) == p.resolvedPath
}

func newPathMatchesCurrent(p approvedNewPath, opened os.FileInfo) bool {
	if opened == nil {
		return false
	}
	lexical, lexicalErr := os.Lstat(p.rawPath)
	resolved, resolvedErr := filepath.EvalSymlinks(p.rawPath)
	target, targetErr := os.Stat(p.rawPath)
	if lexicalErr != nil || resolvedErr != nil || targetErr != nil || lexical.Mode()&os.ModeSymlink != 0 {
		return false
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return false
	}
	return os.SameFile(opened, lexical) && os.SameFile(opened, target) && filepath.Clean(resolvedAbs) == p.stateKey
}

func openApprovedExisting(p approvedExistingPath, flags int, afterOpen func()) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(p.rawPath, flags, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !p.matchesCurrent(opened) {
		_ = f.Close()
		return nil, nil, errors.Join(err, fmt.Errorf("approved file target changed before open: %s", p.rawPath))
	}
	if afterOpen != nil {
		afterOpen()
	}
	return f, opened, nil
}

func readPinnedFile(f *os.File, maxBytes int64) ([]byte, os.FileInfo, error) {
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errors.New("approved target is no longer a regular file")
	}
	if maxBytes > 0 && before.Size() > maxBytes {
		return nil, nil, fmt.Errorf("file too large: %d bytes exceeds %d byte cap", before.Size(), maxBytes)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	reader := io.Reader(f)
	if maxBytes > 0 {
		reader = io.LimitReader(f, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("file too large: exceeds %d byte cap", maxBytes)
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, nil, errors.Join(err, errors.New("file changed while reading approved descriptor"))
	}
	return data, after, nil
}

func verifyPinnedContent(f *os.File, expectedHash string, maxBytes int64) (os.FileInfo, error) {
	data, info, err := readPinnedFile(f, maxBytes)
	if err != nil {
		return nil, err
	}
	if hashBytes(data) != expectedHash {
		return nil, errors.New(FileUnexpectedlyModified)
	}
	return info, nil
}

func replacePinnedFile(f *os.File, content []byte) (os.FileInfo, error) {
	if err := f.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	for written := 0; written < len(content); {
		n, err := f.Write(content[written:])
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrShortWrite
		}
		written += n
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(content)) {
		return nil, errors.New("post-write descriptor state mismatch")
	}
	return info, nil
}

// createApprovedNewFile pins the approved ancestor and every newly-created
// directory as it descends. Each component had to be missing at CanUse and is
// created exactly once; a raced-in file, directory, or symlink fails closed.
func createApprovedNewFile(p approvedNewPath, afterDirectory func(string), beforeLeaf, afterOpen func()) (*os.File, error) {
	ancestorRoot, err := os.OpenRoot(p.ancestor.rawPath)
	if err != nil {
		return nil, err
	}
	roots := []*os.Root{ancestorRoot}
	closeRoots := func() {
		for i := len(roots) - 1; i >= 0; i-- {
			_ = roots[i].Close()
		}
	}
	fail := func(err error) (*os.File, error) {
		closeRoots()
		return nil, err
	}
	ancestorInfo, err := ancestorRoot.Stat(".")
	if err != nil || !p.ancestor.matchesCurrent(ancestorInfo) {
		return fail(errors.Join(err, fmt.Errorf("approved parent changed before create: %s", p.ancestor.rawPath)))
	}
	if len(p.suffix) == 0 {
		return fail(errors.New("approved create target has no leaf"))
	}

	current := ancestorRoot
	currentRawPath := p.ancestor.rawPath
	for i, component := range p.suffix[:len(p.suffix)-1] {
		if _, err := current.Lstat(component); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = fmt.Errorf("approved-missing directory raced into existence: %s", component)
			}
			return fail(err)
		}
		if err := current.Mkdir(component, 0o755); err != nil {
			return fail(err)
		}
		createdInfo, err := current.Lstat(component)
		if err != nil || !createdInfo.IsDir() || createdInfo.Mode()&os.ModeSymlink != 0 {
			return fail(errors.Join(err, fmt.Errorf("created directory was replaced: %s", component)))
		}
		child, err := current.OpenRoot(component)
		if err != nil {
			return fail(err)
		}
		roots = append(roots, child)
		openedInfo, err := child.Stat(".")
		if err != nil || !os.SameFile(createdInfo, openedInfo) {
			return fail(errors.Join(err, fmt.Errorf("created directory changed while pinning: %s", component)))
		}
		current = child
		currentRawPath = filepath.Join(currentRawPath, component)
		if afterDirectory != nil {
			afterDirectory(filepath.Join(p.suffix[:i+1]...))
		}
		if !pinnedCreatedDirectoryMatchesPath(currentRawPath, child, openedInfo) {
			return fail(fmt.Errorf("created directory path changed after pinning: %s", currentRawPath))
		}
	}

	leaf := p.suffix[len(p.suffix)-1]
	if _, err := current.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("approved-new leaf raced into existence: %s", p.rawPath)
		}
		return fail(err)
	}
	if beforeLeaf != nil {
		beforeLeaf()
	}
	if current == ancestorRoot {
		currentInfo, statErr := current.Stat(".")
		if statErr != nil || !p.ancestor.matchesCurrent(currentInfo) {
			return fail(errors.Join(statErr, fmt.Errorf("approved parent changed before leaf create: %s", currentRawPath)))
		}
	} else {
		currentInfo, statErr := current.Stat(".")
		if statErr != nil || !pinnedCreatedDirectoryMatchesPath(currentRawPath, current, currentInfo) {
			return fail(errors.Join(statErr, fmt.Errorf("created parent changed before leaf create: %s", currentRawPath)))
		}
	}
	f, err := current.OpenFile(leaf, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fail(err)
	}
	if afterOpen != nil {
		afterOpen()
	}
	closeRoots()
	return f, nil
}

func pinnedCreatedDirectoryMatchesPath(rawPath string, root *os.Root, approved os.FileInfo) bool {
	lexical, lexicalErr := os.Lstat(rawPath)
	target, targetErr := os.Stat(rawPath)
	opened, openedErr := root.Stat(".")
	if lexicalErr != nil || targetErr != nil || openedErr != nil || lexical.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return lexical.IsDir() && os.SameFile(approved, opened) && os.SameFile(opened, lexical) && os.SameFile(opened, target)
}
