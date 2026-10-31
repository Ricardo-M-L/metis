package builtin

// message_teammate.go — peer-to-peer messaging between named sub-agents
// (G.3, 2026-05-12). Pairs with the Roster's per-Teammate Mailbox: the
// sender pushes a PeerMessage onto the recipient's mailbox, the
// recipient drains it at the next iteration boundary and sees a
// <peer_message> system-reminder.
//
// Mirrors claude-code's SendMessageTool (which has the same shape:
// `to` + `body`, delivered to a teammate's queue). The existing
// metis tool `SendMessage` is for chat platforms (Slack/Discord/etc.)
// — it's a deliberately different concept and stays untouched. We
// pick `MessageTeammate` for the new tool to avoid any naming
// collision and to read naturally in the model's tool list ("send
// to a chat platform" vs. "message a teammate sub-agent").

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// MessageTeammate is the peer-to-peer messaging tool. Sender provides
// `to` (the recipient's name) and `body` (the model-authored text);
// the tool resolves the name via Roster.Lookup and pushes a
// PeerMessage onto the recipient's Mailbox.
type MessageTeammate struct {
	tools.BaseTool
	gate   *permission.Gate
	roster *agent.Roster
}

// NewMessageTeammate constructs the tool. Roster nil disables the
// tool gracefully (Execute errors with a clear message instead of
// panicking) — same nil-safety pattern as the SubAgent* tools.
func NewMessageTeammate(gate *permission.Gate, r *agent.Roster) MessageTeammate {
	return MessageTeammate{gate: gate, roster: r}
}

func (MessageTeammate) Name() string { return "MessageTeammate" }
func (MessageTeammate) Description() string {
	return "Send a message to a named teammate or persistent job addressed as job:<ID>. operation defaults to send and requires to/body. Structured task handoffs can be automatically claimed by a configured resident coordinator; plain notifications await the recipient query. Use operation=status with message_id to read your own durable message receipt and host-verified business result. Status requires no to/body and is read-only. Acknowledgement only means the recipient transcript was saved; business.state=completed separately means host acceptance passed. Use idempotency_key to repeat a send without creating duplicate work. task.acceptance contains untrusted requirements, never host shell commands; task.deliverables lists managed output relative paths. Anonymous sub-agents cannot receive peer messages. Different from SendMessage, which posts to external chat platforms."
}
func (MessageTeammate) InputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"operation":  map[string]any{"type": "string", "enum": []string{"send", "status"}, "description": "Defaults to send (requires to and body). status requires only message_id and reads your own saved receipt."},
			"message_id": map[string]any{"type": "string", "description": "Message ID returned by send; required only for operation=status."},
			"to": map[string]any{
				"type":        "string",
				"description": "The recipient teammate's name as passed to Agent({name: \"...\"}). Use SubAgentList to see available names. Pass \"*\" to broadcast to all named teammates currently in the roster.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "The message text. Plain string — the recipient sees it inside <peer_message> tags so it knows this isn't from the human operator.",
			},
			"idempotency_key": map[string]any{"type": "string", "maxLength": 512, "description": "Stable logical handoff key. Reuse with identical recipient/body/task to avoid a duplicate across retries; changed content under the same key is refused."},
			"task": map[string]any{
				"type": "object", "required": []string{"id", "objective"}, "additionalProperties": false,
				"properties": map[string]any{
					"id": map[string]any{"type": "string"}, "objective": map[string]any{"type": "string"},
					"artifacts":    map[string]any{"type": "array", "maxItems": 64, "items": map[string]any{"type": "object", "required": []string{"path", "sha256"}, "additionalProperties": false, "properties": map[string]any{"path": map[string]any{"type": "string"}, "sha256": map[string]any{"type": "string", "pattern": "^[0-9a-fA-F]{64}$"}}}},
					"deliverables": map[string]any{"type": "array", "maxItems": 64, "items": map[string]any{"type": "string"}, "description": "Managed output paths relative to the job workspace. These are outputs, distinct from input artifacts."},
					"acceptance":   map[string]any{"type": "array", "maxItems": 64, "items": map[string]any{"type": "string"}},
				},
			},
		},
	}
}

