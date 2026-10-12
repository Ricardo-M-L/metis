package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestCLINewPromptInvalidatesDesktopResumeBeforeProvider(t *testing.T) {
	for _, failInvalidation := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalidation_failure=%v", failInvalidation), func(t *testing.T) {
			if failInvalidation && os.Getuid() == 0 {
				t.Skip("read-only file fault requires an unprivileged process")
			}
			isolateResumeRuntimeTest(t)
			for key, value := range map[string]string{
				"METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1", "METIS_NO_UPDATE_CHECK": "1",
				"METIS_RUN_CACHE": "0", "METIS_VERIFICATION_URL": "", "METIS_VERIFICATION_TOKEN": "",
				"METIS_VERIFICATION_REQUIRED_CHECKS": "", "METIS_RUN_MAX_SECONDS": "10", "METIS_RECOVERY_MAX_SECONDS": "0",
				"METIS_DUMP_PROMPTS": "0", "METIS_DEBUG": "0",
			} {
				t.Setenv(key, value)
			}
			t.Chdir(t.TempDir())
			store, err := session.NewStore(filepath.Join(config.Home(), "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			const id = "cli-new-input-invalidates-desktop"
			if err := store.WriteHeaderFull(session.Header{ID: id, Provider: "invalidation-fixture", Model: "model", System: "original system"}); err != nil {
				t.Fatal(err)
			}
			original := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "original task"}}}
			if err := store.AppendMessage(id, original); err != nil {
				t.Fatal(err)
			}
			if err := store.FinishDesktopTurn(id, "stopped", true); err != nil {
				t.Fatal(err)
			}
			stopped, _, err := store.Load(id)
			if err != nil || !session.DesktopResumeAvailable(stopped) {
				t.Fatalf("fixture lacks stopped token: %+v err=%v", stopped, err)
			}
			ledger := filepath.Join(store.Dir, id+".jsonl")
			t.Cleanup(func() { _ = os.Chmod(ledger, 0o600) })
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					fmt.Fprint(w, `{"data":[{"id":"model","object":"model"}]}`)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				posts.Add(1)
				header, durable, err := store.Load(id)
				if err != nil || header.ResumeToken != "" || session.DesktopResumeAvailable(header) || header.Status != "stopped" {
					t.Errorf("provider saw reusable Desktop token or changed status: %+v err=%v", header, err)
				}
				if len(durable) != 2 || durable[1].Content[0].Text != "new input" {
					t.Errorf("provider input not persisted after invalidation: %+v", durable)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\",\"role\":\"assistant\"},\"index\":0}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := fmt.Sprintf("[provider]\ndefault = \"invalidation-fixture\"\n[provider.custom.invalidation-fixture]\ntransport = \"openai_chat\"\nbase_url = %q\nmodel = \"model\"\napi_key_env = \"OPENAI_API_KEY\"\n", server.URL+"/v1")
			if err := os.WriteFile(filepath.Join(config.Home(), "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			previousTrace := rtpkg.CurrentTraceAdapter()
			t.Cleanup(func() {
				if current := rtpkg.CurrentTraceStore(); current != nil {
					_ = current.Close()
				}
				rtpkg.SetTraceAdapter(previousTrace)
				builtin.SetTraceStore(rtpkg.CurrentTraceStore())
				if previousTrace == nil {
					agent.SetTraceHook(nil)
				} else {
					agent.SetTraceHook(previousTrace.OnEvent)
				}
			})
			if failInvalidation {
				if err := os.Chmod(ledger, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = cmdRun(ctx, []string{"--resume", id, "--bare", "--no-auth-wizard", "--tools", "Read", "new input"})
			if failInvalidation {
				if err == nil || !strings.Contains(err.Error(), "invalidate desktop resume before new input") {
					t.Fatalf("invalidation failure = %v", err)
				}
				if posts.Load() != 0 {
					t.Fatalf("failed invalidation allowed %d provider calls", posts.Load())
				}
				header, durable, err := store.Load(id)
				if err != nil || header.ResumeToken != stopped.ResumeToken || !reflect.DeepEqual(durable, []llm.Message{original}) {
					t.Fatalf("failed invalidation appended input or changed token: %+v %+v err=%v", header, durable, err)
				}
				if history, err := rtpkg.LoadRecentHistory(10); err != nil || len(history) != 0 {
					t.Fatalf("failed invalidation recorded input history: %+v err=%v", history, err)
				}
				for _, event := range rtpkg.CurrentTraceStore().Events(id) {
					if event.Kind == "user" {
						t.Fatalf("failed invalidation recorded USER trace: %+v", event)
					}
				}
			} else if err != nil || posts.Load() != 1 {
				t.Fatalf("ordinary new prompt err=%v provider calls=%d", err, posts.Load())
			}
		})
	}
}

