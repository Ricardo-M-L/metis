package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// Names are user-facing identities, not worktree slugs or transcript paths.
// Keep Unicode names addressable without accepting invisible/path separators.
func TestValidateTeammateName_UnicodeContract(t *testing.T) {
	valid := []string{
		"", "alice", "Agent_1.v2", "界面布局", "对话详情", "流式输出",
		"研究Agent-01.v2_甲", "équipe", "研究２", "𠮷野家", strings.Repeat("研", 32),
	}
	for _, name := range valid {
		t.Run("valid_"+name, func(t *testing.T) {
			if err := validateTeammateName(name); err != nil {
				t.Fatalf("valid teammate name %q rejected: %v", name, err)
			}
		})
	}
	invalid := []string{
		"1研究", "２研究", "_研究", "-研究", ".研究", "_anon-deadbeef",
		"研究/目录", `研究\目录`, "研究 空格", "研究\t名字", "研究\n名字",
		"研究\x00名字", "研究\u200b名字", "研究\u202e名字", "研究\u2066名字",
		"研究\ufeff名字", "研究\u3000名字", "研究@名字", "研究🙂", "🙂研究",
		"e" + string(rune(0x0301)) + "quipe", // No implicit NFC normalization.
		strings.Repeat("研", 33), strings.Repeat("a", 33), "研究" + string([]byte{0xff}),
	}
	for _, name := range invalid {
		t.Run("invalid_"+name, func(t *testing.T) {
			if err := validateTeammateName(name); err == nil {
				t.Fatalf("unsafe or overlong teammate name %q accepted", name)
			}
		})
	}
}

func TestAgentSchema_UnicodeNameLength(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")
	schema := tool.InputSchema()
	if err := pubtool.ValidateInput(schema, map[string]any{"prompt": "x", "name": strings.Repeat("研", 32)}); err != nil {
		t.Fatalf("dispatch schema rejected 32 Unicode characters: %v", err)
	}
	err := pubtool.ValidateInput(schema, map[string]any{"prompt": "x", "name": strings.Repeat("研", 33)})
	if err == nil || err.Path != "$.name" || err.Keyword != "maxLength" {
		t.Fatalf("dispatch schema must reject 33 Unicode characters via maxLength: %v", err)
	}
}

func TestAgentExecute_UnicodeNamePersistsAndResumes(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		label := "inherited_name"
		if explicit {
			label = "explicit_name"
		}
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			roster := agent.NewRoster(0)
			tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system").
				WithRoster(roster).WithSessionPersistence(dir, "unicode-parent")
			const name = "界面布局_回归2"
			res, err := tool.Execute(context.Background(), map[string]any{"name": name, "prompt": "初次任务"})
			if err != nil || res == nil || res.IsError {
				t.Fatalf("Chinese named Agent spawn failed: result=%+v err=%v", res, err)
			}
			members := roster.List()
			if len(members) != 1 {
				t.Fatalf("want one retained teammate, got %d", len(members))
			}
			snap := members[0].Snapshot()
			if snap.Name != name || snap.Anonymous || !agent.ValidSubAgentID(snap.AgentID) {
				t.Fatalf("named identity was changed: %+v", snap)
			}
			files, err := os.ReadDir(filepath.Join(dir, agent.SubAgentTranscriptDirname))
			if err != nil || len(files) != 1 || files[0].Name() != snap.AgentID+".jsonl" {
				t.Fatalf("transcript must stay keyed by safe agent ID: entries=%v err=%v", files, err)
			}
			header, err := agent.LoadSubAgentHeader(dir, snap.AgentID)
			if err != nil || header.TeammateName != name || header.SubAgentOf != "unicode-parent" {
				t.Fatalf("persisted Chinese identity changed: header=%+v err=%v", header, err)
			}
			input := map[string]any{"resume_from": snap.AgentID, "prompt": "继续任务"}
			if explicit {
				input["name"] = name
			}
			res, err = tool.Execute(context.Background(), input)
			if err != nil || res == nil || res.IsError {
				t.Fatalf("Chinese named Agent resume failed: result=%+v err=%v", res, err)
			}
			restored, err := agent.LoadSubAgentSnapshot(dir, snap.AgentID)
			if err != nil {
				t.Fatal(err)
			}
			if restored.Header.ID != snap.AgentID || restored.Header.TeammateName != name {
				t.Fatalf("resume did not preserve Chinese name and ID: %+v", restored.Header)
			}
			var first, followup bool
			for _, message := range restored.Messages {
				for _, block := range message.Content {
					first = first || block.Text == "初次任务"
					followup = followup || block.Text == "继续任务"
				}
			}
			if !first || !followup {
				t.Fatalf("resume must retain both actual user turns: first=%v followup=%v", first, followup)
			}
		})
	}
}

