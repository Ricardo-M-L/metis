package mcpoauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

const (
	tokenStoreLockFilename  = ".mcp-oauth.lock"
	tokenStoreLockTimeout   = 2 * time.Second
	tokenStoreLockRetry     = 10 * time.Millisecond
	tokenRefreshLockTimeout = 30 * time.Second
)

var (
	tokenStoreMu             sync.Mutex
	errTokenStoreLockTimeout = errors.New("timed out waiting for MCP OAuth token-store lock")
	tokenRefreshProcessMu    sync.Mutex
	tokenRefreshProcessLocks = make(map[string]*tokenRefreshProcessLock)
)

type tokenRefreshProcessLock struct {
	sem  chan struct{}
	refs int
}

type tokenStoreFileLock struct {
	file *os.File
}

// withTokenStoreLock serializes the complete token-store transaction in this
// process and between concurrently running CLI/Desktop processes. The lock is
// a stable sidecar file: locking mcp-oauth.json itself would be incorrect
// because atomic replacement changes that file's inode.
func withTokenStoreLock(storePath string, timeout time.Duration, fn func() error) (err error) {
	if strings.TrimSpace(storePath) == "" {
		return errors.New("MCP OAuth credential directory is unavailable")
	}
	tokenStoreMu.Lock()
	defer tokenStoreMu.Unlock()

	dir := filepath.Dir(storePath)
	if err := ensurePrivateTokenStoreDir(dir); err != nil {
		return err
	}
	lock, err := acquireTokenStoreFileLock(dir, timeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release MCP OAuth token-store lock: %w", releaseErr))
		}
	}()
	if err := ensurePrivateTokenStoreDir(dir); err != nil {
		return err
	}
	return fn()
}

func ensurePrivateTokenStoreDir(dir string) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create MCP OAuth token-store directory: %w", err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("inspect MCP OAuth token-store directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked MCP OAuth token-store directory %q", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("MCP OAuth token-store parent %q is not a directory", dir)
	}
	// The fixed .credentials directory is always a private boundary even when
	// METIS_HOME itself intentionally points at a shared/project directory.
	// Preserve the old behavior for tests/embedders that explicitly construct a
	// TokenStore at another path.
	if filepath.Base(dir) == auth.CredentialDirectoryName {
		if canonical := auth.CredentialDirectory(); canonical != "" && filepath.Clean(canonical) == filepath.Clean(dir) {
			if _, err := auth.EnsureCredentialDirectory(); err != nil {
				return fmt.Errorf("pin MCP OAuth credential-store directory: %w", err)
			}
		}
		if err := secureTokenStoreDirectory(dir); err != nil {
			return fmt.Errorf("set MCP OAuth token-store directory permissions to 0700: %w", err)
		}
	}
	return nil
}

func acquireTokenStoreFileLock(dir string, timeout time.Duration) (*tokenStoreFileLock, error) {
	return acquireNamedTokenStoreFileLock(context.Background(), dir, tokenStoreLockFilename, timeout)
}

func acquireNamedTokenStoreFileLock(ctx context.Context, dir, filename string, timeout time.Duration) (*tokenStoreFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := filepath.Join(dir, filename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open MCP OAuth token-store lock: %w", err)
	}
	closeWith := func(lockErr error) (*tokenStoreFileLock, error) {
		return nil, errors.Join(lockErr, file.Close())
	}
	if err := verifyTokenStoreLockFile(path, file); err != nil {
		return closeWith(err)
	}
	if err := secureTokenStoreFile(path); err != nil {
		return closeWith(fmt.Errorf("set MCP OAuth token-store lock permissions: %w", err))
	}

	deadline := time.Now().Add(timeout)
	for {
		locked, err := tryLockTokenStoreFile(file)
		if err != nil {
			return closeWith(fmt.Errorf("acquire MCP OAuth token-store lock: %w", err))
		}
		if locked {
			return &tokenStoreFileLock{file: file}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return closeWith(fmt.Errorf("%w after %s", errTokenStoreLockTimeout, timeout))
		}
		if remaining > tokenStoreLockRetry {
			remaining = tokenStoreLockRetry
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

func withTokenRefreshLease(ctx context.Context, storePath, serverKey string, fn func() error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(storePath) == "" {
		return errors.New("MCP OAuth credential directory is unavailable")
	}
	processKey := storePath + "\x00" + serverKey
	releaseProcess, err := acquireTokenRefreshProcessLock(ctx, processKey)
	if err != nil {
		return err
	}
	defer releaseProcess()

	dir := filepath.Dir(storePath)
	if err := ensurePrivateTokenStoreDir(dir); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(serverKey))
	filename := ".mcp-oauth-refresh-" + hex.EncodeToString(digest[:16]) + ".lock"
	lock, err := acquireNamedTokenStoreFileLock(ctx, dir, filename, tokenRefreshLockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release MCP OAuth refresh lease: %w", releaseErr))
		}
	}()
	if err := ensurePrivateTokenStoreDir(dir); err != nil {
		return err
	}
	return fn()
}

func acquireTokenRefreshProcessLock(ctx context.Context, key string) (func(), error) {
	tokenRefreshProcessMu.Lock()
	lock := tokenRefreshProcessLocks[key]
	if lock == nil {
		lock = &tokenRefreshProcessLock{sem: make(chan struct{}, 1)}
		tokenRefreshProcessLocks[key] = lock
	}
	lock.refs++
	tokenRefreshProcessMu.Unlock()

	select {
	case lock.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-lock.sem
				tokenRefreshProcessMu.Lock()
				lock.refs--
				if lock.refs == 0 {
					delete(tokenRefreshProcessLocks, key)
				}
				tokenRefreshProcessMu.Unlock()
			})
		}, nil
	case <-ctx.Done():
		tokenRefreshProcessMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(tokenRefreshProcessLocks, key)
		}
		tokenRefreshProcessMu.Unlock()
		return nil, ctx.Err()
	}
}

func verifyTokenStoreLockFile(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened MCP OAuth token-store lock: %w", err)
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect MCP OAuth token-store lock: %w", err)
	}
	if linked.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlinked MCP OAuth token-store lock")
	}
	if !opened.Mode().IsRegular() || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.New("refusing non-regular or replaced MCP OAuth token-store lock")
	}
	return nil
}

func (lock *tokenStoreFileLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	return errors.Join(unlockTokenStoreFile(lock.file), lock.file.Close())
}