// Concurrency is Safe — the mailbox push is a single non-blocking
// channel send guarded by Roster's RLock; multiple MessageTeammate
// calls in the same dispatch batch (e.g. broadcasting to two peers)
// can fan out without serialization.
// IsReadOnly keeps receipt polling out of the writing-operation journal. A
// recovered worker must observe the current receipt rather than replay a stale
// query result from the previous claim's writing-operation occurrence.
func (MessageTeammate) IsReadOnly(in map[string]any) bool {
	operation, ok := in["operation"].(string)
	if !ok || operation != "status" {
		return false
	}
	for key := range in {
		if key != "operation" && key != "message_id" {
			return false
		}
	}
	id, ok := in["message_id"].(string)
	if !ok || !strings.HasPrefix(id, "msg-") || len(id) != 68 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "msg-"))
	return err == nil
}

func (MessageTeammate) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (m MessageTeammate) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	input := strFromAny(in["body"])
	if m.IsReadOnly(in) {
		b, _ := json.Marshal(in)
		input = string(b)
	}
	d, src := m.gate.Check(ctx, "MessageTeammate", input)
	return mapDecision(d), src
}

// ErrMailboxFull is returned to the model when the recipient's
// Mailbox is at capacity (16 buffered messages). Exported as a
// distinct sentinel so future callers (e.g. a /agents send slash
// command) can distinguish "queue full" from "no such teammate".
var ErrMailboxFull = errors.New("teammate mailbox full")

