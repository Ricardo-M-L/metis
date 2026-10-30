package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// SubAgentFollowup is a durably accepted parent instruction. Its ID survives a
// worker exit and distinguishes delivery from a later instruction with the same
// text. Acknowledgement means it entered checkpointed model history.
type SubAgentFollowup struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// UserText preserves a delivery ID inside the model-visible user message, so
// recovery can recognize a checkpoint saved just before its acknowledgement.
func (f SubAgentFollowup) UserText() string {
	return "[parent follow-up " + f.ID + "]\n" + f.Text
}

type subAgentFollowupAck struct {
	ID string `json:"id"`
}

func (f SubAgentFollowup) validate() error {
	if !ValidSubAgentID(f.ID) || strings.TrimSpace(f.Text) == "" {
		return fmt.Errorf("invalid parent follow-up")
	}
	return nil
}

// EnqueueFollowup returns only after the parent instruction is persisted. The
// caller must separately deliver UserText to the child; a crashed or finishing
// child leaves it pending for the next runner that acquires the producer lease.
func (t *SubAgentTranscript) EnqueueFollowup(text string) (SubAgentFollowup, error) {
	if t == nil {
		return SubAgentFollowup{}, fmt.Errorf("durable subagent transcript unavailable")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return SubAgentFollowup{}, fmt.Errorf("parent follow-up cannot be empty")
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return SubAgentFollowup{}, fmt.Errorf("create parent follow-up ID: %w", err)
	}
	followup := SubAgentFollowup{ID: "pfu-" + hex.EncodeToString(entropy[:]), Text: text}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return SubAgentFollowup{}, fmt.Errorf("subagent transcript is closed")
	}
	if err := t.writeEntryLocked(subagentEntry{Type: "followup", Followup: &followup}); err != nil {
		return SubAgentFollowup{}, err
	}
	if err := t.syncLocked(); err != nil {
		return SubAgentFollowup{}, err
	}
	t.pendingFollowups = append(t.pendingFollowups, followup)
	return followup, nil
}

// PendingFollowups returns an ordered snapshot without exposing writer state.
func (t *SubAgentTranscript) PendingFollowups() []SubAgentFollowup {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]SubAgentFollowup{}, t.pendingFollowups...)
}

func subAgentFollowupConsumed(history []llm.Message, followup SubAgentFollowup) bool {
	want := followup.UserText()
	for _, message := range history {
		if message.Role != llm.RoleUser {
			continue
		}
		var text strings.Builder
		for _, block := range message.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		got := text.String()
		if got == want {
			return true
		}
		// Loop combines concurrent steers in one explicitly marked user
		// message. Require complete payload and line boundaries; a marker in
		// assistant/tool output or a partial parent instruction is not delivery.
		if steering, ok := strings.CutPrefix(got, "[user steer mid-turn] "); ok &&
			strings.Contains("\n"+steering+"\n", "\n"+want+"\n") {
			return true
		}
	}
	return false
}

func pendingSubAgentFollowups(history []llm.Message, pending []SubAgentFollowup) []SubAgentFollowup {
	remaining := make([]SubAgentFollowup, 0, len(pending))
	for _, followup := range pending {
		if !subAgentFollowupConsumed(history, followup) {
			remaining = append(remaining, followup)
		}
	}
	return remaining
}

type subAgentFollowupReplay struct {
	seen    map[string]string
	pending []SubAgentFollowup
}

func (r *subAgentFollowupReplay) enqueue(followup *SubAgentFollowup) error {
	if followup == nil {
		return fmt.Errorf("missing parent follow-up")
	}
	if err := followup.validate(); err != nil {
		return err
	}
	if r.seen == nil {
		r.seen = make(map[string]string)
	}
	if previous, exists := r.seen[followup.ID]; exists {
		if previous != followup.Text {
			return fmt.Errorf("parent follow-up ID reused with different text")
		}
		return nil
	}
	r.seen[followup.ID] = followup.Text
	r.pending = append(r.pending, *followup)
	return nil
}

func (r *subAgentFollowupReplay) acknowledge(ack *subAgentFollowupAck) error {
	if ack == nil {
		return fmt.Errorf("missing parent follow-up acknowledgement")
	}
	if _, exists := r.seen[ack.ID]; !exists {
		return fmt.Errorf("unknown parent follow-up acknowledgement")
	}
	for i, followup := range r.pending {
		if followup.ID == ack.ID {
			r.pending = append(r.pending[:i], r.pending[i+1:]...)
			break
		}
	}
	return nil
}
