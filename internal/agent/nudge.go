package agent

// Package nudge implements Hermes-style memory and skill nudge tracking.
// Tracks iterations since last memory update and skill creation; fires hook
// callbacks when thresholds are exceeded.

import "sync/atomic"

// Tracker holds nudge state. Atomically updated, safe for concurrent use.
type Tracker struct {
	// Counters updated atomically.
	itersSinceSkill   atomic.Int64
	itersSinceMemory  atomic.Int64
	turnsSinceCompact atomic.Int64

	// Thresholds.
	SkillNudgeInterval  int64 // trigger skill nudge after this many iterations
	MemoryNudgeInterval int64 // trigger memory nudge after this many turns
	CompactThreshold    int64 // trigger context compaction after this many turns

	// SkillNudge fires when a complex task has completed and a skill might help.
	// Handler receives the description of what the agent just did.
	SkillNudge func(description string)
	// MemoryNudge fires periodically to suggest memory updates.
	MemoryNudge func()
	// CompactNudge fires when context should be compressed.
	CompactNudge func()
}

// NewTracker returns defaults matching Hermes' behavior.
func NewTracker() *Tracker {
	return &Tracker{
		SkillNudgeInterval:  20,
		MemoryNudgeInterval: 10,
		CompactThreshold:    40,
	}
}

// OnIteration increments the iteration counter. Returns true if a skill nudge fired.
func (t *Tracker) OnIteration(usedSkill bool) bool {
	t.itersSinceSkill.Add(1)
	if usedSkill {
		t.itersSinceSkill.Store(0) // skill was actually used, reset counter
	}
	if t.SkillNudge != nil && t.itersSinceSkill.Load() >= t.SkillNudgeInterval {
		t.itersSinceSkill.Store(0)
		t.SkillNudge("complex task with multiple tool calls")
		return true
	}
	return false
}

// OnTurn increments the turn counter. Returns which nudge fired (empty = none).
func (t *Tracker) OnTurn() (memory, compact bool) {
	t.itersSinceMemory.Add(1)
	t.turnsSinceCompact.Add(1)
	if t.MemoryNudge != nil && t.itersSinceMemory.Load() >= t.MemoryNudgeInterval {
		t.itersSinceMemory.Store(0)
		t.MemoryNudge()
		memory = true
	}
	if t.CompactNudge != nil && t.turnsSinceCompact.Load() >= t.CompactThreshold {
		t.turnsSinceCompact.Store(0)
		t.CompactNudge()
		compact = true
	}
	return
}

// Reset compact counter after a successful compaction.
func (t *Tracker) ResetCompact() {
	t.turnsSinceCompact.Store(0)
}
