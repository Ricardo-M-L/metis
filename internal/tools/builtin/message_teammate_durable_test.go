package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func durableMessageTool(t *testing.T) (MessageTeammate, *agent.Roster, *handoff.Store) {
	t.Helper()
	s, err := handoff.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := agent.NewRoster(0)
	r.BindHandoff(s, "session-a")
	return NewMessageTeammate(permission.New(permission.ModeBypass), r), r, s
}
func TestMessageTeammateDurableOfflineJobAndIdempotency(t *testing.T) {
	tool, _, store := durableMessageTool(t)
	ctx := agent.WithAgentName(context.Background(), "job:sender")
	input := map[string]any{"to": "job:receiver", "body": "findings", "idempotency_key": "handoff-1", "task": map[string]any{"id": "task-1", "objective": "Review finding", "acceptance": []any{"read before responding"}}}
	var first map[string]any
	for i := 0; i < 2; i++ {
		res, err := tool.Execute(ctx, input)
		if err != nil || res.IsError {
			t.Fatalf("send: %#v %v", res, err)
		}
		var got map[string]any
		if err = json.Unmarshal([]byte(res.Output), &got); err != nil {
			t.Fatal(err)
		}
		if got["state"] != "pending" || got["acknowledged"] != false || got["scope"] != "jobs" || got["from"] != "job:sender" {
			t.Fatalf("receipt misstates queueing: %#v", got)
		}
		if i == 0 {
			first = got
		} else if got["id"] != first["id"] || got["duplicate"] != true {
			t.Fatalf("duplicate: %#v", got)
		}
	}
	rows, err := store.List(context.Background(), "jobs", "job:receiver")
	if err != nil || len(rows) != 1 || rows[0].Task == nil {
		t.Fatalf("outbox=%#v %v", rows, err)
	}
	input["body"] = "changed"
	res, err := tool.Execute(ctx, input)
	if err != nil || !res.IsError || !strings.Contains(res.Output, "conflicts") {
		t.Fatalf("changed same-key accepted: %#v %v", res, err)
	}
}
func TestMessageTeammateDurableNamedQueueNotDuplicatedIntoLiveChannel(t *testing.T) {
	tool, r, s := durableMessageTool(t)
	target := &agent.Teammate{Name: "bob", AgentID: "agt-bob"}
	if err := r.Register(target); err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithInvocationID(context.Background(), "physical-tool-call")
	for i := 0; i < 2; i++ {
		res, err := tool.Execute(ctx, map[string]any{"to": target.AgentID, "body": "update"})
		if err != nil || res.IsError {
			t.Fatalf("send: %#v %v", res, err)
		}
	}
	if len(target.Mailbox) != 0 {
		t.Fatal("durable send also pushed legacy channel, causing double injection")
	}
	rows, err := s.List(context.Background(), "session-a", "bob")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %#v %v", rows, err)
	}
}
func TestMessageTeammateDurableRefusesAnonymousAndBadContract(t *testing.T) {
	tool, r, s := durableMessageTool(t)
	target := &agent.Teammate{}
	if err := r.Register(target); err != nil {
		t.Fatal(err)
	}
	for _, in := range []map[string]any{{"to": target.AgentID, "body": "hello"}, {"to": "job:receiver", "body": "hello", "task": map[string]any{"id": "t", "objective": "x", "unknown": true}}, {"to": "job:receiver", "body": "hello", "task": map[string]any{"id": "t", "objective": "x", "artifacts": []any{map[string]any{"path": "x", "sha256": "not-a-sha"}}}}} {
		res, err := tool.Execute(context.Background(), in)
		if err != nil || !res.IsError {
			t.Fatalf("invalid message accepted: %#v %v", res, err)
		}
	}
	rows, err := s.List(context.Background(), "", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid calls persisted: %#v %v", rows, err)
	}
}
func TestMessageTeammateDurableAcknowledgedDuplicateIsHonest(t *testing.T) {
	tool, _, s := durableMessageTool(t)
	in := map[string]any{"to": "job:receiver", "body": "work", "idempotency_key": "same"}
	if res, err := tool.Execute(context.Background(), in); err != nil || res.IsError {
		t.Fatalf("send: %#v %v", res, err)
	}
	claim, err := s.Claim(context.Background(), "jobs", "job:receiver", "receiver", handoff.DefaultLease)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), claim.ID, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(context.Background(), in)
	if err != nil || res.IsError {
		t.Fatalf("duplicate: %#v %v", res, err)
	}
	var got map[string]any
	if err = json.Unmarshal([]byte(res.Output), &got); err != nil {
		t.Fatal(err)
	}
	if got["acknowledged"] != true || got["duplicate"] != true || got["state"] != "acknowledged" || !strings.Contains(got["message"].(string), "work may still be pending") {
		t.Fatalf("receipt implies task success: %#v", got)
	}
}
func TestMessageTeammateJobWithoutStoreFailsExplicitly(t *testing.T) {
	r := agent.NewRoster(0)
	tool := NewMessageTeammate(permission.New(permission.ModeBypass), r)
	res, err := tool.Execute(context.Background(), map[string]any{"to": "job:receiver", "body": "work"})
	if err != nil || !res.IsError || !strings.Contains(res.Output, "durable mailbox") {
		t.Fatalf("result=%#v %v", res, err)
	}
}

func TestMessageTeammateDurableJobRecipientValidation(t *testing.T) {
	tool, r, s := durableMessageTool(t)
	r.SetHandoffRecipientValidator(func(_ context.Context, address string) error {
		if address == "job:paused" {
			return nil
		}
		return errors.New("unknown or retired persistent job")
	})
	for _, to := range []string{"job:unknown", "job:retired"} {
		res, err := tool.Execute(context.Background(), map[string]any{"to": to, "body": "work"})
		if err != nil || !res.IsError || !strings.Contains(res.Output, "unknown or retired") {
			t.Fatalf("validation failed: %#v %v", res, err)
		}
	}
	res, err := tool.Execute(context.Background(), map[string]any{"to": "job:paused", "body": "parked until resume"})
	if err != nil || res.IsError {
		t.Fatalf("paused queue rejected: %#v %v", res, err)
	}
	rows, err := s.List(context.Background(), "jobs", "")
	if err != nil || len(rows) != 1 || rows[0].To != "job:paused" {
		t.Fatalf("invalid recipients persisted: %#v %v", rows, err)
	}
}
