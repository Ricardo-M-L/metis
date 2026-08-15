package tui

// chat_items.go — adapters that let internal/tui/list/List render the
// chat surface.
//
// The list package defines `Item.Render(width int) string`; this file
// wraps the existing `tui.Message` and `tui.ToolEvent` value types into
// list-compatible items WITHOUT touching the underlying renderMessage /
// renderToolEvent functions (which means visual_dump_test.go and the
// existing render_message_test/render_tool_test coverage stays valid).
//
// Each adapter reuses the per-message cache (`renderCache.GetMessage` /
// `renderCache.GetTool`) so the cache+virtualization combination
// achieves: (a) viewport-only items rendered, (b) those items hit the
// renderCache so glamour cost is paid once per content change.

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Ricardo-M-L/metis/internal/tui/list"
)

// chatItemGap is the single source of vertical rhythm for the transcript.
// Individual renderers may contain blank lines inside Markdown, diffs, or tool
// output, but top-level timeline items do not manufacture their own outer
// whitespace. This mirrors Claude Code's per-message marginTop={1}: one blank
// row separates messages while a tool's leader/result rows stay one block.
const chatItemGap = 1

func normalizeChatItemBoundary(rendered string) string {
	return strings.Trim(rendered, "\n")
}

// messageItem adapts a Message into list.Item.
//
// We carry the Message by value, not pointer: the list package walks
// items by index for both Render and AtBottom, and a stable-by-value
// snapshot avoids any cross-frame mutation surprises (the renderCache
// keys on (role, content, width), so a stale snapshot is harmless —
// the next View() rebuild creates a fresh messageItem reflecting the
// updated Message anyway).
type messageItem struct {
	msg    Message
	expand bool
	plain  bool
	cache  *renderCache
}

// Render implements list.Item. Cache-aware: hit returns the stored
// string verbatim; miss runs renderMessage, instruments cost via
// RecordRender (slow-render log + rolling avg), then stores the result.
//
// expand is captured at item-construction time (in buildChatItems) and
// keys into the cache via PutMessage/GetMessage — so toggling Ctrl+O
// invalidates cached folded thinking blocks and forces a re-render.
func (i *messageItem) Render(width int) string {
	if i.plain {
		return normalizeChatItemBoundary(renderMessagePlain(i.msg, width, i.expand))
	}
	if i.cache != nil {
		if cached, ok := i.cache.GetMessage(i.msg, width, i.expand); ok {
			return normalizeChatItemBoundary(cached)
		}
	}
	t0 := time.Now()
	rendered := renderMessage(i.msg, width, i.expand)
	if i.cache != nil {
		i.cache.RecordRender(i.msg.Role, len(i.msg.Content), time.Since(t0))
		i.cache.PutMessage(i.msg, width, i.expand, rendered)
	}
	return normalizeChatItemBoundary(rendered)
}

// compactToolEventItem preserves the normal tool header + result summary but
// removes all preview/error bodies. This is the streamlined/minimal rendering
// contract: execution remains visible and auditable without filling the
// transcript with command output or diffs.
type compactToolEventItem struct {
	te ToolEvent
}

func (i *compactToolEventItem) Render(width int) string {
	_ = width
	full := strings.TrimRight(renderToolEvent(i.te, false), "\n")
	if full == "" {
		return ""
	}
	lines := strings.Split(full, "\n")
	limit := 2
	if i.te.Kind == "start" {
		limit = 1
	}
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return normalizeChatItemBoundary(strings.Join(lines, "\n"))
}

// toolEventItem adapts a ToolEvent into list.Item.
//
// Like messageItem, carries the ToolEvent by value. expand is captured
// at item-construction time (in buildChatItems) — if the user toggles
// Ctrl+O between frames, buildChatItems is called again with the new
// flag and rebuilds the items with the new expand baked in.
type toolEventItem struct {
	te     ToolEvent
	expand bool
	cache  *renderCache
}

