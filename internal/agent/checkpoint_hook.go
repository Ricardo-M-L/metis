package agent

// checkpoint_hook.go — the loop side of the unified /rewind. Before the
// first file-mutating tool of each user turn, snapPreEdit takes a
// shadow-git snapshot of the working tree (via l.Checkpointer). Rewind
// then restores BOTH the files (checkpoint.Restore) and the conversation
// (UndoLastTurn down to the snapshot's turn) — atomically returning to
// the state before that turn's edits. Mirrors claude-code's Esc-Esc
// rewind, which restores code + transcript together.
//
// Best-effort: any Snap/Restore error is swallowed and leaves the loop
// running normally (a broken shadow repo must never block real work).

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ckptEntry pairs a file snapshot with the conversation point to undo to.
type ckptEntry struct {
	hash           string            // shadow-git commit hash to Restore
	restoreToTurns int               // CountTurns() value the conversation should return to
	label          string            // human label ("before turn 3")
	managedPaths   map[string]string // cwd-relative path -> post-tool fingerprint
	incomplete     bool              // never claim code restore across an unverified mutation
	scope          string            // a historical scope cannot authorize a different project set
}

// mutatingTools are the tools whose execution can change the working
// tree, so a pre-edit snapshot is worth taking. Bash is included
// conservatively (a command's apparent intent is not proof that it cannot
// mutate files). A complete baseline is established once per user turn.
var mutatingTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
	"Bash":         true,
}

// snapPreEdit takes a working-tree snapshot before the first mutating
// tool of the current turn. No-op when checkpointing is disabled, the
// tool isn't mutating, or this turn was already snapped.
func (l *Loop) snapPreEdit(toolName string, input map[string]any) {
	_ = l.snapPreEditContext(context.Background(), toolName, input)
}

func (l *Loop) snapPreEditContext(ctx context.Context, toolName string, input map[string]any) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if l.Checkpointer == nil || !mutatingTools[toolName] || l.Checkpointer.AutomaticDisabled() {
		return nil
	}
	if l.Checkpointer.Sparse() {
		return l.snapSparsePreEditContext(ctx, toolName, input)
	}
	turns := l.CountTurns()
	l.ckptMu.Lock()
	already := l.ckptSnappedAt == turns
	l.ckptMu.Unlock()
	if already {
		return nil
	}

	// A new turn needs its own mapping even when the files did not change.
	// BaselineContext validates the live tree and reuses HEAD in that case;
	// it never replaces an incomplete pre-turn point with a later snapshot.
	hash, err := l.Checkpointer.BaselineContext(ctx, toolName, argsHash(input), fmt.Sprintf("before turn %d", turns))
	if err != nil || hash == "" {
		return err
	}
	l.ckptMu.Lock()
	if l.ckptSnappedAt != turns { // re-check: another tool may have won the race
		l.ckptSnappedAt = turns
		l.ckptStack = append(l.ckptStack, ckptEntry{
			hash:           hash,
			restoreToTurns: turns - 1, // rewinding drops this turn's edits + conversation
			label:          fmt.Sprintf("before turn %d", turns),
			scope:          l.Checkpointer.ScopeKey(),
		})
	}
	l.ckptMu.Unlock()
	return nil
}

