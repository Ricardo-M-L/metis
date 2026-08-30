package agent

import "github.com/Ricardo-M-L/metis/internal/llm"

// PresentationHistory returns a detached, display-safe copy of provider
// history. Canonical history must retain the exact tool input for protocol
// continuation; UI/resume consumers instead use this projection so structured
// credentials and recognizable tokens embedded in command/URL strings never
// cross the presentation boundary.
func PresentationHistory(messages []llm.Message) []llm.Message {
	if messages == nil {
		return nil
	}
	out := make([]llm.Message, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].Content = presentationContentBlocks(message.Content)
	}
	return out
}

// PresentationToolInput returns the same detached, key-aware tool-input copy
// used by live events and PresentationHistory. It is exported for UI surfaces
// that render a single tool call rather than a complete transcript.
func PresentationToolInput(input map[string]any) map[string]any {
	return redactedToolInput(input)
}

func presentationContentBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	if blocks == nil {
		return nil
	}
	out := make([]llm.ContentBlock, len(blocks))
	for i, block := range blocks {
		out[i] = block
		out[i].ToolInput = redactedToolInput(block.ToolInput)
		// Presentation is also persisted UI-controlled JSON. Older/imported
		// sessions may predate the live dispatch redaction boundary, so treat it
		// like tool input rather than trusting it merely because it is metadata.
		out[i].Presentation = redactedToolInput(block.Presentation)
		if block.ProviderHint != nil {
			out[i].ProviderHint = make(map[string]string, len(block.ProviderHint))
			for key, value := range block.ProviderHint {
				out[i].ProviderHint[key] = value
			}
		}
		out[i].ToolResultBlocks = presentationContentBlocks(block.ToolResultBlocks)
	}
	return out
}
