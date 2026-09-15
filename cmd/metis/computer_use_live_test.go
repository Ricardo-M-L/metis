package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// Opt-in acceptance uses a real compiled CLI and the user's selected provider,
// but exposes only screen dimensions/cursor coordinates as native tool results,
// never screenshots or credentials in the prompt. OAuth requires an additional
// opt-in and a bounded, isolated copy; no global MCP/plugin config is copied.
func TestComputerUseLiveCLI(t *testing.T) {
	if os.Getenv("METIS_CU_LIVE_TEST") != "1" {
		t.Skip("explicit native/API opt-in required")
	}
	started := time.Now()
	cli, helper := os.Getenv("METIS_CU_TEST_CLI"), os.Getenv("METIS_CU_TEST_HELPER")
	if !filepath.IsAbs(cli) || !filepath.IsAbs(helper) {
		t.Fatal("absolute built CLI/helper paths required")
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal("load provider configuration failed")
	}
	provider := cfg.Provider.Default
	if override := os.Getenv("METIS_CU_LIVE_PROVIDER"); override != "" {
		provider = override
	}
	deadline := started.Add(150 * time.Second)
	var key string
	var credential *auth.OAuthCredential
	var secrets []string
	if provider == "openai-codex" {
		if os.Getenv("METIS_CU_LIVE_OAUTH") != "1" {
			t.Skip("isolated OAuth acceptance requires explicit METIS_CU_LIVE_OAUTH=1")
		}
		originalHome, err := auth.ResolveCredentialHome("")
		if err != nil {
			t.Fatal("cannot resolve existing OAuth credential home")
		}
		originalPath := filepath.Join(originalHome, ".credentials", "llm-oauth.json")
		// Do not call GetOAuth: its legacy migration can modify the source
		// store even when the caller only intends to inspect a credential.
		original, err := os.ReadFile(originalPath)
		if err != nil {
			t.Fatal("cannot read existing private OAuth store; acceptance does not migrate or log in")
		}
		originalHash := sha256.Sum256(original)
		t.Cleanup(func() {
			after, err := os.ReadFile(originalPath)
			if err != nil || sha256.Sum256(after) != originalHash {
				t.Error("original OAuth credential store changed during acceptance")
			}
		})
		credential, err = decodeCULiveOAuth(original, time.Now(), deadline)
		if err != nil {
			t.Fatal(err) // decoder errors deliberately contain no credential data
		}
		secrets = []string{credential.AccessToken, credential.RefreshToken, credential.AccountID}
	} else {
		key, err = cfg.ResolveAPIKey(provider)
		if err != nil || strings.TrimSpace(key) == "" {
			t.Fatal("selected provider has no reusable API-key route; no credential copied")
		}
		secrets = []string{key}
	}
	minimal := config.Config{}
	minimal.Provider.Default = provider
	minimal.Permission.Mode = "bypassPermissions"
	minimal.Tools.Allowed = []string{"ToolSearch", "Skill", "mcp__computer-use__screen_size", "mcp__computer-use__cursor_position"}
	model := ""
	switch provider {
	case "openai-codex":
		minimal.Provider.OpenAICodex = cfg.Provider.OpenAICodex
		model = minimal.Provider.OpenAICodex.Model
	case "openai":
		minimal.Provider.OpenAI = cfg.Provider.OpenAI
		minimal.Provider.OpenAI.APIKey = ""
		minimal.Provider.OpenAI.APIKeyEnv = "METIS_CU_VALIDATION_API_KEY"
		model = minimal.Provider.OpenAI.Model
	case "anthropic":
		minimal.Provider.Anthropic = cfg.Provider.Anthropic
		minimal.Provider.Anthropic.APIKey = ""
		minimal.Provider.Anthropic.APIKeyEnv = "METIS_CU_VALIDATION_API_KEY"
		model = minimal.Provider.Anthropic.Model
	default:
		raw, ok := cfg.Provider.Custom[provider]
		if !ok {
			t.Fatalf("provider %s is not an API-key route", provider)
		}
		raw.APIKey = ""
		raw.APIKeyEnv = "METIS_CU_VALIDATION_API_KEY"
		minimal.Provider.Custom = map[string]config.ProviderRaw{provider: raw}
		model = raw.Model
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal("cannot secure isolated test home")
	}
	minimal.Session.Dir = filepath.Join(home, "sessions")
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(minimal); err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(encoded.String(), secret) {
			t.Fatal("refusing credential-bearing test configuration")
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if credential != nil {
		privateDir := filepath.Join(home, ".credentials")
		if err := os.MkdirAll(privateDir, 0o700); err != nil {
			t.Fatal("cannot create isolated private credential directory")
		}
		payload, err := json.Marshal(struct {
			FormatVersion int                             `json:"format_version"`
			Credentials   map[string]auth.OAuthCredential `json:"credentials"`
		}{1, map[string]auth.OAuthCredential{"openai-codex": *credential}})
		if err != nil {
			t.Fatal("cannot encode isolated OAuth credential")
		}
		isolatedPath := filepath.Join(privateDir, "llm-oauth.json")
		if err := os.WriteFile(isolatedPath, payload, 0o600); err != nil {
			t.Fatal("cannot stage isolated OAuth credential")
		}
		isolatedHash := sha256.Sum256(payload)
		t.Cleanup(func() {
			after, err := os.ReadFile(isolatedPath)
			if err != nil || sha256.Sum256(after) != isolatedHash {
				t.Error("isolated OAuth store changed; acceptance must not refresh credentials")
			}
		})
	}
	env := []string{}
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "METIS_") {
			env = append(env, item)
		}
	}
	env = append(env, "METIS_HOME="+home, "METIS_AUTO_MEMORY=0", "METIS_NO_UPDATE_CHECK=1")
	if credential == nil {
		env = append(env, "METIS_CU_VALIDATION_API_KEY="+key)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, cli, args...)
		cmd.Dir, cmd.Env = home, env
		output, err := cmd.CombinedOutput()
		if err != nil {
			// Model responses and subprocess output can contain sensitive data.
			// Report the exit status only, not the captured response body.
			t.Fatalf("CLI %s failed: %v (%d output bytes suppressed)", args[0], err, len(output))
		}
		return redactCULiveOutput(string(output), secrets)
	}
	run("cu", "install", "--from", helper, "--json")
	run("cu", "enable", "--json")
	prompt := "Use the managed Computer Use tools to read the actual screen size and cursor position. Load the computer-use skill if relevant. Call screen_size and cursor_position, and report their actual returned values. Do not capture a screenshot, read clipboard, change focus, move/click/type, or use any other MCP/server. This is a read-only integration acceptance test."
	result := run("run", "--no-auth-wizard", "--no-markdown", prompt)
	files, err := filepath.Glob(filepath.Join(home, "sessions", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatal("real CLI produced no session")
	}
	succeeded := map[string]bool{}
	evidence := map[string]cuLiveToolEvidence{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		results, observed, err := inspectCULiveToolResults(data)
		if err != nil {
			t.Fatal(err)
		}
		for name := range results {
			succeeded[name] = true
		}
		for name, counts := range observed {
			previous := evidence[name]
			previous.Calls += counts.Calls
			previous.Results += counts.Results
			previous.Errors += counts.Errors
			previous.ValidJSON += counts.ValidJSON
			previous.NativeValues += counts.NativeValues
			evidence[name] = previous
		}
	}
	for _, name := range []string{"mcp__computer-use__screen_size", "mcp__computer-use__cursor_position"} {
		if !succeeded[name] {
			// Fixed tool names and counters only: no model prose, tool payload,
			// identifiers, native values, or credential-bearing diagnostics.
			for _, inspected := range []string{"mcp__computer-use__screen_size", "mcp__computer-use__cursor_position", "ToolSearch", "Skill", "Skill:invoke"} {
				t.Logf("tool evidence %s: %+v", inspected, evidence[inspected])
			}
			t.Fatalf("real CLI has no successful tool_result paired with %s", name)
		}
	}
	if strings.Contains(result, "cleanup failed") {
		t.Fatal("native cleanup was not confirmed")
	}
	toolNames := "screen_size, cursor_position"
	if succeeded["Skill:invoke"] {
		toolNames += ", Skill(invoke)"
	}
	t.Log(redactCULiveOutput(fmt.Sprintf("real CLI provider=%s model=%s: successful tools=%s; elapsed=%s", provider, model, toolNames, time.Since(started).Round(time.Millisecond)), secrets))
}