// explorationGroupItem is Claude Code's collapsed Read/Search cluster. The
// underlying tools remain one event each (and still execute/audit normally),
// but consecutive successful Read/Grep/Glob/LS rows render as one compact
// count summary. Ctrl+O uses the same item to reveal every original row.
//
// 2026-07-27 grouping tweak (user feedback "一堆省略号"): the previous
// groupableExplorationEvent filter rejected any event with a non-empty
// SubAgentParentID, so a fan-out of 5 sub-agents × 3 greps each produced
// 15 un-grouped rows of "⏺ grep …" + "(ctrl+O to expand)" — visually a
// wall of ellipses. We now track SubAgentParentID on the group and let
// same-parent sub-agent events cluster together; the rendered block is
// indented when the whole group came from a child agent, so the
// transcript still reads as a tree.
type explorationGroupItem struct {
	events []ToolEvent
	expand bool
	// subParent is the shared SubAgentParentID across all events ("" for
	// top-level groups). Stamped at flush time so Render can decide
	// whether to indent without re-deriving it.
	subParent string
}

// recoveredErrorGroupItem is the compact, evidence-preserving state between
// success and failure. It contains only tool errors for which this render pass
// has positive recovery evidence: either the same operation later succeeded
// in the same user turn, or the command printed a strong completion
// marker before the outer timeout fired. Permission/security refusals never
// enter this group. The original ToolEvents remain untouched and Ctrl+O
// expands this item back into their full diagnostic rows.
type recoveredErrorGroupItem struct {
	events       []ToolEvent
	expand       bool
	expandID     string
	subParent    string
	partialCount int
	retriedCount int
}

func (i *recoveredErrorGroupItem) Render(width int) string {
	_ = width
	if i.expand {
		var out strings.Builder
		for _, te := range i.events {
			out.WriteString(renderToolEvent(te, true))
		}
		return normalizeChatItemBoundary(out.String())
	}
	parts := make([]string, 0, 2)
	if i.partialCount > 0 {
		parts = append(parts, fmt.Sprintf("%d %s reported complete before timeout; verify", i.partialCount, pluralN(i.partialCount, "install", "installs")))
	}
	if i.retriedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d intermediate %s recovered", i.retriedCount, pluralN(i.retriedCount, "error", "errors")))
	}

	leadIndent := "  "
	if i.subParent != "" {
		leadIndent = "      "
	}
	var out strings.Builder
	out.WriteString(styleWarn.Render(leadIndent + "↻ "))
	label := "recovered"
	if i.partialCount > 0 && i.retriedCount == 0 {
		label = "partial"
	}
	out.WriteString(styleToolName.Render(label))
	out.WriteString(styleDim.Render(" · "))
	out.WriteString(strings.Join(parts, " · "))
	out.WriteString(styleMuted.Render(" (ctrl+O to inspect)"))
	out.WriteString("\n")
	return normalizeChatItemBoundary(out.String())
}

type recoveredErrorGroupPlan struct {
	anchor       *ToolEvent
	events       []ToolEvent
	expandID     string
	subParent    string
	partialCount int
	retriedCount int
}

// recoveryPlanCacheKey is deliberately cheap to compute: it observes slice
// identity plus the result/error state that EventToolResult mutates in place.
// It does not hash Output bytes; summing their lengths is enough to catch the
// production transition from an in-flight start row to its settled result,
// while avoiding the very full-output scan this cache is meant to remove.
// Slice endpoint pointers also invalidate same-sized session replacements.
type recoveryPlanCacheKey struct {
	messageCount int
	toolCount    int
	resultCount  int
	errorCount   int
	outputBytes  int
	firstMessage *Message
	lastMessage  *Message
	firstTool    *ToolEvent
	lastTool     *ToolEvent
}

func (m *Model) currentRecoveryPlanCacheKey() recoveryPlanCacheKey {
	key := recoveryPlanCacheKey{
		messageCount: len(m.messages),
		toolCount:    len(m.toolEvents),
	}
	if len(m.messages) > 0 {
		key.firstMessage = &m.messages[0]
		key.lastMessage = &m.messages[len(m.messages)-1]
	}
	if len(m.toolEvents) > 0 {
		key.firstTool = &m.toolEvents[0]
		key.lastTool = &m.toolEvents[len(m.toolEvents)-1]
	}
	for idx := range m.toolEvents {
		te := &m.toolEvents[idx]
		if te.Kind == "result" {
			key.resultCount++
		}
		if te.IsError {
			key.errorCount++
		}
		key.outputBytes += len(te.Output)
	}
	return key
}