func TestDesktopResumeTurnFlagGuard(t *testing.T) {
	valid := []string{"--desktop-worker", "--desktop-resume-turn", "--resume", "stopped-session"}
	flags, rest, err := parseFlags(valid)
	if err != nil || !flags.desktopResumeTurn || len(rest) != 0 {
		t.Fatalf("flags=%+v rest=%v err=%v", flags, rest, err)
	}
	for _, args := range [][]string{
		{"--desktop-resume-turn", "--resume", "id"},
		{"--desktop-worker", "--desktop-resume-turn"},
		{"--desktop-worker", "--desktop-resume-turn", "--resume="},
		{"--desktop-worker", "--desktop-resume-turn", "--resume", " "},
		{"--desktop-worker", "--desktop-resume-turn", "--resume"},
	} {
		if _, _, err := parseFlags(args); err == nil {
			t.Fatalf("accepted invalid resume flags %v", args)
		}
	}
	for _, incompatible := range [][]string{
		{"--continue"}, {"-c"}, {"--session-id", "fresh"}, {"--worktree", "fresh"}, {"-W"},
		{"--agent", "other"}, {"--tui"}, {"--preflight-only"}, {"--prompt-dump"},
		{"--output-schema", "schema.json"}, {"--input-format", "json"}, {"--output-format", "json"},
		{"--", ""}, {"--", "continue"}, {"--", "--continue"},
	} {
		args := append(append([]string(nil), valid...), incompatible...)
		if _, _, err := parseFlags(args); err == nil {
			t.Fatalf("accepted incompatible resume startup %v", args)
		}
	}
	if err := cmdRun(context.Background(), []string{"--resume", "id"}); err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("ordinary no-input run resume = %v", err)
	}
	if err := cmdChat(context.Background(), valid); err == nil || !strings.Contains(err.Error(), "only supported by metis run") {
		t.Fatalf("chat accepted private run continuation: %v", err)
	}
}

func desktopResumeHistory() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "finish the original task"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "completed-read", ToolName: "Read", ToolInput: map[string]any{"path": "done.txt"}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "completed-read", ToolResult: "already read"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: "interrupted-write", ToolName: "Bash", ToolInput: map[string]any{"command": "write-output"}}}},
	}
}

func TestDesktopResumeReminderReachesManagedAndLegacyProviderInputs(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%v", managed), func(t *testing.T) {
			var request llm.Request
			provider := &checkpointFuncProvider{stream: func(_ context.Context, req llm.Request) (llm.StreamReader, error) {
				request = req
				return checkpointText("continued"), nil
			}}
			loop := agent.NewLoop(provider, tools.NewRegistry(), nil, nil, "original system", 2)
			loop.AppendUser("original user input")
			if managed {
				loop.SystemSections = []llm.SystemSection{{Name: "base", Body: "original system", Cache: true}}
			}
			before := loop.History()
			applyDesktopResumeTurnReminder(loop)
			if !reflect.DeepEqual(before, loop.History()) {
				t.Fatal("resume reminder created a history message")
			}
			_, done := startCheckpointLoop(t, loop)
			if err := waitCheckpointLoop(t, done); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(request.System, desktopResumeTurnReminder) {
				t.Fatal("legacy system input lost the continuation reminder")
			}
			if managed {
				found := false
				for _, section := range request.SystemSections {
					if section.Name == "desktop_resume_turn" && section.Body == desktopResumeTurnReminder && section.Volatile && !section.Cache {
						found = true
					}
				}
				if !found {
					t.Fatal("managed provider sections lost the continuation reminder")
				}
			}
		})
	}
}

