package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// Host-owned provenance persists with history_replace and propagates through
// iterative summaries. It contains identities/hashes, never withdrawn bodies.
const summaryMemoryHint = "metis.summary.memory.v1"
const summaryMemoryInvalidated = "[Earlier summary invalidated because a memory source was withdrawn or is unavailable. Retained user requests and recent conversation remain authoritative. Earlier task details may be missing: recover them from authorized files/tools or ask the user explicitly before proceeding.]"

type summaryMemoryDependencies struct {
	References   []recallReference `json:"references,omitempty"`
	ContextLines []string          `json:"context_lines,omitempty"`
	Unverifiable bool              `json:"unverifiable,omitempty"`
}

// summarySourceSpan identifies source-bearing bytes, not the surrounding recall
// fence or reference metadata. Fitting projects these ranges along with text.
type summarySourceSpan struct {
	start, end int
	source     llm.ContentBlock
}

func shiftSummarySpans(spans []summarySourceSpan, offset int) []summarySourceSpan {
	out := make([]summarySourceSpan, len(spans))
	for i, span := range spans {
		span.start += offset
		span.end += offset
		out[i] = span
	}
	return out
}

// Project unchanged byte ranges through a local text transformation. Inserted
// omission markers never acquire provenance from the bytes they replace.
func projectSummarySpans(before, after string, spans []summarySourceSpan) []summarySourceSpan {
	if len(spans) == 0 || after == "" {
		return nil
	}
	var out []summarySourceSpan
	copyRange := func(start, end, destination int) {
		for _, span := range spans {
			lo, hi := max(start, span.start), min(end, span.end)
			if lo < hi {
				out = append(out, summarySourceSpan{destination + lo - start, destination + hi - start, span.source})
			}
		}
	}
	// Head/tail fitting has exact positional semantics. Do not infer its
	// retained ranges with diff: repeated source bodies are indistinguishable.
	if before == after {
		copyRange(0, len(before), 0)
		return out
	}
	if strings.HasPrefix(before, after) {
		copyRange(0, len(after), 0)
		return out
	}
	marker := regexp.MustCompile(`(?s)\n\.\.\.\[omitted [0-9]+ runes from [^\n]+; head\+tail retained\]\.\.\.\n`)
	if loc := marker.FindStringIndex(after); loc != nil {
		head, tail := after[:loc[0]], after[loc[1]:]
		if strings.HasPrefix(before, head) && strings.HasSuffix(before, tail) {
			copyRange(0, len(head), 0)
			copyRange(len(before)-len(tail), len(before), loc[1])
			return out
		}
	}
	// Skeleton value truncation also retains positional ends. At this point
	// nested children have already been handled separately.
	head := 0
	for head < len(before) && head < len(after) && before[head] == after[head] {
		head++
	}
	tail := 0
	for tail < len(before)-head && tail < len(after)-head && before[len(before)-1-tail] == after[len(after)-1-tail] {
		tail++
	}
	copyRange(0, head, 0)
	copyRange(len(before)-tail, len(before), len(after)-tail)
	return out
}

var summaryRecallBody = regexp.MustCompile(`(?m)^\[[0-9]+\] `)

