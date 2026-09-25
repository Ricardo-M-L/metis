// Package desktopipc defines the private, versioned Desktop worker protocol.
// It carries presentation data only; executable permission arguments and Go
// reply channels never cross the process boundary.
package desktopipc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

const (
	Version         = 1
	MaxMessageBytes = 8 << 20
	TypeEvent       = "event"
	TypeReply       = "reply"
	TypeStatus      = "status"
	TypeSteer       = "steer"
	TypeSteerResult = "steer_result"
)

type Message struct {
	Version int     `json:"version"`
	Type    string  `json:"type"`
	ID      string  `json:"id,omitempty"`
	Event   *Event  `json:"event,omitempty"`
	Status  *Status `json:"status,omitempty"`
	// Always encode the decision: an omitted field must not silently mean allow.
	Decision agent.PermissionDecision `json:"decision"`
	Answer   string                   `json:"answer,omitempty"`
	Input    string                   `json:"input,omitempty"`
	Accepted bool                     `json:"accepted"`
}

// Event deliberately lists wire fields rather than embedding agent.Event,
// whose channels and authorization-only fields cannot be serialized safely.
type Event struct {
	Kind                     agent.EventKind   `json:"kind"`
	TextDelta                string            `json:"textDelta,omitempty"`
	ContextText              string            `json:"contextText,omitempty"`
	Source                   string            `json:"source,omitempty"`
	ToolUseID                string            `json:"toolUseId,omitempty"`
	ToolName                 string            `json:"toolName,omitempty"`
	ToolInput                map[string]any    `json:"toolInput,omitempty"`
	ToolResult               *agent.ToolResult `json:"toolResult,omitempty"`
	Elapsed                  time.Duration     `json:"elapsed,omitempty"`
	ToolCalls                []agent.ToolCall  `json:"toolCalls,omitempty"`
	PermissionTool           string            `json:"permissionTool,omitempty"`
	PermissionInput          map[string]any    `json:"permissionInput,omitempty"`
	PermissionReason         string            `json:"permissionReason,omitempty"`
	PermissionPending        int               `json:"permissionPending,omitempty"`
	AskUserQuestion          string            `json:"askUserQuestion,omitempty"`
	AskUserOptions           []string          `json:"askUserOptions,omitempty"`
	AskUserAllowFreeform     bool              `json:"askUserAllowFreeform,omitempty"`
	InputTokens              int               `json:"inputTokens,omitempty"`
	OutputTokens             int               `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int               `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int               `json:"cacheReadInputTokens,omitempty"`
	PreviousContextTokens    int               `json:"previousContextTokens,omitempty"`
	ContextTokens            int               `json:"contextTokens,omitempty"`
	StopReason               string            `json:"stopReason,omitempty"`
	Info                     string            `json:"info,omitempty"`
	Error                    string            `json:"error,omitempty"`
	SubAgentParentID         string            `json:"subAgentParentId,omitempty"`
	TraceInvocationID        string            `json:"traceInvocationId,omitempty"`
	TraceParentInvocationID  string            `json:"traceParentInvocationId,omitempty"`
	TraceCallID              string            `json:"traceCallId,omitempty"`
}

func FromEvent(event agent.Event) Event {
	e := event.PresentationCopy()
	w := Event{
		Kind: e.Kind, TextDelta: e.TextDelta, ContextText: e.ContextText, Source: e.Source,
		ToolUseID: e.ToolUseID, ToolName: e.ToolName, ToolInput: e.ToolInput, ToolResult: e.ToolResult,
		Elapsed: e.Elapsed, ToolCalls: e.ToolCalls, PermissionTool: e.PermissionTool,
		PermissionInput: e.PermissionInput, PermissionReason: e.PermissionReason, PermissionPending: e.PermissionPending,
		AskUserQuestion: e.AskUserQuestion, AskUserOptions: e.AskUserOptions, AskUserAllowFreeform: e.AskUserAllowFreeform,
		InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CacheCreationInputTokens: e.CacheCreationInputTokens,
		CacheReadInputTokens: e.CacheReadInputTokens, PreviousContextTokens: e.PreviousContextTokens, ContextTokens: e.ContextTokens,
		StopReason: e.StopReason, Info: e.Info, SubAgentParentID: e.SubAgentParentID, TraceInvocationID: e.TraceInvocationID,
		TraceParentInvocationID: e.TraceParentInvocationID, TraceCallID: e.TraceCallID,
	}
	if e.Err != nil {
		w.Error = e.Err.Error()
	}
	return w
}