// A session opened at home protects only explicit file targets. Each new
// target's preimage also extends earlier sparse baselines: a file first edited
// in a later turn must not look like a newly created file when rewinding an
// earlier turn. The manager captures once, then applies the same bytes to all
// baselines without replacing any already-captured preimage (including absence).
func (l *Loop) snapSparsePreEditContext(ctx context.Context, toolName string, input map[string]any) error {
	if toolName == "Bash" {
		return errors.New("主目录文件检查点无法确定此 Shell 命令的写入范围；该轮仅支持会话回退")
	}
	var path string
	for _, key := range []string{"path", "file_path"} {
		if path, _ = input[key].(string); path != "" {
			break
		}
	}
	if path == "" {
		return errors.New("checkpoint: 文件工具没有提供可保护的路径")
	}
	turns := l.CountTurns()
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	var indices []int
	var baselines []string
	current := -1
	for i, entry := range l.ckptStack {
		if entry.restoreToTurns == turns-1 {
			if entry.incomplete {
				return ErrCheckpointIncomplete
			}
			current = i
		}
		if !entry.incomplete && entry.hash != "" && entry.scope == l.Checkpointer.ScopeKey() {
			indices = append(indices, i)
			baselines = append(baselines, entry.hash)
		}
	}
	if current < 0 {
		baselines = append(baselines, "")
	}
	hashes, err := l.Checkpointer.ExtendBaselinePathsContext(ctx, []string{path}, baselines, toolName, argsHash(input), fmt.Sprintf("before turn %d", turns))
	if err != nil {
		return err
	}
	for i, index := range indices {
		l.ckptStack[index].hash = hashes[i]
	}
	if current < 0 {
		l.ckptStack = append(l.ckptStack, ckptEntry{hash: hashes[len(hashes)-1], restoreToTurns: turns - 1,
			label: fmt.Sprintf("before turn %d", turns), scope: l.Checkpointer.ScopeKey()})
	}
	l.ckptSnappedAt = turns
	return nil
}

const (
	checkpointStageBudget   = 60 * time.Second
	checkpointInitialBudget = 5 * time.Minute
)

func (l *Loop) checkpointBudget(phase string) time.Duration {
	if phase == "before tool" && l.Checkpointer != nil && !l.Checkpointer.HasBaseline() {
		return checkpointInitialBudget
	}
	return checkpointStageBudget
}

// Automatic checkpointing is best-effort, but never an unbounded part of a
// tool invocation. Use the original turn context even for atomic tools that
// detach their own execution from Esc. A skipped stage must not look like a
// successfully protected rewind point to the user.
func (l *Loop) runCheckpointStage(ctx context.Context, out chan<- Event, phase, toolName string, run func(context.Context) error) {
	if l.Checkpointer == nil || !mutatingTools[toolName] {
		return
	}
	// A broad workspace is intentionally excluded, not an initialization
	// failure or an incomplete snapshot. This immutable policy check must not
	// wait on the manager's scan mutex outside the bounded stage context.
	if l.Checkpointer.AutomaticDisabled() {
		return
	}
	// A first snapshot may cover a multi-project workspace. It has a larger
	// bounded budget; subsequent scans use incremental mirror writes. Both
	// still inherit the user's cancellation/deadline without detaching work.
	started := time.Now()
	stageCtx, cancel := context.WithTimeout(ctx, l.checkpointBudget(phase))
	defer cancel()
	var progressMu sync.Mutex
	var lastProgress time.Time
	var lastStage string
	stageCtx = checkpoint.WithProgress(stageCtx, func(progress checkpoint.SnapshotProgress) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if time.Since(started) < 2*time.Second || (progress.Stage == lastStage && !lastProgress.IsZero() && time.Since(lastProgress) < 5*time.Second) {
			return
		}
		lastProgress = time.Now()
		lastStage = progress.Stage
		// Progress must not make a busy UI or disconnected consumer delay the
		// snapshot. These are local scalar statistics, never file contents.
		select {
		case out <- Event{Kind: EventInfo, Info: fmt.Sprintf("code checkpoint %s: stage=%s elapsed=%.1fs files=%d processed=%d copied=%d unchanged=%d", phase, progress.Stage, time.Since(started).Seconds(), progress.Files, progress.ProcessedFiles, progress.CopiedFiles, progress.UnchangedFiles)}:
		default:
		}
	})
	if err := run(stageCtx); err != nil {
		// Cancellation before execution means no new tool was started. A budget
		// failure while the turn is live still permits execution, and therefore
		// creates a real protection gap even if there is no baseline hash.
		if phase == "after tool" || ctx.Err() == nil {
			if !l.markCheckpointIncomplete() {
				return // Report the original protection gap once per turn.
			}
		}
		warningCtx, cancelWarning := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
		defer cancelWarning()
		emit(warningCtx, out, Event{Kind: EventInfo, Info: fmt.Sprintf(
			"checkpoint %s skipped: %v; code rewind protection may be incomplete for this invocation", phase, err,
		)})
	} else if elapsed := time.Since(started); elapsed >= 2*time.Second {
		select {
		case out <- Event{Kind: EventInfo, Info: fmt.Sprintf("code checkpoint %s completed in %.1fs", phase, elapsed.Seconds())}:
		default:
		}
	}
}