// decodeCULiveOAuth inspects only the selected entry and returns generic errors
// so malformed input can never echo any part of a credential into test logs.
func decodeCULiveOAuth(data []byte, now, deadline time.Time) (*auth.OAuthCredential, error) {
	var store struct {
		FormatVersion int                        `json:"format_version"`
		Credentials   map[string]json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal(data, &store); err != nil || store.FormatVersion != 1 {
		return nil, errors.New("existing OAuth store is invalid or unsupported")
	}
	var credential auth.OAuthCredential
	if err := json.Unmarshal(store.Credentials["openai-codex"], &credential); err != nil {
		return nil, errors.New("existing OpenAI Codex credential is missing or invalid")
	}
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" || strings.TrimSpace(credential.AccountID) == "" || credential.ExpiresAt.IsZero() {
		return nil, errors.New("existing OpenAI Codex credential is incomplete")
	}
	if credential.ExpiresAt.Sub(now) < 10*time.Minute {
		return nil, errors.New("existing OAuth access token has less than ten minutes remaining; acceptance will not refresh")
	}
	if !deadline.After(now) || !deadline.Before(credential.ExpiresAt.Add(-5*time.Minute)) {
		return nil, errors.New("acceptance deadline must precede the OAuth refresh window")
	}
	return &credential, nil
}

