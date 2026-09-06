// Package sandbox provides the process-local, operating-system sandbox used
// by command runtimes. A Manager owns all mutable policy state and its own
// temporary directory; the package intentionally has no mutable global mode.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

// Mode controls whether commands are sandboxed and whether the sandbox may
// stand in for the application's separate permission prompt.
type Mode string

const (
	ModeOff         Mode = "off"
	ModePermissions Mode = "permissions"
	ModeAutoAllow   Mode = "auto-allow"

	metisCredentialDirectoryName = auth.CredentialDirectoryName
)

// NetworkPolicy controls network access inside an enabled OS sandbox.
type NetworkPolicy string

const (
	NetworkAllow NetworkPolicy = "allow"
	NetworkBlock NetworkPolicy = "block"
)

var (
	ErrInvalidMode          = errors.New("invalid sandbox mode")
	ErrInvalidNetworkPolicy = errors.New("invalid sandbox network policy")
	ErrUnsafeCwd            = errors.New("unsafe sandbox working directory")
	ErrProtectedPathMissing = errors.New("required sandbox protected path is missing")
	ErrUnsupportedPlatform  = errors.New("sandbox is unsupported on this platform")
	ErrDependencyMissing    = errors.New("sandbox dependency is unavailable")
	ErrManagerClosed        = errors.New("sandbox manager is closed")
)

// ParseMode strictly parses a configured sandbox mode. The empty value is the
// backwards-compatible default, off. Aliases and misspellings are errors so a
// bad configuration can never silently disable the requested protection.
func ParseMode(raw string) (Mode, error) {
	switch raw {
	case "", string(ModeOff):
		return ModeOff, nil
	case string(ModePermissions):
		return ModePermissions, nil
	case string(ModeAutoAllow):
		return ModeAutoAllow, nil
	default:
		return "", fmt.Errorf("%w %q (want off, permissions, or auto-allow)", ErrInvalidMode, raw)
	}
}

func parseNetworkPolicy(raw NetworkPolicy) (NetworkPolicy, error) {
	switch raw {
	case "", NetworkAllow:
		return NetworkAllow, nil
	case NetworkBlock:
		return NetworkBlock, nil
	default:
		return "", fmt.Errorf("%w %q (want allow or block)", ErrInvalidNetworkPolicy, raw)
	}
}

// Request contains the policy inputs that can vary for each command.
type Request struct {
	// Cwd is the command's effective working directory. When empty, Cmd.Dir and
	// then the Metis process working directory are used, in that order.
	Cwd string
	// Network defaults to allow. Block asks the OS backend to remove network
	// access rather than relying on proxy environment variables.
	Network NetworkPolicy
	// MinimumMode can raise an otherwise-off command to the permissions
	// sandbox. It never weakens the Manager's effective mode and deliberately
	// does not accept auto-allow, because prompt policy is runtime state rather
	// than a per-process property.
	MinimumMode Mode
}

// Options configures a Manager.
type Options struct {
	// Mode is parsed strictly by ParseMode.
	Mode string
	// Network is the default policy for commands whose Request leaves Network
	// empty. This keeps every execution entry point on the same runtime policy.
	Network NetworkPolicy
	// TempRoot is primarily useful to keep a runtime's private temporary
	// directory on a particular volume. Empty uses os.TempDir().
	TempRoot string
	// MetisHome is the persistent control/credential root. Empty snapshots
	// METIS_HOME, then falls back to ~/.metis. Passing it explicitly is useful
	// for embedders whose config home is not process-global.
	MetisHome string
}

// State is an atomic snapshot of a Manager's mode selection.
type State struct {
	Configured         Mode
	RuntimeOverride    Mode
	HasRuntimeOverride bool
	// CredentialIsolationRequired is a security floor installed by
	// bypassPermissions. It is independent from the user's configured/runtime
	// sandbox selection so leaving bypass restores that selection exactly.
	CredentialIsolationRequired bool
	// FullAccessRequired is the session permission posture equivalent to
	// Codex's danger-full-access sandbox. It disables the process sandbox and
	// ignores per-tool minimum sandbox requests until the permission mode is
	// changed again.
	FullAccessRequired bool
	Effective          Mode
	AutoAllow          bool
}

