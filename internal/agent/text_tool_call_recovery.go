package agent

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

var plainTextToolCallEnvelope = regexp.MustCompile(
	`(?s)^<tool_call>\s*<function=([A-Za-z][A-Za-z0-9_.:-]{0,127})>.*</function>\s*</tool_call>$`,
)

// recoverableTextToolCallName recognizes only a whole assistant response made
// of one XML-like tool envelope naming a tool that is actually registered.
// Thinking blocks are ignored; any other content type or surrounding prose
// fails closed. Arguments are deliberately not parsed because this recovery
// path never executes the text.
func recoverableTextToolCallName(content []llm.ContentBlock, registry *tools.Registry) (string, bool) {
	if registry == nil {
		return "", false
	}
	var visible strings.Builder
	textBlocks := 0
	for _, block := range content {
		switch block.Type {
		case "thinking":
			continue
		case "text":
			textBlocks++
			visible.WriteString(block.Text)
		default:
			return "", false
		}
	}
	if textBlocks == 0 {
		return "", false
	}
	match := plainTextToolCallEnvelope.FindStringSubmatch(strings.TrimSpace(visible.String()))
	if len(match) != 2 {
		return "", false
	}
	if _, ok := registry.Get(match[1]); !ok {
		return "", false
	}
	return match[1], true
}

func textToolCallRecoveryMessage(toolName string) string {
	return fmt.Sprintf(
		"<system-reminder>Your previous response was plain text, so no tool was executed. "+
			"The registered tool %q is available. Reissue the same intended call now using the provider's native structured tool-call interface. "+
			"Do not repeat XML-like <tool_call> text, do not explain, and do not change the intended arguments.</system-reminder>",
		toolName,
	)
}
