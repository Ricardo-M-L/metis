package agent

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/imageprep"
	"github.com/Ricardo-M-L/metis/internal/llm"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

// An image is a native visual input, not base64 text. Selection uses the same
// bounded visual allowance as the active-context meter, never the wire size.
type compactImageCandidate struct {
	block    llm.ContentBlock
	ordinal  int
	message  int
	priority int
	id       string
	sources  []llm.ContentBlock
}

func compactImageCandidates(messages []llm.Message) []compactImageCandidate {
	taskStart := 0
	var task strings.Builder
	requests := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if !messageHasRealUserText(messages[i]) {
			continue
		}
		taskStart = i
		for _, block := range messages[i].Content {
			if block.Type == "text" && !block.Synthetic {
				task.WriteString(block.Text + " ")
			}
		}
		requests++
		// A short continuation often refers to the preceding request/image.
		if requests >= 2 || len([]rune(strings.TrimSpace(task.String()))) > 80 {
			break
		}
	}
	// Task text can itself be a pasted document. Bound the cheap relevance
	// heuristic without altering the evidence that reaches the summarizer.
	taskText := task.String()
	words := strings.FieldsFunc(strings.ToLower(compactRuneWindow(taskText, 2_048, 2_048, "task relevance")), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	seenWords := make(map[string]bool)
	terms := make([]string, 0, min(len(words), 128))
	for _, word := range words {
		if len([]rune(word)) >= 4 && !seenWords[word] && len(terms) < 128 {
			terms = append(terms, word)
			seenWords[word] = true
		}
	}
	var out []compactImageCandidate
	for i, message := range messages {
		var context strings.Builder
		for _, block := range message.Content {
			context.WriteString(compactRuneWindow(block.Text, 1_024, 256, "image context") + " " + compactRuneWindow(block.ToolResult, 1_024, 256, "image context") + " ")
		}
		contextText := strings.ToLower(context.String())
		var visit func([]llm.ContentBlock, bool, []llm.ContentBlock)
		visit = func(blocks []llm.ContentBlock, nested bool, parents []llm.ContentBlock) {
			for _, block := range blocks {
				sources := parents
				if raw := block.ProviderHint[summaryMemoryHint]; raw != "" {
					sources = append(append([]llm.ContentBlock(nil), parents...), llm.ContentBlock{ProviderHint: map[string]string{summaryMemoryHint: raw}})
				}
				if block.Type == "image" {
					id := canonicalImageAttachmentID(block)
					priority := 0
					if i >= taskStart {
						priority += 300
					}
					if message.Role == llm.RoleUser && !nested {
						priority += 100
					}
					if id != "" && strings.Contains(taskText, id) {
						priority += 1_000
					}
					for _, word := range terms {
						if strings.Contains(contextText, word) {
							priority += 10
						}
					}
					out = append(out, compactImageCandidate{block: block, ordinal: len(out), message: i, priority: priority, id: id, sources: sources})
				}
				visit(block.ToolResultBlocks, true, sources)
			}
		}
		visit(message.Content, false, nil)
	}
	return out
}

func imageOffloadPlaceholder(block llm.ContentBlock, reason string) llm.ContentBlock {
	id := canonicalImageAttachmentID(block)
	ref := "attachment_id unavailable; original image could not be resolved"
	if id != "" {
		ref = fmt.Sprintf("attachment_id=%s; use ViewImage with {\"attachment_id\":\"%s\"} to inspect again", id, id)
	}
	return llm.ContentBlock{Type: "text", Synthetic: true, Text: fmt.Sprintf("[image offloaded by compactor; %s; unparsed image: %s. Do not infer visual facts from this reference.]", ref, reason)}
}

// fitImagesToContextBudget only rewrites history when it actually exceeds the
// active-context budget. Current task/user attachments outrank old screenshots;
// recency breaks ties. The tool framing and every unrelated block survive.
func fitImagesToContextBudget(messages []llm.Message, budget int) ([]llm.Message, int) {
	if budget <= 0 || estimateActiveHistoryTokens(messages) <= budget {
		return messages, 0
	}
	candidates := compactImageCandidates(messages)
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].ordinal < candidates[j].ordinal
	})
	replacements := make(map[int]llm.ContentBlock)
	tokens := estimateActiveHistoryTokens(messages)
	for _, candidate := range candidates {
		if tokens <= budget {
			break
		}
		placeholder := imageOffloadPlaceholder(candidate.block, "active context capacity is insufficient; re-read this attachment when needed")
		replacements[candidate.ordinal] = placeholder
		tokens -= estimateActiveContentBlockTokens(candidate.block) - estimateActiveContentBlockTokens(placeholder)
	}
	if len(replacements) == 0 {
		return messages, 0
	}
	ordinal := 0
	var rewrite func([]llm.ContentBlock) []llm.ContentBlock
	rewrite = func(blocks []llm.ContentBlock) []llm.ContentBlock {
		out := append([]llm.ContentBlock(nil), blocks...)
		for i, block := range out {
			if block.Type == "image" {
				if replacement, ok := replacements[ordinal]; ok {
					out[i] = replacement
				}
				ordinal++
			}
			if len(block.ToolResultBlocks) > 0 {
				out[i].ToolResultBlocks = rewrite(block.ToolResultBlocks)
			}
		}
		return out
	}
	out := append([]llm.Message(nil), messages...)
	for i := range out {
		out[i].Content = rewrite(out[i].Content)
	}
	return out, len(replacements)
}

