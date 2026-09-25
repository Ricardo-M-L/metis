package webui

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/processutil"
)

// The helper is a real process with a private stdin/stdout transport. It never
// touches provider configuration, credentials, or a user's session store.
func TestIsolatedWorkerProtocolHelper(t *testing.T) {
	scenario := os.Getenv("METIS_TEST_WORKER_PROTOCOL")
	if scenario == "" {
		return
	}
	encoder := desktopipc.NewEncoder(os.Stdout)
	decoder := desktopipc.NewDecoder(os.Stdin)
	emit := func(id string, event desktopipc.Event) {
		if err := encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeEvent, ID: id, Event: &event}); err != nil {
			os.Exit(41)
		}
	}
	switch scenario {
	case "duplex":
		emit("permission-a", desktopipc.Event{Kind: agent.EventPermissionRequest, PermissionTool: "Bash", ToolUseID: "tool-a"})
		emit("permission-b", desktopipc.Event{Kind: agent.EventPermissionRequest, PermissionTool: "Write", ToolUseID: "tool-b"})
		decisions := make(map[string]agent.PermissionDecision)
		for i := 0; i < 2; i++ {
			reply, err := decoder.Decode()
			if err != nil || reply.Type != desktopipc.TypeReply {
				os.Exit(42)
			}
			decisions[reply.ID] = reply.Decision
		}
		if decisions["permission-a"] != agent.PermissionDecisionDeny || decisions["permission-b"] != agent.PermissionDecisionAlwaysAllow || len(decisions) != 2 {
			os.Exit(43)
		}
		emit("question", desktopipc.Event{Kind: agent.EventAskUser, AskUserQuestion: "Pick a direction", AskUserAllowFreeform: true})
		reply, err := decoder.Decode()
		if err != nil || reply.ID != "question" || reply.Answer != "继续，使用中文" {
			os.Exit(44)
		}
		emit("", desktopipc.Event{Kind: agent.EventTextDelta, TextDelta: "first "})
		emit("", desktopipc.Event{Kind: agent.EventTextDelta, TextDelta: "完整答案"})
		emit("", desktopipc.Event{Kind: agent.EventLoopDone, StopReason: "end_turn"})
		_ = encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeStatus, Status: &desktopipc.Status{SubAgents: 2}})
	case "invalid":
		_, _ = io.WriteString(os.Stdout, "not-json\n")
	case "wrong-direction":
		_ = encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeReply, ID: "bad", Decision: agent.PermissionDecisionAllow})
	case "unknown-kind":
		emit("", desktopipc.Event{Kind: 99999})
	case "missing-id":
		emit("", desktopipc.Event{Kind: agent.EventPermissionRequest})
	case "duplicate-id":
		emit("same", desktopipc.Event{Kind: agent.EventPermissionRequest})
		emit("same", desktopipc.Event{Kind: agent.EventAskUser})
	case "eof":
		emit("", desktopipc.Event{Kind: agent.EventTextDelta, TextDelta: "partial"})
	case "eof-live":
		_ = os.Stdout.Close()
		time.Sleep(time.Minute)
	case "wait-decision":
		emit("cancel-me", desktopipc.Event{Kind: agent.EventPermissionRequest})
		if _, err := decoder.Decode(); err == nil {
			os.Exit(45) // Cancellation must not manufacture an allow/deny reply.
		}
	case "write-failed":
		_ = os.Stdin.Close()
		emit("broken-input", desktopipc.Event{Kind: agent.EventPermissionRequest})
		time.Sleep(time.Minute)
	case "failed-exit":
		emit("", desktopipc.Event{Kind: agent.EventLoopDone, StopReason: "end_turn"})
		os.Exit(47)
	case "persist-delay":
		emit("", desktopipc.Event{Kind: agent.EventLoopDone, StopReason: "end_turn"})
		_ = os.Stdout.Close()
		root := os.Getenv("METIS_TEST_WORKER_ROOT")
		_ = os.WriteFile(filepath.Join(root, "waiting"), []byte("waiting"), 0o600)
		for {
			if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = os.WriteFile(filepath.Join(root, "persisted"), []byte("persisted"), 0o600)
	case "steer-roundtrip":
		first, err := decoder.Decode()
		if err != nil || first.Type != desktopipc.TypeSteer || first.Input != "accept this" {
			os.Exit(51)
		}
		second, err := decoder.Decode()
		if err != nil || second.Type != desktopipc.TypeSteer || second.Input != "decline this" {
			os.Exit(52)
		}
		_ = encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteerResult, ID: second.ID, Accepted: false})
		_ = encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteerResult, ID: first.ID, Accepted: true})
		emit("", desktopipc.Event{Kind: agent.EventLoopDone})
	case "steer-unknown":
		_ = encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteerResult, ID: "unknown", Accepted: true})
	case "steer-duplicate", "steer-pending-exit", "steer-cancel":
		request, err := decoder.Decode()
		if err != nil || request.Type != desktopipc.TypeSteer {
			os.Exit(53)
		}
		if scenario == "steer-duplicate" {
			ack := desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteerResult, ID: request.ID, Accepted: true}
			_ = encoder.Encode(ack)
			_ = encoder.Encode(ack)
		}
		if scenario == "steer-cancel" {
			emit("", desktopipc.Event{Kind: agent.EventTextDelta, TextDelta: "received"})
			_, _ = decoder.Decode()
		} else {
			emit("", desktopipc.Event{Kind: agent.EventLoopDone})
		}
	case "steer-with-permission":
		emit("permission", desktopipc.Event{Kind: agent.EventPermissionRequest})
		request, err := decoder.Decode()
		if err != nil || request.Type != desktopipc.TypeSteer {
			os.Exit(54)
		}
		_ = encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeSteerResult, ID: request.ID, Accepted: true})
		reply, err := decoder.Decode()
		if err != nil || reply.Type != desktopipc.TypeReply || reply.ID != "permission" || reply.Decision != agent.PermissionDecisionAllow {
			os.Exit(55)
		}
		emit("", desktopipc.Event{Kind: agent.EventLoopDone})
	case "large-tail":
		for i := 0; i < 64; i++ {
			emit("", desktopipc.Event{Kind: agent.EventTextDelta, TextDelta: strings.Repeat("流", 4096)})
		}
		emit("", desktopipc.Event{Kind: agent.EventLoopDone, StopReason: "end_turn"})
	default:
		os.Exit(46)
	}
	os.Exit(0)
}

