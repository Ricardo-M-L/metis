package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func isMemoryToolObservation(block llm.ContentBlock) bool {
	return block.Type == "tool_result" && block.Synthetic && block.ProviderHint[memoryToolObservationHint] == "1"
}

// Memory already has an authoritative, revocable full copy. A generic spill
// would create an ordinary Read target with no source identity. Keep complete
// source units inline, and ask for a narrower Memory query when they do not fit.
func projectMemoryToolObservation(original llm.ContentBlock, budget int, omit bool) llm.ContentBlock {
	out := original
	out.ProviderHint = make(map[string]string, len(original.ProviderHint))
	for k, v := range original.ProviderHint {
		out.ProviderHint[k] = v
	}
	var deps summaryMemoryDependencies
	_ = json.Unmarshal([]byte(original.ProviderHint[summaryMemoryHint]), &deps)
	retained := summaryMemoryDependencies{}
	header := "[Memory result reduced by the tool budget. Use Memory.search with a narrower query or smaller limit; Memory.read reads current Core blocks. Sources remain in the authoritative memory repository.]\n"
	body := header
	appendUnit := func(unit string) bool {
		candidate := out
		candidate.ToolResult = body + unit + original.TrustedResultSuffix
		if estimateActiveContentBlockTokens(candidate) > budget {
			return false
		}
		body += unit
		return true
	}
	if !omit {
		allowed := make(map[recallReference]bool, len(deps.References))
		for _, ref := range deps.References {
			allowed[ref] = true
		}
		for _, loc := range recallFence.FindAllStringIndex(memoryResultBody(original), -1) {
			fence := memoryResultBody(original)[loc[0]:loc[1]]
			match := recallRefs.FindStringSubmatch(fence)
			if len(match) != 2 {
				continue
			}
			encoded, err := base64.RawURLEncoding.DecodeString(match[1])
			var refs []recallReference
			if err != nil || json.Unmarshal(encoded, &refs) != nil || len(refs) == 0 {
				continue
			}
			valid := true
			for _, ref := range refs {
				valid = valid && allowed[ref]
			}
			if valid && appendUnit("\n"+fence+"\n") {
				retained.References = append(retained.References, refs...)
			}
		}
		allowedLines := make(map[string]bool, len(deps.ContextLines))
		for _, hash := range deps.ContextLines {
			allowedLines[hash] = true
		}
		for _, line := range strings.Split(memoryResultBody(original), "\n") {
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(line)))
			if allowedLines[hash] && appendUnit(line+"\n") {
				retained.ContextLines = append(retained.ContextLines, hash)
			}
		}
	}
	out.ToolResult = body + original.TrustedResultSuffix
	if len(retained.References) == 0 && len(retained.ContextLines) == 0 {
		delete(out.ProviderHint, summaryMemoryHint)
	} else {
		raw, _ := json.Marshal(retained)
		out.ProviderHint[summaryMemoryHint] = string(raw)
	}
	// Reuse the stable receipt ID, narrowing its identities to source units
	// that actually survived this first-ingestion projection.
	var receipt memoryUsageReceipt
	if json.Unmarshal([]byte(original.ProviderHint[memoryUsageReceiptHint]), &receipt) == nil && receipt.EventID != "" {
		selected := make(map[recallReference]bool, len(retained.References))
		for _, ref := range retained.References {
			selected[ref] = true
		}
		kept := receipt.Passages[:0]
		for i, passage := range receipt.Passages {
			if len(receipt.Passages) == len(deps.References) && deps.References[i].ID == passage.ID && selected[deps.References[i]] {
				kept = append(kept, passage)
			}
		}
		receipt.Passages = kept
		if len(kept) > 0 {
			receipt.BodyHash = memoryResultHash(memoryResultBody(out))
			raw, _ := json.Marshal(receipt)
			out.ProviderHint[memoryUsageReceiptHint] = string(raw)
		} else {
			delete(out.ProviderHint, memoryUsageReceiptHint)
		}
	}
	return out
}
