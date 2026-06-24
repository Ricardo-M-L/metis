package client

// wire.go — the ACP control-plane message shapes the client needs, kept
// local so this package depends on nothing but the standard library (the
// server-side `acp` package pulls in the whole agent loop, which a thin
// out-of-process client must not drag along). These mirror metis' acp
// protocol; the tool contract itself is obtained at runtime from
// `metis schema`, so only these small, stable control messages live here.
//
// TODO: when the wire types are split into a dependency-free package
// (e.g. metis/pkg/acp), import them here instead of redeclaring.

// protocolVersion is the ACP version this client requests. Matches the
// server's acp.ProtocolVersion.
const protocolVersion = "1.0"

type initializeParams struct {
	ProtocolVersion string      `json:"protocol_version,omitempty"`
	Client          *clientInfo `json:"client,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string     `json:"protocol_version"`
	Server          ServerInfo `json:"server"`
}

// ServerInfo identifies the connected metis agent (from the handshake).
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

type promptParams struct {
	Prompt   string `json:"prompt"`
	PlanMode bool   `json:"plan_mode,omitempty"`
	Model    string `json:"model,omitempty"`
}

type abortParams struct {
	PromptID string `json:"prompt_id"`
}

type permissionReplyParams struct {
	ToolUseID string `json:"tool_use_id"`
	Decision  string `json:"decision"` // "allow" | "deny" | "always"
}