// Manager owns sandbox state for one runtime. It is safe for concurrent use.
// Call Close after the runtime and all commands it started have stopped.
type Manager struct {
	mu sync.RWMutex
	// leases tracks long-lived subprocesses that may still be using tempDir.
	// Close rejects new leases first, then waits for existing owners before
	// deleting the directory. A plain Wrap remains suitable for short-lived
	// callers whose enclosing runtime already guarantees shutdown ordering.
	leases    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	configured                  Mode
	network                     NetworkPolicy
	runtimeOverride             Mode
	hasRuntimeOverride          bool
	credentialIsolationRequired bool
	fullAccessRequired          bool
	tempDir                     string
	metisHome                   string
	closed                      bool
}

// NewManager creates a per-runtime manager using the default temporary root.
func NewManager(configuredMode string) (*Manager, error) {
	return NewManagerWithOptions(Options{Mode: configuredMode})
}

// NewManagerWithOptions creates a per-runtime manager and its private writable
// temporary directory.
func NewManagerWithOptions(opts Options) (*Manager, error) {
	mode, err := ParseMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	network, err := parseNetworkPolicy(opts.Network)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(opts.TempRoot, "metis-sandbox-")
	if err != nil {
		return nil, fmt.Errorf("sandbox: create private temp directory: %w", err)
	}
	resolvedTemp, err := resolveExistingPath(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox: resolve private temp directory: %w", err)
	}
	metisHome := opts.MetisHome
	if metisHome == "" {
		metisHome = os.Getenv("METIS_HOME")
	}
	if metisHome == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
			metisHome = filepath.Join(home, ".metis")
		}
	}
	if metisHome != "" {
		resolved, resolveErr := auth.ResolveCredentialHome(metisHome)
		if resolveErr != nil {
			_ = os.RemoveAll(tempDir)
			return nil, fmt.Errorf("sandbox: resolve METIS_HOME credential boundary: %w", resolveErr)
		}
		metisHome = resolved
	}
	return &Manager{configured: mode, network: network, tempDir: resolvedTemp, metisHome: metisHome}, nil
}

// Close removes the manager's private temporary directory. It is idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		tempDir := m.tempDir
		m.mu.Unlock()

		// No Acquire can increment the WaitGroup after closed is published:
		// Acquire performs its closed check and Add while holding m.mu. This
		// makes Wait safe even when Close races with process construction.
		m.leases.Wait()

		m.mu.Lock()
		m.tempDir = ""
		m.mu.Unlock()
		if tempDir != "" {
			if err := os.RemoveAll(tempDir); err != nil {
				m.closeErr = fmt.Errorf("sandbox: remove private temp directory: %w", err)
			}
		}
	})
	return m.closeErr
}

// TempDir returns the private directory writable by sandboxed commands. It is
// empty after Close returns. Active Acquire leases keep it available while a
// concurrent Close is draining subprocesses. Runtimes may set TMPDIR to this
// value before Wrap.
func (m *Manager) TempDir() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tempDir
}

// SetConfiguredMode updates the config-backed mode after strict validation.
func (m *Manager) SetConfiguredMode(raw string) error {
	mode, err := ParseMode(raw)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	m.configured = mode
	return nil
}

// SetRuntimeMode installs a session-only override. Use ClearRuntimeMode to
// return to the configured mode. An empty value explicitly overrides to off.
func (m *Manager) SetRuntimeMode(raw string) error {
	mode, err := ParseMode(raw)
	if err != nil {
		return err
	}
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	m.runtimeOverride = mode
	m.hasRuntimeOverride = true
	return nil
}

// SetRuntimeOverride is an explicit-name alias for SetRuntimeMode.
func (m *Manager) SetRuntimeOverride(raw string) error { return m.SetRuntimeMode(raw) }

// ClearRuntimeMode removes the session override.
func (m *Manager) ClearRuntimeMode() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.runtimeOverride = ""
	m.hasRuntimeOverride = false
}

// ClearRuntimeOverride is an explicit-name alias for ClearRuntimeMode.
func (m *Manager) ClearRuntimeOverride() { m.ClearRuntimeMode() }

