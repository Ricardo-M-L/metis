package provider

import "fmt"

// ToolDefinitionLimiter is optional. Zero means the adapter does not declare
// a count limit; it does not impose an OpenAI-specific cap on other providers.
// The returned budget excludes adapter-owned hosted tools.
type ToolDefinitionLimiter interface{ MaxToolDefinitions() int }

func ToolDefinitionLimit(p Provider) int {
	if v, ok := p.(ToolDefinitionLimiter); ok {
		return v.MaxToolDefinitions()
	}
	return 0
}

// ValidateToolDefinitions protects direct SDK callers as well as agent loops.
// Never silently truncate a request: its tool choices may require that tool.
func ValidateToolDefinitions(specs []ToolSpec, limit int) error {
	if limit > 0 && len(specs) > limit {
		return fmt.Errorf("invalid request: tool definition limit exceeded: request has %d tools, endpoint allows %d; use ToolSearch to load tools on demand or reduce explicitly required tools", len(specs), limit)
	}
	return nil
}