func (l *Loop) markCheckpointIncomplete() bool {
	turns := l.CountTurns()
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	for index := len(l.ckptStack) - 1; index >= 0; index-- {
		if l.ckptStack[index].restoreToTurns == turns-1 {
			newlyIncomplete := !l.ckptStack[index].incomplete
			l.ckptStack[index].incomplete = true
			l.ckptSnappedAt = turns
			return newlyIncomplete
		}
	}
	// An unavailable pre-snapshot must not be replaced later in the same turn
	// by a post-mutation snapshot advertised as the original pre-turn state.
	l.ckptStack = append(l.ckptStack, ckptEntry{restoreToTurns: turns - 1, label: fmt.Sprintf("before turn %d", turns), incomplete: true})
	l.ckptSnappedAt = turns
	return true
}

func checkpointCompleteFromTurn(stack []ckptEntry, turn int) bool {
	for _, entry := range stack {
		if entry.restoreToTurns >= turn-1 && entry.incomplete {
			return false
		}
	}
	return true
}

// RewindResult describes what a Rewind did, for the UI to report.
type RewindResult struct {
	Label                string
	Prompt               string
	Summary              string
	TurnsUndone          int
	CodeRestored         bool
	ConversationRestored bool
}

// RewindScope selects which part of a historical point should be restored.
// It is a bit-mask so the combined action can share the same transaction path
// as the two independent actions exposed by Claude Code's rewind dialog.
type RewindScope uint8

const (
	RewindConversation RewindScope = 1 << iota
	RewindCode
	RewindCodeAndConversation = RewindConversation | RewindCode
)

// RewindPoint is one user-visible message boundary in the current
// conversation. Turn is one-based. Selecting it restores the conversation to
// immediately before Prompt, so Prompt can be put back into the composer.
//
// HasCodeCheckpoint describes whether a materialized file snapshot exists at
// or after this boundary. A false value can also mean that no file edit
// happened after the point, in which case a code restore is a valid no-op.
type RewindPoint struct {
	Turn              int
	Prompt            string
	HasCodeCheckpoint bool
	LatestEdit        bool
}

// RewindSummaryPlan is an immutable snapshot of the conversation suffix that
// an asynchronous summarizer should process. Its internals stay private so a
// caller can carry the plan through a tea.Cmd without accidentally changing
// the compare-and-swap precondition used at commit time.
type RewindSummaryPlan struct {
	history      []llm.Message
	messageIndex int
	turn         int
	prompt       string
	compactor    *Compactor
}

var (
	ErrInvalidRewindPoint    = errors.New("rewind: selected conversation point no longer exists")
	ErrCheckpointUnavailable = errors.New("rewind: code checkpointing is unavailable")
	ErrCheckpointIncomplete  = fmt.Errorf("%w: a checkpoint was cancelled or incomplete; conversation-only rewind remains available", ErrCheckpointUnavailable)
	ErrConversationChanged   = errors.New("rewind: conversation changed while preparing the operation")
	ErrSummarizerUnavailable = errors.New("rewind: conversation summarizer is unavailable")
	ErrInvalidRewindScope    = errors.New("rewind: invalid restore scope")
)

