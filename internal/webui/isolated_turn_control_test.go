package webui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestProcessIsolatedTurnRunnerStopsChildWithoutStoppingParent(t *testing.T) {
	runner := protocolTestRunner(t, "stop-subagent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	controls := make(chan IsolatedSteerRequest, 1)
	reply := make(chan bool, 1)
	controls <- IsolatedSteerRequest{StopSubAgentID: "agt-queued", Reply: reply}
	var status desktopipc.Status
	result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "owner", WorkDir: t.TempDir(), Steer: controls, OnStatus: func(value desktopipc.Status) { status = value }})
	if err != nil || result.Stopped || result.Text != "parent continues" || result.Done == nil || ctx.Err() != nil {
		t.Fatalf("result=%+v err=%v ctx=%v", result, err, ctx.Err())
	}
	if !<-reply || len(status.Agents) != 1 || status.Agents[0].Status != "killed" {
		t.Fatalf("ack/status=%+v", status)
	}
}

func TestSubAgentStopAcknowledgementCannotResolveAnotherControl(t *testing.T) {
	for _, message := range []desktopipc.Message{
		{ID: "stop-1", Type: desktopipc.TypeSteerResult, Accepted: true},
		{ID: "stop-1", Type: desktopipc.TypeStopSubAgentResult, AgentID: "agt-other", Accepted: true},
	} {
		reply := make(chan bool, 1)
		controls := &isolatedWorkerControls{pending: map[string]isolatedControlReply{
			"stop-1": {reply: reply, messageType: desktopipc.TypeStopSubAgentResult, agentID: "agt-target"},
		}}
		if err := controls.acknowledge(message); err == nil {
			t.Fatalf("accepted unrelated acknowledgement: %+v", message)
		}
		select {
		case <-reply:
			t.Fatal("unrelated acknowledgement resolved stop")
		default:
		}
		if err := controls.acknowledge(desktopipc.Message{ID: "stop-1", Type: desktopipc.TypeStopSubAgentResult, AgentID: "agt-target", Accepted: true}); err != nil {
			t.Fatal(err)
		}
		if !<-reply {
			t.Fatal("matching acknowledgement did not accept stop")
		}
	}
}

func TestProcessIsolatedTurnRunnerSteerAcknowledgementsMatchRequests(t *testing.T) {
	runner := protocolTestRunner(t, "steer-roundtrip")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	steer := make(chan IsolatedSteerRequest)
	first, second := make(chan bool, 1), make(chan bool, 1)
	done := make(chan error, 1)
	workdir := t.TempDir()
	go func() {
		_, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "steer", WorkDir: workdir, Steer: steer})
		done <- err
	}()
	for _, request := range []IsolatedSteerRequest{{Input: "accept this", Reply: first}, {Input: "decline this", Reply: second}} {
		select {
		case steer <- request:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !<-first || <-second {
		t.Fatal("out-of-order steering acknowledgements were mixed up")
	}
}

func TestProcessIsolatedTurnRunnerSteerRejectsUnknownOrDuplicateAcknowledgement(t *testing.T) {
	for _, scenario := range []string{"steer-unknown", "steer-duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			runner := protocolTestRunner(t, scenario)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			steer := make(chan IsolatedSteerRequest, 1)
			if scenario == "steer-duplicate" {
				steer <- IsolatedSteerRequest{Input: "hello", Reply: make(chan bool, 1)}
			}
			result, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "steer", WorkDir: t.TempDir(), Steer: steer})
			if err == nil || !strings.Contains(err.Error(), "unknown or resolved request") || result.Done != nil || ctx.Err() != nil {
				t.Fatalf("result=%+v err=%v context=%v", result, err, ctx.Err())
			}
		})
	}
}

func TestProcessIsolatedTurnRunnerSteerPendingRepliesRejectOnExitOrCancel(t *testing.T) {
	for _, scenario := range []string{"steer-pending-exit", "steer-cancel"} {
		t.Run(scenario, func(t *testing.T) {
			runner := protocolTestRunner(t, scenario)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			steer := make(chan IsolatedSteerRequest, 1)
			reply := make(chan bool, 1)
			steer <- IsolatedSteerRequest{Input: "hello", Reply: reply}
			_, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "steer", WorkDir: t.TempDir(), Steer: steer, OnText: func(string) { cancel() }})
			if scenario == "steer-cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel err=%v", err)
			}
			if scenario == "steer-pending-exit" && err != nil {
				t.Fatal(err)
			}
			select {
			case accepted := <-reply:
				if accepted {
					t.Fatal("unacknowledged steering was reported accepted")
				}
			default:
				t.Fatal("pending steering reply was leaked")
			}
		})
	}
}

func TestProcessIsolatedTurnRunnerSteerDoesNotBlockApprovalReplies(t *testing.T) {
	runner := protocolTestRunner(t, "steer-with-permission")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	steer := make(chan IsolatedSteerRequest)
	permission := make(chan chan agent.PermissionDecision, 1)
	done := make(chan error, 1)
	workdir := t.TempDir()
	go func() {
		_, err := runner.Run(ctx, IsolatedTurnRequest{SessionID: "steer", WorkDir: workdir, Steer: steer, OnEvent: func(event agent.Event) {
			if event.Kind == agent.EventPermissionRequest {
				permission <- event.PermissionReply
			}
		}})
		done <- err
	}()
	var decision chan agent.PermissionDecision
	select {
	case decision = <-permission:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	reply := make(chan bool, 1)
	select {
	case steer <- IsolatedSteerRequest{Input: "additional direction", Reply: reply}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case accepted := <-reply:
		if !accepted {
			t.Fatal("worker declined steering")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	decision <- agent.PermissionDecisionAllow
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
