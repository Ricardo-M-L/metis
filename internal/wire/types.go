package wire

import "encoding/json"

// WireMessageEnvelope wraps all messages sent over the wire
type WireMessageEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Event is the base interface for all wire events
type Event interface {
	GetType() string
}

// ContentPart represents a part of content (text, think, image, audio, video)
type ContentPart interface {
	Mark() string
}

// TextPart represents a text content part
type TextPart struct {
	Text string `json:"text"`
}

func (t TextPart) Mark() string { return "text" }

// ThinkPart represents a thinking content part
type ThinkPart struct {
	Think string `json:"think"`
}

func (t ThinkPart) Mark() string { return "think" }

// ImageURLPart represents an image content part
type ImageURLPart struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	AltText  string `json:"alt_text,omitempty"`
}

func (i ImageURLPart) Mark() string { return "image_url" }

// AudioURLPart represents an audio content part
type AudioURLPart struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

func (a AudioURLPart) Mark() string { return "audio_url" }

// VideoURLPart represents a video content part
type VideoURLPart struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

func (v VideoURLPart) Mark() string { return "video_url" }

// TurnBegin event
type TurnBegin struct {
	Type string `json:"type"`
}

func (t TurnBegin) GetType() string { return "turn_begin" }

// TurnEnd event
type TurnEnd struct {
	Type string `json:"type"`
}

func (t TurnEnd) GetType() string { return "turn_end" }

// StepBegin event
type StepBegin struct {
	Type string `json:"type"`
}

func (s StepBegin) GetType() string { return "step_begin" }

// StepInterrupted event
type StepInterrupted struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

func (s StepInterrupted) GetType() string { return "step_interrupted" }

// StepRetry event
type StepRetry struct {
	Type   string `json:"type"`
	Reason string `json:"reason,omitempty"`
}

func (s StepRetry) GetType() string { return "step_retry" }

// CompactionBegin event
type CompactionBegin struct {
	Type string `json:"type"`
}

func (c CompactionBegin) GetType() string { return "compaction_begin" }

// CompactionEnd event
type CompactionEnd struct {
	Type string `json:"type"`
}

func (c CompactionEnd) GetType() string { return "compaction_end" }

// MCPLoadingBegin event
type MCPLoadingBegin struct {
	Type    string   `json:"type"`
	Servers []string `json:"servers,omitempty"`
}

func (m MCPLoadingBegin) GetType() string { return "mcp_loading_begin" }

// MCPLoadingEnd event
type MCPLoadingEnd struct {
	Type    string   `json:"type"`
	Servers []string `json:"servers,omitempty"`
}

func (m MCPLoadingEnd) GetType() string { return "mcp_loading_end" }

// StatusUpdate event
type StatusUpdate struct {
	Type     string   `json:"type"`
	Status   string   `json:"status"`
	Progress *float64 `json:"progress,omitempty"`
	Message  string   `json:"message,omitempty"`
}

func (s StatusUpdate) GetType() string { return "status_update" }

// Notification event
type Notification struct {
	Type    string `json:"type"`
	Level   string `json:"level,omitempty"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

func (n Notification) GetType() string { return "notification" }

// FunctionCall represents a function call
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall represents a tool call request
type ToolCall struct {
	ID       string       `json:"id"`
	Function FunctionCall `json:"function"`
}

func (t ToolCall) GetType() string { return "tool_call" }

// ToolCallRequest event
type ToolCallRequest struct {
	Type     string   `json:"type"`
	ToolCall ToolCall `json:"tool_call"`
}

func (t ToolCallRequest) GetType() string { return "tool_call_request" }

// ToolCallPart represents partial tool call arguments
type ToolCallPart struct {
	ID        string `json:"id"`
	Arguments string `json:"arguments,omitempty"`
}

func (t ToolCallPart) Mark() string { return "tool_call_part" }

// ToolResult represents a tool execution result
type ToolResult struct {
	Type        string `json:"type"`
	ToolCallID  string `json:"tool_call_id"`
	ReturnValue string `json:"return_value,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
}

func (t ToolResult) GetType() string { return "tool_result" }

// ApprovalRequest event
type ApprovalRequest struct {
	Type      string    `json:"type"`
	RequestID string    `json:"request_id,omitempty"`
	Action    string    `json:"action,omitempty"`
	Message   string    `json:"message,omitempty"`
	ToolCall  *ToolCall `json:"tool_call,omitempty"`
}

func (a ApprovalRequest) GetType() string { return "approval_request" }

// ApprovalResponse event
type ApprovalResponse struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	Approved  bool   `json:"approved"`
	Response  string `json:"response,omitempty"`
}

func (a ApprovalResponse) GetType() string { return "approval_response" }

// QuestionRequest event
type QuestionRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	Question  string `json:"question,omitempty"`
}

func (q QuestionRequest) GetType() string { return "question_request" }

// HookRequest event
type HookRequest struct {
	Type     string                 `json:"type"`
	HookName string                 `json:"hook_name,omitempty"`
	Params   map[string]interface{} `json:"params,omitempty"`
}

func (h HookRequest) GetType() string { return "hook_request" }

// SteerInput event
type SteerInput struct {
	Type  string `json:"type"`
	Input string `json:"input,omitempty"`
}

func (s SteerInput) GetType() string { return "steer_input" }

// HookTriggered event
type HookTriggered struct {
	Type     string `json:"type"`
	HookName string `json:"hook_name,omitempty"`
}

func (h HookTriggered) GetType() string { return "hook_triggered" }

// HookResolved event
type HookResolved struct {
	Type     string `json:"type"`
	HookName string `json:"hook_name,omitempty"`
}

func (h HookResolved) GetType() string { return "hook_resolved" }

// PlanDisplay event
type PlanDisplay struct {
	Type string `json:"type"`
	Plan string `json:"plan,omitempty"`
}

func (p PlanDisplay) GetType() string { return "plan_display" }

// BtwBegin event (between turns)
type BtwBegin struct {
	Type string `json:"type"`
}

func (b BtwBegin) GetType() string { return "btw_begin" }

// BtwEnd event (end between turns)
type BtwEnd struct {
	Type string `json:"type"`
}

func (b BtwEnd) GetType() string { return "btw_end" }

// SubagentEvent event
type SubagentEvent struct {
	Type       string                 `json:"type"`
	SubagentID string                 `json:"subagent_id,omitempty"`
	Event      string                 `json:"event,omitempty"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

func (s SubagentEvent) GetType() string { return "subagent_event" }
