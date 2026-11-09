package session

import "time"

type TraceRequestTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type TraceSystemSection struct {
	Name     string `json:"name"`
	Body     string `json:"body"`
	Cache    bool   `json:"cache"`
	Volatile bool   `json:"volatile"`
}

// TraceRequestContext stores the first observed root logical request, rather
// than an historical Header.System that may predate prompt assembly. It never
// implies that translated HTTP request bytes were captured.
type TraceRequestContext struct {
	Available      bool                 `json:"available"`
	System         string               `json:"system"`
	Tools          []TraceRequestTool   `json:"tools"`
	SystemSections []TraceSystemSection `json:"systemSections,omitempty"`
	Source         string               `json:"source"`
	CapturedAt     time.Time            `json:"capturedAt,omitempty"`
	Turn           int                  `json:"turn,omitempty"`
	RequestID      string               `json:"requestID,omitempty"`
	Model          string               `json:"model,omitempty"`
	Provider       string               `json:"provider,omitempty"`
	Truncated      bool                 `json:"truncated,omitempty"`
	Limitation     string               `json:"limitation,omitempty"`
}