// RequireCredentialIsolation installs or removes the filesystem-isolation
// floor used by bypassPermissions. Enabling the floor is fail-closed: the
// permission posture must not change to unattended on a platform where Metis
// cannot enforce its credential boundary. Removing it restores the user's
// configured/runtime sandbox selection without clobbering either value.
func (m *Manager) RequireCredentialIsolation(required bool) error {
	if m == nil {
		return ErrManagerClosed
	}
	if required {
		if err := m.PreflightCredentialIsolation(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	m.credentialIsolationRequired = required
	if required {
		m.fullAccessRequired = false
	}
	return nil
}

// RequireFullAccess installs or removes the process-local no-sandbox posture.
// Enabling it also removes the incompatible credential-isolation floor.
func (m *Manager) RequireFullAccess(required bool) error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	m.fullAccessRequired = required
	if required {
		m.credentialIsolationRequired = false
	}
	return nil
}

// SetPermissionPosture atomically coordinates the two permission-owned
// sandbox overrides. They are mutually exclusive: bypassPermissions needs a
// credential boundary, while fullAccess explicitly disables all sandboxing.
func (m *Manager) SetPermissionPosture(credentialIsolationRequired, fullAccessRequired bool) error {
	if m == nil {
		return ErrManagerClosed
	}
	if credentialIsolationRequired && fullAccessRequired {
		return errors.New("sandbox: credential isolation and full access are mutually exclusive")
	}
	if credentialIsolationRequired {
		if err := m.PreflightCredentialIsolation(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	m.credentialIsolationRequired = credentialIsolationRequired
	m.fullAccessRequired = fullAccessRequired
	return nil
}

// PreflightCredentialIsolation verifies that the current runtime can enforce
// the filesystem boundary required by bypassPermissions without mutating
// manager state. Session switching uses this before it closes the source
// session, so a destination that cannot be restored safely never becomes the
// active session and is never reported as a successful resume.
func (m *Manager) PreflightCredentialIsolation() error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return ErrManagerClosed
	}
	diagnostic := Doctor()
	if !diagnostic.Available {
		if diagnostic.Err != nil {
			return fmt.Errorf("sandbox: bypassPermissions credential isolation is unavailable: %w", diagnostic.Err)
		}
		return fmt.Errorf("sandbox: bypassPermissions credential isolation is unavailable on %s", diagnostic.Platform)
	}
	return nil
}

// PreflightFullAccess verifies that the manager can accept a no-sandbox
// posture without changing it. Unlike credential isolation there is no
// platform backend requirement; the only failure is a closed runtime. Keeping
// this check separate lets the permission gate become fullAccess before the
// sandbox is relaxed, so concurrent calls can only observe the safer
// intermediate state.
func (m *Manager) PreflightFullAccess() error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ErrManagerClosed
	}
	return nil
}

// RuntimeMode reports the current session override and whether one is active.
func (m *Manager) RuntimeMode() (Mode, bool) {
	state := m.State()
	return state.RuntimeOverride, state.HasRuntimeOverride
}

// EffectiveMode reports the mode currently applied to new commands.
func (m *Manager) EffectiveMode() Mode { return m.State().Effective }

// AutoAllow reports whether the effective mode permits the runtime to bypass
// its separate confirmation UI. It says nothing about other permission modes
// such as plan-only operation, which remain the caller's responsibility.
func (m *Manager) AutoAllow() bool { return m.State().AutoAllow }

