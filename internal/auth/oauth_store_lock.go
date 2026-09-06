package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	oauthStoreLockRetry         = 10 * time.Millisecond
	oauthRefreshFailureCooldown = 5 * time.Second
)

var errOAuthRefreshRecentlyFailed = errors.New("llm oauth: a recent refresh attempt failed; retry later or sign in again")

var (
	oauthStoreProcessSem  = make(chan struct{}, 1)
	oauthRefreshProcessMu sync.Mutex
	oauthRefreshProcesses = make(map[string]*oauthRefreshProcessLock)
)

type oauthRefreshProcessLock struct {
	sem  chan struct{}
	refs int
}

type oauthStoreFileLock struct{ file *os.File }

func withOAuthStoreLock(storePath string, timeout time.Duration, fn func() error) (err error) {
	return withOAuthStoreLockContext(context.Background(), storePath, timeout, fn)
}

func withOAuthStoreLockContext(ctx context.Context, storePath string, timeout time.Duration, fn func() error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case oauthStoreProcessSem <- struct{}{}:
		defer func() { <-oauthStoreProcessSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	dir := filepath.Dir(storePath)
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	lock, err := acquireOAuthStoreFileLock(ctx, dir, ".llm-oauth.lock", timeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release LLM OAuth credential-store lock: %w", releaseErr))
		}
	}()
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func oauthRefreshFailedRecently(file *os.File, now time.Time) bool {
	if file == nil {
		return false
	}
	if _, err := file.Seek(0, 0); err != nil {
		return false
	}
	buffer := make([]byte, 64)
	n, err := file.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		// Malformed/unreadable advisory metadata must not block refresh. The
		// credential itself remains protected by the store and provider locks.
		return false
	}
	nanos, err := strconv.ParseInt(strings.TrimSpace(string(buffer[:n])), 10, 64)
	if err != nil || nanos <= 0 {
		return false
	}
	failedAt := time.Unix(0, nanos)
	return !failedAt.After(now) && now.Sub(failedAt) < oauthRefreshFailureCooldown
}

func writeOAuthRefreshFailureMarker(file *os.File, now time.Time) error {
	if file == nil {
		return errors.New("write LLM OAuth refresh failure marker: lock file unavailable")
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate LLM OAuth refresh failure marker: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek LLM OAuth refresh failure marker: %w", err)
	}
	if _, err := file.WriteString(strconv.FormatInt(now.UnixNano(), 10)); err != nil {
		return fmt.Errorf("write LLM OAuth refresh failure marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync LLM OAuth refresh failure marker: %w", err)
	}
	return nil
}

func clearOAuthRefreshFailureMarker(file *os.File) error {
	if file == nil {
		return nil
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("clear LLM OAuth refresh failure marker: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek cleared LLM OAuth refresh failure marker: %w", err)
	}
	return file.Sync()
}

func withOAuthRefreshLease(ctx context.Context, storePath, provider string, fn func() (markFailure bool, err error)) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	processKey := storePath + "\x00" + provider
	releaseProcess, err := acquireOAuthRefreshProcessLock(ctx, processKey)
	if err != nil {
		return err
	}
	defer releaseProcess()

	dir := filepath.Dir(storePath)
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(provider))
	filename := ".llm-oauth-refresh-" + hex.EncodeToString(digest[:16]) + ".lock"
	lock, err := acquireOAuthStoreFileLock(ctx, dir, filename, oauthRefreshLockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release LLM OAuth refresh lease: %w", releaseErr))
		}
	}()
	if err := ensurePrivateOAuthStoreDir(dir); err != nil {
		return err
	}
	if oauthRefreshFailedRecently(lock.file, time.Now()) {
		return errOAuthRefreshRecentlyFailed
	}
	markFailure, err := fn()
	if err != nil {
		if markFailure {
			if markerErr := writeOAuthRefreshFailureMarker(lock.file, time.Now()); markerErr != nil {
				return errors.Join(err, markerErr)
			}
		}
		return err
	}
	if markerErr := clearOAuthRefreshFailureMarker(lock.file); markerErr != nil {
		return markerErr
	}
	return nil
}

