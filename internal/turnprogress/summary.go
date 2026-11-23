package turnprogress

// StartupSummary stores parent-observed startup timing. ReceivedMS is zero;
// WorkerMS through TotalMS are milestones from API receipt on the parent's
// monotonic clock. QueueMS is the total admission queue duration, and
// ExecutionQueueMS is the first execution-guard queue duration. Milestones and
// durations must not be added together. nil means unavailable, while a pointer
// to zero is observed. Worker-local HTTP timing remains in the trace sidecar.
type StartupSummary struct {
	RequestID        string `json:"requestId,omitempty"`
	Source           string `json:"source"`
	ReceivedMS       *int64 `json:"receivedMs"`
	QueueMS          *int64 `json:"queueMs"`
	ExecutionQueueMS *int64 `json:"executionQueueMs,omitempty"`
	WorkerMS         *int64 `json:"workerMs"`
	RuntimeMS        *int64 `json:"runtimeMs"`
	MCPMS            *int64 `json:"mcpMs"`
	RequestWrittenMS *int64 `json:"requestWrittenMs"`
	FirstByteMS      *int64 `json:"firstByteMs"`
	FirstThinkingMS  *int64 `json:"firstThinkingMs"`
	FirstTextMS      *int64 `json:"firstTextMs"`
	TotalMS          *int64 `json:"totalMs"`
}

// MergeStartupSummary preserves known milestones across usage-only partial
// updates. A new request identity starts a fresh summary. All pointers are
// copied so callers can safely retain or modify their incoming value.
func MergeStartupSummary(current, incoming *StartupSummary) *StartupSummary {
	if current == nil && incoming == nil {
		return nil
	}
	var out StartupSummary
	if current != nil && (incoming == nil || incoming.RequestID == "" || current.RequestID == "" || incoming.RequestID == current.RequestID) {
		out = *current
	}
	if incoming != nil {
		if incoming.RequestID != "" {
			out.RequestID = incoming.RequestID
		}
		if incoming.Source == SourceServerObserved {
			out.Source = incoming.Source
		}
		mergeMS := func(dst **int64, src *int64) {
			if src != nil && *src >= 0 {
				*dst = src
			}
		}
		mergeMS(&out.ReceivedMS, incoming.ReceivedMS)
		mergeMS(&out.QueueMS, incoming.QueueMS)
		mergeMS(&out.ExecutionQueueMS, incoming.ExecutionQueueMS)
		mergeMS(&out.WorkerMS, incoming.WorkerMS)
		mergeMS(&out.RuntimeMS, incoming.RuntimeMS)
		mergeMS(&out.MCPMS, incoming.MCPMS)
		mergeMS(&out.RequestWrittenMS, incoming.RequestWrittenMS)
		mergeMS(&out.FirstByteMS, incoming.FirstByteMS)
		mergeMS(&out.FirstThinkingMS, incoming.FirstThinkingMS)
		mergeMS(&out.FirstTextMS, incoming.FirstTextMS)
		mergeMS(&out.TotalMS, incoming.TotalMS)
	}
	for _, field := range []**int64{&out.ReceivedMS, &out.QueueMS, &out.ExecutionQueueMS, &out.WorkerMS, &out.RuntimeMS, &out.MCPMS, &out.RequestWrittenMS, &out.FirstByteMS, &out.FirstThinkingMS, &out.FirstTextMS, &out.TotalMS} {
		if *field != nil {
			value := **field
			*field = &value
		}
	}
	return &out
}