// RewindPoints returns every real user prompt in the active history, newest
// first. File snapshots are sparse (taken only before an edit), while message
// points are dense; the UI can therefore always offer conversation-only and
// summary actions even for turns that did not touch code.
func (l *Loop) RewindPoints() []RewindPoint {
	if l == nil {
		return nil
	}
	history := l.History()

	l.ckptMu.Lock()
	stack := append([]ckptEntry(nil), l.ckptStack...)
	l.ckptMu.Unlock()

	latestEditTurn := 0
	if len(stack) > 0 {
		latestEditTurn = stack[len(stack)-1].restoreToTurns + 1
	}
	points := make([]RewindPoint, 0, transcript.CountTurns(history))
	turn := 0
	for _, message := range history {
		prompt, ok := rewindPrompt(message)
		if !ok {
			continue
		}
		turn++
		hash := checkpointHashForTurn(stack, turn)
		// A boundary newer than the latest edit already has the requested
		// code state, so code restore is an available, proven no-op. With no
		// snapshot history at all, absence is not evidence and remains
		// unavailable.
		codeAvailable := l.Checkpointer != nil && !l.Checkpointer.DisabledByPolicy() && checkpointScopeMatches(stack, turn, l.Checkpointer.ScopeKey()) &&
			(hash != "" || (latestEditTurn > 0 && turn > latestEditTurn)) && checkpointCompleteFromTurn(stack, turn)
		points = append(points, RewindPoint{
			Turn:              turn,
			Prompt:            prompt,
			HasCodeCheckpoint: codeAvailable,
			LatestEdit:        turn == latestEditTurn,
		})
	}
	for left, right := 0, len(points)-1; left < right; left, right = left+1, right-1 {
		points[left], points[right] = points[right], points[left]
	}
	return points
}

// RewindToTurn applies one of the three restore scopes to an arbitrary user
// prompt. Code is restored first so the combined operation never truncates
// the conversation when the file restore fails.
func (l *Loop) RewindToTurn(turn int, scope RewindScope) (RewindResult, error) {
	if l == nil || (scope != RewindConversation && scope != RewindCode && scope != RewindCodeAndConversation) {
		return RewindResult{}, ErrInvalidRewindScope
	}
	return l.rewindToTurnExpectedPersist(l.History(), turn, scope, nil)
}

// RewindToTurnWithPersist coordinates the durable conversation replacement
// with the in-memory/code transaction. The target history is persisted before
// any file mutation; a later file failure appends the original history again
// before returning, so resume and the live Loop do not fork on known errors.
func (l *Loop) RewindToTurnWithPersist(turn int, scope RewindScope, persist func([]llm.Message) error) (RewindResult, error) {
	if l == nil {
		return RewindResult{}, ErrInvalidRewindScope
	}
	return l.rewindToTurnExpectedPersist(l.History(), turn, scope, persist)
}

// rewindToTurnExpected performs the operation only if the conversation still
// equals expected. For a combined restore, the CAS is checked while holding
// l.mu before touching files, and the lock is retained until both parts have
// committed. A known conversation conflict therefore causes zero file
// mutation instead of the former half-rewind state.
func (l *Loop) rewindToTurnExpected(expected []llm.Message, turn int, scope RewindScope) (RewindResult, error) {
	return l.rewindToTurnExpectedPersist(expected, turn, scope, nil)
}

