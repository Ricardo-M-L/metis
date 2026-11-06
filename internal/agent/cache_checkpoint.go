package agent

import (
	"encoding/json"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const dynamicCheckpointHint = "metis.dynamic_context"

// checkpointDynamicContextLocked appends privileged snapshots only on an exact
// transport opt-in. History is the source of truth, including after resume or
// compaction; no out-of-band 'last snapshot' can suppress reconstruction.
func (l *Loop) checkpointDynamicContextLocked(sections []llm.SystemSection) []llm.SystemSection {
	capability, ok := l.Provider.(interface{ DynamicContextCheckpoints() bool })
	if !ok || !capability.DynamicContextCheckpoints() || len(sections) == 0 {
		return sections
	}
	messages, stable := projectCheckpointSnapshot(l.Messages, sections)
	l.Messages = messages
	return stable
}

// projectCheckpointSnapshot simulates the append-only local request checkpoint.
// Both request assembly and preflight use this pure projection; it never edits
// the input history, even when its backing array has spare capacity.
func projectCheckpointSnapshot(messages []llm.Message, sections []llm.SystemSection) ([]llm.Message, []llm.SystemSection) {
	stable := make([]llm.SystemSection, 0, len(sections))
	var dynamic strings.Builder
	var dynamicSections []llm.SystemSection
	for _, section := range sections {
		if section.Volatile || section.Name == "memory_index" || section.Name == "runtime_state" {
			if section.Body != "" {
				dynamicSections = append(dynamicSections, section)
				dynamic.WriteString("\n\n[" + section.Name + "]\n" + section.Body)
			}
		} else {
			stable = append(stable, section)
		}
	}
	// Always emit the complete current snapshot, including an empty replacement,
	// so disabling plan mode/removing memory does not leave stale policy active.
	body := "<dynamic_context_snapshot>\nThe following is the complete current runtime context. It supersedes earlier dynamic_context_snapshot checkpoints; absent sections are no longer active." + dynamic.String() + "\n</dynamic_context_snapshot>"
	previous := ""
scanPrevious:
	for i := len(messages) - 1; i >= 0; i-- {
		for _, block := range messages[i].Content {
			// A managed continuation after the last local checkpoint marks a
			// mode boundary. Re-establish a complete local snapshot even when
			// its bytes are unchanged, so returning to managed mode can reset
			// the chain without rewriting durable history.
			if block.Type == "provider_state" {
				break scanPrevious
			}
			if messages[i].Role == llm.RoleSystem && block.ProviderHint[dynamicCheckpointHint] == "checkpoint" {
				previous = block.Text
				break
			}
		}
		if previous != "" {
			break
		}
	}
	if previous != body {
		metadata, _ := json.Marshal(dynamicSections)
		messages = append(append([]llm.Message(nil), messages...), llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: body, ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint", dynamicCheckpointHint + ".sections": string(metadata)}}}})
	}
	// Keep an explicit stable section even for a dynamic-only prompt so adapters
	// do not fall back to the legacy flattened System containing dynamic bytes.
	if len(stable) == 0 {
		stable = append(stable, llm.SystemSection{Name: "base", Body: ""})
	}
	return messages, stable
}

// projectDynamicCheckpoints projects only Metis-owned runtime checkpoints out
// of requests to transports without ordered privileged tail messages. The live
// System/SystemSections already contain the complete authoritative snapshot.
// Never rewrite durable history (which would invalidate a local replay prefix),
// downgrade safety policy to user content, or strip unrelated system messages.
// This is a pure projection: it allocates filtered slices without modifying
// messages or blocks, so estimators may also call it directly on live history.
// Actual requests still need a deep snapshot so retained blocks cannot alias history.
func (l *Loop) projectDynamicCheckpoints(messages []llm.Message) []llm.Message {
	if capability, ok := l.Provider.(interface{ DynamicContextCheckpoints() bool }); ok && capability.DynamicContextCheckpoints() {
		return messages
	}
	// A local checkpoint newer than a continuation token means local replay
	// happened since that managed chain. Do not resume an obsolete server-side
	// prefix; rebuild once, then accept the new managed continuation normally.
	lastCheckpoint := -1
	for i, message := range messages {
		for _, block := range message.Content {
			if message.Role == llm.RoleSystem && block.ProviderHint[dynamicCheckpointHint] == "checkpoint" {
				lastCheckpoint = i
			}
		}
	}
	projected := make([]llm.Message, 0, len(messages))
	for i, message := range messages {
		if message.Role == llm.RoleSystem || i < lastCheckpoint {
			blocks := make([]llm.ContentBlock, 0, len(message.Content))
			for _, block := range message.Content {
				if block.ProviderHint[dynamicCheckpointHint] != "checkpoint" && !(i < lastCheckpoint && block.Type == "provider_state") {
					blocks = append(blocks, block)
				}
			}
			if len(blocks) == 0 {
				continue
			}
			message.Content = blocks
		}
		projected = append(projected, message)
	}
	return projected
}
