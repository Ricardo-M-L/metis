package agent

import (
	"context"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

const durableBashDescription = `Durable coordinator execution: every Bash command must run in the foreground and return its completed command result. Use ordinary foreground Bash for finite builds and acceptance checks. Background execution and background completion flags are unavailable in this worker. Provide command and description, and use timeout_ms when a longer finite foreground timeout is needed. Never request a background job, even for a long-running check. A successful confirmed operation can be replayed after a worker restart; an unknown external outcome holds the work for trusted resolution.`

// toolSpecsWithContext publishes the actual execution contract for one run.
// Interactive callers keep the ordinary catalog. Durable callers get a copied
// Bash schema and description; registry schemas and lazy-discovery state are
// shared with other loops and must never be rewritten in place.
func (l *Loop) toolSpecsWithContext(ctx context.Context) []llm.ToolSpec {
	specs := l.toolSpecs()
	if pubtool.OperationExecutorFromContext(ctx) == nil {
		return specs
	}
	for index, spec := range specs {
		if spec.Name != "Bash" {
			continue
		}
		baseDescription := spec.Description
		if tool, ok := l.Registry.GetForModel(spec.Name); ok {
			// ShortDescription contains ordinary interactive background advice.
			// Preserve the full tool's execution and safety guidance instead.
			baseDescription = tool.Description()
		}
		if before, _, found := strings.Cut(baseDescription, "Long-running commands:"); found {
			baseDescription = before
		}
		spec.Description = strings.TrimSpace(baseDescription) + "\n\n" + durableBashDescription
		spec.InputSchema = cloneDurableToolSchema(spec.InputSchema)
		if properties, ok := spec.InputSchema["properties"].(map[string]any); ok {
			for _, field := range durableBashBackgroundFields {
				delete(properties, field)
			}
			if timeout, ok := properties["timeout_ms"].(map[string]any); ok {
				timeout["description"] = "Override the finite foreground command timeout in milliseconds, up to 600000. The command must finish within the project deadline."
			}
		}
		switch required := spec.InputSchema["required"].(type) {
		case []string:
			filtered := make([]string, 0, len(required))
			for _, field := range required {
				if !durableBashBackgroundField(field) {
					filtered = append(filtered, field)
				}
			}
			spec.InputSchema["required"] = filtered
		case []any:
			filtered := make([]any, 0, len(required))
			for _, value := range required {
				field, _ := value.(string)
				if !durableBashBackgroundField(field) {
					filtered = append(filtered, value)
				}
			}
			spec.InputSchema["required"] = filtered
		}
		if defaults, ok := spec.InputSchema["default"].(map[string]any); ok {
			for _, field := range durableBashBackgroundFields {
				delete(defaults, field)
			}
		}
		specs[index] = spec
	}
	return specs
}

var durableBashBackgroundFields = [...]string{"run_in_background", "await_completion", "required_for_completion"}

func durableBashBackgroundField(field string) bool {
	for _, forbidden := range durableBashBackgroundFields {
		if field == forbidden {
			return true
		}
	}
	return false
}

func cloneDurableToolSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	result := make(map[string]any, len(schema))
	for key, value := range schema {
		result[key] = cloneDurableToolSchemaValue(value)
	}
	return result
}

func cloneDurableToolSchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneDurableToolSchema(typed)
	case []any:
		result := make([]any, len(typed))
		for index, entry := range typed {
			result[index] = cloneDurableToolSchemaValue(entry)
		}
		return result
	case []string:
		return append([]string(nil), typed...)
	case []map[string]any:
		result := make([]map[string]any, len(typed))
		for index, entry := range typed {
			result[index] = cloneDurableToolSchema(entry)
		}
		return result
	default:
		return value
	}
}
