package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type liveFile struct {
	rel  string
	info os.FileInfo
}

type liveTree struct {
	files []liveFile
	dirs  []string
}

// Inventory holds an os.Root for each directory while descending it. Each
// directory is checked once, rather than re-Lstat'ing every ancestor for every
// file. Holding directory handles and matching identity before/after opening
// also rejects a directory exchanged for an in-root symlink to a skipped tree.
func (m *Manager) inventoryTree(ctx context.Context, root *os.Root, observer *scanObserver) (liveTree, error) {
	observer.stage("inventory")
	var tree liveTree
	cwd, err := filepath.Abs(m.cwd)
	if err != nil {
		return tree, err
	}
	shadow, err := filepath.Abs(m.shadowDir)
	if err != nil {
		return tree, err
	}
	shadowRel, _ := filepath.Rel(cwd, shadow)
	shadowRel = filepath.ToSlash(shadowRel)
	var visit func(*os.Root, string) error
	visit = func(directory *os.Root, prefix string) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		f, err := directory.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := f.ReadDir(-1)
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			rel := entry.Name()
			if prefix != "" {
				rel = prefix + "/" + rel
			}
			if skipDirs[entry.Name()] || rel == shadowRel || strings.HasPrefix(shadowRel, rel+"/") {
				continue
			}
			// DirEntry.Info may resolve its stored name after an ancestor was
			// renamed; resolve metadata against the held parent handle instead.
			info, err := directory.Lstat(entry.Name())
			if err != nil {
				return err
			}
			if info.IsDir() {
				child, err := openInventoryDirectory(directory, entry.Name(), info)
				if err != nil {
					return err
				}
				tree.dirs = append(tree.dirs, rel)
				observer.stats.Directories++
				observer.notify(false)
				walkErr := visit(child, rel)
				closeErr := child.Close()
				if err := errors.Join(walkErr, closeErr); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
				continue
			}
			tree.files = append(tree.files, liveFile{rel: rel, info: info})
			observer.stats.Files++
			observer.stats.Bytes += info.Size()
			observer.notify(false)
		}
		return nil
	}
	err = visit(root, "")
	return tree, err
}

func openInventoryDirectory(parent *os.Root, name string, expected os.FileInfo) (*os.Root, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, statErr := child.Stat(".")
	current, lstatErr := parent.Lstat(name)
	if err := errors.Join(statErr, lstatErr); err != nil {
		_ = child.Close()
		return nil, err
	}
	if !current.IsDir() || !os.SameFile(expected, actual) || !os.SameFile(expected, current) {
		_ = child.Close()
		return nil, errors.New("checkpoint: directory changed while opening inventory")
	}
	return child, nil
}

func readLiveFile(root *os.Root, file liveFile) ([]byte, error) {
	// A regular inventory entry can be replaced by a FIFO before Open. On
	// Unix a blocking open would wait indefinitely before Stat can reject it.
	// Nonblocking/no-follow flags apply only to opening; regular-file reads
	// still have the underlying filesystem's IO latency.
	f, err := root.OpenFile(file.rel, os.O_RDONLY|regularFileOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || !os.SameFile(file.info, before) || before.Size() != file.info.Size() ||
		!before.ModTime().Equal(file.info.ModTime()) || before.Mode() != file.info.Mode() {
		return nil, errors.New("checkpoint: live file changed during inventory")
	}
	body, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != before.Size() || after.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		return nil, errors.New("checkpoint: live file changed while reading")
	}
	return body, nil
}

type fileResult struct {
	rel       string
	changed   bool
	copied    bool
	unchanged bool
	err       error
}

