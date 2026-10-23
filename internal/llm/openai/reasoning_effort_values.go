package openai

import (
	"fmt"
)

// resolveReasoningEffort preserves the transport's legacy conversion when no
// model-specific declaration exists. Declared mappings are complete for their
// advertised subset: an absent slot must fail before any HTTP request.
func resolveReasoningEffort(effort Effort, values map[string]string, disabled bool, fallback string) (string, error) {
	if disabled || effort == EffortDefault {
		return "", nil
	}
	if len(values) == 0 {
		return fallback, nil
	}
	if !effort.Valid() {
		return "", fmt.Errorf("reasoning effort is not a canonical model option")
	}
	value, ok := values[string(effort)]
	if !ok {
		return "", fmt.Errorf("reasoning effort %q is not configured for this model", effort)
	}
	if len(value) == 0 || len(value) > 64 {
		return "", fmt.Errorf("configured reasoning effort value is invalid")
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return "", fmt.Errorf("configured reasoning effort value is invalid")
		}
	}
	return value, nil
}
