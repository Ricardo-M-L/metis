package mcp_tools

import "encoding/json"

// Carry the CU execution contract to the model without dumping arbitrary
// structured payloads (which can contain image bytes). The normal final MCP
// redaction boundary still processes this text. This is evidence, not policy.
func renderMCPActionOutcome(raw json.RawMessage) string {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return ""
	}
	var value struct {
		Outcome *struct {
			Dispatch     string `json:"dispatch"`
			Verification string `json:"verification"`
			Reason       string `json:"reason,omitempty"`
		} `json:"outcome"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Outcome == nil {
		return ""
	}
	switch value.Outcome.Dispatch {
	case "not_sent", "sent", "unknown":
	default:
		return ""
	}
	switch value.Outcome.Verification {
	case "passed", "failed", "unknown", "not_requested":
	default:
		return ""
	}
	if len(value.Outcome.Reason) > 4096 {
		return "[MCP action outcome omitted: reason exceeds limit]"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return "Computer Use execution evidence: " + string(encoded)
}
