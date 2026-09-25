package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestDesktopWorkerRoutesConcurrentRepliesByRequestID(t *testing.T) {
	input, parent := io.Pipe()
	defer parent.Close()
	var output bytes.Buffer
	bridge := newDesktopWorkerBridge(context.Background(), input, &output)
	defer bridge.Close()
	first := make(chan agent.PermissionDecision, 1)
	second := make(chan agent.PermissionDecision, 1)
	answer := make(chan string, 1)
	for _, event := range []agent.Event{
		{Kind: agent.EventPermissionRequest, ToolUseID: "same-tool-id", PermissionReply: first},
		{Kind: agent.EventPermissionRequest, ToolUseID: "same-tool-id", PermissionReply: second},
		{Kind: agent.EventAskUser, AskUserQuestion: "which?", AskUserReply: answer},
	} {
		if err := bridge.emit(event); err != nil {
			t.Fatal(err)
		}
	}
	decoder := desktopipc.NewDecoder(&output)
	var messages []desktopipc.Message
	for range 3 {
		message, err := decoder.Decode()
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	if messages[0].ID == messages[1].ID || messages[0].ID == "" {
		t.Fatal("requests did not receive independent identifiers")
	}
	encoder := desktopipc.NewEncoder(parent)
	for _, reply := range []desktopipc.Message{
		{Version: desktopipc.Version, Type: desktopipc.TypeReply, ID: messages[1].ID, Decision: agent.PermissionDecisionDeny},
		{Version: desktopipc.Version, Type: desktopipc.TypeReply, ID: messages[2].ID, Answer: "blue"},
		{Version: desktopipc.Version, Type: desktopipc.TypeReply, ID: messages[0].ID, Decision: agent.PermissionDecisionAllow},
	} {
		if err := encoder.Encode(reply); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-first:
		if got != agent.PermissionDecisionAllow {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("first reply blocked")
	}
	select {
	case got := <-second:
		if got != agent.PermissionDecisionDeny {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("second reply blocked")
	}
	select {
	case got := <-answer:
		if got != "blue" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("answer blocked")
	}
	if bridge.Err() != nil {
		t.Fatal(bridge.Err())
	}
}

func TestDesktopWorkerRejectsInvalidRepliesWithoutGrantingPermission(t *testing.T) {
	for _, name := range []string{"eof", "unknown-id", "invalid-json", "missing-decision"} {
		t.Run(name, func(t *testing.T) {
			input, parent := io.Pipe()
			defer parent.Close()
			bridge := newDesktopWorkerBridge(context.Background(), input, io.Discard)
			defer bridge.Close()
			reply := make(chan agent.PermissionDecision, 1)
			if err := bridge.emit(agent.Event{Kind: agent.EventPermissionRequest, PermissionReply: reply}); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "eof":
				_ = parent.Close()
			case "unknown-id":
				_ = desktopipc.NewEncoder(parent).Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeReply, ID: "foreign", Decision: agent.PermissionDecisionAllow})
			case "invalid-json":
				_, _ = io.WriteString(parent, "not json\n")
			case "missing-decision":
				_, _ = io.WriteString(parent, "{\"version\":1,\"type\":\"reply\",\"id\":\"x\"}\n")
			}
			select {
			case <-bridge.ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("invalid reply did not cancel worker")
			}
			if bridge.Err() == nil {
				t.Fatal("protocol error lost")
			}
			select {
			case decision := <-reply:
				t.Fatalf("transport failure fabricated permission decision %v", decision)
			default:
			}
		})
	}
}

func TestDesktopWorkerCloseUnblocksReaderAndPendingRequests(t *testing.T) {
	input, parent := io.Pipe()
	defer parent.Close()
	bridge := newDesktopWorkerBridge(context.Background(), input, io.Discard)
	if err := bridge.emit(agent.Event{Kind: agent.EventAskUser, AskUserReply: make(chan string, 1)}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { bridge.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close left blocked stdin reader")
	}
	if bridge.Err() != nil {
		t.Fatalf("intentional close became error: %v", bridge.Err())
	}
	if !errors.Is(bridge.ctx.Err(), context.Canceled) {
		t.Fatal("close did not cancel pending work")
	}
}

func TestDesktopWorkerFlagAndIncompatibleOutputModes(t *testing.T) {
	flags, rest, err := parseFlags([]string{"--desktop-worker", "--resume", "test-id", "--streamlined", "--", "prompt"})
	if err != nil || !flags.desktopWorker || len(rest) != 1 {
		t.Fatalf("flags=%+v rest=%v err=%v", flags, rest, err)
	}
	for _, args := range [][]string{
		{"--desktop-worker", "--output-schema", "{}", "prompt"},
		{"--desktop-worker", "--prompt-dump", "prompt"},
		{"--desktop-worker", "--preflight-only"},
	} {
		if err := cmdRun(context.Background(), args); err == nil {
			t.Fatalf("accepted incompatible options %v", args)
		}
	}
}
