package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type App struct {
	ctx      context.Context
	workDir  string
	metisBin string
	sendMu   sync.Mutex

	findMetis func() (string, error)
	runMetis  func(ctx context.Context, binary string, args []string, dir string) (stdout, stderr string, err error)
}

func NewApp() *App {
	app := &App{
		workDir:  parseWorkspaceArg(os.Args),
		metisBin: parseMetisBinArg(os.Args),
		runMetis: runMetisCommand,
	}
	app.findMetis = func() (string, error) { return findMetisBinary(app.metisBin) }
	return app
}

func parseMetisBinArg(args []string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == "--metis-bin" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--metis-bin=") {
			return strings.TrimPrefix(args[i], "--metis-bin=")
		}
	}
	return ""
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func parseWorkspaceArg(args []string) string {
	for i := 1; i < len(args); i++ {
		if args[i] == "--workspace" && i+1 < len(args) {
			if abs, err := filepath.Abs(args[i+1]); err == nil {
				return abs
			}
		}
		if strings.HasPrefix(args[i], "--workspace=") {
			if abs, err := filepath.Abs(strings.TrimPrefix(args[i], "--workspace=")); err == nil {
				return abs
			}
		}
	}
	dir, _ := os.Getwd()
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func (a *App) GetVersion() string {
	return "0.2.8"
}

func (a *App) GetProjectDir() string {
	return a.workDir
}

type SessionInfo struct {
	ID        string `json:"ID"`
	Title     string `json:"Title"`
	Model     string `json:"Model"`
	CreatedAt string `json:"CreatedAt"`
}

// ScheduledTaskInfo is the native client's compact view of a durable Metis
// cron job. The CLI remains the source of truth; the desktop process never
// reads or mutates cron files directly.
type ScheduledTaskInfo struct {
	ID          string `json:"ID"`
	Name        string `json:"Name"`
	Prompt      string `json:"Prompt"`
	Schedule    string `json:"Schedule"`
	Enabled     bool   `json:"Enabled"`
	Paused      bool   `json:"Paused"`
	NextRun     string `json:"NextRun"`
	LastRun     string `json:"LastRun"`
	RunCount    int    `json:"RunCount"`
	Repeat      int    `json:"Repeat"`
	Silent      bool   `json:"Silent"`
	SessionMode string `json:"SessionMode"`
}

type cronListWireRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Schedule struct {
		Kind     string `json:"kind"`
		EveryMs  int64  `json:"every_ms"`
		CronExpr string `json:"cron_expr"`
		At       string `json:"at"`
		JitterMs int64  `json:"jitter_ms"`
		TZ       string `json:"tz"`
	} `json:"schedule"`
	Enabled     bool   `json:"enabled"`
	Paused      bool   `json:"paused"`
	NextRun     string `json:"nextRun"`
	LastRun     string `json:"lastRun"`
	RunCount    int    `json:"runCount"`
	Repeat      int    `json:"repeat"`
	Silent      bool   `json:"silent"`
	SessionMode string `json:"sessionMode"`
}

// GetScheduledTasks returns durable jobs from `metis cron list --json`.
// Keeping this behind the CLI ensures custom METIS_HOME/config resolution and
// any scheduler migrations are shared with terminal users.
func (a *App) GetScheduledTasks() ([]ScheduledTaskInfo, error) {
	stdout, stderr, err := a.runCLI(20*time.Second, "cron", "list", "--json")
	if err != nil {
		return nil, cronCLIError("list scheduled tasks", stderr, err)
	}
	var records []cronListWireRecord
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		return nil, fmt.Errorf("list scheduled tasks: decode CLI response: %w", err)
	}
	out := make([]ScheduledTaskInfo, 0, len(records))
	for _, record := range records {
		if !validCronJobID(record.ID) {
			continue
		}
		name := strings.TrimSpace(record.Name)
		if name == "" {
			name = "Untitled task"
		}
		out = append(out, ScheduledTaskInfo{
			ID:          record.ID,
			Name:        name,
			Prompt:      record.Prompt,
			Schedule:    formatCronSchedule(record),
			Enabled:     record.Enabled,
			Paused:      record.Paused,
			NextRun:     record.NextRun,
			LastRun:     record.LastRun,
			RunCount:    record.RunCount,
			Repeat:      record.Repeat,
			Silent:      record.Silent,
			SessionMode: record.SessionMode,
		})
	}
	return out, nil
}