func (w Event) ToEvent() agent.Event {
	e := agent.Event{
		Kind: w.Kind, TextDelta: w.TextDelta, ContextText: w.ContextText, Source: w.Source,
		ToolUseID: w.ToolUseID, ToolName: w.ToolName, ToolInput: w.ToolInput, ToolResult: w.ToolResult,
		Elapsed: w.Elapsed, ToolCalls: w.ToolCalls, PermissionTool: w.PermissionTool,
		PermissionInput: w.PermissionInput, PermissionReason: w.PermissionReason, PermissionPending: w.PermissionPending,
		AskUserQuestion: w.AskUserQuestion, AskUserOptions: w.AskUserOptions, AskUserAllowFreeform: w.AskUserAllowFreeform,
		InputTokens: w.InputTokens, OutputTokens: w.OutputTokens, CacheCreationInputTokens: w.CacheCreationInputTokens,
		CacheReadInputTokens: w.CacheReadInputTokens, PreviousContextTokens: w.PreviousContextTokens, ContextTokens: w.ContextTokens,
		StopReason: w.StopReason, Info: w.Info, SubAgentParentID: w.SubAgentParentID, TraceInvocationID: w.TraceInvocationID,
		TraceParentInvocationID: w.TraceParentInvocationID, TraceCallID: w.TraceCallID,
	}
	if w.Error != "" {
		e.Err = errors.New(w.Error)
	}
	return e
}

type Encoder struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewEncoder(writer io.Writer) *Encoder { return &Encoder{writer: writer} }
func (e *Encoder) Encode(message Message) error {
	if err := validate(message); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data) > MaxMessageBytes {
		return fmt.Errorf("desktop worker message exceeds %d bytes", MaxMessageBytes)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	data = append(data, '\n')
	n, err := e.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

type Decoder struct{ scanner *bufio.Scanner }

func NewDecoder(reader io.Reader) *Decoder {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), MaxMessageBytes+2)
	return &Decoder{scanner: scanner}
}
func (d *Decoder) Decode() (Message, error) {
	if !d.scanner.Scan() {
		if err := d.scanner.Err(); err != nil {
			return Message{}, fmt.Errorf("desktop worker message: %w", err)
		}
		return Message{}, io.EOF
	}
	data := d.scanner.Bytes()
	if len(data) > MaxMessageBytes {
		return Message{}, fmt.Errorf("desktop worker message exceeds %d bytes", MaxMessageBytes)
	}
	var message Message
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return Message{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Message{}, errors.New("desktop worker message contains trailing data")
	}
	if err := validate(message); err != nil {
		return Message{}, err
	}
	if message.Type == TypeReply {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		if raw, ok := fields["decision"]; !ok || bytes.Equal(raw, []byte("null")) {
			return Message{}, errors.New("desktop worker reply requires an explicit decision")
		}
	}
	if message.Type == TypeSteerResult {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		if raw, ok := fields["accepted"]; !ok || bytes.Equal(raw, []byte("null")) {
			return Message{}, errors.New("desktop worker steer result requires explicit acceptance")
		}
	}
	return message, nil
}
func validate(message Message) error {
	if message.Version != Version {
		return fmt.Errorf("unsupported desktop worker protocol version %d", message.Version)
	}
	switch message.Type {
	case TypeSteer, TypeSteerResult:
		if message.ID == "" || message.Event != nil || message.Status != nil {
			return errors.New("invalid desktop worker steer message")
		}
		if message.Type == TypeSteer && message.Input == "" {
			return errors.New("desktop worker steer input is empty")
		}
	case TypeEvent:
		if message.Event == nil || message.Status != nil {
			return errors.New("desktop worker event is missing")
		}
	case TypeStatus:
		if message.Status == nil || message.Event != nil || message.ID != "" {
			return errors.New("invalid desktop worker status")
		}
	case TypeReply:
		if message.ID == "" || message.Event != nil || message.Status != nil {
			return errors.New("invalid desktop worker reply")
		}
		if message.Decision < agent.PermissionDecisionAllow || message.Decision > agent.PermissionDecisionAlwaysAllow {
			return errors.New("invalid desktop worker permission decision")
		}
	default:
		return fmt.Errorf("unsupported desktop worker message type %q", message.Type)
	}
	return nil
}