// NetworkPolicy reports the runtime's default network policy for commands
// that do not request an explicit override.
func (m *Manager) NetworkPolicy() NetworkPolicy {
	if m == nil {
		return NetworkAllow
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.fullAccessRequired {
		return NetworkAllow
	}
	if m.network == "" {
		return NetworkAllow
	}
	return m.network
}

// State returns a consistent snapshot of all mode fields.
func (m *Manager) State() State {
	if m == nil {
		return State{Configured: ModeOff, Effective: ModeOff}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	effective := m.effectiveModeLocked()
	return State{
		Configured:                  m.configured,
		RuntimeOverride:             m.runtimeOverride,
		HasRuntimeOverride:          m.hasRuntimeOverride,
		CredentialIsolationRequired: m.credentialIsolationRequired,
		FullAccessRequired:          m.fullAccessRequired,
		Effective:                   effective,
		AutoAllow:                   m.fullAccessRequired || effective == ModeAutoAllow,
	}
}

// effectiveModeLocked returns the current policy while m.mu is held for read
// or write. Credential isolation is a floor, not a replacement for an
// explicitly stronger auto-allow sandbox selection.
func (m *Manager) effectiveModeLocked() Mode {
	if m.fullAccessRequired {
		return ModeOff
	}
	effective := m.configured
	if m.hasRuntimeOverride {
		effective = m.runtimeOverride
	}
	if m.credentialIsolationRequired && effective == ModeOff {
		return ModePermissions
	}
	return effective
}

// Wrap changes only the executable path and argv needed to enter the platform
// sandbox. Cmd.Env, Cmd.Dir, stdin/stdout/stderr, ExtraFiles, SysProcAttr and
// cancellation behavior remain attached to the same *exec.Cmd.
func (m *Manager) Wrap(cmd *exec.Cmd, req Request) (*exec.Cmd, error) {
	if cmd == nil {
		return nil, errors.New("sandbox: nil command")
	}
	if m == nil {
		return nil, ErrManagerClosed
	}

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, ErrManagerClosed
	}
	// Keep Close/override mutation out until the wrapper is fully prepared.
	// Linux may create its manager-private credential-mask directory here;
	// releasing the lock before wrapPlatform would let Close remove tempDir and
	// a concurrent Wrap recreate it after the Manager had become closed.
	defer m.mu.RUnlock()
	mode := m.effectiveModeLocked()
	fullAccess := m.fullAccessRequired
	if !fullAccess {
		switch req.MinimumMode {
		case "", ModeOff:
		case ModePermissions:
			if mode == ModeOff {
				mode = ModePermissions
			}
		default:
			return nil, fmt.Errorf("%w minimum %q (want off or permissions)", ErrInvalidMode, req.MinimumMode)
		}
	}
	tempDir := m.tempDir
	metisHome := m.metisHome
	defaultNetwork := m.network

	if fullAccess || mode == ModeOff {
		return cmd, nil
	}
	requestedNetwork := req.Network
	if requestedNetwork == "" {
		requestedNetwork = defaultNetwork
	}
	network, err := parseNetworkPolicy(requestedNetwork)
	if err != nil {
		return nil, err
	}
	cwd, err := effectiveCwd(cmd, req.Cwd)
	if err != nil {
		return nil, err
	}
	if tempDir == "" {
		return nil, ErrManagerClosed
	}
	home := ""
	if rawHome, homeErr := os.UserHomeDir(); homeErr == nil && rawHome != "" {
		if resolvedHome, resolveErr := resolveExistingPath(rawHome); resolveErr == nil {
			home = resolvedHome
		}
	}
	if err := validateCredentialTopology(home, metisHome); err != nil {
		return nil, err
	}

	if err := wrapPlatform(cmd, platformRequest{
		mode:                        mode,
		cwd:                         cwd,
		tempDir:                     tempDir,
		managerOwnedCwd:             pathWithinRoot(tempDir, cwd),
		credentialIsolationRequired: m.credentialIsolationRequired,
		network:                     network,
		home:                        home,
		metisHome:                   metisHome,
	}); err != nil {
		return nil, err
	}
	return cmd, nil
}

// Acquire wraps cmd and retains the Manager's private filesystem resources
// until the returned release function is called. Long-lived subprocess owners
// must use Acquire rather than Wrap so Manager.Close cannot remove tempDir
// while a child is still running. The release function is idempotent.
func (m *Manager) Acquire(cmd *exec.Cmd, req Request) (*exec.Cmd, func(), error) {
	if m == nil {
		return nil, nil, ErrManagerClosed
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, ErrManagerClosed
	}
	m.leases.Add(1)
	m.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(m.leases.Done)
	}
	wrapped, err := m.Wrap(cmd, req)
	if err != nil {
		release()
		return nil, nil, err
	}
	return wrapped, release, nil
}

// pathWithinRoot reports whether path is root itself or one of its
// descendants. Wrap resolves both the Manager temp root and command cwd before
// calling this helper, so lexical traversal and symlink aliases cannot make a
// persistent directory look manager-owned.
func pathWithinRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func effectiveCwd(cmd *exec.Cmd, requested string) (string, error) {
	cwd := requested
	if cwd == "" {
		cwd = cmd.Dir
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("sandbox: get working directory: %w", err)
		}
	}
	resolved, err := resolveExistingPath(cwd)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("sandbox: stat cwd %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sandbox: cwd %q is not a directory", resolved)
	}
	if filepath.Clean(resolved) == string(filepath.Separator) {
		return "", fmt.Errorf("%w: refusing to make filesystem root writable", ErrUnsafeCwd)
	}
	return resolved, nil
}

func resolveExistingPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// sensitiveHomeDirectories and sensitiveHomeFiles enumerate credential stores
// that a model-authored command must not be able to inspect or modify merely
// because the sandbox otherwise permits host-wide reads. Keep this list
// intentionally narrow: denying all of ~/.config would break ordinary compiler
// and package-manager discovery, while these locations commonly contain
// reusable authentication material.
func sensitiveHomeDirectories(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".azure"),
		filepath.Join(home, ".kube"),
		filepath.Join(home, ".config", "gcloud"),
		filepath.Join(home, ".config", "gh"),
	}
}

func sensitiveHomeFiles(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".netrc"),
		filepath.Join(home, ".git-credentials"),
		filepath.Join(home, ".npmrc"),
		filepath.Join(home, ".pypirc"),
		filepath.Join(home, ".docker", "config.json"),
	}
}

// metisControlRoots returns every persistent Metis root that may contain
// credentials or auto-loaded control files. A custom METIS_HOME does not make
// an older/default ~/.metis harmless, so enabled sandboxes protect both.
func metisControlRoots(home, configured string) []string {
	candidates := []string{configured}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".metis"))
	}
	out := make([]string, 0, len(candidates)*2)
	seen := make(map[string]struct{}, len(candidates)*2)
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		for _, path := range policyPathVariants(candidate) {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			out = append(out, path)
		}
	}
	return out
}

// dangerousWriteFiles/directories mirror the persistence surfaces protected
// by Claude Code's sandbox runtime. They remain readable for normal tooling,
// but model-authored commands may not replace shell startup files, editor
// automation, repository submodule configuration, or MCP configuration.
func dangerousWriteFiles(root string) []string {
	if root == "" {
		return nil
	}
	return []string{
		filepath.Join(root, ".gitconfig"),
		filepath.Join(root, ".gitmodules"),
		filepath.Join(root, ".bashrc"),
		filepath.Join(root, ".bash_profile"),
		filepath.Join(root, ".zshrc"),
		filepath.Join(root, ".zprofile"),
		filepath.Join(root, ".profile"),
		filepath.Join(root, ".ripgreprc"),
		filepath.Join(root, ".mcp.json"),
	}
}

func dangerousWriteDirectories(root string) []string {
	if root == "" {
		return nil
	}
	return []string{
		filepath.Join(root, ".vscode"),
		filepath.Join(root, ".idea"),
	}
}

// policyPathVariants includes a resolved symlink target when it exists. This
// prevents a credential directory such as ~/.ssh -> /Volumes/keys/ssh from
// escaping a lexical path rule while retaining the lexical rule for stable
// diagnostics and for paths created after the profile is built.
func policyPathVariants(path string) []string {
	clean := filepath.Clean(path)
	variants := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		resolved = filepath.Clean(resolved)
		if resolved != clean {
			variants = append(variants, resolved)
		}
	}
	return variants
}

type platformRequest struct {
	mode            Mode
	cwd             string
	tempDir         string
	managerOwnedCwd bool
	cwdReadOnly     bool // Linux fail-closed fallback; ignored elsewhere.
	// credentialIsolationRequired distinguishes unattended bypass from an
	// ordinary permissions sandbox. Linux may need a stricter cwd fallback
	// when bubblewrap cannot mask a protected path that does not yet exist.
	credentialIsolationRequired bool
	network                     NetworkPolicy
	home                        string
	metisHome                   string

	blockedUnixSockets []string // Linux network=block hardening; ignored elsewhere
}

// Diagnostic describes whether this build can enforce a sandbox right now.
type Diagnostic struct {
	Platform   string
	Backend    string
	Supported  bool
	Available  bool
	Executable string
	Err        error
}

// Available reports whether the current platform backend and its executable
// dependency are available.
func Available() bool { return Doctor().Available }

// Available reports package availability through an injected Manager.
func (m *Manager) Available() bool { return Available() }

// Doctor returns a dependency-level diagnosis suitable for /sandbox status.
func Doctor() Diagnostic { return doctorPlatform() }

// Doctor reports package diagnostics through an injected Manager.
func (m *Manager) Doctor() Diagnostic { return Doctor() }
