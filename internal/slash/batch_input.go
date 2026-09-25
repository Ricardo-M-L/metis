package slash

import (
	"strings"
	"unicode"
)

// ExpandBatchInput expands the Desktop command only for the model-facing
// prompt. Callers retain the original user input in the durable transcript.
func ExpandBatchInput(input string) string {
	trimmed := strings.TrimSpace(input)
	const command = "/batch"
	if len(trimmed) <= len(command) || !strings.EqualFold(trimmed[:len(command)], command) || !unicode.IsSpace(rune(trimmed[len(command)])) {
		return input
	}
	task := strings.TrimSpace(trimmed[len(command):])
	if task == "" {
		return input
	}
	return BatchPrompt(task)
}