func acquireOAuthRefreshProcessLock(ctx context.Context, key string) (func(), error) {
	oauthRefreshProcessMu.Lock()
	lock := oauthRefreshProcesses[key]
	if lock == nil {
		lock = &oauthRefreshProcessLock{sem: make(chan struct{}, 1)}
		oauthRefreshProcesses[key] = lock
	}
	lock.refs++
	oauthRefreshProcessMu.Unlock()

	select {
	case lock.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-lock.sem
				oauthRefreshProcessMu.Lock()
				lock.refs--
				if lock.refs == 0 {
					delete(oauthRefreshProcesses, key)
				}
				oauthRefreshProcessMu.Unlock()
			})
		}, nil
	case <-ctx.Done():
		oauthRefreshProcessMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(oauthRefreshProcesses, key)
		}
		oauthRefreshProcessMu.Unlock()
		return nil, ctx.Err()
	}
}

func ensurePrivateOAuthStoreDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create LLM OAuth credential-store directory: %w", err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("inspect LLM OAuth credential-store directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked LLM OAuth credential-store directory %q", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("LLM OAuth credential-store parent %q is not a directory", dir)
	}
	// `dir` is the dedicated .credentials boundary, never the caller's whole
	// METIS_HOME. It is therefore safe and required to secure it even when the
	// user deliberately points METIS_HOME at a shared or project directory.
	if err := secureOAuthStoreDirectory(dir); err != nil {
		return fmt.Errorf("secure LLM OAuth credential-store directory: %w", err)
	}
	after, err := os.Lstat(dir)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(info, after) {
		return errors.New("LLM OAuth credential-store directory changed while securing")
	}
	return verifyAndPinCredentialLayout(dir)
}

// EnsureCredentialDirectory creates, secures, and pins the canonical private
// credential directory. MCP OAuth shares this boundary with LLM credentials.
func EnsureCredentialDirectory() (string, error) {
	layout, err := currentCredentialLayout()
	if err != nil {
		return "", err
	}
	if err := ensurePrivateOAuthStoreDir(layout.dir); err != nil {
		return "", err
	}
	return layout.dir, nil
}

func acquireOAuthStoreFileLock(ctx context.Context, dir, filename string, timeout time.Duration) (*oauthStoreFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := filepath.Join(dir, filename)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, errors.New("refusing symlinked or non-regular LLM OAuth credential-store lock")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect LLM OAuth credential-store lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open LLM OAuth credential-store lock: %w", err)
	}
	closeWith := func(lockErr error) (*oauthStoreFileLock, error) {
		return nil, errors.Join(lockErr, file.Close())
	}
	if err := verifyOAuthStoreLockFile(path, file); err != nil {
		return closeWith(err)
	}
	if err := secureOAuthStoreFile(path); err != nil {
		return closeWith(fmt.Errorf("secure LLM OAuth credential-store lock: %w", err))
	}

	deadline := time.Now().Add(timeout)
	for {
		locked, err := tryLockOAuthStoreFile(file)
		if err != nil {
			return closeWith(fmt.Errorf("acquire LLM OAuth credential-store lock: %w", err))
		}
		if locked {
			return &oauthStoreFileLock{file: file}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return closeWith(fmt.Errorf("timed out waiting for LLM OAuth credential-store lock after %s", timeout))
		}
		if remaining > oauthStoreLockRetry {
			remaining = oauthStoreLockRetry
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return closeWith(ctx.Err())
		case <-timer.C:
		}
	}
}

func verifyOAuthStoreLockFile(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.New("refusing symlinked, non-regular, or replaced LLM OAuth credential-store lock")
	}
	return nil
}

func (lock *oauthStoreFileLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	return errors.Join(unlockOAuthStoreFile(lock.file), lock.file.Close())
}
