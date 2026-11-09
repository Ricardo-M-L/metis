package agent

// contextCompactionDisplay is an immutable display-only view of the last
// committed context while the compaction transaction prepares a replacement.
type contextCompactionDisplay struct {
	usage           ContextUsage
	historyRevision uint64
	routingRevision uint64
	epoch           uint64
}

// ContextDisplaySnapshot returns status for a UI without changing the live
// request context used by hooks, pressure checks or provider requests.
func (l *Loop) ContextDisplaySnapshot() ContextUsage {
	if l == nil {
		return ContextUsage{}
	}
	if !l.mu.TryRLock() {
		if display := l.contextCompactionDisplay.Load(); display != nil && display.epoch == l.contextUsageEpoch.Load() {
			return cloneContextUsage(display.usage)
		}
		return l.ContextUsageSnapshot()
	}
	defer l.mu.RUnlock()
	if display := l.contextCompactionDisplay.Load(); display != nil &&
		display.historyRevision == l.historyRevision && display.routingRevision == l.routingRevision &&
		display.epoch == l.contextUsageEpoch.Load() {
		return cloneContextUsage(display.usage)
	}
	return l.contextUsageSnapshotLocked()
}

// DisplayContextTokens is the compact form of ContextDisplaySnapshot for
// terminal status lines. It has the same non-blocking read semantics.
func (l *Loop) DisplayContextTokens() int {
	return l.ContextDisplaySnapshot().Used
}

// All lifecycle writes run under mu. Only the immutable pointer is read by
// render paths, so a busy history writer cannot hold up a UI frame.
func (l *Loop) beginContextCompactionDisplayLocked() ContextUsage {
	usage := l.contextUsageSnapshotLocked()
	display := &contextCompactionDisplay{usage: cloneContextUsage(usage),
		historyRevision: l.historyRevision, routingRevision: l.routingRevision,
		epoch: l.contextUsageEpoch.Load()}
	display.usage.CompactionStage = "summarizing"
	l.contextCompactionDisplay.Store(display)
	return usage
}

func (l *Loop) advanceContextCompactionDisplayLocked(stage string, previousEpoch uint64) {
	prior := l.contextCompactionDisplay.Load()
	if prior == nil || prior.epoch != previousEpoch || prior.routingRevision != l.routingRevision {
		return
	}
	next := *prior
	next.historyRevision = l.historyRevision
	next.epoch = l.contextUsageEpoch.Load()
	next.usage.CompactionStage = stage
	l.contextCompactionDisplay.Store(&next)
}

// Recomputing a request prefix does not commit a candidate. Keep an already
// valid display freeze alive across that cache refresh, but never resurrect a
// display invalidated by a session, history, identity or route replacement.
func (l *Loop) refreshContextCompactionDisplayPrefixLocked(previousEpoch uint64) {
	prior := l.contextCompactionDisplay.Load()
	if prior == nil || prior.epoch != previousEpoch ||
		prior.historyRevision != l.historyRevision || prior.routingRevision != l.routingRevision {
		return
	}
	next := *prior
	next.epoch = l.contextUsageEpoch.Load()
	l.contextCompactionDisplay.Store(&next)
}

func (l *Loop) finishContextCompactionDisplay() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.contextCompactionDisplay.Store(nil)
	// Prime the complete committed reading before success/end callbacks or a
	// busy next writer asks the UI to paint another frame.
	l.contextUsageSnapshotLocked()
}
