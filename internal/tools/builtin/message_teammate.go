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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
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
	return "Send a message to another named sub-agent (teammate) in the current session. The recipient will see the message as a <peer_message> system-reminder on its next turn boundary. Only works for named teammates spawned via Agent({name: \"...\"}); anonymous sub-agents cannot receive peer messages (they have no addressable name). Different from SendMessage (which posts to chat platforms like Slack)."
}
func (MessageTeammate) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"to", "body"},
		"properties": map[string]any{
			"to": map[string]any{
				"type":        "string",
				"description": "The recipient teammate's name as passed to Agent({name: \"...\"}). Use SubAgentList to see available names. Pass \"*\" to broadcast to all named teammates currently in the roster.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "The message text. Plain string — the recipient sees it inside <peer_message> tags so it knows this isn't from the human operator.",
			},
		},
	}
}

// Concurrency is Safe — the mailbox push is a single non-blocking
// channel send guarded by Roster's RLock; multiple MessageTeammate
// calls in the same dispatch batch (e.g. broadcasting to two peers)
// can fan out without serialization.
func (MessageTeammate) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}

func (m MessageTeammate) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := m.gate.Check(context.Background(), "MessageTeammate", strFromAny(in["body"]))
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
			Output: fmt.Sprintf("message delivered to %s (agent_id=%s)", t.Name, t.AgentID),
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
		parts = append(parts, fmt.Sprintf("delivered to: %s", strings.Join(sent, ", ")))
	}
	if len(full) > 0 {
		parts = append(parts, fmt.Sprintf("mailbox full (retry): %s", strings.Join(full, ", ")))
	}
	return &tools.Result{Output: "broadcast " + strings.Join(parts, "; ")}, nil
}
