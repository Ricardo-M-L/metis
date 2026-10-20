package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func statusDigest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func decodeMessageStatus(t *testing.T, r *tools.Result, err error) map[string]any {
	t.Helper()
	if err != nil || r == nil || r.IsError {
		t.Fatalf("status %#v %v", r, err)
	}
	var got map[string]any
	if err = json.Unmarshal([]byte(r.Output), &got); err != nil {
		t.Fatal(err)
	}
	return got
}
func sendStatusFixture(t *testing.T, tool MessageTeammate, ctx context.Context) map[string]any {
	t.Helper()
	r, err := tool.Execute(ctx, map[string]any{"to": "job:receiver", "body": "create report", "idempotency_key": "owned-task", "task": map[string]any{"id": "task", "objective": "create report", "deliverables": []string{"report.json"}}})
	return decodeMessageStatus(t, r, err)
}
func completeStatusFixture(t *testing.T, s *handoff.Store, id string) handoff.Record {
	t.Helper()
	u := handoff.BusinessUpdate{Assignment: handoff.BusinessAssignment{JobID: "receiver", Workspace: "/private/workspace/owned", RunID: "run-1", WorkItemID: "work-1", VerificationItemID: "verify-1", ConfigSHA256: strings.Repeat("c", 64)}, State: handoff.BusinessAccepted}
	if _, err := s.ReconcileBusiness(context.Background(), id, u); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimID(context.Background(), id, "jobs", "job:receiver", "private-consumer-identity", time.Minute, u.Assignment)
	if err != nil || claim == nil {
		t.Fatal(claim, err)
	}
	if err = s.Ack(context.Background(), id, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	u.SourceRevision = 1
	u.State = handoff.BusinessCompleted
	command := "host-check /private/host/credential token=fixture-opaque-secret"
	u.Output = "Report ready /private/workspace/owned/report.json /private/host/secret C:\\Users\\host\\private token=fixture-opaque-secret " + claim.Token + " " + command
	u.OutputSHA256 = statusDigest(u.Output)
	u.Artifacts = []handoff.Artifact{{Path: "report.json", SHA256: strings.Repeat("d", 64)}}
	u.Acceptance = &handoff.BusinessAcceptance{Passed: true, Source: "host", EvidenceSHA256: strings.Repeat("e", 64), Summary: "verified /private/host/secret token=fixture-opaque-secret", Checks: []handoff.BusinessCheck{{Command: command, OutputSHA256: strings.Repeat("f", 64)}}}
	rec, err := s.ReconcileBusiness(context.Background(), id, u)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}
func TestMessageTeammateStatusOwnerReadsFreshBusinessReceipt(t *testing.T) {
	tool, _, s := durableMessageTool(t)
	ctx := agent.WithAgentName(context.Background(), "alice")
	sent := sendStatusFixture(t, tool, ctx)
	id := sent["id"].(string)
	input := map[string]any{"operation": "status", "message_id": id}
	if !tools.IsReadOnly(tool, input) {
		t.Fatal("status entered durable write operation boundary")
	}
	res, err := tool.Execute(ctx, input)
	pending := decodeMessageStatus(t, res, err)
	if pending["state"] != "pending" || pending["business"] != nil {
		t.Fatal(pending)
	}
	rec := completeStatusFixture(t, s, id)
	res, err = tool.Execute(ctx, input)
	done := decodeMessageStatus(t, res, err)
	b := done["business"].(map[string]any)
	if b["state"] != "completed" || done["acknowledged"] != true || b["output_sha256"] != rec.Business.OutputSHA256 || b["output_redacted"] != true {
		t.Fatal(done)
	}
	if got := b["assignment"].(map[string]any); got["job_id"] != "receiver" || got["run_id"] != "run-1" {
		t.Fatal(got)
	}
	a := b["acceptance"].(map[string]any)
	if a["source"] != "host" || a["passed"] != true {
		t.Fatal(a)
	}
	for _, secret := range []string{rec.Token, rec.Consumer, rec.Business.Assignment.Workspace, "/private/host/secret", "C:\\Users\\host\\private", "fixture-opaque-secret", rec.Business.Acceptance.Checks[0].Command, `"command":`, `"workspace":`, `"token":`, `"consumer":`} {
		if strings.Contains(res.Output, secret) {
			t.Fatalf("model receipt leaked protected field %q", secret)
		}
	}
	// An idempotent resend exposes the same safe receipt, never the full Record.
	res, err = tool.Execute(ctx, map[string]any{"to": "job:receiver", "body": "create report", "idempotency_key": "owned-task", "task": map[string]any{"id": "task", "objective": "create report", "deliverables": []string{"report.json"}}})
	duplicate := decodeMessageStatus(t, res, err)
	if duplicate["duplicate"] != true || duplicate["business"].(map[string]any)["state"] != "completed" {
		t.Fatal(duplicate)
	}
	for _, secret := range []string{rec.Token, rec.Consumer, rec.Business.Assignment.Workspace, rec.Business.Acceptance.Checks[0].Command} {
		if strings.Contains(res.Output, secret) {
			t.Fatal("resend leaked secret")
		}
	}
}
func TestMessageTeammateStatusRejectsOtherSendersAndRecipients(t *testing.T) {
	tool, _, s := durableMessageTool(t)
	ctx := agent.WithAgentName(context.Background(), "alice")
	sent := sendStatusFixture(t, tool, ctx)
	id := sent["id"].(string)
	otherRoster := agent.NewRoster(0)
	otherRoster.BindHandoff(s, "other-session")
	other := NewMessageTeammate(permission.New(permission.ModeBypass), otherRoster)
	for _, tc := range []struct {
		tool MessageTeammate
		ctx  context.Context
	}{{tool, agent.WithAgentName(context.Background(), "bob")}, {other, ctx}, {tool, agent.WithAgentName(context.Background(), "job:receiver")}} {
		r, err := tc.tool.Execute(tc.ctx, map[string]any{"operation": "status", "message_id": id})
		if err != nil || !r.IsError || strings.Contains(r.Output, "create report") {
			t.Fatalf("foreign receipt exposed: %#v %v", r, err)
		}
	}
	// A persistent job sender keeps its jobs namespace across session rotation.
	jobCtx := agent.WithAgentName(context.Background(), "job:sender")
	sent = sendStatusFixture(t, tool, jobCtx)
	r, err := other.Execute(jobCtx, map[string]any{"operation": "status", "message_id": sent["id"]})
	if got := decodeMessageStatus(t, r, err); got["sender_scope"] != "jobs" {
		t.Fatal(got)
	}
}
func TestMessageTeammateStatusAckDoesNotClaimBusinessCompletion(t *testing.T) {
	tool, _, s := durableMessageTool(t)
	r, err := tool.Execute(context.Background(), map[string]any{"to": "job:receiver", "body": "notification"})
	sent := decodeMessageStatus(t, r, err)
	claim, err := s.Claim(context.Background(), "jobs", "job:receiver", "receiver", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ack(context.Background(), claim.ID, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	r, err = tool.Execute(context.Background(), map[string]any{"operation": "status", "message_id": sent["id"]})
	got := decodeMessageStatus(t, r, err)
	if got["acknowledged"] != true || got["business"] != nil || strings.Contains(r.Output, "completed") {
		t.Fatal(got)
	}
}
func TestMessageTeammateOperationValidationAndLegacySend(t *testing.T) {
	tool, _, _ := durableMessageTool(t)
	for _, in := range []map[string]any{{"operation": "status"}, {"operation": "status", "message_id": "id", "to": "job:receiver"}, {"operation": "unknown", "to": "x", "body": "x"}, {"operation": true}, {"operation": "send", "message_id": "id", "to": "x", "body": "x"}, {"operation": "send", "to": "x"}} {
		res, err := tool.Execute(context.Background(), in)
		if err != nil || !res.IsError {
			t.Fatalf("invalid input accepted %#v: %#v %v", in, res, err)
		}
		if tools.IsReadOnly(tool, in) {
			t.Fatalf("invalid input classified read-only: %#v", in)
		}
	}
	if required, ok := tool.InputSchema()["required"]; ok {
		t.Fatalf("status cannot supply legacy send-only required fields: %#v", required)
	}
	live := agent.NewRoster(0)
	peer := &agent.Teammate{Name: "bob", AgentID: "agt-bob"}
	if err := live.Register(peer); err != nil {
		t.Fatal(err)
	}
	legacy := NewMessageTeammate(permission.New(permission.ModeBypass), live)
	res, err := legacy.Execute(context.Background(), map[string]any{"to": "bob", "body": "old call"})
	if err != nil || res.IsError || len(peer.Mailbox) != 1 {
		t.Fatalf("legacy send changed %#v %v", res, err)
	}
	res, err = legacy.Execute(context.Background(), map[string]any{"operation": "status", "message_id": "msg-" + strings.Repeat("a", 64)})
	if err != nil || !res.IsError {
		t.Fatal(res, err)
	}
}

func TestMessageTeammateStatusPermissionUsesReadOnlyInput(t *testing.T) {
	gate := permission.New(permission.ModePlan)
	tool := NewMessageTeammate(gate, agent.NewRoster(0))
	gate.SetReadOnlyHook(func(name, input string) bool {
		var in map[string]any
		if json.Unmarshal([]byte(input), &in) != nil {
			return false
		}
		return name == tool.Name() && tool.IsReadOnly(in)
	})
	status := map[string]any{"operation": "status", "message_id": "msg-" + strings.Repeat("a", 64)}
	if got, _ := tool.CanUse(context.Background(), status); got != tools.PermissionAllow {
		t.Fatalf("read-only status was denied by original gate: %v", got)
	}
	if got, _ := tool.CanUse(context.Background(), map[string]any{"to": "job:receiver", "body": "mutating send"}); got == tools.PermissionAllow {
		t.Fatal("send bypassed original permission gate")
	}
}