func (m *Model) cachedRecoveredErrorPlans(merged []timelineItem) map[*ToolEvent]*recoveredErrorGroupPlan {
	key := m.currentRecoveryPlanCacheKey()
	if m.recoveryPlanCacheValid && key == m.recoveryPlanCacheKey {
		return m.recoveryPlanCache
	}
	plans := recoveredErrorPlans(merged)
	m.recoveryPlanCache = plans
	m.recoveryPlanCacheKey = key
	m.recoveryPlanCacheValid = true
	return plans
}

var recoveryDescriptionStopWords = map[string]struct{}{
	"a": {}, "again": {}, "an": {}, "and": {}, "check": {}, "command": {},
	"build": {}, "builds": {}, "data": {}, "dir": {}, "directory": {}, "fetch": {},
	"file": {}, "files": {}, "find": {}, "for": {}, "from": {}, "higher": {}, "install": {},
	"installation": {}, "query": {}, "read": {}, "retry": {}, "run": {}, "search": {},
	"output": {}, "repo": {}, "repository": {}, "request": {}, "requests": {},
	"skill": {}, "skills": {}, "task": {}, "tasks": {}, "test": {}, "tests": {},
	"the": {}, "timeout": {}, "try": {}, "url": {}, "using": {}, "via": {}, "with": {},
}

// importantToolErrorRE marks outputs that must never be swallowed by
// recovered/compacted grouping. `denied:` matches the TUI-shaped deny
// envelope ("denied: newline inside …") emitted by dispatch.go since
// the 2026-08 deny-reason rework; `bash-security` remains for
// historical transcripts resumed from older sessions.
var importantToolErrorRE = regexp.MustCompile(`(?i)(access denied|authentication failed|forbidden|unauthorized|denied by permission|denied by user|permission denied|permission policy|requires approval|security rule|bash-security|operation not permitted|user denied|unsafe command|denied:)`)

func importantToolError(te ToolEvent) bool {
	return te.IsError && importantToolErrorRE.MatchString(te.Output)
}

func recoveryWords(s string) map[string]struct{} {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	})
	out := make(map[string]struct{}, len(words))
	for _, word := range words {
		word = strings.Trim(word, "-_")
		if len([]rune(word)) < 3 {
			continue
		}
		if _, stop := recoveryDescriptionStopWords[word]; stop {
			continue
		}
		out[word] = struct{}{}
	}
	return out
}