const summaryImageInstructions = `
Image evidence rules: native images below are part of this same summary request. Preserve task-relevant visual facts (for example text, layout, chart values, or observed UI state) with their exact attachment_id. Reuse explicit observations in the transcript while distinguishing an observation from a guess. Image metadata and attachment IDs do not reveal image contents. If an image is not supplied, or its contents cannot be read, record "unparsed image; re-read by attachment_id" and never invent a caption. Preserve the reference manifest so a later agent can use ViewImage with {"attachment_id":"<ID>"}.`

type summaryImageEvidence struct {
	content  []llm.ContentBlock
	sources  []llm.ContentBlock
	manifest string
}

// Plan one bounded multimodal request. There is no per-image caption call.
// The text transcript keeps at least two thirds of the available evidence
// allowance; visual evidence competes by task relevance, then recency.
func planSummaryImageEvidence(messages []llm.Message, provider llm.Provider, available int) summaryImageEvidence {
	candidates := compactImageCandidates(messages)
	if len(candidates) == 0 {
		return summaryImageEvidence{}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].ordinal > candidates[j].ordinal
	})
	visualBudget := 32_000
	if available > 0 {
		visualBudget = available / 3
	}
	// Reuse the existing tool-batch visual admission allowance. Separately
	// bound this local summary's wire payload; neither is a claim about every
	// provider's image-count, pixel-token, or HTTP limits.
	visualBudget = min(visualBudget, batchLimitEnv("METIS_TOOL_BATCH_TOKENS", 24_000))
	wireBudget := 4 * imageprep.MaxBase64Bytes
	vision := pubprovider.ProviderVisionCapability(provider) != pubprovider.VisionUnsupported
	seen := make(map[string]bool)
	var plan summaryImageEvidence
	var manifest strings.Builder
	manifest.WriteString("\nImage references (an attachment reference alone is not a visual observation; re-read using ViewImage attachment_id):\n")
	for _, candidate := range candidates {
		if candidate.id != "" && seen[candidate.id] {
			continue
		}
		seen[candidate.id] = candidate.id != ""
		status := "unparsed image; native evidence unavailable"
		if !vision {
			status = "unparsed image; summary model has no vision"
		} else if candidate.id != "" && candidate.block.Data != "" {
			label := llm.ContentBlock{Type: "text", Text: fmt.Sprintf("Native image attachment_id=%s:", candidate.id)}
			cost := estimateActiveContentBlockTokens(label) + estimateActiveContentBlockTokens(candidate.block)
			if len(candidate.block.Data) > imageprep.MaxBase64Bytes || len(candidate.block.Data) > wireBudget {
				status = "unparsed image; summary image transport budget exhausted"
			} else if cost <= visualBudget {
				plan.content = append(plan.content, label, candidate.block)
				plan.sources = append(plan.sources, candidate.sources...)
				visualBudget -= cost
				wireBudget -= len(candidate.block.Data)
				status = "native evidence supplied to summary model"
			} else {
				status = "unparsed image; summary visual budget exhausted"
			}
		}
		id := candidate.id
		if id == "" {
			id = "unavailable"
		}
		fmt.Fprintf(&manifest, "- attachment_id=%s: %s.\n", id, status)
	}
	plan.manifest = manifest.String()
	return plan
}

// Metadata references cannot donate visual-source provenance. Only the exact
// transcript text that survives fitting and the native images actually admitted
// to the request may contribute source dependencies.
func summaryTranscriptImageReferences(messages []llm.Message) []llm.Message {
	var evidenceKinds func(llm.ContentBlock) (text, image bool)
	evidenceKinds = func(block llm.ContentBlock) (text, image bool) {
		image = block.Type == "image"
		text = (block.Type == "text" || block.Type == "thinking" || block.Type == "reasoning") && strings.TrimSpace(block.Text) != ""
		text = text || (block.Type == "tool_result" && strings.TrimSpace(block.ToolResult) != "")
		for _, child := range block.ToolResultBlocks {
			childText, childImage := evidenceKinds(child)
			text, image = text || childText, image || childImage
		}
		return text, image
	}
	var strip func([]llm.ContentBlock) []llm.ContentBlock
	strip = func(blocks []llm.ContentBlock) []llm.ContentBlock {
		out := append([]llm.ContentBlock(nil), blocks...)
		for i := range out {
			if out[i].Type == "image" {
				out[i].ProviderHint = nil
			} else if text, image := evidenceKinds(out[i]); image && !text && out[i].ProviderHint[summaryMemoryHint] != "" {
				// An image-only tool wrapper contributes framing and identity,
				// not a visual observation. Its source is admitted only if a
				// descendant native image is supplied to the checkpoint model.
				hints := make(map[string]string, len(out[i].ProviderHint))
				for key, value := range out[i].ProviderHint {
					if key != summaryMemoryHint {
						hints[key] = value
					}
				}
				out[i].ProviderHint = hints
			}
			if len(out[i].ToolResultBlocks) > 0 {
				out[i].ToolResultBlocks = strip(out[i].ToolResultBlocks)
			}
		}
		return out
	}
	out := append([]llm.Message(nil), messages...)
	for i := range out {
		out[i].Content = strip(out[i].Content)
	}
	return out
}

func summaryWithImageReferences(summary, manifest string) string {
	if manifest == "" {
		return summary
	}
	return strings.TrimSpace(summary) + "\n" + manifest
}