// The ready edge ensures both runners have passed their initial mailbox drain
// before the test sends messages; no timing assumption about goroutine start.
type unicodeNameBlockingProvider struct {
	blockingFakeProvider
	started chan struct{}
}

func (p unicodeNameBlockingProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	reader, err := p.blockingFakeProvider.Stream(ctx, req)
	p.started <- struct{}{}
	return reader, err
}

func TestAgentExecute_UnicodeNameCollisionAndMessageRouting(t *testing.T) {
	dir := t.TempDir()
	roster := agent.NewRoster(0)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := roster.CancelAndWait(ctx); err != nil {
			t.Errorf("background runner cleanup: %v", err)
		}
	})
	provider := unicodeNameBlockingProvider{started: make(chan struct{}, 3)}
	tool := NewAgent(permission.New(permission.ModeBypass), provider, tools.NewRegistry(), "model", "system").
		WithRoster(roster).WithSessionPersistence(dir, "unicode-parent")
	requestedNames := []string{"界面布局", "界面布局", "审阅é"}
	for i, name := range requestedNames {
		res, err := tool.Execute(context.Background(), map[string]any{"name": name, "prompt": "等待独立消息", "run_in_background": true})
		if err != nil || res == nil || res.IsError {
			t.Fatalf("Chinese named background spawn %d failed: result=%+v err=%v", i, res, err)
		}
	}
	for range requestedNames {
		select {
		case <-provider.started:
		case <-time.After(2 * time.Second):
			t.Fatal("named runners did not enter their streams")
		}
	}
	messages := NewMessageTeammate(permission.New(permission.ModeBypass), roster)
	for _, name := range []string{"界面布局", "界面布局-2", "审阅é"} {
		target, ok := roster.Lookup(name)
		if !ok || target.Snapshot().Name != name || target.Snapshot().Anonymous {
			t.Fatalf("Chinese collision identity %q not addressable", name)
		}
		header, err := agent.LoadSubAgentHeader(dir, target.Snapshot().AgentID)
		if err != nil || header.TeammateName != name {
			t.Fatalf("collision suffix not persisted exactly: header=%+v err=%v", header, err)
		}
		body := "只投递给：" + name
		res, err := messages.Execute(context.Background(), map[string]any{"to": name, "body": body})
		if err != nil || res == nil || res.IsError {
			t.Fatalf("Chinese recipient %q refused: result=%+v err=%v", name, res, err)
		}
		select {
		case message := <-target.Mailbox:
			if message.Body != body || message.From != "main" {
				t.Fatalf("message delivered to wrong named recipient %q: %+v", name, message)
			}
		default:
			t.Fatalf("Chinese recipient %q has no delivered message", name)
		}
	}
	for _, unknown := range []string{"界面布局-3", "审阅e" + string(rune(0x0301))} {
		res, err := messages.Execute(context.Background(), map[string]any{"to": unknown, "body": "不存在的收件人"})
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("unknown recipient %q should not alias an existing name: result=%+v err=%v", unknown, res, err)
		}
	}
}
