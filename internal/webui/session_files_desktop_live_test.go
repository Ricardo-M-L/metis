package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Opt-in fixture serves the actual embedded Desktop UI against an isolated
// session and fake recorded outputs. It never contacts a provider or touches
// the installed Desktop. POST /fixture-stop releases the bounded test server.
func TestSessionFilesDesktopLive(t *testing.T) {
	if os.Getenv("METIS_SESSION_FILES_LIVE") != "1" {
		t.Skip("opt-in browser fixture")
	}
	duration := 12 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("METIS_SESSION_FILES_LIVE_DURATION")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 || parsed > 60*time.Minute {
			t.Fatalf("METIS_SESSION_FILES_LIVE_DURATION must be a positive Go duration no greater than 60m, got %q", raw)
		}
		duration = parsed
	}
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	s, store := testServer(t)
	s.loop = agent.NewLoop(&activationTestProvider{model: "fixture"}, tools.NewRegistry(), permission.New(permission.ModeDefault), nil, "", 2)
	old := rtpkg.CurrentTraceAdapter()
	adapter := rtpkg.InstallTrace(filepath.Join(home, "traces"))
	t.Cleanup(func() { _ = rtpkg.CurrentTraceStore().Close(); rtpkg.SetTraceAdapter(old) })
	workspace := t.TempDir()
	for _, name := range []string{"验收报告.md", "regen.sh", "worker.go", "notes.txt", "security.md"} {
		contents, err := os.ReadFile(filepath.Join("testdata", "session-files-preview", name))
		if err != nil {
			t.Fatal(err)
		}
		// Samples are view-only: even regen.sh is deliberately non-executable.
		if err := os.WriteFile(filepath.Join(workspace, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	reportMessage := fmt.Sprintf("报告已整理完成：[查看验收报告](<%s>)。报告包含验收结论、检查清单、结果表格和代码示例。", filepath.Join(workspace, "验收报告.md"))
	filesMessage := fmt.Sprintf("配套文件：\n\n- [regen.sh · 报告生成脚本](<%s>)\n- [worker.go · 任务处理示例](<%s>)\n- [notes.txt · 交付备注](<%s>)\n\n[security.md · 独立安全样例](<%s>) 供单独检查，不属于报告正文。", filepath.Join(workspace, "regen.sh"), filepath.Join(workspace, "worker.go"), filepath.Join(workspace, "notes.txt"), filepath.Join(workspace, "security.md"))
	sid := "session-file-desktop-check"
	if err := store.WriteHeaderFull(session.Header{ID: sid, Model: "fixture", Title: "文件预览验收", WorkDir: workspace}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "请整理文件预览的验收报告，并附上生成脚本、Go 示例和交付备注，方便我逐个查看。"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: reportMessage}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: filesMessage}}},
	} {
		if err := store.AppendMessage(sid, msg); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Sync(sid); err != nil {
		t.Fatal(err)
	}
	s.activeSessionID = sid
	adapter.SetSession(sid)
	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: reportMessage + "\n\n" + filesMessage})
	adapter.OnEvent(agent.Event{Kind: agent.EventContextInjected, Source: "subagent", ContextText: "<sub_agent_idle>测试子代理已完成校验。</sub_agent_idle>"})
	rtpkg.FlushTrace()
	// Optionally inspect an explicitly selected legacy session in the same UI.
	// Copy both inputs: opening the fixture must never mutate the original files.
	if sample := os.Getenv("METIS_TRACE_SAMPLE_SESSION"); sample != "" {
		tracePath := os.Getenv("METIS_TRACE_SAMPLE_EVENTS")
		if tracePath == "" {
			t.Fatal("METIS_TRACE_SAMPLE_EVENTS is required with a sample session")
		}
		sampleID := strings.TrimSuffix(filepath.Base(sample), ".jsonl")
		if !validSessionID(sampleID) {
			t.Fatal("invalid sample session ID")
		}
		for destination, source := range map[string]string{
			filepath.Join(store.Dir, sampleID+".jsonl"):      sample,
			filepath.Join(home, "traces", sampleID+".jsonl"): tracePath,
		} {
			contents, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, contents, 0600); err != nil {
				t.Fatal(err)
			}
		}
		s.activeSessionID = sampleID
		sid = sampleID
	}
	stop := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture-stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		select {
		case stop <- struct{}{}:
		default:
		}
	})
	mux.Handle("/", s.handler())
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Logf("browser fixture: %s session=%s duration=%s", server.URL, sid, duration)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-stop:
	case <-timer.C:
		t.Fatal("browser fixture timed out")
	}
}