func formatCronSchedule(record cronListWireRecord) string {
	var schedule string
	switch record.Schedule.Kind {
	case "every":
		schedule = "Every " + (time.Duration(record.Schedule.EveryMs) * time.Millisecond).String()
	case "cron":
		schedule = record.Schedule.CronExpr
	case "at":
		schedule = "At " + record.Schedule.At
	default:
		schedule = record.Schedule.Kind
	}
	if schedule == "" {
		schedule = "Unknown schedule"
	}
	if record.Schedule.TZ != "" {
		schedule += " · " + record.Schedule.TZ
	}
	if record.Schedule.JitterMs > 0 {
		schedule += " · jitter " + (time.Duration(record.Schedule.JitterMs) * time.Millisecond).String()
	}
	return schedule
}

func (a *App) PauseScheduledTask(id string) error {
	_, err := a.runCronCommand(20*time.Second, "pause", id)
	return err
}

func (a *App) ResumeScheduledTask(id string) error {
	_, err := a.runCronCommand(20*time.Second, "resume", id)
	return err
}

func (a *App) DeleteScheduledTask(id string) error {
	_, err := a.runCronCommand(20*time.Second, "rm", id)
	return err
}

func (a *App) RunScheduledTask(id string) (string, error) {
	return a.runCronCommand(10*time.Minute, "run", id)
}

func (a *App) runCronCommand(timeout time.Duration, action, id string) (string, error) {
	if !validCronJobID(id) {
		return "", fmt.Errorf("%s scheduled task: invalid job id", action)
	}
	stdout, stderr, err := a.runCLI(timeout, "cron", action, id)
	if err != nil {
		return strings.TrimSpace(stdout), cronCLIError(action+" scheduled task", stderr, err)
	}
	return strings.TrimSpace(stdout), nil
}

func cronCLIError(action, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "unknown CLI error"
	}
	return fmt.Errorf("%s: %s", action, detail)
}

func validCronJobID(id string) bool {
	return validSessionID(id)
}

func (a *App) GetSessions() []SessionInfo {
	// Ask the CLI to apply the workspace predicate before the limit. Fetching
	// the global top 30 and filtering here makes an older conversation vanish
	// as soon as other workspaces have enough newer sessions.
	stdout, _, err := a.runCLI(20*time.Second, "sessions", "list", "--json", "--work-dir", a.workDir, "--limit", "30")
	if err != nil {
		return nil
	}
	var records []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Model     string `json:"model"`
		WorkDir   string `json:"workDir"`
		CreatedAt string `json:"createdAt"`
	}
	if json.Unmarshal([]byte(stdout), &records) != nil {
		return nil
	}
	out := make([]SessionInfo, 0, len(records))
	for _, record := range records {
		if !validSessionID(record.ID) {
			continue
		}
		if record.WorkDir != "" && !samePath(record.WorkDir, a.workDir) {
			continue
		}
		title := record.Title
		if title == "" {
			title = "Untitled"
		}
		out = append(out, SessionInfo{
			ID:        record.ID,
			Title:     title,
			Model:     record.Model,
			CreatedAt: record.CreatedAt,
		})
		if len(out) == 30 {
			break
		}
	}
	return out
}

func samePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil {
		left = leftAbs
	}
	if rightErr == nil {
		right = rightAbs
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

type ChatMessage struct {
	Role    string `json:"Role"`
	Content string `json:"Content"`
}

// GetSessionMessages returns the persisted text transcript for a sidebar
// selection. Tool-only blocks are intentionally omitted from this compact
// chat view; their surrounding assistant text remains visible.
func (a *App) GetSessionMessages(id string) []ChatMessage {
	if !validSessionID(id) {
		return nil
	}
	stdout, _, err := a.runCLI(20*time.Second, "sessions", "export", id)
	if err != nil {
		return nil
	}
	var out []ChatMessage
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message *struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != "message" || entry.Message == nil {
			continue
		}
		if entry.Message.Role != "user" && entry.Message.Role != "assistant" {
			continue
		}
		var text []string
		for _, block := range entry.Message.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		}
		if len(text) > 0 {
			out = append(out, ChatMessage{Role: entry.Message.Role, Content: strings.Join(text, "\n")})
		}
	}
	return out
}