func (m MessageTeammate) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if m.roster == nil {
		return &tools.Result{Output: "MessageTeammate unavailable: no Roster wired in this session", IsError: true}, nil
	}
	operation := "send"
	if raw, ok := in["operation"]; ok {
		text, ok := raw.(string)
		if !ok || (text != "send" && text != "status") {
			return &tools.Result{Output: "operation must be send or status", IsError: true}, nil
		}
		operation = text
	}
	if operation == "status" {
		return m.executeStatus(ctx, in)
	}
	if _, ok := in["message_id"]; ok {
		return &tools.Result{Output: "message_id is only valid with operation=status", IsError: true}, nil
	}
	to, _ := in["to"].(string)
	body, _ := in["body"].(string)
	to = strings.TrimSpace(to)
	if to == "" {
		return &tools.Result{Output: "`to` (recipient teammate name) is required", IsError: true}, nil
	}
	if strings.TrimSpace(body) == "" {
		return &tools.Result{Output: "`body` is required and cannot be empty", IsError: true}, nil
	}

	// Resolve sender identity from context. Sub-agents spawned with
	// Agent({name: "alice"}) have their name stamped into ctx by
	// agent.go's Execute; the top-level loop falls back to "main".
	// Fixes the previous hardcoded `from := "main"` that made every
	// peer-to-peer chain show "main → bob".
	from := agent.AgentNameFromContext(ctx)
	if store, scope := m.roster.HandoffBinding(); store != nil && scope != "" {
		return m.executeDurable(ctx, in, store, scope, to, body, from)
	}
	if strings.HasPrefix(to, "job:") {
		return &tools.Result{Output: "persistent job handoff unavailable: durable mailbox is not configured", IsError: true}, nil
	}

	// Broadcast: to=="*" delivers to every named teammate in the roster.
	if to == "*" {
		return m.executeBroadcast(body, from)
	}

	t, ok := m.roster.Lookup(to)
	if !ok {
		// Check by AgentID too — sometimes the model addresses by id
		// after a SubAgentList; treat both as same-thing-different-name.
		t, ok = m.roster.LookupByAgentID(to)
	}
	if !ok {
		// Last resort: check recentlyFinished by name. Production
		// signal that the recipient *did* exist but exited recently
		// is much more actionable than "maybe never existed" — the
		// 2026-05-16 longrun produced 12 messages addressed to qa/ui
		// after those teammates SubAgentStopped, and the model kept
		// retrying because it had no way to know they were gone.
		if finished, since, found := m.roster.LookupRecentlyFinished(to); found {
			return &tools.Result{
				Output: fmt.Sprintf(
					"%q finished %s ago and is no longer reachable. Use SubAgentList to see active teammates, or Agent({name: %q, ...}) to spawn a fresh one.",
					finished.Name, agent.HumanizeDuration(since), to,
				),
				IsError: true,
			}, nil
		}
		return &tools.Result{
			Output: fmt.Sprintf(
				"no teammate with name=%s (or agent_id=%s) — it may have finished and been evicted from the recently-finished buffer, or never existed. Use SubAgentList to see active teammates.",
				to, to,
			),
			IsError: true,
		}, nil
	}
	// ID lookup intentionally includes finished generations for output readers.
	// They are not live peer-mailbox recipients, even when the old channel is
	// still buffered and could otherwise accept an unread message.
	if snap := t.Snapshot(); !snap.Status.IsActive() {
		return &tools.Result{
			Output:  fmt.Sprintf("%q already finished and is no longer reachable; use Agent({resume_from: %q, prompt: ...}) to continue it through its parent", snap.Name, snap.AgentID),
			IsError: true,
		}, nil
	}

	// 2026-05-16: hard refuse anonymous sub-agents at the type level.
	// Anonymous teammates have Mailbox == nil (set in Roster.Register)
	// to enforce claude-code's Sub-Agent paradigm constraint that
	// sub-agents must not communicate. Pre-fix the refusal lived in
	// the tool description text — a determined model could still try.
	if t.Mailbox == nil {
		return &tools.Result{
			Output: fmt.Sprintf(
				"%q is an anonymous sub-agent — anonymous sub-agents cannot receive peer messages by design (Sub-Agent paradigm: isolation only). Use Agent({name: \"...\"}) to spawn a named teammate if you need bidirectional communication.",
				t.Name,
			),
			IsError: true,
		}, nil
	}

	select {
	case t.Mailbox <- agent.PeerMessage{From: from, Body: body, Sent: time.Now()}:
		return &tools.Result{
			Output: fmt.Sprintf("message queued in live mailbox for %s (agent_id=%s); recipient acknowledgement is unavailable in this legacy session", t.Name, t.AgentID),
		}, nil
	default:
		// Mailbox is buffered (size 16); a full mailbox means the
		// recipient is slow at draining. Returning IsError lets the
		// sender's model decide to back off + retry, vs. silently
		// dropping (claude-code's behavior is similar: full queue → error).
		return &tools.Result{
			Output: fmt.Sprintf(
				"%s: %q has 16 messages queued and isn't draining. Wait for its next turn and retry.",
				ErrMailboxFull.Error(), t.Name,
			),
			IsError: true,
		}, nil
	}
}

// executeBroadcast fans out body to every named (non-anonymous) teammate
// currently in the Roster. Non-blocking send per mailbox: full mailboxes
// are reported in the result rather than silently dropped or blocking.
// Mirrors claude-code's SendMessage({to: "*"}) semantics.
func (m MessageTeammate) executeBroadcast(body, from string) (*tools.Result, error) {
	named := m.roster.ListNamed()
	if len(named) == 0 {
		return &tools.Result{Output: "broadcast: no named teammates in roster"}, nil
	}
	var sent, full []string
	for _, t := range named {
		if t.Mailbox == nil {
			continue
		}
		msg := agent.PeerMessage{From: from, Body: body, Sent: time.Now()}
		select {
		case t.Mailbox <- msg:
			sent = append(sent, t.Name)
		default:
			full = append(full, t.Name)
		}
	}
	var parts []string
	if len(sent) > 0 {
		parts = append(parts, fmt.Sprintf("queued in live mailboxes (not acknowledged): %s", strings.Join(sent, ", ")))
	}
	if len(full) > 0 {
		parts = append(parts, fmt.Sprintf("mailbox full (retry): %s", strings.Join(full, ", ")))
	}
	return &tools.Result{Output: "broadcast " + strings.Join(parts, "; ")}, nil
}

