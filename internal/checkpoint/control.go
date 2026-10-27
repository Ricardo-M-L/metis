package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrDisabledByUser = errors.New("checkpoint: 已手动关闭，使用 /checkpoint on 开启")

type ControlStatus struct {
	SessionID string   `json:"sessionId"`
	Workspace string   `json:"workspace"`
	Enabled   bool     `json:"enabled"`
	Mode      string   `json:"mode"`
	Roots     []string `json:"roots"`
	Reason    string   `json:"reason,omitempty"`
	Revision  string   `json:"revision"`
}
type controlSettings struct {
	Version   int      `json:"version"`
	SessionID string   `json:"sessionId"`
	Workspace string   `json:"workspace"`
	Enabled   bool     `json:"enabled"`
	Roots     []string `json:"roots"`
	Revision  string   `json:"revision"`
}

// Control arguments are paths, not a shell program. Quotes/backslashes group
// spaces; no variables, substitutions, globbing or command execution occur.
func ParseControlArgs(input string) (string, []string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range input {
		if escaped {
			word.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return "", nil, errors.New("checkpoint: 路径引号或转义未闭合")
	}
	if started {
		words = append(words, word.String())
	}
	if len(words) == 0 {
		return "status", nil, nil
	}
	action := strings.ToLower(words[0])
	paths := words[1:]
	if action != "on" && action != "off" && action != "status" {
		return "", nil, errors.New("用法：/checkpoint [status|on|off]")
	}
	if action != "on" && len(paths) > 0 {
		return "", nil, errors.New("只有 /checkpoint on 可以指定项目目录")
	}
	return action, paths, nil
}

func controlPaths(sessionID, workspace string) (string, string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", "", errors.New("checkpoint: 当前会话未绑定工作区")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	if sessionID == "" {
		// A new-chat preview has no durable owner yet. It may display defaults,
		// but must never inherit or create a workspace-wide preference.
		return canonical, "", nil
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + canonical))
	metisHome := os.Getenv("METIS_HOME")
	if metisHome == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", homeErr
		}
		metisHome = filepath.Join(home, ".metis")
	}
	base := filepath.Join(metisHome, "checkpoint-controls", "sessions")
	return canonical, filepath.Join(base, hex.EncodeToString(sum[:])+".json"), nil
}
func readControl(sessionID, workspace string) (controlSettings, string, error) {
	cwd, path, err := controlPaths(sessionID, workspace)
	if err != nil {
		return controlSettings{}, "", err
	}
	state := controlSettings{Version: 2, SessionID: sessionID, Workspace: cwd, Enabled: !isUnsafeCheckpointRoot(cwd), Revision: "auto"}
	if state.Enabled {
		state.Roots = []string{"."}
	}
	if sessionID == "" {
		return state, "", nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return state, path, nil
	}
	if err != nil {
		return state, path, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 16<<10 {
		return state, path, errors.New("checkpoint: invalid settings file")
	}
	file, err := os.Open(path)
	if err != nil {
		return state, path, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return state, path, errors.New("checkpoint: settings file changed")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil {
		return state, path, fmt.Errorf("checkpoint settings: %w", err)
	}
	if state.Version != 2 || state.SessionID != sessionID || state.Workspace != cwd || state.Revision == "" {
		return state, path, errors.New("checkpoint: settings identity mismatch")
	}
	if state.Roots, err = normalizeRoots(cwd, state.Roots, false); err != nil {
		return state, path, err
	}
	if state.Enabled && len(state.Roots) == 0 {
		return state, path, errors.New("checkpoint: enabled settings require project scope")
	}
	return state, path, nil
}

func canonicalControlCandidate(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = canonicalControlCandidate(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
func normalizeRoots(cwd string, paths []string, requireExisting bool) ([]string, error) {
	if len(paths) > 64 {
		return nil, errors.New("checkpoint: at most 64 project directories")
	}
	var result []string
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("checkpoint: 项目目录不能为空")
		}
		if path == "~" || strings.HasPrefix(path, "~/") {
			home, e := os.UserHomeDir()
			if e != nil {
				return nil, e
			}
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		rel, err := filepath.Rel(cwd, path)
		// macOS /var and /private/var aliases identify the same workspace.
		// Store the canonical selected target, then validate its descendants.
		if err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
				path = resolved
				rel, err = filepath.Rel(cwd, path)
			}
		}
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, errors.New("checkpoint: 项目目录必须位于当前工作区内")
		}
		if rel == "." && isUnsafeCheckpointRoot(cwd) {
			return nil, errors.New("checkpoint: 主目录或根目录须明确指定项目范围，例如 /checkpoint on Documents/project")
		}
		current := cwd
		if rel != "." {
			for _, part := range strings.Split(rel, string(os.PathSeparator)) {
				if skipDirs[part] {
					return nil, fmt.Errorf("checkpoint: excluded directory %q", part)
				}
				current = filepath.Join(current, part)
				info, e := os.Lstat(current)
				if os.IsNotExist(e) && !requireExisting {
					break
				}
				if e != nil {
					return nil, e
				}
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("checkpoint: project scope must be a directory without symlinks: %s", current)
				}
			}
		}
		result = append(result, filepath.ToSlash(rel))
	}
	sort.Strings(result)
	out := result[:0]
	for _, r := range result {
		covered := false
		for _, parent := range out {
			if parent == "." || r == parent || strings.HasPrefix(r, parent+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, r)
		}
	}
	return out, nil
}
func writeControl(path string, state controlSettings) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(body, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func controlLease(sessionID, workspace string, exclusive bool) (func(), error) {
	if sessionID == "" {
		return nil, errors.New("checkpoint: 请先创建会话")
	}
	_, path, err := controlPaths(sessionID, workspace)
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(path)
	if err = os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("checkpoint: invalid settings directory")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path) + ".lock"
	if old, e := root.Lstat(name); e == nil && (!old.Mode().IsRegular() || old.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("checkpoint: invalid settings lock")
	}
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	actual, e := file.Stat()
	current, e2 := root.Lstat(name)
	if e != nil || e2 != nil || !current.Mode().IsRegular() || !os.SameFile(actual, current) {
		file.Close()
		return nil, errors.New("checkpoint: settings lock changed")
	}
	ok, err := tryControlLock(file, exclusive)
	if err != nil || !ok {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("checkpoint: session busy，任务结束后再更改检查点设置")
	}
	return sync.OnceFunc(func() { _ = unlockControl(file); _ = file.Close() }), nil
}
func (m *Manager) applyControlLocked(state controlSettings) bool {
	changed := m.controlRevision != state.Revision
	m.controlRevision = state.Revision
	m.scopeRoots = append([]string(nil), state.Roots...)
	key := strings.Join(state.Roots, "\x00")
	if len(state.Roots) == 1 && state.Roots[0] == "." {
		key = ""
	}
	m.scopeKey.Store(key)
	m.automaticOff.Store(!state.Enabled)
	m.disabledByPolicy.Store(isUnsafeCheckpointRoot(m.cwd) && len(state.Roots) == 0)
	m.disabled = m.initErr != nil || m.disabledByPolicy.Load()
	m.controlErr = nil
	if changed {
		m.hasBaseline.Store(false)
	}
	return changed
}
func (m *Manager) refreshControlLocked() (bool, error) {
	state, _, err := readControl(m.sessionID, m.cwd)
	if err != nil {
		m.controlErr = err
		m.automaticOff.Store(true)
		return false, err
	}
	return m.applyControlLocked(state), nil
}

