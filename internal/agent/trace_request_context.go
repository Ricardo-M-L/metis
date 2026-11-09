package agent

import "github.com/Ricardo-M-L/metis/internal/llm"

// TraceRequestContext is a synchronous, whitelisted view of the assembled
// logical request. The trace sink copies it only for a session's first root
// request. Providers can subsequently translate/reorder it for their wire API.
// Messages, image data and credentials are deliberately absent.
type TraceRequestContext struct {
	System         string
	SystemSections []llm.SystemSection
	Tools          []llm.ToolSpec
	Model          string
	Provider       string
}