func (m MessageTeammate) executeDurable(ctx context.Context, in map[string]any, store *handoff.Store, scope, to, body, from string) (*tools.Result, error) {
	key, _ := in["idempotency_key"].(string)
	if key == "" {
		key = tools.InvocationIDFromContext(ctx)
	}
	var task *handoff.Task
	if raw, ok := in["task"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err != nil {
			return &tools.Result{Output: "invalid task contract: " + err.Error(), IsError: true}, nil
		}
		task = &handoff.Task{}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(task); err != nil {
			return &tools.Result{Output: "invalid task contract: " + err.Error(), IsError: true}, nil
		}
	}
	recipients := []string{to}
	if to == "*" {
		recipients = nil
		for _, t := range m.roster.ListNamed() {
			recipients = append(recipients, t.Name)
		}
		if len(recipients) == 0 {
			return &tools.Result{Output: "broadcast: no named teammates in roster"}, nil
		}
	}
	rows := make([]map[string]any, 0, len(recipients))
	var failures []string
	for _, recipient := range recipients {
		targetScope := scope
		senderScope := scope
		if strings.HasPrefix(from, "job:") {
			senderScope = "jobs"
		}
		if strings.HasPrefix(recipient, "job:") {
			if strings.TrimPrefix(recipient, "job:") == "" {
				failures = append(failures, "job ID is required")
				continue
			}
			targetScope = "jobs"
			if err := m.roster.ValidateHandoffRecipient(ctx, recipient); err != nil {
				failures = append(failures, err.Error())
				continue
			}
		} else {
			t, ok := m.roster.Lookup(recipient)
			if !ok {
				t, ok = m.roster.LookupByAgentID(recipient)
			}
			if ok {
				if t.Anonymous || t.Mailbox == nil {
					failures = append(failures, fmt.Sprintf("%q is an anonymous sub-agent and cannot receive peer messages", t.Name))
					continue
				}
				recipient = t.Name
			} else if strings.HasPrefix(recipient, "agt-") {
				failures = append(failures, "unknown agent ID; use a stable named teammate or job:<ID>")
				continue
			}
		}
		r, duplicate, err := store.Send(ctx, handoff.SendRequest{Scope: targetScope, SenderScope: senderScope, From: from, To: recipient, Body: body, Task: task, IdempotencyKey: key})
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		status := "durably queued; awaiting recipient transcript acknowledgement"
		if r.State == "acknowledged" {
			status = "already saved and acknowledged by recipient; requested work may still be pending"
		}
		row := messageTeammateReceipt(r)
		row["duplicate"] = duplicate
		row["message"] = status
		rows = append(rows, row)
	}
	var output any
	if to != "*" && len(rows) == 1 && len(failures) == 0 {
		output = rows[0]
	} else {
		output = map[string]any{"messages": rows, "errors": failures}
	}
	b, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: string(b), IsError: len(failures) > 0}, nil
}

func (m MessageTeammate) executeStatus(ctx context.Context, in map[string]any) (*tools.Result, error) {
	for k := range in {
		if k != "operation" && k != "message_id" {
			return &tools.Result{Output: "status accepts only operation and message_id", IsError: true}, nil
		}
	}
	id, ok := in["message_id"].(string)
	if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id {
		return &tools.Result{Output: "message_id is required for status", IsError: true}, nil
	}
	store, scope := m.roster.HandoffBinding()
	if store == nil || scope == "" {
		return &tools.Result{Output: "durable handoff status is unavailable in this session", IsError: true}, nil
	}
	from := agent.AgentNameFromContext(ctx)
	senderScope := scope
	if strings.HasPrefix(from, "job:") {
		senderScope = "jobs"
	}
	rec, err := store.Get(ctx, id)
	if err != nil {
		return &tools.Result{Output: "handoff receipt is unavailable for this sender", IsError: true}, nil
	}
	// The recipient is deliberately not an alternate authorization path: knowing
	// a message ID, recipient name, or shared jobs scope never grants sender access.
	owned := rec.From == from && rec.SenderScope == senderScope
	// Legacy envelopes lack sender_scope. Only their own session namespace (or a
	// durable, host-stamped job identity) can disambiguate the original sender.
	if rec.SenderScope == "" && rec.From == from && rec.Scope == senderScope && (senderScope != "jobs" || strings.HasPrefix(from, "job:")) {
		owned = true
	}
	if !owned {
		return &tools.Result{Output: "handoff receipt is unavailable for this sender", IsError: true}, nil
	}
	b, err := json.Marshal(messageTeammateReceipt(rec))
	if err != nil {
		return nil, err
	}
	return &tools.Result{Output: string(b)}, nil
}

