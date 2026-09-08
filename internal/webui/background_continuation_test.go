package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type backgroundContinuationProvider struct {
	activationTestProvider
	mu          sync.Mutex
	calls       int
	requests    chan llm.Request
	first       func()
	firstReason string
	firstErr    error
	resumeBlock bool
	resumeTool  bool
}

func (p *backgroundContinuationProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 && p.first != nil {
		p.first()
	}
	p.requests <- req
	if call == 1 && p.firstErr != nil {
		return nil, p.firstErr
	}
	if call > 1 && p.resumeBlock {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if call == 2 && p.resumeTool {
		return &composerSummaryStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: "background-protected", ToolName: "ProtectedBackgroundResume"},
			{Type: "tool_input_delta", ToolUseID: "background-protected", InputDelta: `{}`},
			{Type: "tool_use_stop", ToolUseID: "background-protected", InputDelta: `{}`},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	text, reason := "waiting for the background task", "end_turn"
	if call > 1 {
		text = "continued after background completion"
	} else if p.firstReason != "" {
		reason = p.firstReason
	}
	return &composerSummaryStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: text},
		{Type: "message_delta", StopReason: reason},
		{Type: "message_stop"},
	}}, nil
}

type protectedBackgroundResumeTool struct {
	tools.BaseTool
	executions *atomic.Int32
}

func (protectedBackgroundResumeTool) Name() string { return "ProtectedBackgroundResume" }
func (protectedBackgroundResumeTool) Description() string {
	return "permission-protected continuation fixture"
}
func (protectedBackgroundResumeTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (protectedBackgroundResumeTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (protectedBackgroundResumeTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAsk, "explicit approval required"
}
func (t protectedBackgroundResumeTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	t.executions.Add(1)
	return &tools.Result{Output: "approved continuation effect"}, nil
}

func TestBackgroundContinuationPreservesDesktopPermissionApproval(t *testing.T) {
	f := newBackgroundContinuationFixture(t)
	f.provider.resumeTool = true
	var executions atomic.Int32
	f.server.loop.Registry.Register(protectedBackgroundResumeTool{executions: &executions})
	sid := f.prompt(t)
	events := f.server.hub.subscribe()
	defer f.server.hub.unsubscribe(events)
	_ = f.release.Close()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.ev.Kind != agent.EventPermissionRequest {
				continue
			}
			if event.session != sid || executions.Load() != 0 {
				t.Fatal("continuation bypassed permission or crossed session")
			}
			id, _ := event.extra["permId"].(string)
			rr := httptest.NewRecorder()
			f.server.handlePermission(rr, httptest.NewRequest(http.MethodPost, "/api/permission", strings.NewReader(`{"id":"`+id+`","approve":true}`)))
			if rr.Code != http.StatusOK {
				t.Fatalf("approval = %d: %s", rr.Code, rr.Body.String())
			}
			waitBackgroundCondition(t, "approved effect", func() bool { return executions.Load() == 1 })
			return
		case <-timer.C:
			t.Fatal("continuation did not publish normal approval card")
		}
	}
}

func TestDesktopStopDuringFinalUnwindCannotRearmBackgroundContinuation(t *testing.T) {
	f := newBackgroundContinuationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	f.server.cancelMu.Lock()
	f.server.cancelTurn, f.server.runningSession, f.server.turnDone = cancel, "finishing", done
	f.server.cancelMu.Unlock()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		f.server.handleStop(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/stop", strings.NewReader(`{"sessionId":"finishing"}`)))
	}()
	<-ctx.Done()
	// Model/persistence already succeeded; HTTP handler tries to arm after
	// Stop has observed and cancelled its still-registered ownership slot.
	f.server.armBackgroundContinuation(ctx, "finishing")
	f.server.cancelMu.Lock()
	armed := f.server.backgroundCancel != nil
	f.server.cancelTurn, f.server.runningSession, f.server.turnDone = nil, "", nil
	f.server.cancelMu.Unlock()
	close(done)
	<-stopped
	if armed {
		t.Fatal("cancelled finishing turn rearmed background continuation")
	}
}