func sameRecoveryOperation(failed, succeeded ToolEvent) bool {
	if strings.TrimPrefix(failed.ToolName, "sub: ") != strings.TrimPrefix(succeeded.ToolName, "sub: ") ||
		failed.SubAgentParentID != succeeded.SubAgentParentID {
		return false
	}
	// Dispatcher-style tools reuse one ToolName for distinct operations. A
	// successful Skill get does not recover a failed Skill invoke merely because
	// both name the same skill; the same applies to job/task/session methods.
	for _, key := range []string{"action", "operation", "method", "job_id", "task_id", "session_id", "id"} {
		aValue, aOK := recoveryIdentityValue(failed.Input, key)
		bValue, bOK := recoveryIdentityValue(succeeded.Input, key)
		if !aOK && !bOK {
			continue
		}
		if !aOK || !bOK || !strings.EqualFold(aValue, bValue) {
			return false
		}
	}
	// Concrete targets beat prose intent labels. A path/query/URL mismatch is
	// decisive; an exact command replay is decisive success evidence. Bash retry
	// commands may legitimately differ, so a non-equal command falls through to
	// the description check instead of being treated as a match by itself.
	structuredTarget := false
	for _, key := range []string{"path", "file_path", "pattern", "query", "url", "name"} {
		aValue, _ := failed.Input[key].(string)
		bValue, _ := succeeded.Input[key].(string)
		aValue, bValue = strings.TrimSpace(aValue), strings.TrimSpace(bValue)
		if aValue == "" && bValue == "" {
			continue
		}
		if aValue == "" || bValue == "" || !strings.EqualFold(aValue, bValue) {
			return false
		}
		structuredTarget = true
	}
	if structuredTarget {
		return true
	}
	failedCommand, _ := failed.Input["command"].(string)
	succeededCommand, _ := succeeded.Input["command"].(string)
	if strings.TrimSpace(failedCommand) != "" && strings.EqualFold(strings.TrimSpace(failedCommand), strings.TrimSpace(succeededCommand)) {
		return true
	}
	failedDesc, _ := failed.Input["description"].(string)
	succeededDesc, _ := succeeded.Input["description"].(string)
	aText, bText := strings.TrimSpace(failedDesc), strings.TrimSpace(succeededDesc)
	if aText == "" || bText == "" {
		return false
	}
	a, b := recoveryWords(aText), recoveryWords(bText)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	shared := 0
	for word := range a {
		if _, ok := b[word]; ok {
			shared++
		}
	}
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	if minLen == 1 {
		if shared != 1 {
			return false
		}
		for word := range a {
			if _, ok := b[word]; ok {
				// A single shared generic word is not enough ("Run tests" /
				// "Retry tests"). Accept only an identifier-like token such as
				// IMG_0309 or anti-ui-slop.
				return len([]rune(word)) >= 6 && strings.ContainsAny(word, "0123456789_-")
			}
		}
		return false
	}
	return shared >= 2 && shared*4 >= minLen*3
}