// messageTeammateReceipt is the model-visible boundary shared by status and
// idempotent sends. It never serializes the Record, claim tokens, consumers,
// workspace paths, host commands or their raw stdout/stderr.
func messageTeammateReceipt(r handoff.Record) map[string]any {
	message := "durably queued; awaiting recipient transcript acknowledgement"
	if r.State == "acknowledged" {
		message = "already saved and acknowledged by recipient; requested work may still be pending"
	}
	row := map[string]any{"id": r.ID, "scope": r.Scope, "sender_scope": r.SenderScope, "from": r.From, "to": r.To, "state": r.State, "acknowledged": r.State == "acknowledged", "checkpoint_sha256": r.CheckpointSHA256, "message": message, "business": nil}
	if r.Admission != nil {
		row["admission"] = r.Admission.SafeStatus()
	}
	b := r.Business
	if b == nil {
		return row
	}
	a := b.Assignment
	business := map[string]any{"state": b.State, "source_revision": b.SourceRevision, "assignment": map[string]any{"job_id": a.JobID, "run_id": a.RunID, "work_item_id": a.WorkItemID, "verification_item_id": a.VerificationItemID, "config_sha256": a.ConfigSHA256}, "accepted_at": b.AcceptedAt, "updated_at": b.UpdatedAt}
	if !b.StartedAt.IsZero() {
		business["started_at"] = b.StartedAt
	}
	if !b.CompletedAt.IsZero() {
		business["completed_at"] = b.CompletedAt
	}
	protected := []string{a.Workspace, r.Token, r.Consumer}
	if b.Acceptance != nil {
		for _, check := range b.Acceptance.Checks {
			protected = append(protected, check.Command)
		}
	}
	clean := func(v string) string { return messageTeammateSafeText(v, protected...) }
	if b.Output != "" {
		business["output"] = clean(b.Output)
		business["output_sha256"] = b.OutputSHA256
		business["output_redacted"] = clean(b.Output) != b.Output
	}
	if len(b.Artifacts) > 0 {
		business["artifacts"] = b.Artifacts
	}
	if b.Acceptance != nil {
		a := b.Acceptance
		checks := make([]map[string]any, 0, len(a.Checks))
		for i, c := range a.Checks {
			checks = append(checks, map[string]any{"index": i, "exit_code": c.ExitCode, "output_sha256": c.OutputSHA256})
		}
		business["acceptance"] = map[string]any{"passed": a.Passed, "source": a.Source, "evidence_sha256": a.EvidenceSHA256, "summary": clean(a.Summary), "checks": checks}
	}
	if b.Failure != nil {
		business["failure"] = map[string]any{"code": clean(b.Failure.Code), "summary": clean(b.Failure.Summary), "evidence_sha256": b.Failure.EvidenceSHA256}
	}
	row["business"] = business
	if b.State == handoff.BusinessCompleted {
		row["message"] = "requested work completed with independent host acceptance; inspect business for verified output and artifact hashes"
	}
	return row
}

var messageTeammatePrivatePathRE = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s"'<>]+`)

func messageTeammateSafeText(v string, protected ...string) string {
	v = security.RedactValues(v, protected...)
	v = security.RedactSubprocessText(v)
	return messageTeammatePrivatePathRE.ReplaceAllString(v, "[PRIVATE_PATH]")
}
