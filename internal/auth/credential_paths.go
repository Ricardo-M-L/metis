package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// CredentialDirectoryName is the fixed private directory that contains all
	// provider secrets and their coordination files. Keeping the whole
	// namespace behind one directory lets the process sandbox hide credentials
	// created after a long-lived command has already started.
	CredentialDirectoryName = ".credentials"
	authFileName            = "auth.json"
	oauthFileName           = "llm-oauth.json"
)

var userHomeDir = os.UserHomeDir

var credentialHomeCache sync.Map
var credentialDirectoryIdentities sync.Map

func resolvedMetisHome() (string, error) {
	return ResolveCredentialHome("")
}

// ResolveCredentialHome resolves the persistent METIS root once per lexical
// path. Configuration readers/writers deliberately share this resolver with
// credential storage: freezing an existing symlink target prevents a later
// METIS_HOME retarget from pairing credentials from one root with provider
// endpoints from another. It also keeps both stores aligned with a sandbox
// created earlier in the same process.
func ResolveCredentialHome(configured string) (string, error) {
	candidate := configured
	if candidate == "" {
		candidate = os.Getenv("METIS_HOME")
	}
	if candidate == "" {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home for credentials: %w", err)
		}
		if home == "" {
			return "", errors.New("resolve user home for credentials: empty home directory")
		}
		candidate = filepath.Join(home, ".metis")
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve METIS_HOME: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if cached, ok := credentialHomeCache.Load(absolute); ok {
		return cached.(string), nil
	}
	resolved, err := resolvePathWithMissingSuffix(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve METIS_HOME: %w", err)
	}
	actual, _ := credentialHomeCache.LoadOrStore(absolute, resolved)
	return actual.(string), nil
}

// EnsureCredentialHome resolves, creates, and pins the persistent METIS root
// to the directory identity first observed by this process. Configuration and
// credential callers must share this guard so a long-lived CLI/Desktop process
// cannot be redirected to a replacement directory between requests.
func EnsureCredentialHome(configured string) (string, error) {
	home, err := ResolveCredentialHome(configured)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("create METIS credential root: %w", err)
	}
	if err := verifyAndPinCredentialDirectory(home, "METIS credential root"); err != nil {
		return "", err
	}
	return home, nil
}

func resolvePathWithMissingSuffix(absolute string) (string, error) {
	current := absolute
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing ancestor for credential home")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// verifyAndPinCredentialDirectory binds a canonical directory pathname to the
// directory identity first observed by this process. This prevents a writable
// parent from being renamed/replaced between long-lived CLI/Desktop requests
// and redirecting later credential writes to a different inode.
func verifyAndPinCredentialDirectory(path, label string) error {
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("inspect %s %q: %w", label, clean, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s %q is not a real directory", label, clean)
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("resolve %s %q: %w", label, clean, err)
	}
	if filepath.Clean(resolved) != clean {
		return fmt.Errorf("%s %q changed through a symlink", label, clean)
	}
	actual, loaded := credentialDirectoryIdentities.LoadOrStore(clean, info)
	if loaded && !os.SameFile(actual.(os.FileInfo), info) {
		return fmt.Errorf("%s %q was replaced while METIS was running", label, clean)
	}
	after, err := os.Lstat(clean)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(info, after) {
		return fmt.Errorf("%s %q changed while it was being verified", label, clean)
	}
	return nil
}

func verifyAndPinCredentialLayout(dir string) error {
	if err := verifyAndPinCredentialDirectory(filepath.Dir(dir), "METIS credential root"); err != nil {
		return err
	}
	return verifyAndPinCredentialDirectory(dir, "METIS private credential directory")
}

// CredentialDirectory returns the canonical private credential directory.
// It returns an empty string only when the operating system cannot resolve a
// user home and METIS_HOME was not supplied. Credential operations surface
// that condition as an error instead of falling back to a relative .metis
// directory in the current workspace.
func CredentialDirectory() string {
	home, err := resolvedMetisHome()
	if err != nil {
		return ""
	}
	return filepath.Join(home, CredentialDirectoryName)
}

type credentialLayout struct {
	dir         string
	auth        string
	oauth       string
	legacyAuth  string
	legacyOAuth string
}

func currentCredentialLayout() (credentialLayout, error) {
	home, err := resolvedMetisHome()
	if err != nil {
		return credentialLayout{}, err
	}
	dir := filepath.Join(home, CredentialDirectoryName)
	return credentialLayout{
		dir:         dir,
		auth:        filepath.Join(dir, authFileName),
		oauth:       filepath.Join(dir, oauthFileName),
		legacyAuth:  filepath.Join(home, authFileName),
		legacyOAuth: filepath.Join(home, oauthFileName),
	}, nil
}

// Path returns the canonical API-key store path.
func Path() string {
	layout, err := currentCredentialLayout()
	if err != nil {
		return ""
	}
	return layout.auth
}

// OAuthPath returns the canonical refreshable OAuth store path.
func OAuthPath() string {
	layout, err := currentCredentialLayout()
	if err != nil {
		return ""
	}
	return layout.oauth
}