func (a *App) GetCurrentModel() string {
	stdout, _, err := a.runCLI(20*time.Second, "config", "show", "--json")
	if err != nil {
		return "unknown"
	}
	var summary struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(stdout), &summary) != nil || strings.TrimSpace(summary.Model) == "" {
		return "unknown"
	}
	return summary.Model
}

type MessageResponse struct {
	Text     string `json:"text"`
	ThreadID string `json:"threadId"`
	Warning  string `json:"warning,omitempty"`
	Error    string `json:"error,omitempty"`
}

// SendMessage runs one normal `metis run` turn against the selected session.
// Using the CLI's existing runtime keeps model/provider/tool/session behaviour
// identical to terminal use and avoids the old global daemon conversation,
// which ignored threadID and could deadlock on permission events.
func (a *App) SendMessage(input, threadID, approvalMode string) MessageResponse {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()

	input = strings.TrimSpace(input)
	if input == "" {
		return MessageResponse{ThreadID: threadID, Error: "message is empty"}
	}
	if threadID != "" && !validSessionID(threadID) {
		return MessageResponse{Error: "invalid session id"}
	}
	binary, err := a.findMetis()
	if err != nil {
		return MessageResponse{ThreadID: threadID, Error: err.Error()}
	}
	mode := normalizeApprovalMode(approvalMode)
	args := []string{"run", "--no-auth-wizard"}
	newSession := threadID == ""
	if threadID == "" {
		threadID, err = newDesktopSessionID()
		if err != nil {
			return MessageResponse{Error: "create session id: " + err.Error()}
		}
		args = append(args, "--session-id", threadID, "--name", sessionTitle(input))
	} else {
		args = append(args, "--resume", threadID)
	}
	args = append(args, "--mode", mode, "--no-stream")
	args = append(args, "--", input)

	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Minute)
	defer cancel()
	stdout, stderr, err := a.runMetis(ctx, binary, args, a.workDir)
	text := strings.TrimSpace(stdout)
	if err != nil {
		// setupRuntime can fail before `metis run` writes the fresh header (for
		// example, when provider configuration is invalid). Do not hand that
		// unpersisted random ID to the frontend: its next retry would use
		// `--resume` and fail with "session not found". If the header was
		// already written and the provider failed later, keep the ID so the
		// user can continue the persisted conversation.
		if newSession && !a.sessionPersisted(binary, threadID) {
			threadID = ""
		}
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return MessageResponse{Text: text, ThreadID: threadID, Error: detail}
	}
	if text == "" {
		text = "Metis completed without a text response."
	}
	return MessageResponse{Text: text, ThreadID: threadID, Warning: userVisibleCLIWarning(stderr)}
}

func (a *App) sessionPersisted(binary, id string) bool {
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 5*time.Second)
	defer cancel()
	stdout, _, err := a.runMetis(ctx, binary, []string{"sessions", "export", id}, a.workDir)
	return err == nil && strings.TrimSpace(stdout) != ""
}

func newDesktopSessionID() (string, error) {
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("desktop-%x", token[:]), nil
}

func sessionTitle(firstPrompt string) string {
	runes := []rune(strings.TrimSpace(firstPrompt))
	if len(runes) > 60 {
		runes = runes[:60]
	}
	return string(runes)
}

func userVisibleCLIWarning(stderr string) string {
	var warnings []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[permission]") || strings.HasPrefix(line, "[askuser]") {
			warnings = append(warnings, line)
		}
	}
	return strings.Join(warnings, "\n")
}

func normalizeApprovalMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions":
		return strings.TrimSpace(mode)
	case "auto", "accept":
		return "acceptEdits"
	case "ask":
		return "default"
	case "bypass":
		return "bypassPermissions"
	case "deny":
		return "dontAsk"
	default:
		return "acceptEdits"
	}
}

func (a *App) metisHome() string {
	if home := os.Getenv("METIS_HOME"); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".metis")
}

func validSessionID(id string) bool {
	trimmed := strings.TrimSpace(id)
	if id != trimmed || id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func findMetisBinary(preferred string) (string, error) {
	if preferred = strings.TrimSpace(preferred); preferred != "" {
		if info, err := os.Stat(preferred); err == nil && executableMetisFile(preferred, info, runtime.GOOS) {
			return preferred, nil
		}
		return "", fmt.Errorf("launcher-provided Metis CLI is not executable: %s", preferred)
	}
	if override := strings.TrimSpace(os.Getenv("METIS_BIN")); override != "" {
		if info, err := os.Stat(override); err == nil && executableMetisFile(override, info, runtime.GOOS) {
			return override, nil
		}
		return "", fmt.Errorf("METIS_BIN does not point to an executable file: %s", override)
	}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	for _, candidate := range []string{
		filepath.Join(cwd, "metis"),
		filepath.Join(cwd, "bin", "metis"),
		"/opt/homebrew/bin/metis",
		"/usr/local/bin/metis",
		filepath.Join(home, "go", "bin", "metis"),
		filepath.Join(home, ".local", "bin", "metis"),
	} {
		if info, err := os.Stat(candidate); err == nil && executableMetisFile(candidate, info, runtime.GOOS) {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath("metis"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("metis CLI not found; set METIS_BIN or install metis in PATH")
}

// Windows FileMode does not expose Unix executable bits. Require an .exe
// there; Unix platforms continue to require at least one execute bit.
func executableMetisFile(path string, info os.FileInfo, goos string) bool {
	if info == nil || info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	if goos == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	return info.Mode()&0o111 != 0
}

func (a *App) runCLI(timeout time.Duration, args ...string) (string, string, error) {
	binary, err := a.findMetis()
	if err != nil {
		return "", "", err
	}
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	return a.runMetis(ctx, binary, args, a.workDir)
}

func runMetisCommand(ctx context.Context, binary string, args []string, dir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type DesktopSettings struct {
	Theme           string `json:"theme"`
	WorkMode        string `json:"workMode"`
	Language        string `json:"language"`
	FileOpenTarget  string `json:"fileOpenTarget"`
	DefaultPerms    bool   `json:"defaultPermissions"`
	AutoReview      bool   `json:"autoReview"`
	FullAccess      bool   `json:"fullAccess"`
	MarkdownEnabled bool   `json:"markdownEnabled"`
	ShowTokens      bool   `json:"showTokens"`
}

func (a *App) settingsPath() string {
	return filepath.Join(a.metisHome(), "desktop-settings.json")
}

func (a *App) GetSettings() string {
	data, err := os.ReadFile(a.settingsPath())
	if err != nil {
		defaults := DesktopSettings{
			Theme:           "dark",
			WorkMode:        "coding",
			Language:        "en",
			FileOpenTarget:  "terminal",
			DefaultPerms:    true,
			AutoReview:      true,
			FullAccess:      false,
			MarkdownEnabled: true,
			ShowTokens:      true,
		}
		out, _ := json.Marshal(defaults)
		return string(out)
	}
	return string(data)
}

func (a *App) SaveSettings(settingsJson string) error {
	var settings DesktopSettings
	if err := json.Unmarshal([]byte(settingsJson), &settings); err != nil {
		return fmt.Errorf("invalid desktop settings: %w", err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode desktop settings: %w", err)
	}
	if err := writeFileAtomic(a.settingsPath(), data, 0o644); err != nil {
		return fmt.Errorf("save desktop settings: %w", err)
	}
	return nil
}

// writeFileAtomic writes a complete temporary file in the destination
// directory and then renames it into place. A crash can therefore leave the
// previous settings or the new settings, but never a truncated JSON file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