func writeDesktopResumeHistory(t *testing.T, store *session.Store, id, provider string) {
	t.Helper()
	if err := store.WriteHeaderFull(session.Header{ID: id, Provider: provider, Model: "desktop-resume-model", System: "original system"}); err != nil {
		t.Fatal(err)
	}
	for _, message := range desktopResumeHistory() {
		if err := store.AppendMessage(id, message); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareDesktopResumeCheckpointPersistsOrphanRepairBeforeProvider(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "resume-repair"
	writeDesktopResumeHistory(t, store, id, "fixture")
	_, durable, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := &checkpointFuncProvider{stream: func(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
		calls.Add(1)
		_, persisted, err := store.Load(id)
		if err != nil || len(persisted) != 5 || persisted[4].Content[0].ToolUseID != "interrupted-write" {
			t.Errorf("provider started before orphan repair was durable: history=%+v err=%v", persisted, err)
		}
		return checkpointText("continued"), nil
	}}
	loop := agent.NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 2)
	loop.Restore(durable)
	checkpoint, err := prepareDesktopResumeCheckpoint(loop, store, id)
	if err != nil {
		t.Fatal(err)
	}
	_, saved, err := store.Load(id)
	if err != nil || !reflect.DeepEqual(saved, loop.History()) {
		t.Fatalf("repair checkpoint did not save restored history: history=%+v err=%v", saved, err)
	}
	if calls.Load() != 0 {
		t.Fatal("preparation called the provider")
	}
	before, err := os.ReadFile(filepath.Join(store.Dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(store.Dir, id+".jsonl"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("normal cursor duplicated the repair checkpoint: %v", err)
	}
	_, done := startCheckpointLoop(t, loop)
	if err := waitCheckpointLoop(t, done); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d, want one continuation", calls.Load())
	}
}

func TestPrepareDesktopResumeCheckpointFailureBlocksProvider(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("read-only file fault requires an unprivileged process")
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "resume-repair-failure"
	writeDesktopResumeHistory(t, store, id, "fixture")
	_, history, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := &checkpointFuncProvider{stream: func(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
		calls.Add(1)
		return checkpointText("must not run"), nil
	}}
	loop := agent.NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 2)
	loop.Restore(history)
	path := filepath.Join(store.Dir, id+".jsonl")
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	checkpoint, err := prepareDesktopResumeCheckpoint(loop, store, id)
	if err == nil || !strings.Contains(err.Error(), "save desktop resume repair checkpoint") || checkpoint != nil {
		t.Fatalf("failed repair checkpoint=%+v err=%v", checkpoint, err)
	}
	if calls.Load() != 0 || loop.HistoryCheckpoint != nil || loop.CompactionCheckpoint != nil {
		t.Fatal("failed preparation allowed provider work or installed run checkpoints")
	}
	_, saved, err := store.Load(id)
	if err != nil || !reflect.DeepEqual(saved, history) {
		t.Fatalf("failed repair changed durable history: %v", err)
	}
}

func TestCLIDesktopResumeTurnDoesNotRecordNewInput(t *testing.T) {
	for _, scenario := range []struct {
		name                string
		cancelProvider      bool
		failFinalCheckpoint bool
	}{
		{name: "completed"},
		{name: "canceled", cancelProvider: true},
		{name: "canceled_checkpoint_failure", cancelProvider: true, failFinalCheckpoint: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.failFinalCheckpoint && os.Getuid() == 0 {
				t.Skip("read-only file fault requires an unprivileged process")
			}
			cancelProvider := scenario.cancelProvider
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			for key, value := range map[string]string{
				"METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1", "METIS_NO_UPDATE_CHECK": "1",
				"METIS_RUN_CACHE": "0", "METIS_SIMPLE": "", "METIS_COORDINATOR_MODE": "",
				"METIS_VERIFICATION_URL": "", "METIS_VERIFICATION_TOKEN": "", "METIS_VERIFICATION_REQUIRED_CHECKS": "",
				"METIS_RUN_MAX_SECONDS": "10", "METIS_RECOVERY_MAX_SECONDS": "0",
				"METIS_DUMP_PROMPTS": "0", "METIS_DEBUG": "0", "METIS_DESKTOP_RESUME_KEY": "local-fixture-key",
			} {
				t.Setenv(key, value)
			}
			t.Chdir(t.TempDir())
			store, err := session.NewStore(filepath.Join(home, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			const id = "desktop-resume-input"
			writeDesktopResumeHistory(t, store, id, "desktop-resume-fixture")
			ledger := filepath.Join(store.Dir, id+".jsonl")
			t.Cleanup(func() { _ = os.Chmod(ledger, 0o600) })
			if err := rtpkg.AppendHistory(rtpkg.HistoryEntry{SessionID: id, Input: "finish the original task", Source: "desktop"}); err != nil {
				t.Fatal(err)
			}
			historyBefore, err := os.ReadFile(rtpkg.HistoryJSONLPath())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					fmt.Fprint(w, `{"data":[{"id":"desktop-resume-model","object":"model"}]}`)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				posts.Add(1)
				_, durable, loadErr := store.Load(id)
				if loadErr != nil || len(durable) != 5 || !durable[4].Content[0].IsError {
					t.Errorf("provider ran before durable orphan repair: history=%+v err=%v", durable, loadErr)
				}
				var request struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				users, reminder := 0, false
				for _, message := range request.Messages {
					if message.Role == "user" {
						users++
						if !strings.Contains(string(message.Content), "finish the original task") {
							t.Errorf("fabricated provider user input: %s", message.Content)
						}
					}
					if message.Role == "system" && strings.Contains(string(message.Content), desktopResumeTurnReminder) {
						reminder = true
					}
				}
				if users != 1 || !reminder {
					t.Errorf("provider users=%d reminder=%v, want original user and system continuation reminder", users, reminder)
				}
				if cancelProvider {
					if scenario.failFinalCheckpoint {
						if err := os.Chmod(ledger, 0o400); err != nil {
							t.Error(err)
						}
					}
					cancel()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"continued\",\"role\":\"assistant\"},\"index\":0}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := fmt.Sprintf("[provider]\ndefault = \"desktop-resume-fixture\"\n[provider.custom.desktop-resume-fixture]\ntransport = \"openai_chat\"\nbase_url = %q\nmodel = \"desktop-resume-model\"\napi_key_env = \"METIS_DESKTOP_RESUME_KEY\"\n", server.URL+"/v1")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			previousTrace := rtpkg.CurrentTraceAdapter()
			t.Cleanup(func() {
				if current := rtpkg.CurrentTraceStore(); current != nil {
					_ = current.Close()
				}
				rtpkg.SetTraceAdapter(previousTrace)
				builtin.SetTraceStore(rtpkg.CurrentTraceStore())
				if previousTrace == nil {
					agent.SetTraceHook(nil)
				} else {
					agent.SetTraceHook(previousTrace.OnEvent)
				}
			})
			input, parentInput, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer parentInput.Close()
			output, err := os.CreateTemp(t.TempDir(), "worker-output")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			oldInput, oldOutput := os.Stdin, os.Stdout
			os.Stdin, os.Stdout = input, output
			defer func() { os.Stdin, os.Stdout = oldInput, oldOutput }()
			err = cmdRun(ctx, []string{"--desktop-worker", "--desktop-resume-turn", "--resume", id, "--bare", "--tools", "Read"})
			if (!cancelProvider && err != nil) || (cancelProvider && !errors.Is(err, context.Canceled)) {
				t.Fatalf("cmdRun canceled=%v err=%v", cancelProvider, err)
			}
			if scenario.failFinalCheckpoint && !strings.Contains(err.Error(), "save headless session checkpoint") {
				t.Fatalf("final checkpoint failure was not reported: %v", err)
			}
			if posts.Load() != 1 {
				t.Fatalf("generation calls=%d, want one", posts.Load())
			}
			historyAfter, err := os.ReadFile(rtpkg.HistoryJSONLPath())
			if err != nil || !bytes.Equal(historyBefore, historyAfter) {
				t.Fatalf("resume appended input history: %v", err)
			}
			traceStore := rtpkg.CurrentTraceStore()
			if traceStore == nil {
				t.Fatal("no trace store installed")
			}
			for _, event := range traceStore.Events(id) {
				if event.Kind == "user" {
					t.Fatalf("resume fabricated USER trace: %+v", event)
				}
			}
			header, final, err := store.Load(id)
			if err != nil || header.System != "original system" {
				t.Fatalf("resume changed durable system header: %+v err=%v", header, err)
			}
			plainUsers := 0
			for _, message := range final {
				if message.Role == llm.RoleUser {
					for _, block := range message.Content {
						if block.Type == "text" {
							plainUsers++
						}
					}
				}
			}
			if plainUsers != 1 {
				t.Fatalf("durable user messages=%d, want original one", plainUsers)
			}
			if _, err := output.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			decoder := desktopipc.NewDecoder(output)
			checkpoints := 0
			for {
				message, err := decoder.Decode()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if message.Type == desktopipc.TypeCheckpoint && message.Checkpointed {
					checkpoints++
				}
			}
			wantCheckpoints := 1
			if scenario.failFinalCheckpoint {
				wantCheckpoints = 0
			}
			if checkpoints != wantCheckpoints {
				t.Fatalf("final successful checkpoint signals=%d, want %d", checkpoints, wantCheckpoints)
			}
		})
	}
}
