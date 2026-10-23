package desktopipc

import "time"

// Status is a detached worker snapshot. Session ownership comes from the
// parent process's request, never from whichever session is selected in the UI.
type Status struct {
	SubAgents       int        `json:"subAgents"`
	NamedAgents     int        `json:"namedAgents"`
	BackgroundTasks int        `json:"backgroundTasks"`
	Agents          []Subagent `json:"agents"`
	Jobs            []Job      `json:"jobs"`
	// Context belongs to this worker's root session, which the parent keys by
	// its IsolatedTurnRequest.SessionID. nil means no trustworthy reading.
	Context *ContextPressure `json:"context,omitempty"`
}

type ContextBreakdown struct {
	SystemTokens  int  `json:"systemTokens"`
	ToolTokens    int  `json:"toolTokens"`
	MessageTokens int  `json:"messageTokens"`
	Estimated     bool `json:"estimated"`
}

func (b *ContextBreakdown) Valid() bool {
	return b != nil && b.Estimated && b.SystemTokens >= 0 && b.ToolTokens >= 0 && b.MessageTokens >= 0
}

type ContextPressure struct {
	Breakdown            *ContextBreakdown `json:"contextBreakdown,omitempty"`
	Used                 int               `json:"used"`
	Window               int               `json:"window"`
	WindowKind           string            `json:"windowKind,omitempty"`
	ModelWindow          int               `json:"modelWindow,omitempty"`
	ModelMaxInputTokens  int               `json:"modelMaxInputTokens,omitempty"`
	ModelMaxOutputTokens int               `json:"modelMaxOutputTokens,omitempty"`
	ModelSource          string            `json:"modelSource,omitempty"`
	RequestedWindow      int               `json:"requestedWindow,omitempty"`
	LimitSource          string            `json:"limitSource,omitempty"`
	BudgetSource         string            `json:"budgetSource,omitempty"`
	ProviderID           string            `json:"providerId,omitempty"`
	Model                string            `json:"model,omitempty"`
	RouteKey             string            `json:"routeKey,omitempty"`
	DefaultWindow        int               `json:"defaultWindow,omitempty"`
	ProfileDefaultWindow int               `json:"profileDefaultWindow,omitempty"`
	ProfileMaxWindow     int               `json:"profileMaxWindow,omitempty"`
	MaxWindow            int               `json:"maxWindow,omitempty"`
	MaxInputTokens       int               `json:"maxInputTokens,omitempty"`
	MaxOutputTokens      int               `json:"maxOutputTokens,omitempty"`
	Source               string            `json:"source,omitempty"`
	CompactThreshold     float64           `json:"compactThreshold"`
	CompactAtTokens      int               `json:"compactAtTokens"`
}

type Subagent struct {
	Name               string    `json:"name"`
	AgentID            string    `json:"agentId"`
	Anonymous          bool      `json:"anonymous"`
	Status             string    `json:"status"`
	ExecutionPhase     string    `json:"executionPhase"`
	HoldsExecutionSlot bool      `json:"holdsExecutionSlot"`
	Background         bool      `json:"background"`
	StartedAt          time.Time `json:"startedAt"`
	EndedAt            time.Time `json:"endedAt"`
	ElapsedMS          int64     `json:"elapsedMs"`
	Output             string    `json:"output"`
	OutputTruncated    bool      `json:"outputTruncated"`
	Result             string    `json:"result"`
	ResultTruncated    bool      `json:"resultTruncated"`
	StopHint           string    `json:"stopHint"`
	ExitError          string    `json:"exitError"`
}

type Job struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"startedAt"`
}
