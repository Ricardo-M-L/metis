package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// durableOperationsBridge shares Desktop's trusted CLI/workspace binding. It
// deliberately accepts neither an executable nor a working directory from HTTP.
type durableOperationsBridge struct {
	options  AutomationOptions
	command  func(context.Context, string, ...string) *exec.Cmd
	actionMu sync.Mutex
	limit    int
	timeout  time.Duration
}

func newDurableOperationsBridge(options AutomationOptions) *durableOperationsBridge {
	return &durableOperationsBridge{options: options, command: exec.CommandContext, limit: 4 << 20, timeout: 20 * time.Second}
}

type durableOperationAction struct {
	Action          string `json:"action"`
	ID              string `json:"id,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Name            string `json:"name,omitempty"`
	ExpectedVersion string `json:"expected_version,omitempty"`
	ExpectedEpoch   uint64 `json:"expected_epoch,omitempty"`
	Note            string `json:"note,omitempty"`
}

func (a durableOperationAction) validate() error {
	validText := func(s string, max int) bool {
		return len(s) <= max && strings.IndexFunc(s, unicode.IsControl) < 0
	}
	if len(a.Note) > 512 || strings.IndexFunc(a.Note, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return errors.New("reason must be at most 512 UTF-8 bytes and contain no unsupported control characters")
	}
	switch a.Action {
	case "pause_job", "resume_job":
		if !validSessionID(a.ID) || a.Kind != "" || a.Name != "" || a.ExpectedVersion != "" || a.ExpectedEpoch != 0 || a.Note != "" {
			return errors.New("a valid job identifier is required")
		}
	case "rollback_learning", "resume_learning":
		if a.ID != "" || (a.Kind != "skill" && a.Kind != "workflow") || strings.TrimSpace(a.Name) == "" || !validText(a.Name, 256) || len(a.ExpectedVersion) != 64 || a.ExpectedEpoch == 0 || !validText(a.ExpectedVersion, 64) || strings.TrimSpace(a.Note) == "" {
			return errors.New("review the learning name, current version and reason before confirming")
		}
	default:
		return errors.New("unsupported operations action")
	}
	return nil
}

func (s *Server) handleDurableOperations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "operations workspace is fixed by Desktop")
		return
	}
	if r.URL.Path == "/api/operations" && r.Method != http.MethodGet || r.URL.Path == "/api/operations/action" && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	bridge := s.durableOps
	if bridge == nil {
		writeError(w, http.StatusServiceUnavailable, "岗位管理未接入本地 CLI；请通过 Desktop 打开工作区。")
		return
	}
	workspace := s.selectedWorkspacePath()
	if workspace == "" {
		workspace = bridge.options.WorkDir
	}
	workspace, err := validateWorkspacePath(workspace)
	if err != nil {
		writeError(w, http.StatusConflict, "所选工作区不可用；请恢复目录或重新选择工作区。")
		return
	}
	var action *durableOperationAction
	if r.Method == http.MethodPost {
		var input struct {
			durableOperationAction
			ExpectedWorkspace string `json:"expected_workspace"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid operations action")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, http.StatusBadRequest, "one action object is required")
			return
		}
		action = &input.durableOperationAction
		if err := action.validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// This is an equality precondition, never a browser-selected path.
		// Capture the trusted selection once so a later navigation cannot move
		// this reviewed action into another workspace.
		if input.ExpectedWorkspace != workspace {
			writeError(w, http.StatusConflict, "工作区已变化；请刷新岗位状态后重新确认操作。")
			return
		}
	}
	data, err := bridge.execute(r.Context(), workspace, action)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (b *durableOperationsBridge) execute(parent context.Context, workspace string, action *durableOperationAction) (map[string]any, error) {
	if !filepath.IsAbs(b.options.Executable) || !filepath.IsAbs(workspace) {
		return nil, errors.New("岗位管理需要有效的本地 CLI 和工作区；请重新打开 Desktop。")
	}
	args := []string{"operations", "snapshot", "--cwd", workspace, "--json"}
	if action != nil {
		b.actionMu.Lock()
		defer b.actionMu.Unlock()
		// The CLI requires host review files outside the target workspace.
		// The OS temp root is trusted composition, never a browser input.
		file, err := os.CreateTemp("", ".metis-desktop-operations-*.json")
		if err != nil {
			return nil, errors.New("无法保存操作请求；请检查本地存储权限。")
		}
		defer os.Remove(file.Name())
		err = json.NewEncoder(file).Encode(action)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, errors.New("无法保存操作请求；请检查本地存储权限。")
		}
		args = []string{"operations", "action", "--cwd", workspace, "--request-file", file.Name()}
	}
	ctx, cancel := context.WithTimeout(parent, b.timeout)
	defer cancel()
	cmd := b.command(ctx, b.options.Executable, args...)
	cmd.Dir = workspace
	cmd.WaitDelay = time.Second
	// Native-shell shutdown credentials are not part of the operations CLI.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	filtered := cmd.Env[:0]
	for _, entry := range cmd.Env {
		if !strings.HasPrefix(entry, "METIS_DESKTOP_FRAME_TOKEN=") {
			filtered = append(filtered, entry)
		}
	}
	cmd.Env = filtered
	stdout := &durableOperationsOutput{limit: b.limit}
	cmd.Stdout = stdout
	cmd.Stderr = &durableOperationsOutput{limit: 8192}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("岗位状态读取超时或已取消。操作可能已保存，请刷新核对后再重试。")
		}
		return nil, errors.New("本地 CLI 未完成岗位操作。请刷新核对状态，并确认 Desktop 与 CLI 版本一致。")
	}
	if stdout.exceeded {
		return nil, errors.New("岗位状态超过显示容量，请在 CLI 中筛选检查。")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil || data == nil {
		return nil, errors.New("本地 CLI 返回了无效的岗位状态。")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("本地 CLI 返回了无效的岗位状态。")
	}
	if action != nil {
		if data["ok"] != true {
			return nil, errors.New("本地 CLI 未确认操作结果；请刷新状态后核对。")
		}
		return map[string]any{"ok": true}, nil
	}
	if data["schema_version"] != json.Number("1") {
		return nil, errors.New("本地 CLI 的岗位状态版本不兼容；请更新 Desktop 与 CLI。")
	}
	projected := make(map[string]any, 8)
	for _, key := range []string{"schema_version", "generated_at", "workspace", "jobs", "handoffs", "runs", "learning", "warnings"} {
		if value, ok := data[key]; ok {
			projected[key] = value
		}
	}
	// The CLI owns the safe projection. This additional boundary makes an older
	// CLI accidentally returning raw records unable to expose credentials or
	// tool results through the HTTP endpoint.
	return durableOperationsSafeView(projected).(map[string]any), nil
}

type durableOperationsOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *durableOperationsOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		b.exceeded = true
		p = p[:max(0, remaining)]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func durableOperationsSafeView(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") && lower != "input_tokens" && lower != "output_tokens" && lower != "max_input_tokens" && lower != "max_output_tokens" && lower != "cache_read_tokens" && lower != "cache_write_tokens" {
				continue
			}
			if strings.Contains(lower, "secret") || strings.Contains(lower, "credential") || strings.Contains(lower, "prompt") || strings.Contains(lower, "password") || lower == "result_json" || lower == "output" || lower == "command" || lower == "check_commands" || lower == "payload" || lower == "raw" || lower == "body" || lower == "definition" || lower == "task" || lower == "lease" || lower == "binding" || lower == "authorization" || lower == "api_key" || lower == "headers" || lower == "env" {
				continue
			}
			out[key] = durableOperationsSafeView(item)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(v), 500))
		for _, item := range v[:min(len(v), 500)] {
			out = append(out, durableOperationsSafeView(item))
		}
		return out
	case string:
		if len(v) > 4096 {
			return fmt.Sprintf("%s…", v[:4096])
		}
	}
	return value
}