// A shared session lease keeps settings stable through the whole model turn,
// including every before/after-tool snapshot. Other CLI/Desktop processes must
// wait for idle rather than changing attribution halfway through a mutation.
func (m *Manager) BeginRun() (func(), bool, error) {
	release, err := controlLease(m.sessionID, m.cwd, false)
	if err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	changed, err := m.refreshControlLocked()
	m.mu.Unlock()
	if err != nil {
		release()
		return nil, false, err
	}
	return release, changed, nil
}

func (m *Manager) RefreshControl() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.refreshControlLocked()
	return err
}
func (m *Manager) AutomaticDisabled() bool { return m.automaticOff.Load() || m.DisabledByPolicy() }
func (m *Manager) ScopeKey() string {
	if value := m.scopeKey.Load(); value != nil {
		return value.(string)
	}
	return ""
}
func (m *Manager) scopeContains(rel string) bool {
	for _, root := range m.scopeRoots {
		if root == "." || rel == root || strings.HasPrefix(rel, root+"/") {
			return true
		}
	}
	return false
}
func (m *Manager) scopeTraverses(rel string) bool {
	if m.scopeContains(rel) {
		return true
	}
	for _, root := range m.scopeRoots {
		if strings.HasPrefix(root, rel+"/") {
			return true
		}
	}
	return false
}
func (m *Manager) Protects(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.cwd, path)
	}
	cwd, err := filepath.Abs(m.cwd)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		cwd, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			return false
		}
		path, err = canonicalControlCandidate(path)
		if err != nil {
			return false
		}
		rel, err = filepath.Rel(cwd, path)
		if err != nil {
			return false
		}
	}
	// Only exclude paths outside the selected projects here. Unsupported or
	// symlink paths inside that scope must still reach CapturePathStates so
	// a protection gap is reported rather than silently advertised as covered.
	return rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && m.scopeContains(filepath.ToSlash(rel))
}
func (m *Manager) ControlStatus() ControlStatus {
	state, _, err := readControl(m.sessionID, m.cwd)
	status := ControlStatus{SessionID: m.sessionID, Workspace: state.Workspace, Enabled: state.Enabled, Mode: "auto", Roots: []string{}, Revision: state.Revision}
	if state.Revision != "auto" {
		if state.Enabled {
			status.Mode = "on"
		} else {
			status.Mode = "off"
		}
	}
	for _, root := range state.Roots {
		status.Roots = append(status.Roots, filepath.Join(state.Workspace, filepath.FromSlash(root)))
	}
	if err != nil {
		status.Enabled = false
		status.Reason = err.Error()
	} else if !state.Enabled {
		if status.Mode == "off" {
			status.Reason = "已手动关闭；已有快照保留"
		} else {
			status.Reason = "请在项目工作区中开启"
		}
	}
	return status
}
func (s ControlStatus) String() string {
	state := "关闭"
	if s.Enabled {
		state = "开启"
	}
	result := "代码检查点：" + state
	if s.Reason != "" {
		result += "\n" + s.Reason
	}
	return result + "\n/checkpoint on · /checkpoint off · /checkpoint status"
}
func (m *Manager) Command(ctx context.Context, args string) (ControlStatus, error) {
	action, paths, err := ParseControlArgs(args)
	if err != nil {
		return ControlStatus{}, err
	}
	if action == "status" {
		return m.ControlStatus(), nil
	}
	release, err := controlLease(m.sessionID, m.cwd, true)
	if err != nil {
		return ControlStatus{}, err
	}
	defer release()
	old, path, err := readControl(m.sessionID, m.cwd)
	if err != nil {
		return ControlStatus{}, err
	}
	if action == "on" && len(paths) == 0 && isUnsafeCheckpointRoot(old.Workspace) {
		return ControlStatus{}, errors.New("请在项目工作区中开启")
	}
	next := old
	next.Enabled = action == "on"
	next.Revision = fmt.Sprintf("%d", time.Now().UnixNano())
	if action == "on" {
		if len(paths) == 0 {
			paths = old.Roots
			if len(paths) == 0 && !isUnsafeCheckpointRoot(old.Workspace) {
				paths = []string{"."}
			}
		}
		if len(paths) == 0 {
			return ControlStatus{}, errors.New("checkpoint: 请指定项目目录，例如 /checkpoint on Documents/project")
		}
		next.Roots, err = normalizeRoots(old.Workspace, paths, true)
		if err != nil {
			return ControlStatus{}, err
		}
	}
	m.mu.Lock()
	if action == "on" && m.initErr != nil {
		m.initErr = nil
		m.initialized = false
	}
	m.applyControlLocked(next)
	m.mu.Unlock()
	if action == "on" {
		baselineCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, err = m.BaselineContext(baselineCtx, "checkpoint", "", "manual enable baseline")
		cancel()
	}
	if err == nil {
		err = writeControl(path, next)
	}
	if err != nil {
		m.mu.Lock()
		m.applyControlLocked(old)
		m.mu.Unlock()
		return ControlStatus{}, err
	}
	return m.ControlStatus(), nil
}