// Memory observations carry an aggregate dependency set for whole-result
// withdrawal. A summary, however, depends only on source-bearing bytes that
// survived serialization and fitting. Labels, reference envelopes and recovery
// instructions must not donate dependencies from omitted passages or Core lines.
func summaryMemoryToolSourceSpans(role string, block llm.ContentBlock, redact bool) []summarySourceSpan {
	var aggregate summaryMemoryDependencies
	if json.Unmarshal([]byte(block.ProviderHint[summaryMemoryHint]), &aggregate) != nil {
		return nil
	}
	body := block.ToolResult
	var spans []summarySourceSpan
	add := func(start, end int, deps summaryMemoryDependencies) {
		if start >= end {
			return
		}
		raw, _ := json.Marshal(deps)
		spans = append(spans, summarySourceSpan{start, end, llm.ContentBlock{ProviderHint: map[string]string{summaryMemoryHint: string(raw)}}})
	}
	allowedRefs := make(map[recallReference]bool, len(aggregate.References))
	for _, ref := range aggregate.References {
		allowedRefs[ref] = true
	}
	for _, loc := range recallFence.FindAllStringIndex(body, -1) {
		fence := body[loc[0]:loc[1]]
		match := recallRefs.FindStringSubmatch(fence)
		var refs []recallReference
		if len(match) != 2 {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(match[1])
		if err != nil || json.Unmarshal(raw, &refs) != nil || len(refs) != 1 || !allowedRefs[refs[0]] {
			continue
		}
		label := summaryRecallBody.FindStringIndex(fence)
		if label == nil {
			continue
		}
		start, end := label[1], strings.LastIndex(fence, "</auto-retrieve>")
		// The builtin's [date · type] prefix describes the source; it is
		// not itself source content. A clipped label cannot retain a fact.
		if strings.HasPrefix(fence[start:], "[") {
			if close := strings.Index(fence[start:], "] "); close >= 0 {
				start += close + len("] ")
			}
		}
		for end > start && (fence[end-1] == '\n' || fence[end-1] == '\r') {
			end--
		}
		add(loc[0]+start, loc[0]+end, summaryMemoryDependencies{References: refs})
	}
	allowedLines := make(map[string]bool, len(aggregate.ContextLines))
	for _, hash := range aggregate.ContextLines {
		allowedLines[hash] = true
	}
	offset := 0
	for _, line := range strings.Split(body, "\n") {
		hash := memoryLineHash(line)
		if line != "" && allowedLines[hash] {
			add(offset, offset+len(line), summaryMemoryDependencies{ContextLines: []string{hash}})
		}
		offset += len(line) + 1
	}
	window := compactRuneWindow(body, summaryToolResultHeadRunes, summaryToolResultTailRunes, "tool result")
	spans = projectSummarySpans(body, window, spans)
	// Serialize an empty own body to locate its header exactly, even when a
	// display/presentation field happens to contain identical body text.
	own := block
	own.ToolResult, own.ToolResultBlocks = "", nil
	var header strings.Builder
	writeSummaryContentBlock(&header, role, own, redact)
	base := header.Len() - 1 // the serializer's final newline follows the body
	for i := range spans {
		spans[i].start = base + len(maybeRedact(window[:spans[i].start], redact))
		spans[i].end = base + len(maybeRedact(window[:spans[i].end], redact))
	}
	return spans
}

func summaryBlockSourceSpans(role string, block llm.ContentBlock, redact bool) []summarySourceSpan {
	var rendered strings.Builder
	writeSummaryContentBlock(&rendered, role, block, redact)
	text := rendered.String()
	var out []summarySourceSpan
	add := func(start, end int, source llm.ContentBlock) {
		if start < end {
			out = append(out, summarySourceSpan{start, end, source})
		}
	}
	if raw, ok := block.ProviderHint[summaryMemoryHint]; ok {
		if block.Type == "tool_result" && block.Synthetic && block.ProviderHint[memoryToolObservationHint] == "1" {
			out = append(out, summaryMemoryToolSourceSpans(role, block, redact)...)
		} else {
			// Derived summaries and legacy opaque sources retain their full
			// existing dependency set when any of their narrative is used.
			add(0, len(text), llm.ContentBlock{ProviderHint: map[string]string{summaryMemoryHint: raw}})
		}
	}
	if block.Synthetic {
		for _, loc := range recallFence.FindAllStringIndex(block.Text, -1) {
			fence := block.Text[loc[0]:loc[1]]
			match := recallRefs.FindStringSubmatch(fence)
			var refs []recallReference
			if len(match) == 2 {
				data, _ := base64.RawURLEncoding.DecodeString(match[1])
				_ = json.Unmarshal(data, &refs)
			}
			bodies := summaryRecallBody.FindAllStringIndex(fence, -1)
			if len(bodies) == 0 {
				// Legacy/malformed recalls have no verifiable body boundaries.
				// Preserve fail-closed semantics only if their contents survive.
				start, end := strings.Index(fence, ">")+1, strings.LastIndex(fence, "</auto-retrieve>")
				base := strings.Index(text, maybeRedact(block.Text, redact))
				if base >= 0 && start < end {
					raw, _ := json.Marshal(summaryMemoryDependencies{Unverifiable: true})
					lo := base + len(maybeRedact(block.Text[:loc[0]+start], redact))
					add(lo, lo+len(maybeRedact(fence[start:end], redact)), llm.ContentBlock{ProviderHint: map[string]string{summaryMemoryHint: string(raw)}})
				}
			}
			// A single reference owns its entire body, including numbered lines
			// quoted inside the source itself.
			if len(refs) == 1 && len(bodies) > 1 {
				bodies = bodies[:1]
			}
			for i, body := range bodies {
				end := strings.LastIndex(fence, "</auto-retrieve>")
				if i+1 < len(bodies) {
					end = bodies[i+1][0]
				}
				start := body[1]
				for end > start && (fence[end-1] == '\n' || fence[end-1] == '\r') {
					end--
				}
				deps := summaryMemoryDependencies{Unverifiable: len(refs) != len(bodies)}
				if !deps.Unverifiable {
					deps.References = []recallReference{refs[i]}
				}
				raw, _ := json.Marshal(deps)
				// Locate the redacted body in the serialized block before any fitting.
				prefix := maybeRedact(block.Text[:loc[0]+start], redact)
				content := maybeRedact(fence[start:end], redact)
				base := strings.Index(text, maybeRedact(block.Text, redact))
				if base >= 0 {
					add(base+len(prefix), base+len(prefix)+len(content), llm.ContentBlock{ProviderHint: map[string]string{summaryMemoryHint: string(raw)}})
				}
			}
		}
	}
	if block.ProviderHint[dynamicCheckpointHint] == "checkpoint" {
		var sections []llm.SystemSection
		if json.Unmarshal([]byte(block.ProviderHint[dynamicCheckpointHint+".sections"]), &sections) != nil {
			add(0, len(text), block)
		}
		for _, section := range sections {
			if section.Name == "memory_index" {
				// The same policy line can appear in several Core blocks. Its
				// serialized offsets, rather than the first matching bytes in the
				// checkpoint, determine which copy survives input fitting.
				base := strings.Index(text, maybeRedact(block.Text, redact))
				bodyStart := strings.Index(block.Text, "\n\n[memory_index]\n"+section.Body)
				if bodyStart >= 0 {
					bodyStart += len("\n\n[memory_index]\n")
				} else {
					bodyStart = strings.Index(block.Text, section.Body)
				}
				offset := 0
				for _, line := range strings.Split(section.Body, "\n") {
					if strings.TrimSpace(line) != "" && base >= 0 && bodyStart >= 0 {
						needle := maybeRedact(line, redact)
						pos := base + len(maybeRedact(block.Text[:bodyStart+offset], redact))
						raw, _ := json.Marshal(summaryMemoryDependencies{ContextLines: []string{memoryLineHash(line)}})
						add(pos, pos+len(needle), llm.ContentBlock{ProviderHint: map[string]string{summaryMemoryHint: string(raw)}})
					}
					offset += len(line) + 1
				}
			}
		}
	}
	search := 0
	for _, child := range block.ToolResultBlocks {
		var b strings.Builder
		writeSummaryContentBlock(&b, role+" tool_result_part", child, redact)
		if pos := strings.Index(text[search:], b.String()); pos >= 0 {
			pos += search
			out = append(out, shiftSummarySpans(summaryBlockSourceSpans(role+" tool_result_part", child, redact), pos)...)
			search = pos + b.Len()
		}
	}
	return out
}

func summaryToolMinimumSpans(role string, block llm.ContentBlock, redact bool, minimum string) []summarySourceSpan {
	// Nested parts omitted by the skeleton must never donate provenance via
	// a coincidentally matching byte elsewhere in the enclosing transaction.
	own := block
	own.ToolResultBlocks = nil
	var full strings.Builder
	writeSummaryContentBlock(&full, role, own, redact)
	ownMinimum := buildSummaryToolAtomMinimum(role, own, redact)
	out := projectSummarySpans(full.String(), ownMinimum, summaryBlockSourceSpans(role, own, redact))
	if len(block.ToolResultBlocks) > 0 {
		indices := []int{0}
		if len(block.ToolResultBlocks) > 1 {
			indices = append(indices, len(block.ToolResultBlocks)-1)
		}
		cursor := len(ownMinimum)
		for _, index := range indices {
			child := block.ToolResultBlocks[index]
			var part strings.Builder
			writeSummaryContentBlock(&part, role+" tool_result_part", child, redact)
			fitted := compactSummarySkeletonValue(part.String(), 32, 16, "tool result part")
			if pos := strings.Index(minimum[cursor:], fitted); pos >= 0 {
				pos += cursor
				out = append(out, shiftSummarySpans(projectSummarySpans(part.String(), fitted, summaryBlockSourceSpans(role+" tool_result_part", child, redact)), pos)...)
				cursor = pos + len(fitted)
			}
		}
	}
	return out
}

func summarySegmentRetainedSpans(seg summaryTranscriptSegment, piece string) []summarySourceSpan {
	if seg.transaction == summaryTransactionNone {
		return projectSummarySpans(seg.text, piece, seg.spans)
	}
	if piece == seg.text {
		return seg.spans
	}
	out := append([]summarySourceSpan(nil), seg.minimumSpans...)
	prefix := seg.minimum + "[additional transaction detail; skeleton above is authoritative]\n"
	if strings.HasPrefix(piece, prefix) {
		out = append(out, shiftSummarySpans(projectSummarySpans(seg.text, piece[len(prefix):], seg.spans), len(prefix))...)
	}
	return out
}

func memoryLineHash(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
func summaryDependencies(messages []llm.Message) summaryMemoryDependencies {
	var deps summaryMemoryDependencies
	seenRefs, seenLines := map[recallReference]bool{}, map[string]bool{}
	var scan func(llm.ContentBlock)
	scan = func(b llm.ContentBlock) {
		if raw, exists := b.ProviderHint[summaryMemoryHint]; exists {
			var prior summaryMemoryDependencies
			if json.Unmarshal([]byte(raw), &prior) != nil {
				deps.Unverifiable = true
			} else {
				deps.Unverifiable = deps.Unverifiable || prior.Unverifiable
				for _, ref := range prior.References {
					seenRefs[ref] = true
				}
				for _, line := range prior.ContextLines {
					seenLines[line] = true
				}
			}
		}
		if b.Synthetic {
			for _, fence := range recallFence.FindAllString(b.Text, -1) {
				match := recallRefs.FindStringSubmatch(fence)
				var refs []recallReference
				if len(match) != 2 {
					deps.Unverifiable = true
					continue
				}
				data, err := base64.RawURLEncoding.DecodeString(match[1])
				if err != nil || json.Unmarshal(data, &refs) != nil || len(refs) == 0 {
					deps.Unverifiable = true
					continue
				}
				for _, ref := range refs {
					seenRefs[ref] = true
				}
			}
		}
		if b.ProviderHint[dynamicCheckpointHint] == "checkpoint" {
			var sections []llm.SystemSection
			if json.Unmarshal([]byte(b.ProviderHint[dynamicCheckpointHint+".sections"]), &sections) != nil {
				deps.Unverifiable = true
			}
			for _, section := range sections {
				if section.Name == "memory_index" {
					for _, line := range strings.Split(section.Body, "\n") {
						if strings.TrimSpace(line) != "" {
							seenLines[memoryLineHash(line)] = true
						}
					}
				}
			}
		}
		for _, child := range b.ToolResultBlocks {
			scan(child)
		}
	}
	for _, m := range messages {
		for _, b := range m.Content {
			scan(b)
		}
	}
	for ref := range seenRefs {
		deps.References = append(deps.References, ref)
	}
	for line := range seenLines {
		deps.ContextLines = append(deps.ContextLines, line)
	}
	sort.Slice(deps.References, func(i, j int) bool {
		if deps.References[i].ID != deps.References[j].ID {
			return deps.References[i].ID < deps.References[j].ID
		}
		return deps.References[i].Hash < deps.References[j].Hash
	})
	sort.Strings(deps.ContextLines)
	return deps
}
func summaryMemoryHints(messages []llm.Message) map[string]string {
	deps := summaryDependencies(messages)
	if len(deps.References) == 0 && len(deps.ContextLines) == 0 && !deps.Unverifiable {
		return nil
	}
	raw, _ := json.Marshal(deps)
	return map[string]string{summaryMemoryHint: string(raw)}
}
func sanitizeDerivedMemory(block llm.ContentBlock, body string, visible map[string]string, valid bool) (llm.ContentBlock, bool) {
	if raw, exists := block.ProviderHint[summaryMemoryHint]; exists && !validSummaryMemory(raw, body, visible, valid) {
		if block.Type == "tool_result" {
			// Preserve provider tool pairing even for a restored nested wrapper.
			return llm.ContentBlock{Type: "tool_result", ToolUseID: block.ToolUseID, TraceCallID: block.TraceCallID, ToolResult: summaryMemoryInvalidated, IsError: true}, true
		}
		return llm.ContentBlock{Type: "text", Text: summaryMemoryInvalidated, Synthetic: true}, true
	}
	changed := false
	if len(block.ToolResultBlocks) > 0 {
		children := make([]llm.ContentBlock, len(block.ToolResultBlocks))
		for i, child := range block.ToolResultBlocks {
			var removed bool
			children[i], removed = sanitizeDerivedMemory(child, body, visible, valid)
			changed = changed || removed
		}
		if changed {
			block.ToolResultBlocks = children
		}
	}
	return block, changed
}

func validSummaryMemory(raw, body string, visible map[string]string, ok bool) bool {
	var deps summaryMemoryDependencies
	if !ok || json.Unmarshal([]byte(raw), &deps) != nil || deps.Unverifiable {
		return false
	}
	for _, ref := range deps.References {
		if visible[ref.ID] != ref.Hash {
			return false
		}
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		lines[memoryLineHash(line)] = true
	}
	for _, hash := range deps.ContextLines {
		if !lines[hash] {
			return false
		}
	}
	return true
}
