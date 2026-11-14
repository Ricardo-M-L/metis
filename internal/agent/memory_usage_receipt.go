package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/google/uuid"
)

const memoryUsageReceiptHint = "metis.memory.usage.v1"
const memoryToolObservationHint = "metis.memory.observation.v1"

type memoryUsageReceipt struct {
	EventID  string           `json:"event_id"`
	BodyHash string           `json:"body_hash"`
	Passages []memory.Passage `json:"passages"`
}

func memoryResultBody(block llm.ContentBlock) string {
	if block.Type == "text" {
		return block.Text
	}
	body := block.ToolResult
	if suffix := block.TrustedResultSuffix; suffix != "" && strings.HasSuffix(body, suffix) {
		body = strings.TrimSuffix(body, suffix)
	}
	return body
}

func memoryResultHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

func attachMemoryUsageReceipt(block *llm.ContentBlock, passages []memory.Passage) {
	if len(passages) == 0 {
		return
	}
	identities := make([]memory.Passage, len(passages))
	for i, p := range passages {
		identities[i] = memory.Passage{ID: p.ID, Scope: p.Scope, SourceSessionID: p.SourceSessionID,
			CreatedAt: p.CreatedAt, Tags: append([]string(nil), p.Tags...)}
	}
	raw, _ := json.Marshal(memoryUsageReceipt{EventID: uuid.NewString(), BodyHash: memoryResultHash(memoryResultBody(*block)), Passages: identities})
	if block.ProviderHint == nil {
		block.ProviderHint = map[string]string{}
	}
	block.ProviderHint[memoryUsageReceiptHint] = string(raw)
}

// Only typed host metadata from Memory is promoted. An ordinary tool or user
// cannot acquire provenance by printing an auto-retrieve fence.
func attachMemoryToolProvenance(block *llm.ContentBlock, toolName string, result *tools.Result, complete bool) {
	if toolName != "Memory" || result == nil || result.IsError {
		return
	}
	meta, ok := result.Meta[tools.MemoryRecallMetaKey].(*tools.MemoryRecallResult)
	if !ok || meta == nil || (len(meta.References) == 0 && len(meta.ContextLineHashes) == 0) || len(meta.Passages) != len(meta.References) {
		return
	}
	deps := summaryMemoryDependencies{ContextLines: append([]string(nil), meta.ContextLineHashes...)}
	passages := make([]memory.Passage, 0, len(meta.Passages))
	for i, p := range meta.Passages {
		passage := memory.Passage{ID: p.ID, Content: p.Content, Scope: p.Scope, SourceSessionID: p.SourceSessionID, CreatedAt: p.CreatedAt, Tags: p.Tags}
		ref := meta.References[i]
		if ref.ID != passage.ID || ref.Hash != recallPassageHash(passage) {
			return
		}
		deps.References = append(deps.References, recallReference{ID: ref.ID, Hash: ref.Hash})
		passages = append(passages, passage)
	}
	raw, _ := json.Marshal(deps)
	if block.ProviderHint == nil {
		block.ProviderHint = map[string]string{}
	}
	block.ProviderHint[summaryMemoryHint] = string(raw)
	block.ProviderHint[memoryToolObservationHint] = "1"
	block.Synthetic = true
	if complete {
		attachMemoryUsageReceipt(block, passages)
	}
}

// Receipts survive history persistence, so the repository event ID makes
// retries and restored requests idempotent. Budget/spill replacements fail the
// exact body digest and cannot record a result the model did not receive.
func (l *Loop) commitRequestMemoryUsage(ctx context.Context, req llm.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.RLock()
	repository := l.Memory
	l.mu.RUnlock()
	if repository == nil {
		return nil
	}
	seen := map[string]bool{}
	var scan func(llm.ContentBlock) error
	scan = func(block llm.ContentBlock) error {
		if raw := block.ProviderHint[memoryUsageReceiptHint]; raw != "" {
			var receipt memoryUsageReceipt
			if json.Unmarshal([]byte(raw), &receipt) != nil || receipt.EventID == "" {
				return fmt.Errorf("invalid memory usage receipt")
			}
			if !seen[receipt.EventID] && receipt.BodyHash == memoryResultHash(memoryResultBody(block)) {
				seen[receipt.EventID] = true
				key := repository.Root() + "\x00" + receipt.EventID
				identity := memoryResultHash(raw)
				prior, committed := l.memoryUsageCommitted.Load(key)
				if committed {
					if prior != identity {
						return fmt.Errorf("memory usage receipt identity changed")
					}
				}
				if !committed {
					var err error
					if recorder, ok := repository.(interface {
						MarkRetrievedEvent(string, []memory.Passage) error
					}); ok {
						err = recorder.MarkRetrievedEvent(receipt.EventID, receipt.Passages)
					} else {
						err = repository.MarkRetrieved(receipt.Passages)
					}
					if err != nil {
						return fmt.Errorf("persist memory usage: %w", err)
					}
					l.memoryUsageCommitted.Store(key, identity)
				}
			}
		}
		for _, child := range block.ToolResultBlocks {
			if err := scan(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if err := scan(block); err != nil {
				return err
			}
		}
	}
	return nil
}