func (l *Loop) rewindToTurnExpectedPersist(expected []llm.Message, turn int, scope RewindScope, persist func([]llm.Message) error) (RewindResult, error) {
	if l == nil || (scope != RewindConversation && scope != RewindCode && scope != RewindCodeAndConversation) {
		return RewindResult{}, ErrInvalidRewindScope
	}
	if scope&RewindCode != 0 && l.Checkpointer != nil {
		release, _, err := l.Checkpointer.BeginRun()
		if err != nil {
			return RewindResult{}, err
		}
		defer release()
	}
	messageIndex, prompt, ok := rewindTarget(expected, turn)
	if !ok {
		return RewindResult{}, ErrInvalidRewindPoint
	}
	turnsBefore := transcript.CountTurns(expected)
	result := RewindResult{
		Label:  fmt.Sprintf("before turn %d", turn),
		Prompt: prompt,
	}

	var (
		codeHash  string
		codePaths map[string]string
		codeNoop  bool
	)
	if scope&RewindCode != 0 {
		if err := l.checkpointUnavailableReason(); err != nil {
			return RewindResult{}, err
		}
		l.ckptMu.Lock()
		stack := append([]ckptEntry(nil), l.ckptStack...)
		l.ckptMu.Unlock()
		if !checkpointScopeMatches(stack, turn, l.Checkpointer.ScopeKey()) {
			return RewindResult{}, fmt.Errorf("%w: 检查点的项目范围已改变，请先恢复原保护范围", ErrCheckpointUnavailable)
		}
		if !checkpointCompleteFromTurn(stack, turn) {
			return RewindResult{}, ErrCheckpointIncomplete
		}
		codeHash, codePaths = checkpointRestoreForTurn(stack, turn)
		if codeHash == "" {
			latestEditTurn := 0
			if len(stack) > 0 {
				latestEditTurn = stack[len(stack)-1].restoreToTurns + 1
			}
			codeNoop = latestEditTurn > 0 && turn > latestEditTurn
			if !codeNoop {
				return RewindResult{}, ErrCheckpointUnavailable
			}
		}
	}

	if scope&RewindConversation != 0 {
		l.mu.Lock()
		if !reflect.DeepEqual(l.Messages, expected) {
			l.mu.Unlock()
			return RewindResult{}, ErrConversationChanged
		}
		replacement := RepairOrphanedToolUses(append([]llm.Message(nil), expected[:messageIndex]...))
		persisted := false
		if persist != nil {
			if err := persist(replacement); err != nil {
				l.mu.Unlock()
				return RewindResult{}, err
			}
			persisted = true
		}
		if scope&RewindCode != 0 && !codeNoop {
			if err := l.Checkpointer.RestorePathStates(codeHash, codePaths); err != nil {
				if persisted {
					if rollbackErr := persist(expected); rollbackErr != nil {
						err = errors.Join(err, fmt.Errorf("rewind: persistence rollback failed: %w", rollbackErr))
					}
				}
				l.mu.Unlock()
				return RewindResult{}, err
			}
			result.CodeRestored = true
			l.refreshCheckpointPathStates(codePaths)
		} else if scope&RewindCode != 0 {
			result.CodeRestored = true
		}
		l.restoreMessagesLocked(replacement)
		l.storeContextEstimateFromHistory(estimateTokens(l.Messages))
		l.mu.Unlock()
		result.ConversationRestored = true
		result.TurnsUndone = turnsBefore - (turn - 1)
		l.discardCheckpointMappingsFrom(turn)
	} else if scope&RewindCode != 0 {
		if !codeNoop {
			if err := l.Checkpointer.RestorePathStates(codeHash, codePaths); err != nil {
				return RewindResult{}, err
			}
			l.refreshCheckpointPathStates(codePaths)
		}
		result.CodeRestored = true
	}
	return result, nil
}

// SummarizeFromTurn replaces the selected prompt and everything after it with
// a focused LLM summary, leaving files untouched. The original prompt is
// returned for the TUI composer, matching Claude Code's "Summarize from here"
// workflow.
func (l *Loop) SummarizeFromTurn(ctx context.Context, turn int) (RewindResult, error) {
	plan, err := l.PrepareSummarizeFromTurn(turn)
	if err != nil {
		return RewindResult{}, err
	}
	summary, err := l.GenerateRewindSummary(ctx, plan)
	if err != nil {
		return RewindResult{}, err
	}
	return l.CommitRewindSummary(plan, summary)
}

