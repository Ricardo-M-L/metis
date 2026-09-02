package runtime

// Structured output for `metis run` — claude-code's jsonSchema
// QueryParam (QueryEngine.ts:149) adapted to metis's local-first
// shape: instead of a server-side response_format (not portable
// across metis's OpenAI/Gemini/Anthropic adapters), the schema is
// injected as a prompt-level contract, validated LOCALLY after the
// loop finishes, and invalid output buys the model up to
// MaxSchemaRetries correction turns with the precise validation
// error. Eval pipelines and shell wrappers get parseable-or-exit-11
// semantics either way.
//
// The validator covers the practical JSON Schema subset: type,
// properties, required, items, enum, additionalProperties=false.
// Schemas using constructs beyond that still work — unknown keywords
// are ignored (validation is best-effort permissive, same direction
// as protobuf's unknown-field rule).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// MaxSchemaRetries bounds the correction turns after invalid output.
const MaxSchemaRetries = 2

// OutputSchemaEnforcer holds one parsed schema for a run.
type OutputSchemaEnforcer struct {
	schema map[string]any
	raw    string
}

// NewOutputSchemaEnforcer loads a JSON Schema file. ("", nil) input
// path → (nil, nil): the feature is off.
func NewOutputSchemaEnforcer(path string) (*OutputSchemaEnforcer, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--output-schema: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		return nil, fmt.Errorf("--output-schema: %s is not valid JSON: %w", path, err)
	}
	return &OutputSchemaEnforcer{schema: schema, raw: strings.TrimSpace(string(b))}, nil
}

// Instruction is appended to the user prompt so the model knows the
// output contract up front.
func (e *OutputSchemaEnforcer) Instruction() string {
	return "<system-reminder>Your FINAL message must be a single JSON value conforming to this JSON Schema — no prose, no markdown fences, just the JSON:\n" +
		e.raw + "\n</system-reminder>"
}

// ResponseFormat exposes the same contract to providers that support native
// JSON Schema output. The prompt instruction and local validator remain in
// place as a compatibility and correctness fallback.
func (e *OutputSchemaEnforcer) ResponseFormat() *provider.ResponseFormat {
	if e == nil {
		return nil
	}
	return &provider.ResponseFormat{
		Name:       "metis_output",
		JSONSchema: e.schema,
		Strict:     true,
	}
}

// RetryMessage tells the model exactly what failed validation.
func (e *OutputSchemaEnforcer) RetryMessage(verr error) string {
	return fmt.Sprintf("<system-reminder>Your previous final message did not validate against the required output schema: %v.\nReply with ONLY the corrected JSON value — no explanation, no fences.</system-reminder>", verr)
}

// Validate extracts the JSON value from text (tolerating markdown
// fences and surrounding prose) and checks it against the schema.
// Returns the canonical JSON string on success.
func (e *OutputSchemaEnforcer) Validate(text string) (string, error) {
	candidate := extractJSON(text)
	if candidate == "" {
		return "", fmt.Errorf("no JSON value found in output")
	}
	var v any
	if err := json.Unmarshal([]byte(candidate), &v); err != nil {
		return "", fmt.Errorf("output is not valid JSON: %v", err)
	}
	if err := validateValue(e.schema, v, "$"); err != nil {
		return "", err
	}
	return candidate, nil
}

// extractJSON pulls the most plausible JSON value out of model text:
// prefer a ```json fence, else the first balanced {...} / [...] span,
// else the trimmed text itself (covers bare scalars).
func extractJSON(text string) string {
	t := strings.TrimSpace(text)
	if fenced := extractFence(t); fenced != "" {
		return fenced
	}
	for _, open := range []byte{'{', '['} {
		if span := balancedSpan(t, open); span != "" {
			return span
		}
	}
	return t
}

func extractFence(t string) string {
	for _, marker := range []string{"```json", "```"} {
		start := strings.Index(t, marker)
		if start < 0 {
			continue
		}
		rest := t[start+len(marker):]
		end := strings.Index(rest, "```")
		if end < 0 {
			continue
		}
		body := strings.TrimSpace(rest[:end])
		if body != "" && (body[0] == '{' || body[0] == '[') {
			return body
		}
	}
	return ""
}

// balancedSpan returns the first balanced bracket span starting with
// `open`, respecting JSON string literals and escapes.
func balancedSpan(t string, open byte) string {
	var close byte = '}'
	if open == '[' {
		close = ']'
	}
	start := strings.IndexByte(t, open)
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	for i := start; i < len(t); i++ {
		c := t[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return t[start : i+1]
			}
		}
	}
	return ""
}

// validateValue checks v against one schema node. path is the JSON
// path for error messages ("$.items[2].name").
func validateValue(schema map[string]any, v any, path string) error {
	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, allowed := range enum {
			if jsonEqual(allowed, v) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value not in enum", path)
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "":
		return nil // untyped node — accept anything
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object, got %s", path, jsonTypeName(v))
		}
		if reqs, ok := schema["required"].([]any); ok {
			for _, r := range reqs {
				name, _ := r.(string)
				if _, present := obj[name]; name != "" && !present {
					return fmt.Errorf("%s: missing required property %q", path, name)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for name, val := range obj {
			sub, hasSub := props[name].(map[string]any)
			if hasSub {
				if err := validateValue(sub, val, path+"."+name); err != nil {
					return err
				}
				continue
			}
			if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
				return fmt.Errorf("%s: unexpected property %q", path, name)
			}
		}
		return nil
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array, got %s", path, jsonTypeName(v))
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range arr {
				if err := validateValue(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: expected string, got %s", path, jsonTypeName(v))
		}
		return nil
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("%s: expected number, got %s", path, jsonTypeName(v))
		}
		return nil
	case "integer":
		f, ok := v.(float64)
		if !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s: expected integer, got %s", path, jsonTypeName(v))
		}
		return nil
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %s", path, jsonTypeName(v))
		}
		return nil
	case "null":
		if v != nil {
			return fmt.Errorf("%s: expected null, got %s", path, jsonTypeName(v))
		}
		return nil
	default:
		return nil // unknown type keyword — permissive
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// RunLoopCollectText runs one correction turn on the loop and returns
// the concatenated assistant text. Headless contract: permission asks
// are denied, AskUser is dismissed — same posture as cmdRun's main
// event loop, minus metrics (a correction turn is expected to be a
// single text-only reply).
func RunLoopCollectText(ctx context.Context, loop *agent.Loop, sessionID string) (string, error) {
	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- RunWithTraceTurn(ctx, sessionID, func(turnCtx context.Context) error {
			return loop.Run(turnCtx, events)
		})
		close(events)
	}()
	var b strings.Builder
	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			b.WriteString(ev.TextDelta)
		case agent.EventPermissionRequest:
			if ev.PermissionReply != nil {
				ev.PermissionReply <- agent.PermissionDecisionDeny
			}
		case agent.EventAskUser:
			if ev.AskUserReply != nil {
				ev.AskUserReply <- ""
			}
		case agent.EventError:
			// drain remaining events; the error also lands on done.
		}
	}
	return b.String(), <-done
}
