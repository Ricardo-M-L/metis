package webui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type historyTraceInputSpan struct {
	text    string
	anchors [][]string
	closed  bool
}

// restoreMissingTraceUserInputs augments display only. It never mutates live
// rows, invents timestamps, renumbers turns, or assumes transcript turn counts
// still equal trace turns after compaction/branching. A request must match a
// unique live turn through assistant/tool/user anchors in monotonic order.
func restoreMissingTraceUserInputs(sid string, nodes []session.TracedNode, messages []llm.Message) []session.TracedNode {
	if len(nodes) == 0 || len(messages) == 0 {
		return nodes
	}
	anchorTurns := make(map[string]map[int]struct{})
	firstNode := make(map[int]int)
	hasUser := make(map[int]bool)
	automaticTurn := make(map[int]bool)
	failedTurn := make(map[int]bool)
	hasContext := make(map[int]bool)
	hasAssistant := make(map[int]bool)
	for i, node := range nodes {
		ev := node.Event
		if node.Depth != 0 || ev.SubAgentOf != "" || ev.Turn <= 0 || (ev.SessionID != "" && ev.SessionID != sid) {
			continue
		}
		if _, ok := firstNode[ev.Turn]; !ok {
			firstNode[ev.Turn] = i
		}
		if ev.Kind == "user" {
			hasUser[ev.Turn] = true
		}
		if ev.Kind == "context" {
			hasContext[ev.Turn] = true
			if ev.Source == "cron" {
				automaticTurn[ev.Turn] = true
			}
		}
		if ev.Kind == "error" {
			failedTurn[ev.Turn] = true
		}
		if ev.Kind == "text" || ev.Kind == "tool_start" {
			hasAssistant[ev.Turn] = true
		}
		var keys []string
		switch ev.Kind {
		case "text", "user":
			keys = traceInputTextKeys(ev.Kind, ev.Text)
		case "tool_start":
			keys = traceInputToolKeys(ev.ToolUseID, ev.ToolName, ev.Text)
		}
		for _, key := range keys {
			if anchorTurns[key] == nil {
				anchorTurns[key] = make(map[int]struct{})
			}
			anchorTurns[key][ev.Turn] = struct{}{}
		}
	}

	var spans []historyTraceInputSpan
	promptCounts := make(map[string]int)
	for _, message := range messages {
		if text := historyTraceUserInput(message); text != "" {
			spans = append(spans, historyTraceInputSpan{text: text})
			promptCounts[text]++
		}
		if message.Role == llm.RoleUser && len(spans) > 0 && len(spans[len(spans)-1].anchors) > 0 {
			if source, _ := historyTraceContext(message); source != "" {
				// Runtime notifications can wake another execution turn after a
				// response. Their later answers do not relocate the original
				// human request to that automatic continuation.
				spans[len(spans)-1].closed = true
			}
		}
		if message.Role != llm.RoleAssistant || len(spans) == 0 {
			continue
		}
		span := &spans[len(spans)-1]
		if span.closed {
			continue
		}
		for _, block := range message.Content {
			switch block.Type {
			case "text":
				if keys := traceInputTextKeys("text", block.Text); len(keys) > 0 {
					span.anchors = append(span.anchors, keys)
				}
			case "tool_use":
				if block.ToolUseID != "" {
					input, _ := json.Marshal(agent.PresentationToolInput(block.ToolInput))
					span.anchors = append(span.anchors, traceInputToolKeys(block.ToolUseID, block.ToolName, string(input)))
				}
			}
		}
	}

	candidates := make([]map[int]struct{}, len(spans))
	for i, span := range spans {
		// Repeated prompts (e.g. "continue") are not reliable anchors to a
		// later USER row: only their assistant/tool evidence may locate them.
		if promptCounts[span.text] == 1 {
			span.anchors = append(span.anchors, traceInputTextKeys("user", span.text))
		}
		for _, keys := range span.anchors {
			matches := make(map[int]struct{})
			for _, key := range keys {
				for turn := range anchorTurns[key] {
					matches[turn] = struct{}{}
				}
			}
			if len(matches) == 0 {
				continue // an older/partial trace may omit this specific anchor
			}
			if candidates[i] == nil {
				candidates[i] = matches
				continue
			}
			for turn := range candidates[i] {
				if _, ok := matches[turn]; !ok {
					delete(candidates[i], turn)
				}
			}
		}
	}
	// Bound ambiguous matches by their verified neighbors. Conflicting unique
	// anchors indicate a branched/reordered history; decline that reconstruction.
	for pass := 0; pass < len(spans); pass++ {
		changed, previous := false, 0
		for _, turns := range candidates {
			if turn := uniqueTraceInputTurn(turns); turn != 0 {
				if turn <= previous {
					return nodes
				}
				previous = turn
			}
		}
		for i, turns := range candidates {
			if len(turns) <= 1 {
				continue
			}
			lower, upper := 0, int(^uint(0)>>1)
			for j := i - 1; j >= 0; j-- {
				if turn := uniqueTraceInputTurn(candidates[j]); turn != 0 {
					lower = turn
					break
				}
			}
			for j := i + 1; j < len(candidates); j++ {
				if turn := uniqueTraceInputTurn(candidates[j]); turn != 0 {
					upper = turn
					break
				}
			}
			for turn := range turns {
				if turn <= lower || turn >= upper {
					delete(turns, turn)
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	// A provider error may leave no assistant/tool anchor at all. Recover only
	// the single unclaimed failed turn between two immediately adjacent,
	// independently verified requests. Never interpolate ordinary missing turns
	// or a span whose anchors contradicted one another (a non-nil empty set).
	for i := 1; i+1 < len(candidates); i++ {
		if candidates[i] != nil {
			continue
		}
		lower, upper := uniqueTraceInputTurn(candidates[i-1]), uniqueTraceInputTurn(candidates[i+1])
		turn := lower + 1
		if lower != 0 && upper == turn+1 && failedTurn[turn] && !hasAssistant[turn] && !hasContext[turn] && !hasUser[turn] {
			candidates[i] = map[int]struct{}{turn: {}}
		}
	}
	insertions := make(map[int]session.TracedNode)
	for i, turns := range candidates {
		turn := uniqueTraceInputTurn(turns)
		if turn == 0 || hasUser[turn] || automaticTurn[turn] {
			continue
		}
		insertions[firstNode[turn]] = session.TracedNode{Event: session.TraceEvent{
			ID: fmt.Sprintf("history-user-%s-%d", sid, turn), SessionID: sid,
			Turn: turn, Kind: "user", Source: "history-reconstructed", Text: spans[i].text,
		}}
	}
	if len(insertions) == 0 {
		return nodes
	}
	out := make([]session.TracedNode, 0, len(nodes)+len(insertions))
	for i, node := range nodes {
		if recovered, ok := insertions[i]; ok {
			out = append(out, recovered)
		}
		out = append(out, node)
	}
	return out
}

func traceInputToolKeys(id, name, input string) []string {
	if id == "" || strings.TrimSpace(input) == "" {
		return nil
	}
	input = security.RedactSubprocessText(input)
	var value any
	if json.Unmarshal([]byte(input), &value) == nil {
		if value == nil {
			value = map[string]any{}
		}
		canonical, _ := json.Marshal(value)
		input = string(canonical)
	}
	return traceInputTextKeys("tool\x00"+id+"\x00"+name, input)
}

func uniqueTraceInputTurn(turns map[int]struct{}) int {
	if len(turns) == 1 {
		for turn := range turns {
			return turn
		}
	}
	return 0
}

func traceInputTextKeys(kind, text string) []string {
	text = strings.TrimSpace(security.RedactSubprocessText(text))
	if text == "" {
		return nil
	}
	keys := []string{kind + "\x00" + text}
	// Old runtime traces store at most 2,000 runes per text/USER row. Matching
	// that exact historical representation is allowed; arbitrary prefix/fuzzy
	// matches would map generic progress prose to unrelated turns.
	if runes := []rune(text); len(runes) > 2000 {
		keys = append(keys, kind+"\x00"+string(runes[:2000])+"...(truncated)")
	}
	return keys
}

// historyTraceUserInput extracts presentation text from a genuine user-shaped
// message. Runtime envelopes, tool-result payloads and in-turn steering do not
// open new human turns. Legacy text without provenance is considered only when
// restoreMissingTraceUserInputs can independently align its assistant evidence.
var historyTraceRuntimeSections = regexp.MustCompile(`(?is)<subdirectory_hints(?:\s[^>]*)?>.*?</subdirectory_hints\s*>|<project_context(?:\s[^>]*)?>.*?</project_context\s*>|<session_context(?:\s[^>]*)?>.*?</session_context\s*>`)

func historyTraceUserInput(message llm.Message) string {
	if message.Role != llm.RoleUser {
		return ""
	}
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			return ""
		}
	}
	var parts []string
	for _, block := range message.Content {
		if block.Synthetic {
			continue
		}
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if strings.HasPrefix(text, "[user steer mid-turn]") || strings.HasPrefix(text, "[post-compact hook context]") {
				continue
			}
			text = historyTraceRuntimeSections.ReplaceAllString(text, "")
			if text = transcript.VisibleUserText(text); text != "" {
				parts = append(parts, text)
			}
		case "image", "image_url", "input_image":
			parts = append(parts, "[image attachment]")
		case "document", "file", "input_file":
			parts = append(parts, "[document attachment]")
		case "audio", "input_audio":
			parts = append(parts, "[audio attachment]")
		case "video":
			parts = append(parts, "[video attachment]")
		}
	}
	return security.RedactSubprocessText(strings.Join(parts, "\n"))
}

// historyTraceContext classifies complete runtime-only history messages for
// history fallback. Provider tool results stay their own events; mixed real
// user messages are handled by historyTraceUserInput instead.
func historyTraceContext(message llm.Message) (source, text string) {
	if message.Role != llm.RoleUser || historyTraceUserInput(message) != "" {
		return "", ""
	}
	var parts []string
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			return "", ""
		}
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	if len(parts) == 0 {
		return "", ""
	}
	text = security.RedactSubprocessText(strings.Join(parts, "\n"))
	for _, category := range []struct{ prefix, source string }{
		{"<job_notification", "job"}, {"<peer_message", "peer"}, {"<memory_consolidation_done", "dream"},
		{"<sub_agent_idle", "subagent"}, {"<monitor_event", "monitor"},
		{"[user steer mid-turn]", "steer"}, {"[post-compact hook context]", "hook"},
	} {
		if strings.HasPrefix(text, category.prefix) {
			return category.source, text
		}
	}
	return "context", text
}