// PrepareSummarizeFromTurn captures the cheap validation and CAS baseline on
// the caller goroutine. It never invokes the provider.
func (l *Loop) PrepareSummarizeFromTurn(turn int) (*RewindSummaryPlan, error) {
	if l == nil {
		return nil, ErrSummarizerUnavailable
	}
	// Capture the replaceable compactor binding and its matching history under
	// one lock. A concurrent model rebind may retire this compactor after the
	// snapshot, but the immutable provider/config binding remains valid for the
	// prepared operation and cannot be mixed with a different history read.
	l.mu.Lock()
	compactor := l.Compactor
	history := transcript.Snapshot(l.Messages)
	l.mu.Unlock()
	if compactor == nil {
		return nil, ErrSummarizerUnavailable
	}
	messageIndex, prompt, ok := rewindTarget(history, turn)
	if !ok {
		return nil, ErrInvalidRewindPoint
	}
	return &RewindSummaryPlan{history: history, messageIndex: messageIndex, turn: turn, prompt: prompt, compactor: compactor}, nil
}

// GenerateRewindSummary is provider-only work and is safe to run in a
// Bubble Tea command. It does not mutate Loop state.
func (l *Loop) GenerateRewindSummary(ctx context.Context, plan *RewindSummaryPlan) (string, error) {
	if l == nil || plan == nil || plan.compactor == nil {
		return "", ErrSummarizerUnavailable
	}
	return plan.compactor.SummarizeSegment(ctx, plan.history[plan.messageIndex:], "")
}

// CommitRewindSummary applies a previously generated summary only when the
// history still matches the plan captured before the provider call.
func (l *Loop) CommitRewindSummary(plan *RewindSummaryPlan, summary string) (RewindResult, error) {
	return l.CommitRewindSummaryWithPersist(plan, summary, nil)
}

// CommitRewindSummaryWithPersist writes the replacement snapshot before
// swapping Loop history. A persistence failure therefore leaves both the live
// conversation and the resumable session unchanged.
func (l *Loop) CommitRewindSummaryWithPersist(plan *RewindSummaryPlan, summary string, persist func([]llm.Message) error) (RewindResult, error) {
	if l == nil || plan == nil {
		return RewindResult{}, ErrInvalidRewindPoint
	}
	replacement := append([]llm.Message(nil), plan.history[:plan.messageIndex]...)
	// A summary boundary is assistant-authored. When the retained prefix is
	// empty or already ends with assistant, bridge it with a synthetic user
	// attachment so strict providers never receive assistant→assistant (or an
	// assistant-first history). transcript.VisibleUserText strips this marker,
	// so it does not become a fake rewind point or inflate CountTurns.
	if len(replacement) == 0 || replacement[len(replacement)-1].Role == llm.RoleAssistant {
		replacement = append(replacement, llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type: "text",
				Text: "<system-reminder>Conversation content after the selected rewind point was summarized.</system-reminder>",
			}},
		})
	}
	replacement = append(replacement, llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: "[Conversation from here summarized: " + summary + "]",
		}},
	})

	l.mu.Lock()
	if !reflect.DeepEqual(l.Messages, plan.history) {
		l.mu.Unlock()
		return RewindResult{}, ErrConversationChanged
	}
	if persist != nil {
		if err := persist(replacement); err != nil {
			l.mu.Unlock()
			return RewindResult{}, err
		}
	}
	l.restoreMessagesLocked(replacement)
	l.storeContextEstimateFromHistory(estimateTokens(l.Messages))
	l.mu.Unlock()
	l.discardCheckpointMappingsFrom(plan.turn)

	return RewindResult{
		Label:       fmt.Sprintf("from turn %d", plan.turn),
		Prompt:      plan.prompt,
		Summary:     summary,
		TurnsUndone: transcript.CountTurns(plan.history) - (plan.turn - 1),
	}, nil
}