func recoveryIdentityValue(input map[string]any, key string) (string, bool) {
	value, ok := input[key]
	if !ok || value == nil {
		return "", false
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	return text, text != ""
}

// recoveredErrorPlans searches for recovery within one user turn but keeps
// each recovered error at its original timeline position. Assistant
// commentary is not a boundary: models commonly say what they will retry
// between two attempts. We intentionally avoid collecting non-contiguous
// failures into one earlier row because expanding such a group would reorder
// evidence around that commentary.
func recoveredErrorPlans(merged []timelineItem) map[*ToolEvent]*recoveredErrorGroupPlan {
	plans := make(map[*ToolEvent]*recoveredErrorGroupPlan)
	turn := make([]*ToolEvent, 0, 8)
	flush := func() {
		if len(turn) == 0 {
			return
		}
		for idx, te := range turn {
			if !te.IsError || benignReadOnlyNoMatch(*te) {
				continue
			}
			if importantToolError(*te) {
				continue
			}
			partial := completedBeforeTimeoutAfterImportanceCheck(*te)
			recovered := partial
			if !recovered {
				// A bounded lookahead keeps this per-frame render planning O(n)
				// even in pathological agent loops while covering ordinary retry
				// sequences (which are normally adjacent or only a few calls apart).
				end := idx + 1 + 24
				if end > len(turn) {
					end = len(turn)
				}
				for _, later := range turn[idx+1 : end] {
					if later.Kind == "result" && !later.IsError && sameRecoveryOperation(*te, *later) {
						recovered = true
						break
					}
				}
			}
			if recovered {
				plan := &recoveredErrorGroupPlan{
					anchor:       te,
					events:       []ToolEvent{*te},
					expandID:     te.ID,
					subParent:    te.SubAgentParentID,
					partialCount: boolInt(partial),
					retriedCount: boolInt(!partial),
				}
				plans[te] = plan
			}
		}
		turn = turn[:0]
	}

	for _, item := range merged {
		if item.msg != nil && item.msg.Role == "user" {
			flush()
			continue
		}
		if item.te != nil && item.te.Kind == "result" {
			turn = append(turn, item.te)
		}
	}
	flush()
	return plans
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (i *explorationGroupItem) Render(width int) string {
	_ = width
	if i.expand {
		var out strings.Builder
		for _, te := range i.events {
			out.WriteString(renderToolEvent(te, true))
		}
		return normalizeChatItemBoundary(out.String())
	}
	reads, searches, listings := 0, 0, 0
	for _, te := range i.events {
		switch strings.TrimPrefix(te.ToolName, "sub: ") {
		case "Read":
			reads++
		case "Grep":
			searches++
		case "Glob":
			searches++
		case "LS":
			listings++
		}
	}
	parts := make([]string, 0, 3)
	if reads > 0 {
		parts = append(parts, fmt.Sprintf("Read %d %s", reads, pluralN(reads, "file", "files")))
	}
	if searches > 0 {
		parts = append(parts, fmt.Sprintf("Searched %d %s", searches, pluralN(searches, "pattern", "patterns")))
	}
	if listings > 0 {
		parts = append(parts, fmt.Sprintf("Listed %d %s", listings, pluralN(listings, "directory", "directories")))
	}
	// Sub-agent groups render INDENTED under their parent Agent row,
	// mirroring the per-tool sub-agent indentation in render_tool.go's
	// isSub branch. Keeps the tree visual: top-level "explored" is
	// flush-left, sub-agent groups sit at +4.
	isSub := i.subParent != ""
	leadIndent, resultIndent := "  ", "    "
	if isSub {
		leadIndent, resultIndent = "      ", "        "
	}
	var out strings.Builder
	out.WriteString(styleSuccess.Render(leadIndent + glyphBullet + " "))
	out.WriteString(styleToolName.Render("explored"))
	out.WriteString("\n")
	out.WriteString(styleDim.Render(resultIndent + glyphTreeLeaf + "  "))
	out.WriteString(styleAccent.Render("✓ "))
	out.WriteString(strings.Join(parts, " · "))
	out.WriteString(styleMuted.Render(" (ctrl+O to expand)"))
	out.WriteString("\n\n")
	return normalizeChatItemBoundary(out.String())
}

func pluralN(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// groupableExplorationEvent reports whether te can join an exploration
// cluster. Errors and in-flight "start" rows are excluded — they need
// their own visual treatment. Sub-agent events ARE included now (they
// were excluded pre-2026-07-27, causing the un-grouped "wall of dots");
// buildChatItems' flushExploration additionally keys clusters by
// SubAgentParentID so events from different children never mix.
func groupableExplorationEvent(te ToolEvent) bool {
	if te.Kind == "start" || te.IsError {
		return false
	}
	switch strings.TrimPrefix(te.ToolName, "sub: ") {
	case "Read", "Grep", "Glob", "LS":
		return true
	default:
		return false
	}
}

// Render implements list.Item. The width parameter is ignored because
// renderToolEvent doesn't take a width — its lipgloss styling uses
// terminal default. Reserved for future width-aware tool rendering.
func (i *toolEventItem) Render(width int) string {
	_ = width
	if i.cache != nil {
		if cached, ok := i.cache.GetTool(i.te, i.expand, width); ok {
			return normalizeChatItemBoundary(cached)
		}
	}
	t0 := time.Now()
	rendered := renderToolEvent(i.te, i.expand)
	if i.cache != nil {
		i.cache.RecordRender("tool:"+i.te.ToolName, len(i.te.Output), time.Since(t0))
		i.cache.PutTool(i.te, i.expand, width, rendered)
	}
	return normalizeChatItemBoundary(rendered)
}

// staticItem is a list.Item whose render is precomputed once per
// buildChatItems call. It carries transient rows such as spinner status that
// should scroll with the transcript but do not need a dedicated item type.
// Width is intentionally ignored because the string was sized at build time.
type staticItem struct {
	rendered string
}

func (s *staticItem) Render(width int) string {
	_ = width
	return normalizeChatItemBoundary(s.rendered)
}

// inProgressThinkingItem renders the live thinking summary for the
// current turn at the tail of the chat list, so it scrolls with the
// transcript instead of staying pinned above the input (image #12 user
// feedback 2026-05-15: the thinking summary visually matched the
// historical thinking rows in the transcript but didn't follow the
// mouse wheel, causing it to "stick" on screen as the user scrolled).
// Not cached — content updates on every spinner tick.
type inProgressThinkingItem struct {
	text    string
	expand  bool
	width   int           // captured for thinkingHintFits gate; safe to ignore Render arg
	elapsed time.Duration // time since thinking started; 0 = omit from header
}

// thinkingLiveWindow is the number of trailing thinking lines shown
// during streaming. Matches DeepSeek-TUI's collapsed reasoning card
// (3-4 content lines) — long enough to show the current thread,
// short enough that 60+ token/s streams don't drown the viewport.
const thinkingLiveWindow = 4

// Bound work on every spinner frame. The complete trace remains in
// m.thinkingText and is rendered from the cached historical message after the
// stream finishes; the live card only needs a recent, responsive window.
const thinkingLiveMaxBytes = 16 * 1024

func liveThinkingTail(text string) (string, bool) {
	if len(text) <= thinkingLiveMaxBytes {
		return text, false
	}
	start := len(text) - thinkingLiveMaxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:], true
}

func (it *inProgressThinkingItem) Render(width int) string {
	if it.text == "" {
		return ""
	}
	var sb strings.Builder
	// Header: "✻ thinking … live · 3.2s". Mirrors DeepSeek-TUI's
	// "… reasoning live|done · 12.3s" pattern. Elapsed is optional —
	// callers that haven't been taught about the field pass 0 and
	// the header omits it.
	sb.WriteString(styleAccent.Render("  " + glyphAsterisk + " "))
	sb.WriteString(styleAccent.Render("thinking"))
	sb.WriteString(styleMuted.Render(" … live"))
	if it.elapsed > 0 {
		sb.WriteString(styleMuted.Render(" · " + formatElapsed(it.elapsed)))
	}
	sb.WriteString("\n")

	thinkStyle := styleDim.Italic(true)
	bodyW := width - 4
	if bodyW < 20 {
		bodyW = 20
	}
	liveText, clipped := liveThinkingTail(it.text)
	wrapped := xansi.Wrap(liveText, bodyW, " /-_.")
	lines := strings.Split(wrapped, "\n")
	// Sliding window: keep only the LAST thinkingLiveWindow lines.
	// A leading "…" row hints that earlier lines exist; the full
	// text is preserved on the historical thinking message that
	// lands after streaming ends (renderMessage::case "thinking").
	truncated := clipped
	if !it.expand && len(lines) > thinkingLiveWindow {
		lines = lines[len(lines)-thinkingLiveWindow:]
		truncated = true
	}
	railStyle := styleDim
	if truncated {
		sb.WriteString(railStyle.Render("  ╎ "))
		sb.WriteString(thinkStyle.Render("…"))
		sb.WriteString("\n")
	}
	for i, ln := range lines {
		sb.WriteString(railStyle.Render("  ╎ "))
		sb.WriteString(thinkStyle.Render(ln))
		// Trailing cursor on the LAST line — DeepSeek-TUI's "▎"
		// glyph, signals "stream is still appending".
		if i == len(lines)-1 {
			sb.WriteString(styleAccent.Render(" ▎"))
		}
		sb.WriteString("\n")
	}
	return normalizeChatItemBoundary(sb.String())
}

// inProgressStreamingItem renders the partial assistant reply for the
// current turn at the tail of the chat list. Same rationale as
// inProgressThinkingItem — keeps the visible streaming text aligned
// with transcript scroll. Suppressed when the turn has been
// backgrounded (Ctrl+B): the bytes still accumulate, we just don't
// paint them until finalizeTurn flushes.
type inProgressStreamingItem struct {
	text         string
	backgrounded bool
}

func (it *inProgressStreamingItem) Render(width int) string {
	if strings.TrimSpace(it.text) == "" || it.backgrounded {
		return ""
	}
	// Streaming text arrives before the final assistant message goes through
	// glamour, so a provider may give us one very long paragraph with no
	// newlines. Wrap it here using the same width budget as historical
	// assistant rows. Otherwise the list counts it as one row while the
	// terminal paints/truncates it at the right edge, which desynchronizes
	// frame geometry during long, frequently repainted conversations.
	bodyW := width - 4
	if bodyW < 20 {
		bodyW = 20
	}
	wrapped := xansi.Wrap(it.text, bodyW, " /-_.")

	var sb strings.Builder
	sb.WriteString(styleAsst.Render("  " + glyphBullet + " "))
	lines := strings.Split(wrapped, "\n")
	if len(lines) > 0 {
		sb.WriteString(styleText.Render(lines[0]))
		for _, ln := range lines[1:] {
			sb.WriteString("\n  ")
			sb.WriteString(styleText.Render(ln))
		}
	}
	sb.WriteString("\n")
	return normalizeChatItemBoundary(sb.String())
}

// buildChatItems composes a chronologically-ordered []list.Item from the
// Model's messages and toolEvents. Same merge logic as `m.timeline()`
// (sort by Timestamp / StartTime, stable order on ties), but produces
// list-compatible items so the chat list can virtualize the render.
//
// Streaming text (m.streamingText / m.thinkingText) is appended at the
// tail as live in-progress items so they scroll WITH the transcript
// rather than sticking above the input (image #12 user feedback). The
// items live outside the renderCache because their content updates
// every spinner tick; the rest of the list (historical messages +
// tool events) still hits the cache normally — only the tail is
// re-rendered per frame.
func (m *Model) buildChatItems() []list.Item {
	merged := m.timeline()
	// Recovery grouping is historical state, not animation state. Cache it so
	// spinner-only redraws still update elapsed time smoothly without rescanning
	// every prior error payload 25 times per second.
	recoveryPlans := m.cachedRecoveredErrorPlans(merged)
	out := make([]list.Item, 0, len(merged)+2)
	// thinkingDisplay = "hide" drops every reasoning row from the
	// transcript and from the live-streaming preview. "show" forces
	// expanded view regardless of ctrl+o state. "auto" (default) keeps
	// the old collapsed-by-default-with-ctrl+o behaviour.
	style := normalizeOutputStyle(m.outputStyle)
	hideThinking := m.thinkingDisplay == "hide" || style != outputStyleFull
	forceExpandThinking := m.thinkingDisplay == "show"
	compactTools := style != outputStyleFull
	plainAssistant := style == outputStyleMinimal
	var explorationRun []ToolEvent
	flushExploration := func() {
		if len(explorationRun) == 0 {
			return
		}
		if len(explorationRun) == 1 {
			// A1 (2026-08-02): expand only when this event's ID matches
			// expandedToolID. The legacy m.expandToolOutputs global
			// toggle has been removed (see tui.go) — the old OR-fallback
			// branch was dead code in production but a foot-gun in tests.
			out = append(out, &toolEventItem{te: explorationRun[0], expand: explorationRun[0].ID == m.expandedToolID, cache: m.renderCache})
		} else {
			events := append([]ToolEvent(nil), explorationRun...)
			// A grouped row expands when Ctrl+O targets any member ID. This
			// keeps the compact default while making the advertised affordance
			// real (the old construction hard-coded expand=false).
			expand := false
			for _, event := range events {
				if event.ID != "" && event.ID == m.expandedToolID {
					expand = true
					break
				}
			}
			out = append(out, &explorationGroupItem{
				events:    events,
				expand:    expand,
				subParent: events[0].SubAgentParentID,
			})
		}
		explorationRun = explorationRun[:0]
	}
	for _, it := range merged {
		switch {
		case it.msg != nil:
			flushExploration()
			// Defense in depth for imported/legacy state. Even though live
			// streams and resume hydration now discard these, do not let a
			// whitespace-only assistant occupy an invisible list row + gap.
			if it.msg.Role == "assistant" && strings.TrimSpace(it.msg.Content) == "" {
				continue
			}
			if hideThinking && (it.msg.Role == "thinking" || it.msg.Role == "redacted_thinking") {
				continue
			}
			if style != outputStyleFull && it.msg.Role == "thought-summary" {
				continue
			}
			// A1 (2026-08-02): thinking rows honour /thinking show only.
			// The legacy expandToolOutputs global toggle is gone; ctrl+O
			// against a thinking row no longer expands it (only tool
			// events participate in one-at-a-time expansion).
			expand := forceExpandThinking && it.msg.Role == "thinking"
			out = append(out, &messageItem{msg: *it.msg, expand: expand, plain: plainAssistant && it.msg.Role == "assistant", cache: m.renderCache})
		case it.te != nil:
			if compactTools {
				flushExploration()
				out = append(out, &compactToolEventItem{te: *it.te})
				continue
			}
			if plan, recovered := recoveryPlans[it.te]; recovered {
				flushExploration()
				if it.te == plan.anchor {
					expand := plan.expandID != "" && plan.expandID == m.expandedToolID
					if !expand && m.expandedToolID != "" {
						for _, event := range plan.events {
							if event.ID == m.expandedToolID {
								expand = true
								break
							}
						}
					}
					out = append(out, &recoveredErrorGroupItem{
						events:       append([]ToolEvent(nil), plan.events...),
						expand:       expand,
						expandID:     plan.expandID,
						subParent:    plan.subParent,
						partialCount: plan.partialCount,
						retriedCount: plan.retriedCount,
					})
				}
				continue
			}
			// Cluster boundary: a groupable event joins the current run
			// only if its SubAgentParentID MATCHES the run's existing
			// parent. Prevents 5 parallel sub-agents' grep calls from
			// collapsing into one giant "Searched 15 patterns" blob
			// that loses track of which child produced what.
			if groupableExplorationEvent(*it.te) {
				if len(explorationRun) > 0 &&
					explorationRun[0].SubAgentParentID != it.te.SubAgentParentID {
					flushExploration()
				}
				explorationRun = append(explorationRun, *it.te)
				continue
			}
			flushExploration()
			// A1: expand only when this event's ID matches expandedToolID.
			out = append(out, &toolEventItem{te: *it.te, expand: it.te.ID == m.expandedToolID, cache: m.renderCache})
		}
	}
	flushExploration()
	if m.thinkingText != "" && !hideThinking {
		// Elapsed: time since the first streaming delta arrived. Falls
		// back to 0 when firstStreamAt is unset (e.g. thinking started
		// before any text — rare but possible) so the header just omits
		// the duration rather than showing a bogus value.
		var elapsed time.Duration
		if !m.firstStreamAt.IsZero() {
			elapsed = time.Since(m.firstStreamAt)
		}
		out = append(out, &inProgressThinkingItem{
			text:    m.thinkingText,
			expand:  forceExpandThinking,
			width:   m.width,
			elapsed: elapsed,
		})
	}
	if m.streamingText != "" {
		out = append(out, &inProgressStreamingItem{
			text:         m.streamingText,
			backgrounded: m.turnBackgrounded,
		})
	}
	// Spinner status (`* scaffolding (5.5s · ↑ 95k tokens)`) used to
	// live in the `upper` chrome block, but it visually matches the
	// asterisked rows already inside the transcript and the user
	// reported it as "stuck" when scrolling (image #16 feedback). We
	// snapshot the spinner string at this frame's buildChatItems call
	// so it appears at the tail of the chat list and scrolls together
	// with the thinking/streaming items. Permission prompt and active
	// screen overlays stay in `upper` because they require keyboard
	// focus and must remain on screen.
	if m.spinnerActive {
		out = append(out, &staticItem{rendered: renderSpinnerStatus(m)})
	}
	return out
}

// buildChatSurfaceItems wraps the chronological message/tool items with the
// conversation's visual prologue. Claude Code's Messages component renders
// LogoHeader before every MessageRow inside the scrollable transcript. Keep
// the Metis welcome card in the same place: it remains visually unchanged
// after the first submit, then scrolls away with older history instead of
// being replaced by a second, sticky header design.
//
// Keeping this wrapper separate from buildChatItems also preserves the latter
// as a pure message/tool adapter for export, grouping, and focused unit tests.
func (m *Model) buildChatSurfaceItems() []list.Item {
	items := m.buildChatItems()
	out := make([]list.Item, 0, len(items)+1)
	out = append(out, &staticItem{rendered: m.renderWelcomeBannerCard()})
	return append(out, items...)
}
