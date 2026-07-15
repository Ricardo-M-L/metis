package agent

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestResetSessionClearsSessionScopedState(t *testing.T) {
	loop := NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 5)
	loop.BypassNextCache = true
	loop.estTokens.Store(1234)
	loop.todoWriteIter = 9
	loop.todoReminderIter = 8
	loop.todoReconciledThisTurn = true
	loop.haltRequested = true
	loop.haltReason = "old halt"
	loop.steerBuf = []string{"old steer"}
	loop.compactCircuitNoticeSent = true
	loop.lastAutoMemoryTurn = 7
	loop.discoveredMCP = map[string]bool{"mcp__old": true}
	loop.discoveredMCPHydrated = true
	loop.contract = contractTracker{
		mainWrites: 6, agentDispatches: 10, verifyDispatched: true,
		reminderFired: true, gateAttempts: 2, lastVerifyVerdict: "FAIL", verdictGateAttempts: 2,
	}
	oldSubAgentNotify := loop.subAgentNotify
	loop.subAgentNotify <- SubAgentNotification{}
	jobNotify := make(chan jobs.Notification, 1)
	jobNotify <- jobs.Notification{JobID: "old-job"}
	loop.JobNotify = jobNotify

	loop.Compactor = &Compactor{
		consecutiveFailures: MaxConsecutiveCompactFailures,
		LastSummary:         "old summary",
	}
	loop.CacheStats = NewCacheStatsRing(3)
	loop.CacheStats.Add(CacheStat{Turn: 4, Input: 100})
	loop.Detector = NewLoopDetector()
	loop.Detector.WarningThreshold = 17
	loop.Detector.callCounts["Bash"] = 9
	loop.Detector.toolSeq = []string{"Bash", "Read"}
	loop.Detector.globalCount = 22
	loop.Detector.pollPatterns["Read"] = 3
	loop.Detector.pingPongPairs["Read->Bash"] = 4
	loop.Detector.signatureWindow = []string{"old-signature"}
	loop.Detector.signatureTripped = true

	tracker := budget.NewTracker(1, budget.Rates{InputPerMTok: 1})
	tracker.AddUsage(900_000, 0, 0, 0)
	if tracker.TakeWarning() == "" {
		t.Fatal("precondition: budget warning should have fired")
	}
	loop.Budget = tracker

	target := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: "tool_use", ToolUseID: "search-target", ToolName: "ToolSearch",
		}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{
			Type: "tool_result", ToolUseID: "search-target",
			ToolResult: `{"matches":[{"name":"mcp__target"}]}`,
		}}},
	}

	before := time.Now()
	loop.ResetSession(target)

	if got := loop.History(); len(got) != len(target) || got[0].Content[0].ToolUseID != "search-target" {
		t.Fatalf("target history was not restored: %+v", got)
	}
	if loop.estTokens.Load() != 0 || loop.todoWriteIter != 0 || loop.todoReminderIter != 0 || loop.todoReconciledThisTurn {
		t.Errorf("turn-derived counters leaked: est=%d todo=(%d,%d,%v)", loop.estTokens.Load(), loop.todoWriteIter, loop.todoReminderIter, loop.todoReconciledThisTurn)
	}
	if loop.haltRequested || loop.haltReason != "" || len(loop.steerBuf) != 0 || loop.BypassNextCache {
		t.Errorf("turn controls leaked: halt=%v reason=%q steer=%v bypass=%v", loop.haltRequested, loop.haltReason, loop.steerBuf, loop.BypassNextCache)
	}
	if loop.compactCircuitNoticeSent || loop.lastAutoMemoryTurn != 0 || loop.lastTimeBasedMicrocompactAt.Before(before) {
		t.Errorf("compaction/memory state not reset")
	}
	if loop.contract != (contractTracker{}) {
		t.Errorf("dispatch contract leaked: %+v", loop.contract)
	}
	if len(loop.subAgentNotify) != 0 {
		t.Errorf("stale sub-agent notifications were not drained")
	}
	if loop.subAgentNotify == oldSubAgentNotify {
		t.Error("sub-agent notification channel was not rotated at the session boundary")
	}
	if len(jobNotify) != 0 {
		t.Error("stale background-job notifications were not drained")
	}
	if loop.Compactor.CircuitTripped() || loop.Compactor.LastSummary != "" {
		t.Errorf("compactor circuit leaked: failures=%d summary=%q", loop.Compactor.consecutiveFailures, loop.Compactor.LastSummary)
	}
	if got := loop.CacheStats.Snapshot(); len(got) != 0 || loop.CacheStats.cap != 3 {
		t.Errorf("cache stats not reset while preserving capacity: cap=%d stats=%+v", loop.CacheStats.cap, got)
	}
	if loop.Detector.WarningThreshold != 17 {
		t.Errorf("detector configuration was reset: threshold=%d", loop.Detector.WarningThreshold)
	}
	if len(loop.Detector.callCounts) != 0 || len(loop.Detector.toolSeq) != 0 || loop.Detector.globalCount != 0 ||
		len(loop.Detector.pollPatterns) != 0 || len(loop.Detector.pingPongPairs) != 0 ||
		len(loop.Detector.signatureWindow) != 0 || loop.Detector.signatureTripped {
		t.Errorf("detector counters leaked: %+v", loop.Detector)
	}
	if spent := tracker.SpentUSD(); spent != 0 {
		t.Errorf("budget spend leaked: %f", spent)
	}
	tracker.AddUsage(900_000, 0, 0, 0)
	if tracker.TakeWarning() == "" {
		t.Error("budget warning was not re-armed")
	}
	tracker.AddUsage(100_000, 0, 0, 0)
	if !tracker.Exceeded() {
		t.Error("budget cap/rates were not preserved")
	}

	discovered := loop.snapshotDiscoveredMCP()
	if discovered["mcp__old"] || !discovered["mcp__target"] || len(discovered) != 1 {
		t.Errorf("MCP discovery was not rebuilt from target history: %v", discovered)
	}
}