func rewindPrompt(message llm.Message) (string, bool) {
	if message.Role != llm.RoleUser {
		return "", false
	}
	var parts []string
	for _, block := range message.Content {
		if block.Type != "text" {
			continue
		}
		if visible := transcript.VisibleUserText(block.Text); visible != "" {
			parts = append(parts, visible)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func rewindTarget(history []llm.Message, targetTurn int) (messageIndex int, prompt string, ok bool) {
	if targetTurn < 1 {
		return 0, "", false
	}
	turn := 0
	for index, message := range history {
		visible, isPrompt := rewindPrompt(message)
		if !isPrompt {
			continue
		}
		turn++
		if turn == targetTurn {
			return index, visible, true
		}
	}
	return 0, "", false
}

// checkpointHashForTurn returns the first materialized code state at or after
// the selected message boundary. If that turn itself did not edit files, the
// next editing turn's pre-edit snapshot is exactly the same desired state.
func checkpointHashForTurn(stack []ckptEntry, turn int) string {
	for _, entry := range stack {
		if entry.restoreToTurns >= turn-1 {
			return entry.hash
		}
	}
	return ""
}

func checkpointRestoreForTurn(stack []ckptEntry, turn int) (string, map[string]string) {
	hash := checkpointHashForTurn(stack, turn)
	paths := make(map[string]string)
	for _, entry := range stack {
		if entry.restoreToTurns < turn-1 {
			continue
		}
		for path, state := range entry.managedPaths {
			paths[path] = state
		}
	}
	return hash, paths
}

func (l *Loop) refreshCheckpointPathStates(paths map[string]string) {
	if len(paths) == 0 || l.Checkpointer == nil {
		return
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	states, err := l.Checkpointer.CapturePathStates(names)
	if err != nil {
		return
	}
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	for index := range l.ckptStack {
		for path, state := range states {
			if _, tracked := l.ckptStack[index].managedPaths[path]; tracked {
				l.ckptStack[index].managedPaths[path] = state
			}
		}
	}
}

func (l *Loop) discardCheckpointMappingsFrom(turn int) {
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	keep := l.ckptStack[:0]
	discardedIncomplete := false
	for _, entry := range l.ckptStack {
		if entry.restoreToTurns < turn-1 {
			keep = append(keep, entry)
		} else if entry.incomplete {
			discardedIncomplete = true
		}
	}
	// Conversation-only rewind/summary does not restore files. Do not let it
	// erase a code-protection gap and make older checkpoints look complete.
	// Carry the barrier at the last retained message boundary; new later turns
	// may still establish a fresh baseline from the actual current files.
	if discardedIncomplete && len(keep) > 0 {
		keep = append(keep, ckptEntry{restoreToTurns: turn - 2, label: "incomplete code state after conversation rewind", incomplete: true})
	}
	l.ckptStack = keep
	l.ckptSnappedAt = -1
}

// recordCheckpointMutation extends the deletion-safe managed-path union after
// a successful tool. Bash is attributed from the actual file delta, never by
// guessing file names or read-only behavior from its command string.
func (l *Loop) recordCheckpointMutation(toolName string, input map[string]any, res *tools.Result, err error) {
	_ = l.recordCheckpointMutationContext(context.Background(), toolName, input, res, err)
}

func (l *Loop) recordCheckpointMutationContext(ctx context.Context, toolName string, input map[string]any, res *tools.Result, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if l == nil || l.Checkpointer == nil || l.Checkpointer.AutomaticDisabled() || err != nil || (res != nil && res.IsError) {
		return nil
	}
	l.ckptMu.Lock()
	if len(l.ckptStack) == 0 {
		l.ckptMu.Unlock()
		return nil
	}
	entryIndex := len(l.ckptStack) - 1
	entry := l.ckptStack[entryIndex]
	l.ckptMu.Unlock()
	if entry.incomplete || entry.hash == "" || entry.restoreToTurns != l.CountTurns()-1 {
		return ErrCheckpointIncomplete
	}

	var paths []string
	switch toolName {
	case "Bash":
		changed, changedErr := l.Checkpointer.ChangedPathsContext(ctx, entry.hash)
		if changedErr != nil {
			return changedErr
		}
		paths = changed
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if path, _ := input["path"].(string); path != "" {
			paths = append(paths, path)
		} else if path, _ := input["file_path"].(string); path != "" {
			paths = append(paths, path)
		}
	default:
		return nil
	}
	if len(paths) == 0 {
		return nil
	}
	if toolName != "Bash" {
		protected := paths[:0]
		for _, path := range paths {
			if l.Checkpointer.Protects(path) {
				protected = append(protected, path)
			}
		}
		paths = protected
		if len(paths) == 0 {
			return nil
		}
	}
	states, stateErr := l.Checkpointer.CapturePathStatesContext(ctx, paths)
	if stateErr != nil {
		return stateErr
	}
	// ChangedPaths yields unique relative paths; explicit file tools yield one
	// path. A filtered symlink or unsupported file must not turn a partial
	// fingerprint set into an apparently complete restore.
	if len(states) != len(paths) {
		return fmt.Errorf("%w: could not capture every changed file", ErrCheckpointIncomplete)
	}
	if err := l.Checkpointer.RecordManagedPathsContext(ctx, paths); err != nil {
		return err
	}
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	if entryIndex >= len(l.ckptStack) || l.ckptStack[entryIndex].restoreToTurns != entry.restoreToTurns ||
		(l.ckptStack[entryIndex].hash != entry.hash && !l.Checkpointer.Sparse()) {
		return nil
	}
	if l.ckptStack[entryIndex].managedPaths == nil {
		l.ckptStack[entryIndex].managedPaths = make(map[string]string)
	}
	for path, state := range states {
		l.ckptStack[entryIndex].managedPaths[path] = state
	}
	return nil
}

// Rewind restores the working tree to the most recent pre-edit snapshot
// AND undoes the conversation back to that point. Returns ok=false when
// checkpointing is off or there's nothing to rewind. Mirrors claude-code's
// Esc-Esc: files and transcript move together.
func (l *Loop) Rewind() (RewindResult, bool) {
	result, err := l.RewindWithPersist(nil)
	return result, err == nil
}

// RewindWithPersist is the transactional legacy "last edit" shortcut.
func (l *Loop) RewindWithPersist(persist func([]llm.Message) error) (RewindResult, error) {
	if err := l.checkpointUnavailableReason(); err != nil {
		return RewindResult{}, err
	}
	l.ckptMu.Lock()
	if len(l.ckptStack) == 0 {
		l.ckptMu.Unlock()
		return RewindResult{}, ErrCheckpointUnavailable
	}
	e := l.ckptStack[len(l.ckptStack)-1]
	l.ckptMu.Unlock()
	result, err := l.RewindToTurnWithPersist(e.restoreToTurns+1, RewindCodeAndConversation, persist)
	if err != nil {
		return RewindResult{}, err
	}
	result.Label = e.label
	return result, nil
}

func (l *Loop) checkpointUnavailableReason() error {
	if l == nil || l.Checkpointer == nil {
		return ErrCheckpointUnavailable
	}
	if err := l.Checkpointer.RefreshControl(); err != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointUnavailable, err)
	}
	if reason := l.Checkpointer.DisabledReason(); reason != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointUnavailable, reason)
	}
	return nil
}

// HasRewindPoints reports whether there's at least one snapshot to rewind
// to — lets the TUI show /rewind only when it would do something.
func (l *Loop) HasRewindPoints() bool {
	if len(l.RewindPoints()) > 0 {
		return true
	}
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	return len(l.ckptStack) > 0
}

// argsHash is a short stable digest of a tool's input, stored in the
// checkpoint commit message for debugging (which edit triggered it).
func argsHash(input map[string]any) string {
	b, err := json.Marshal(input)
	if err != nil {
		return "?"
	}
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])[:8]
}