// Bound both parallel IO and retained file buffers. Every worker is joined
// before returning, including cancellation/error paths: no background writer
// can mutate the mirror after the caller reports a failed checkpoint.
func processLiveFiles(ctx context.Context, files []liveFile, observer *scanObserver, apply func(liveFile) fileResult) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	const concurrency = 8
	jobs := make(chan liveFile)
	results := make(chan fileResult, concurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				if ctx.Err() != nil {
					return
				}
				result := apply(file)
				results <- result
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, file := range files {
			select {
			case jobs <- file:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	var firstErr error
	var changed []string
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
			cancel()
		}
		if result.err == nil {
			observer.stats.ProcessedFiles++
			if result.copied {
				observer.stats.CopiedFiles++
			}
			if result.unchanged {
				observer.stats.UnchangedFiles++
			}
			if result.changed {
				changed = append(changed, result.rel)
			}
		}
		observer.notify(false)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return changed, nil
}

func (m *Manager) syncTreeContext(ctx context.Context, observer *scanObserver) error {
	source, err := os.OpenRoot(m.cwd)
	if err != nil {
		return err
	}
	defer source.Close()
	tree, err := m.inventoryTree(ctx, source, observer)
	if err != nil {
		return err
	}
	mirror, err := os.OpenRoot(m.shadowDir)
	if err != nil {
		return err
	}
	defer mirror.Close()
	observer.stage("reconcile")
	live := make(map[string]struct{}, len(tree.files))
	for _, file := range tree.files {
		live[file.rel] = struct{}{}
	}
	// Include untracked leftovers from an interrupted copy. A subsequent
	// successful snapshot must not resurrect a live file removed meanwhile.
	tracked, err := m.gitOutputBytesContext(ctx, "ls-files", "--cached", "--others", "-z")
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(string(tracked), "\x00") {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if rel == "" {
			continue
		}
		if normalized, ok := m.validRelativePath(rel); !ok || normalized != filepath.ToSlash(rel) {
			return errors.New("checkpoint: unsafe tracked shadow path")
		}
		if _, exists := live[rel]; !exists {
			if err := mirror.RemoveAll(rel); err != nil {
				return err
			}
			observer.stats.RemovedFiles++
		}
		observer.notify(false)
	}
	// Parent-before-child order resolves file-to-directory replacements once,
	// before parallel leaf writes. Root operations remain confined to mirror.
	for _, rel := range tree.dirs {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		info, err := mirror.Lstat(rel)
		if err == nil && !info.IsDir() {
			if err := mirror.RemoveAll(rel); err != nil {
				return err
			}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err != nil || (info != nil && !info.IsDir()) {
			if err := mirror.MkdirAll(rel, 0o700); err != nil {
				return err
			}
		}
		observer.notify(false)
	}
	observer.stage("copy")
	_, err = processLiveFiles(ctx, tree.files, observer, func(file liveFile) fileResult {
		changed, err := syncLiveFile(ctx, source, mirror, file)
		return fileResult{rel: file.rel, copied: changed, unchanged: !changed, err: err}
	})
	return err
}

func syncLiveFile(ctx context.Context, source, mirror *os.Root, file liveFile) (bool, error) {
	body, err := readLiveFile(source, file)
	if err != nil {
		return false, err
	}
	if ctx.Err() != nil {
		return false, context.Cause(ctx)
	}
	mode := os.FileMode(0o600)
	if file.info.Mode()&0o111 != 0 {
		mode = 0o700
	}
	info, err := mirror.Lstat(file.rel)
	if err == nil && !info.Mode().IsRegular() {
		if err := mirror.RemoveAll(file.rel); err != nil {
			return false, err
		}
		info = nil
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	// Compare actual bytes, never just size/mtime. This also repairs an
	// interrupted mirror write or a same-size/same-mtime external edit.
	if info != nil && info.Size() == int64(len(body)) && info.Mode().Perm() == mode {
		previous, err := readLiveFile(mirror, liveFile{rel: file.rel, info: info})
		if err != nil {
			return false, err
		}
		if bytes.Equal(previous, body) {
			return false, nil
		}
	}
	if ctx.Err() != nil {
		return false, context.Cause(ctx)
	}
	f, err := mirror.OpenFile(file.rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|regularFileOpenFlags, mode)
	if err != nil {
		return false, err
	}
	opened, statErr := f.Stat()
	if statErr != nil || !opened.Mode().IsRegular() {
		_ = f.Close()
		if statErr != nil {
			return false, statErr
		}
		return false, errors.New("checkpoint: mirror path replaced by non-regular file")
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return false, err
	}
	_, writeErr := f.Write(body)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return false, fmt.Errorf("checkpoint: mirror write: %w", err)
	}
	return true, nil
}
