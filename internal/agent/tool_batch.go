package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/spill"
)

// ToolBatchBudgetError stops before another model request. The full completed
// batch is retained/checkpointed, including user steering and safety guidance.
type ToolBatchBudgetError struct{ Reason string }

func (e *ToolBatchBudgetError) Error() string { return "tool batch budget: " + e.Reason }

func batchLimitEnv(key string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err == nil && n > 0 {
		return n
	}
	return fallback
}

func batchTokens(blocks []llm.ContentBlock) int {
	// Context admission uses the same native-image estimate as active history.
	// Base64 bytes belong to storage/HTTP transport limits, not token budgets.
	return estimateActiveHistoryTokens([]llm.Message{{Role: llm.RoleUser, Content: blocks}})
}

// projectToolBatch only touches a new batch, never any previously sent history.
// Non-result text (including user steering/reminders) and images are mandatory.
func (l *Loop) projectToolBatch(blocks []llm.ContentBlock) ([]llm.ContentBlock, error) {
	single, total := l.ToolResultTokenLimit, l.ToolBatchTokenLimit
	if single <= 0 {
		single = batchLimitEnv("METIS_TOOL_RESULT_TOKENS", 8000)
	}
	if total <= 0 {
		total = batchLimitEnv("METIS_TOOL_BATCH_TOKENS", 24000)
	}
	out := append([]llm.ContentBlock(nil), blocks...)
	for pass := 0; pass < 2; pass++ {
		for i, b := range out {
			if b.Type != "tool_result" {
				continue
			}
			over := estimateActiveContentBlockTokens(b) > single || batchTokens(out) > total
			if !over {
				continue
			}
			original := blocks[i]
			if original.ToolResult == "" {
				continue
			}
			if l.SpillDir == "" {
				return blocks, &ToolBatchBudgetError{"spill storage unavailable; full batch retained"}
			}
			digest := sha256.Sum256([]byte(original.ToolResult))
			stored, err := spill.Store(l.SpillDir, fmt.Sprintf("batch-%x", digest), original.ToolResult)
			if err != nil {
				return blocks, &ToolBatchBudgetError{fmt.Sprintf("saving full result failed: %v; full batch retained", err)}
			}
			// spill.Store intentionally treats an existing file as idempotent;
			// verify it before trusting it as the only full copy of this result.
			full, readErr := os.ReadFile(stored.Path)
			if readErr != nil || string(full) != original.ToolResult {
				return blocks, &ToolBatchBudgetError{"full result storage verification failed; full batch retained"}
			}
			// Safety/hook reminders must remain inline, not merely discoverable on disk.
			mandatory := original.TrustedResultSuffix
			if pass == 1 {
				stored.Preview = ""
			}
			b.ToolResult = stored.Stub() + mandatory
			if len(b.ToolResultBlocks) > 0 {
				b.ToolResultBlocks = append([]llm.ContentBlock(nil), original.ToolResultBlocks...)
				// The dispatch projection's leading text is the same body as ToolResult.
				// Preserve arbitrary additional text parts and all ordered images verbatim.
				if b.ToolResultBlocks[0].Type == "text" && b.ToolResultBlocks[0].Text == original.ToolResult {
					b.ToolResultBlocks[0].Text = b.ToolResult
				}
			}
			out[i] = b
		}
	}
	for _, b := range out {
		if b.Type == "tool_result" && estimateActiveContentBlockTokens(b) > single {
			return blocks, &ToolBatchBudgetError{"mandatory result/image exceeds per-result limit; full batch retained (raise configured limit)"}
		}
	}
	if batchTokens(out) > total {
		return blocks, &ToolBatchBudgetError{"mandatory text/images exceed batch limit; full batch retained (raise configured limit)"}
	}
	return out, nil
}

// commitToolBatch is the single first-history boundary, including early stops.
func (l *Loop) commitToolBatch(blocks []llm.ContentBlock) error {
	return l.commitToolBatchWithContext(context.Background(), blocks)
}

func (l *Loop) commitToolBatchWithContext(ctx context.Context, blocks []llm.ContentBlock) error {
	if steer := l.drainSteer(); steer != "" {
		blocks = append(append([]llm.ContentBlock(nil), blocks...), llm.ContentBlock{Type: "text", Text: "[user steer mid-turn] " + steer})
	}
	projected, budgetErr := l.projectToolBatch(blocks)
	projected = canonicalImageReferenceBlocks(projected)
	l.mu.Lock()
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: projected})
	l.mu.Unlock()
	attachmentErr := recordInvocationImageAttachments(ctx, projected)
	checkpointErr := l.checkpointHistory()
	if attachmentErr != nil {
		return errors.Join(fmt.Errorf("checkpoint image attachments: %w", attachmentErr), checkpointErr, budgetErr)
	}
	if checkpointErr != nil {
		return checkpointErr
	}
	return budgetErr
}