func redactCULiveOutput(output string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			output = strings.ReplaceAll(output, secret, "[REDACTED]")
		}
	}
	return output
}

// Tool names mentioned in text, tool_use without a result, and failed/orphaned
// tool_result blocks are not evidence that the real native tool succeeded.
func successfulCULiveToolResults(data []byte) (map[string]bool, error) {
	succeeded, _, err := inspectCULiveToolResults(data)
	return succeeded, err
}

type cuLiveToolEvidence struct {
	Calls, Results, Errors, ValidJSON, NativeValues int
}

func inspectCULiveToolResults(data []byte) (map[string]bool, map[string]cuLiveToolEvidence, error) {
	pending := map[string]string{}
	succeeded := map[string]bool{}
	evidence := map[string]cuLiveToolEvidence{}
	observe := func(message llm.Message) {
		for _, block := range message.Content {
			switch block.Type {
			case "tool_use":
				if block.ToolUseID != "" {
					pending[block.ToolUseID] = block.ToolName
					if block.ToolName == "Skill" && block.ToolInput["action"] == "invoke" {
						pending[block.ToolUseID] = "Skill:invoke"
					}
					name := pending[block.ToolUseID]
					counts := evidence[name]
					counts.Calls++
					evidence[name] = counts
				}
			case "tool_result":
				name, found := pending[block.ToolUseID]
				delete(pending, block.ToolUseID)
				if !found {
					continue
				}
				counts := evidence[name]
				counts.Results++
				if block.IsError {
					counts.Errors++
				}
				if json.Valid([]byte(block.ToolResult)) {
					counts.ValidJSON++
				}
				valid := validCULiveToolResult(name, block.ToolResult)
				if valid && (name == "mcp__computer-use__screen_size" || name == "mcp__computer-use__cursor_position") {
					counts.NativeValues++
				}
				evidence[name] = counts
				if !block.IsError && valid {
					succeeded[name] = true
				}
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var entry session.Entry
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, nil, errors.New("real CLI session is not valid JSONL")
		}
		switch entry.Type {
		case "message":
			if entry.Message != nil {
				observe(*entry.Message)
			}
		case "history_replace":
			clear(pending)
			for _, message := range entry.Messages {
				observe(message)
			}
		}
	}
	return succeeded, evidence, nil
}

func validCULiveToolResult(name, output string) bool {
	if strings.TrimSpace(output) == "" {
		return false
	}
	if name != "mcp__computer-use__screen_size" && name != "mcp__computer-use__cursor_position" {
		return true
	}
	var fields map[string]*int
	if name == "mcp__computer-use__screen_size" {
		var report struct {
			Width  int `json:"display_width_px"`
			Height int `json:"display_height_px"`
		}
		return json.Unmarshal([]byte(output), &report) == nil && report.Width > 0 && report.Height > 0
	}
	if json.Unmarshal([]byte(output), &fields) != nil {
		return false
	}
	return fields["x"] != nil && fields["y"] != nil // zero and negative coordinates are valid, null is not
}

func TestComputerUseLiveOAuthIsolationPreflight(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	base := auth.OAuthCredential{
		AccessToken: "fake-access", RefreshToken: "fake-refresh", AccountID: "fake-account",
		ExpiresAt: now.Add(time.Hour),
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*auth.OAuthCredential)
		deadline time.Time
		wantErr  bool
	}{
		{name: "complete", deadline: now.Add(150 * time.Second)},
		{name: "access-only", mutate: func(c *auth.OAuthCredential) { c.RefreshToken = "" }, deadline: now.Add(150 * time.Second), wantErr: true},
		{name: "no-account", mutate: func(c *auth.OAuthCredential) { c.AccountID = "" }, deadline: now.Add(150 * time.Second), wantErr: true},
		{name: "expires-too-soon", mutate: func(c *auth.OAuthCredential) { c.ExpiresAt = now.Add(9 * time.Minute) }, deadline: now.Add(150 * time.Second), wantErr: true},
		{name: "deadline-at-refresh-boundary", deadline: base.ExpiresAt.Add(-5 * time.Minute), wantErr: true},
		{name: "deadline-after-refresh-boundary", deadline: base.ExpiresAt.Add(-4 * time.Minute), wantErr: true},
		{name: "deadline-elapsed", deadline: now, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := base
			if tc.mutate != nil {
				tc.mutate(&credential)
			}
			data, err := json.Marshal(map[string]any{
				"format_version": 1, "credentials": map[string]any{
					"openai-codex": credential,
					"unrelated":    map[string]any{"invalid_for_oauth": true},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeCULiveOAuth(data, now, tc.deadline)
			if (err != nil) != tc.wantErr {
				t.Fatalf("preflight error=%v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil && (got == nil || *got != credential) {
				t.Fatal("preflight did not preserve the complete selected credential")
			}
		})
	}
	if _, err := decodeCULiveOAuth([]byte(`{"format_version":1,"credentials":{"openai-codex":{"expires_at":"fake-access"}}}`), now, now.Add(time.Minute)); err == nil || strings.Contains(err.Error(), "fake-access") {
		t.Fatal("malformed credential error must be generic and redact input")
	}
}

func TestComputerUseLiveToolResultsRequireSuccessfulPair(t *testing.T) {
	const name = "mcp__computer-use__screen_size"
	call := llm.ContentBlock{Type: "tool_use", ToolUseID: "call-1", ToolName: name}
	success := llm.ContentBlock{Type: "tool_result", ToolUseID: "call-1", ToolResult: `{"display_width_px":1920,"display_height_px":1080}`}
	failed := success
	failed.IsError = true
	empty := success
	empty.ToolResult = ""
	orphan := success
	orphan.ToolUseID = "different-call"
	null := success
	null.ToolResult = "null"
	for _, tc := range []struct {
		name   string
		blocks []llm.ContentBlock
		want   bool
	}{
		{name: "name-in-text", blocks: []llm.ContentBlock{{Type: "text", Text: name}}},
		{name: "call-only", blocks: []llm.ContentBlock{call}},
		{name: "failed", blocks: []llm.ContentBlock{call, failed}},
		{name: "empty", blocks: []llm.ContentBlock{call, empty}},
		{name: "orphan", blocks: []llm.ContentBlock{call, orphan}},
		{name: "null-result", blocks: []llm.ContentBlock{call, null}},
		{name: "paired", blocks: []llm.ContentBlock{call, success}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			encoder := json.NewEncoder(&data)
			for _, block := range tc.blocks {
				if err := encoder.Encode(session.Entry{Type: "message", Message: &llm.Message{Content: []llm.ContentBlock{block}}}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := successfulCULiveToolResults(data.Bytes())
			if err != nil || got[name] != tc.want {
				t.Fatalf("successful paired result=%v, err=%v, want=%v", got[name], err, tc.want)
			}
		})
	}
}

func TestComputerUseLiveOutputRedactsEveryCredentialField(t *testing.T) {
	secrets := []string{"fake-access", "fake-refresh", "fake-account", ""}
	got := redactCULiveOutput("fake-access / fake-refresh / fake-account", secrets)
	if got != "[REDACTED] / [REDACTED] / [REDACTED]" {
		t.Fatal("output redaction did not cover all credential fields")
	}
}

func TestComputerUseLiveNativeResultSchema(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		want         bool
	}{
		{name: "screen_size", output: `{"display_width_px":1920,"display_height_px":1080}`, want: true},
		{name: "screen_size", output: `{"display_width_px":0,"display_height_px":1080}`},
		{name: "screen_size", output: `{}`},
		{name: "screen_size", output: "unavailable"},
		{name: "cursor_position", output: `{"x":0,"y":0}`, want: true},
		{name: "cursor_position", output: `{"x":-120,"y":90}`, want: true},
		{name: "cursor_position", output: `{"x":1}`},
		{name: "cursor_position", output: `{"x":"NaN","y":90}`},
		{name: "cursor_position", output: `null`},
		{name: "cursor_position", output: `{"x":null,"y":0}`},
		{name: "cursor_position", output: `{"x":0,"y":null}`},
	} {
		if got := validCULiveToolResult("mcp__computer-use__"+tc.name, tc.output); got != tc.want {
			t.Errorf("%s schema valid=%v, want=%v for synthetic fixture %s", tc.name, got, tc.want, tc.output)
		}
	}
}
