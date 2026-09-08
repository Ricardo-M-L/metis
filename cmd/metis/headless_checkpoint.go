package main

import (
	"errors"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// headlessCheckpoint owns one cursor for the entire invocation, including
// compaction and schema-correction Runs. Save may only be called before the
// producer starts or after it has joined; event consumers must not read live
// loop history at an EventTurnEnd/EventLoopDone notification.
type headlessCheckpoint struct {
	loop                       *agent.Loop
	store                      *session.Store
	id                         string
	cursor                     session.HistoryCursor
	modelPrompt, displayPrompt string
	// Auto-compaction treats ordinary summary errors as recoverable. A failed
	// persistence boundary is not: keep it until this invocation returns even
	// when a later best-effort final save can repair the disk state.
	compactionErr error
}

func newHeadlessCheckpoint(loop *agent.Loop, store *session.Store, id string) *headlessCheckpoint {
	c := &headlessCheckpoint{
		loop: loop, store: store, id: id,
		cursor: session.NewHistoryCursor(loop.History()),
	}
	if store != nil && id != "" {
		loop.HistoryCheckpoint = c.persist
		loop.CompactionCheckpoint = func(before, after []llm.Message) error {
			err := store.CheckpointCompaction(id, c.persistable(before), c.persistable(after), &c.cursor)
			if err != nil && c.compactionErr == nil {
				c.compactionErr = err
			}
			return err
		}
	}
	return c
}

func (c *headlessCheckpoint) Save() error {
	return c.persist(c.loop.History())
}

// MarkPrompt maps the already-appended raw input to the loop's enriched input.
// Keep that mapping on subsequent replacement/compaction snapshots too: a
// synthetic recall attachment can otherwise move the cursor anchor and leak
// the LLM-only directory hints/schema instruction into the visible transcript.
func (c *headlessCheckpoint) MarkPrompt(modelPrompt, displayPrompt string) {
	c.modelPrompt, c.displayPrompt = modelPrompt, displayPrompt
	c.cursor.Mark(c.persistable(c.loop.History()))
}

func (c *headlessCheckpoint) persistable(history []llm.Message) []llm.Message {
	if c.modelPrompt == "" || c.modelPrompt == c.displayPrompt {
		return history
	}
	var result []llm.Message
	for i, message := range history {
		if message.Role != llm.RoleUser {
			continue
		}
		for j, block := range message.Content {
			if block.Type != "text" || block.Text != c.modelPrompt {
				continue
			}
			if result == nil {
				result = append([]llm.Message(nil), history...)
			}
			result[i].Content = append([]llm.ContentBlock(nil), result[i].Content...)
			result[i].Content[j].Text = c.displayPrompt
		}
	}
	if result != nil {
		return result
	}
	return history
}

func (c *headlessCheckpoint) persist(history []llm.Message) error {
	if c.store == nil || c.id == "" {
		return nil
	}
	return errors.Join(c.compactionErr, c.store.CheckpointHistory(c.id, c.persistable(history), &c.cursor))
}
