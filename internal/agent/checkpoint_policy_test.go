package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCheckpointPolicySkipsAutomaticStagesWithoutProtectionGap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	shadow := t.TempDir()
	l := &Loop{Checkpointer: checkpoint.NewManager("policy", filepath.VolumeName(home)+string(filepath.Separator), shadow), ckptSnappedAt: -1}
	out := make(chan Event, 20)
	called := 0
	for range 2 {
		l.Messages = append(l.Messages, msg(llm.RoleUser, "run a harmless command"))
		l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(ctx context.Context) error {
			called++
			return l.snapPreEditContext(ctx, "Bash", nil)
		})
		l.runCheckpointStage(context.Background(), out, "after tool", "Bash", func(ctx context.Context) error {
			called++
			return l.recordCheckpointMutationContext(ctx, "Bash", nil, nil, nil)
		})
		l.Messages = append(l.Messages, msg(llm.RoleAssistant, "done"))
	}
	if called != 0 || len(l.ckptStack) != 0 {
		t.Fatalf("policy-disabled workspace ran %d snapshot callbacks and stored %d protection gaps", called, len(l.ckptStack))
	}
	if len(out) != 0 {
		t.Fatalf("automatic disabled hooks must stay quiet; /checkpoint owns the status, got %d events", len(out))
	}
	if entries, err := os.ReadDir(shadow); err != nil || len(entries) != 0 {
		t.Fatalf("policy-disabled workspace touched snapshot storage: %v %v", entries, err)
	}
	if err := l.snapPreEditContext(context.Background(), "Bash", nil); err != nil {
		t.Fatalf("direct automatic baseline did not skip policy: %v", err)
	}
}

func TestCheckpointRealInitializationFailureStillWarns(t *testing.T) {
	brokenRoot := filepath.Join(t.TempDir(), "ordinary-file")
	if err := os.WriteFile(brokenRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("init-error", t.TempDir(), brokenRoot), ckptSnappedAt: -1,
		Messages: []llm.Message{msg(llm.RoleUser, "edit")}}
	out := make(chan Event, 5)
	l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(ctx context.Context) error {
		return l.snapPreEditContext(ctx, "Bash", nil)
	})
	if len(out) != 1 || len(l.ckptStack) != 1 || !l.ckptStack[0].incomplete {
		t.Fatalf("real initialization failure was hidden: notices=%d stack=%+v", len(out), l.ckptStack)
	}
	if notice := (<-out).Info; !strings.Contains(notice, "skipped") || !strings.Contains(notice, "init") || strings.Contains(notice, "workspace policy") {
		t.Fatalf("real initialization failure was misclassified: %s", notice)
	}
	// User-facing scan errors intentionally omit private filesystem paths;
	// retain the actual cause in the error chain for diagnostics.
	var pathError *os.PathError
	if !errors.As(l.Checkpointer.DisabledReason(), &pathError) || !strings.Contains(pathError.Path, "ordinary-file") {
		t.Fatalf("real initialization cause was lost: %v", l.Checkpointer.DisabledReason())
	}
	if _, err := l.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("real initialization failure allowed code rewind: %v", err)
	}
}

func TestCheckpointPolicyPreservesConversationRewindAndExplainsCodeRewind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	l := &Loop{Checkpointer: checkpoint.NewManager("policy-rewind", filepath.VolumeName(home)+string(filepath.Separator), t.TempDir()), ckptSnappedAt: -1,
		Messages: []llm.Message{msg(llm.RoleUser, "first"), msg(llm.RoleAssistant, "done"), msg(llm.RoleUser, "second")}}
	before := l.History()
	for _, scope := range []RewindScope{RewindCode, RewindCodeAndConversation} {
		if _, err := l.RewindToTurn(2, scope); !errors.Is(err, ErrCheckpointUnavailable) || !errors.Is(err, checkpoint.ErrDisabledByPolicy) {
			t.Fatalf("code rewind lost policy reason: scope=%v error=%v", scope, err)
		}
		if !reflect.DeepEqual(before, l.History()) {
			t.Fatal("rejected code rewind changed conversation")
		}
	}
	if _, err := l.RewindWithPersist(nil); !errors.Is(err, checkpoint.ErrDisabledByPolicy) {
		t.Fatalf("empty-stack legacy rewind lost policy reason: %v", err)
	}
	if result, err := l.RewindToTurn(2, RewindConversation); err != nil || !result.ConversationRestored || result.CodeRestored {
		t.Fatalf("policy blocked conversation-only rewind: result=%+v err=%v", result, err)
	}
	if len(l.History()) != 2 {
		t.Fatalf("conversation-only rewind did not remove the selected turn: %+v", l.History())
	}
}

func TestCheckpointPolicyNoticeResetsOnWorkspaceRebind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	l := &Loop{Messages: []llm.Message{msg(llm.RoleUser, "one")}}
	out := make(chan Event, 5)
	for range 2 {
		l.SetCheckpointer(checkpoint.NewManager("rebind", filepath.VolumeName(home)+string(filepath.Separator), t.TempDir()))
		l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(context.Context) error {
			t.Fatal("policy ran snapshot callback after rebind")
			return nil
		})
	}
	if len(out) != 0 {
		t.Fatalf("workspace rebinding must not spam checkpoint policy notices: %d", len(out))
	}
	project := t.TempDir()
	path := filepath.Join(project, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.SetCheckpointer(checkpoint.NewManager("project", project, t.TempDir()))
	if err := l.snapPreEditContext(context.Background(), "Write", map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}
	if len(l.ckptStack) != 1 || l.ckptStack[0].incomplete || !l.Checkpointer.HasBaseline() {
		t.Fatalf("project rebind inherited disabled policy: %+v", l.ckptStack)
	}
}