type backgroundContinuationFixture struct {
	server   *Server
	provider *backgroundContinuationProvider
	registry *jobs.Registry
	store    *session.Store
	release  io.WriteCloser
	jobID    string
}

func newBackgroundContinuationFixture(t *testing.T) *backgroundContinuationFixture {
	t.Helper()
	f := &backgroundContinuationFixture{}
	f.registry = jobs.NewRegistry(t.TempDir())
	var err error
	f.store, err = session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.provider = &backgroundContinuationProvider{
		activationTestProvider: activationTestProvider{name: "background-fixture", model: "background-fixture"},
		requests:               make(chan llm.Request, 16),
	}
	f.provider.first = func() {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf 'background-ready\\n'")
		writer, err := cmd.StdinPipe()
		if err != nil {
			cancel()
			t.Error(err)
			return
		}
		job, err := f.registry.Spawn(jobs.SpawnArgs{Command: "fixture background wait", Cmd: cmd, Cancel: cancel})
		if err != nil {
			cancel()
			_ = writer.Close()
			t.Error(err)
			return
		}
		f.release, f.jobID = writer, job.ID
	}
	loop := agent.NewLoop(f.provider, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "fixture", 3)
	loop.Model = f.provider.ModelID()
	loop.Jobs, loop.JobNotify = f.registry, f.registry.Notify()
	f.server = NewServer("127.0.0.1:0", loop, f.store)
	t.Cleanup(func() {
		f.server.beginClosing()
		if f.release != nil {
			_ = f.release.Close()
		}
		if err := f.server.persistDesktopClose(); err != nil {
			t.Error(err)
		}
		f.registry.ResetAndWait(time.Second)
	})
	return f
}