func protocolTestRunner(t *testing.T, scenario string) *processIsolatedTurnRunner {
	t.Helper()
	t.Setenv("METIS_TEST_WORKER_PROTOCOL", scenario)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &processIsolatedTurnRunner{executable: executable, command: func(binary string, args ...string) *exec.Cmd {
		if len(args) != 7 || args[0] != "run" || args[1] != "--resume" || args[3] != "--desktop-worker" || args[4] != "--streamlined" || args[5] != "--" {
			t.Errorf("unexpected worker arguments: %q", args)
		}
		return exec.Command(binary, "-test.run=^TestIsolatedWorkerProtocolHelper$")
	}}
}

func TestProcessIsolatedTurnRunnerRelaysConcurrentInteractions(t *testing.T) {
	runner := protocolTestRunner(t, "duplex")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first chan agent.PermissionDecision
	var text strings.Builder
	var loopDone, legacyText, statuses, interactions int
	result, err := runner.Run(ctx, IsolatedTurnRequest{
		SessionID: "duplex", WorkDir: t.TempDir(), Input: "hello",
		OnText: func(string) { legacyText++ },
		OnStatus: func(status desktopipc.Status) {
			if status.SubAgents == 2 {
				statuses++
			}
		},
		OnEvent: func(event agent.Event) {
			switch event.Kind {
			case agent.EventPermissionRequest:
				interactions++
				if cap(event.PermissionReply) != 1 {
					t.Error("permission reply is not buffered")
				}
				if event.ToolUseID == "tool-a" {
					first = event.PermissionReply
				} else {
					event.PermissionReply <- agent.PermissionDecisionAlwaysAllow
					first <- agent.PermissionDecisionDeny
				}
			case agent.EventAskUser:
				interactions++
				if cap(event.AskUserReply) != 1 {
					t.Error("question reply is not buffered")
				}
				event.AskUserReply <- "继续，使用中文"
			case agent.EventTextDelta:
				text.WriteString(event.TextDelta)
			case agent.EventLoopDone:
				loopDone++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "first 完整答案" || result.Text != text.String() || result.Done == nil || result.Done.StopReason != "end_turn" || loopDone != 0 || legacyText != 0 || interactions != 3 || statuses != 1 {
		t.Fatalf("result=%+v text=%q loopDone=%d legacyText=%d interactions=%d statuses=%d", result, text.String(), loopDone, legacyText, interactions, statuses)
	}
}

func TestProcessIsolatedTurnRunnerRejectsInvalidProtocolAndEarlyEOF(t *testing.T) {
	for _, scenario := range []string{"invalid", "wrong-direction", "unknown-kind", "missing-id", "duplicate-id", "eof", "eof-live"} {
		t.Run(scenario, func(t *testing.T) {
			runner := protocolTestRunner(t, scenario)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "invalid", WorkDir: t.TempDir(), Input: "hello", OnEvent: func(agent.Event) {}})
			if err == nil || result.Done != nil || ctx.Err() != nil {
				t.Fatalf("result=%+v err=%v context=%v", result, err, ctx.Err())
			}
		})
	}
}

func TestProcessIsolatedTurnRunnerCancelsPendingDecision(t *testing.T) {
	runner := protocolTestRunner(t, "wait-decision")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "cancel", WorkDir: t.TempDir(), Input: "hello", OnEvent: func(event agent.Event) {
		if event.Kind == agent.EventPermissionRequest {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) || !result.Stopped || result.Done != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestProcessIsolatedTurnRunnerRejectsClosedReplyChannel(t *testing.T) {
	runner := protocolTestRunner(t, "wait-decision")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "closed", WorkDir: t.TempDir(), Input: "hello", OnEvent: func(event agent.Event) {
		if event.Kind == agent.EventPermissionRequest {
			close(event.PermissionReply)
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "reply channel closed") || ctx.Err() != nil {
		t.Fatalf("err=%v context=%v", err, ctx.Err())
	}
}

func TestProcessIsolatedTurnRunnerDrainsFinalOutputBeforeWait(t *testing.T) {
	runner := protocolTestRunner(t, "large-tail")
	result, err := runner.Run(context.Background(), IsolatedTurnRequest{SessionID: "tail", WorkDir: t.TempDir(), Input: "hello"})
	if err != nil || result.Done == nil || result.Text != strings.Repeat("流", 64*4096) {
		t.Fatalf("text bytes=%d done=%v err=%v", len(result.Text), result.Done, err)
	}
}

func TestProcessIsolatedTurnRunnerCancellationKillsDescendant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires a POSIX process group")
	}
	workdir := t.TempDir()
	script := filepath.Join(workdir, "worker")
	body := `#!/bin/sh
trap '' TERM
sleep 60 &
child=$!
printf '{"version":1,"type":"event","event":{"kind":0,"textDelta":"%s"}}\n' "$child"
wait
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := newProcessIsolatedTurnRunner(IsolatedTurnOptions{Executable: script})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var child int
	result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "tree", WorkDir: workdir, Input: "hello", OnText: func(text string) {
		child, _ = strconv.Atoi(text)
		cancel()
	}})
	if !errors.Is(err, context.Canceled) || !result.Stopped || child <= 0 {
		t.Fatalf("child=%d result=%+v err=%v", child, result, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processutil.Alive(child) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processutil.Alive(child) {
		t.Fatalf("worker descendant %d survived cancellation", child)
	}
}

func TestProcessIsolatedTurnRunnerFailsClosedOnReplyWriteAndDecisionErrors(t *testing.T) {
	for _, scenario := range []string{"write-failed", "invalid-decision"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := scenario
			if fixture == "invalid-decision" {
				fixture = "wait-decision"
			}
			runner := protocolTestRunner(t, fixture)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "write-failed", WorkDir: t.TempDir(), OnEvent: func(event agent.Event) {
				if event.Kind == agent.EventPermissionRequest {
					decision := agent.PermissionDecisionAllow
					if scenario == "invalid-decision" {
						decision = agent.PermissionDecision(99)
					}
					event.PermissionReply <- decision
				}
			}})
			if err == nil || !strings.Contains(err.Error(), "write desktop worker reply") || ctx.Err() != nil {
				t.Fatalf("err=%v context=%v", err, ctx.Err())
			}
		})
	}
}

func TestProcessIsolatedTurnRunnerDoesNotCompleteBeforeSuccessfulExit(t *testing.T) {
	t.Run("nonzero exit after loop completion", func(t *testing.T) {
		runner := protocolTestRunner(t, "failed-exit")
		result, err := runner.Run(context.Background(), IsolatedTurnRequest{SessionID: "failed-exit", WorkDir: t.TempDir()})
		if err == nil || result.Done != nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("wait for durable worker exit", func(t *testing.T) {
		runner := protocolTestRunner(t, "persist-delay")
		root := t.TempDir()
		t.Setenv("METIS_TEST_WORKER_ROOT", root)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "persist", WorkDir: root})
			if err == nil && result.Done == nil {
				err = errors.New("worker success missing done event")
			}
			done <- err
		}()
		for {
			if _, err := os.Stat(filepath.Join(root, "waiting")); err == nil {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("worker finished before persistence barrier: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
		select {
		case err := <-done:
			t.Fatalf("worker finished before persistence release: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		if err := os.WriteFile(filepath.Join(root, "release"), []byte("release"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "persisted")); err != nil {
			t.Fatalf("successful result preceded persisted output: %v", err)
		}
	})
}
