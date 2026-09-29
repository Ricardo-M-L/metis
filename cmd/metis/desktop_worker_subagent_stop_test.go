package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestDesktopWorkerStopSubAgentCancelsOnlyOwnedChild(t *testing.T) {
	for _, queued := range []bool{true, false} {
		t.Run(map[bool]string{true: "queued", false: "running"}[queued], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			input, parent := io.Pipe()
			output, writer := io.Pipe()
			defer parent.Close()
			defer output.Close()
			defer writer.Close()
			bridge := newDesktopWorkerBridge(ctx, input, writer)
			defer bridge.Close()
			roster := agent.NewRoster(2)
			child, stopChild := context.WithCancel(ctx)
			defer stopChild()
			sibling, stopSibling := context.WithCancel(ctx)
			defer stopSibling()
			target := &agent.Teammate{Name: "target", AgentID: "agt-target"}
			target.SetCancel(stopChild)
			other := &agent.Teammate{Name: "sibling", AgentID: "agt-sibling"}
			other.SetCancel(stopSibling)
			if queued {
				if err := roster.RegisterQueued(target); err != nil {
					t.Fatal(err)
				}
			} else if err := roster.Register(target); err != nil {
				t.Fatal(err)
			}
			if err := roster.Register(other); err != nil {
				t.Fatal(err)
			}
			bridge.setSubAgentRoster(roster)
			encoder, decoder := desktopipc.NewEncoder(parent), desktopipc.NewDecoder(output)
			for _, agentID := range []string{"agt-foreign", "agt-target"} {
				if err := encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeStopSubAgent, ID: "request-" + agentID, AgentID: agentID}); err != nil {
					t.Fatal(err)
				}
				ack, err := decoder.Decode()
				if err != nil {
					t.Fatal(err)
				}
				if ack.Type != desktopipc.TypeStopSubAgentResult || ack.AgentID != agentID || ack.ID != "request-"+agentID || ack.Accepted != (agentID == "agt-target") {
					t.Fatalf("ack=%+v", ack)
				}
				if agentID == "agt-foreign" && child.Err() != nil {
					t.Fatal("foreign id canceled target")
				}
			}
			if child.Err() != context.Canceled || sibling.Err() != nil || bridge.ctx.Err() != nil || bridge.Err() != nil {
				t.Fatalf("target=%v sibling=%v parent=%v bridge=%v", child.Err(), sibling.Err(), bridge.ctx.Err(), bridge.Err())
			}
		})
	}
}