func (f *backgroundContinuationFixture) prompt(t *testing.T) string {
	t.Helper()
	rr := httptest.NewRecorder()
	f.server.handleTurn(rr, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"input":"finish this background task"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("turn status %d: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	<-f.provider.requests
	return result.SessionID
}

func waitBackgroundCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestBackgroundCompletionResumesIdleDesktopAndPersistsWithoutUserPrompt(t *testing.T) {
	f := newBackgroundContinuationFixture(t)
	sid := f.prompt(t)
	_ = f.release.Close()
	var request llm.Request
	select {
	case request = <-f.provider.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("completed background job did not resume idle Desktop")
	}
	encoded, _ := json.Marshal(request)
	if !strings.Contains(string(encoded), "job_notification") || !strings.Contains(string(encoded), f.jobID) {
		t.Fatalf("resumption did not receive original job notification: %s", encoded)
	}
	waitBackgroundCondition(t, "persisted continuation", func() bool {
		hdr, history, err := f.store.Load(sid)
		if err != nil || hdr.Status != "completed" {
			return false
		}
		data, _ := json.Marshal(history)
		return strings.Contains(string(data), "continued after background completion")
	})
	_, history, err := f.store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if got := displayTurnCount(history); got != 1 {
		t.Fatalf("automatic continuation invented a user prompt: display turns = %d", got)
	}
	metrics, err := f.store.ReadMessageMetrics(sid)
	if err != nil || len(metrics) != 2 {
		t.Fatalf("continuation metrics = %+v, err %v", metrics, err)
	}
	select {
	case <-f.provider.requests:
		t.Fatal("one completion caused duplicate continuation")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestBackgroundCompletionDoesNotResumeFailedOrIncompleteDesktopTurn(t *testing.T) {
	for _, reason := range []string{"error", "max_tokens"} {
		t.Run(reason, func(t *testing.T) {
			f := newBackgroundContinuationFixture(t)
			if reason == "error" {
				f.provider.firstErr = errors.New("fixture provider failure")
			} else {
				f.provider.firstReason = reason
			}
			rr := httptest.NewRecorder()
			f.server.handleTurn(rr, httptest.NewRequest(http.MethodPost, "/api/turns", strings.NewReader(`{"input":"task"}`)))
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("turn status = %d: %s", rr.Code, rr.Body.String())
			}
			<-f.provider.requests
			_ = f.release.Close()
			waitBackgroundCondition(t, "pending completion", f.registry.HasPendingNotifications)
			f.server.cancelMu.Lock()
			armed := f.server.backgroundCancel != nil
			f.server.cancelMu.Unlock()
			if armed {
				t.Fatal("unsuccessful turn armed automatic continuation")
			}
			select {
			case <-f.provider.requests:
				t.Fatal("failed/incomplete turn resumed automatically")
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestBackgroundCompletionDoesNotReviveClosedOrSwitchedDesktopSession(t *testing.T) {
	for _, boundary := range []string{"close", "switch", "clear-history", "undo", "idle-stop"} {
		t.Run(boundary, func(t *testing.T) {
			f := newBackgroundContinuationFixture(t)
			sid := f.prompt(t)
			if boundary == "close" {
				f.server.beginClosing()
			} else if boundary == "switch" {
				// Exercise the real activation boundary even when an embedder
				// has not wired SessionBoundary to reset the shared job pool.
				hdr := &session.Header{ID: "other", Provider: f.provider.Name(), Model: f.provider.ModelID()}
				if err := f.store.WriteHeaderFull(*hdr); err != nil {
					t.Fatal(err)
				}
				f.server.runMu.Lock()
				err := f.server.activateSession(hdr.ID, hdr, nil)
				f.server.runMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				rr := httptest.NewRecorder()
				if boundary == "idle-stop" {
					f.server.handleStop(rr, httptest.NewRequest(http.MethodPost, "/api/stop", strings.NewReader(`{"sessionId":"`+sid+`"}`)))
				} else {
					f.server.handleSessionCommand(rr, httptest.NewRequest(http.MethodPost, "/api/commands/session", strings.NewReader(`{"sessionId":"`+sid+`","command":"`+boundary+`"}`)))
				}
				if rr.Code != http.StatusOK {
					t.Fatalf("boundary %s = %d: %s", boundary, rr.Code, rr.Body.String())
				}
			}
			_ = f.release.Close()
			waitBackgroundCondition(t, "pending old-session completion", f.registry.HasPendingNotifications)
			select {
			case <-f.provider.requests:
				t.Fatal("old session resumed after boundary")
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestBackgroundContinuationWaitsForLatePublicationAndRechecksPending(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy-turn-slot", true: "late-publication"}[late], func(t *testing.T) {
			f := newBackgroundContinuationFixture(t)
			spawn := f.provider.first
			if late {
				f.provider.first = nil
			}
			f.prompt(t)
			f.server.runMu.Lock()
			if late {
				spawn()
			}
			_ = f.release.Close()
			waitBackgroundCondition(t, "pending notification", f.registry.HasPendingNotifications)
			// The completion arrives while another operation owns the turn
			// slot; the watcher must preserve it until ownership is released.
			f.server.runMu.Unlock()
			select {
			case <-f.provider.requests:
			case <-time.After(3 * time.Second):
				t.Fatal("late/coalesced notification was stranded")
			}
		})
	}
}

func TestBackgroundContinuationUsesNormalDesktopStopPath(t *testing.T) {
	f := newBackgroundContinuationFixture(t)
	f.provider.resumeBlock = true
	sid := f.prompt(t)
	_ = f.release.Close()
	select {
	case <-f.provider.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("no continuation")
	}
	rr := httptest.NewRecorder()
	f.server.handleStop(rr, httptest.NewRequest(http.MethodPost, "/api/stop", strings.NewReader(`{"sessionId":"`+sid+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", rr.Code, rr.Body.String())
	}
	waitBackgroundCondition(t, "watcher disarmed", func() bool {
		f.server.cancelMu.Lock()
		defer f.server.cancelMu.Unlock()
		return f.server.backgroundCancel == nil && f.server.turnDone == nil
	})
	hdr, _, err := f.store.Load(sid)
	if err != nil || hdr.Status != "stopped" {
		t.Fatalf("continuation status = %+v, err %v", hdr, err)
	}
}
