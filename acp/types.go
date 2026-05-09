package acp

// types.go — Wire protocol handshake + DisplayBlock structured-output
// types for the metis ACP server. Mirrors kimi-agent-sdk's
// `go/wire/message.go` + openclaude's parallel definitions.
//
// Why these types: ACP clients (Zed, Cursor, custom IDE plugins)
// need:
//
//  1. **Version negotiation** at session start so a future protocol
//     bump doesn't break old clients silently. `Initialize` exchanges
//     `protocol_version`, advertises server capabilities, and lets
//     the client register its external tools (which the server may
//     accept or reject explicitly — claude-code's CCR REPL skips
//     this; we follow kimi's stricter shape).
//
//  2. **Structured display blocks** so a client can render rich UI
//     (diffs, todo lists, shell runs) instead of dumping the raw
//     tool_result string into a code block. metis tools optionally
//     emit DisplayBlock alongside Result.Output for client-side
//     pretty-rendering; clients without ACP support just see the
//     plain text.
//
// Both Initialize and DisplayBlock are PURE DATA — encoding logic
// lives in server.go's handle() switch and the tools that emit
// them. Keep this file dependency-free so future ACP-client
// libraries can vendor the types without dragging in the agent loop.

// ProtocolVersion is what `metis acp` advertises in the Initialize
// reply. Bump when the wire shape changes in a backward-incompatible
// way; in-place additive changes keep the same version. Mirrors
// kimi-agent-sdk Wire 1.7 numbering — we start at 1.0 so the
// implementations don't spuriously claim parity with kimi's hooks
// extension we haven't shipped yet.
const ProtocolVersion = "1.0"

// InitializeParams is what a client sends to begin a session. All
// fields optional — a client can supply just `protocol_version`
// and let the server fill in defaults for everything else.
type InitializeParams struct {
	// ProtocolVersion is the version the client wants to speak. The
	// server compares to its own and either accepts (returns the
	// matching version in InitializeResult) or rejects (responds
	// with an error). Major-version mismatch ⇒ reject.
	ProtocolVersion string `json:"protocol_version,omitempty"`

	// Client identifies the connecting application (Zed / Cursor /
	// custom IDE plugin). Used for analytics + log tagging — server
	// behaviour is NOT gated on client name (clients spoof, and the
	// protocol should be enough).
	Client *ClientInfo `json:"client,omitempty"`

	// ExternalTools are tools the client wants the agent to be able
	// to call back into the client for. Each tool definition is
	// validated against the server's policy; the InitializeResult
	// tells the client which were accepted and which rejected.
	ExternalTools []ExternalTool `json:"external_tools,omitempty"`
}

// ClientInfo names the calling app. Both fields free-form strings.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ExternalTool is a callable the client offers to the agent. The
// server has the option to accept it (relay tool_use → client) or
// reject it with a reason (typically "duplicates a built-in tool"
// or "schema doesn't match what we expect").
type ExternalTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// InitializeResult is the server's reply to InitializeParams.
type InitializeResult struct {
	// ProtocolVersion is what the SERVER will speak — may differ
	// from the client's request when the client asked for a higher
	// version that the server can't honour. Client must check this
	// is acceptable.
	ProtocolVersion string `json:"protocol_version"`

	// Server identifies us — name + version + commit hash.
	Server ServerInfo `json:"server"`

	// SlashCommands the server understands. Clients can offer these
	// in their UI's command palette without re-querying. Each entry
	// has a name (`/help`) and a one-line description.
	SlashCommands []SlashCommand `json:"slash_commands,omitempty"`

	// ExternalTools — per-tool accept/reject decisions for the tools
	// the client offered in InitializeParams.ExternalTools. Order is
	// not guaranteed to match the request; clients should look up by
	// Name.
	ExternalTools []ExternalToolDecision `json:"external_tools,omitempty"`
}

// ServerInfo identifies metis to the client.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// SlashCommand is one entry in InitializeResult.SlashCommands.
type SlashCommand struct {
	Name        string `json:"name"` // includes leading `/`
	Description string `json:"description,omitempty"`
}

// ExternalToolDecision tells the client whether the server accepted
// each external tool it offered.
type ExternalToolDecision struct {
	Name     string `json:"name"`
	Accepted bool   `json:"accepted"`
	// Reason is filled when Accepted=false — typically "duplicates
	// built-in" or "schema validation failed: <detail>".
	Reason string `json:"reason,omitempty"`
}

// DisplayBlock is a structured representation of a tool result that
// clients can render with rich UI. Tools optionally emit one or more
// DisplayBlock alongside the plain-text Output; clients without ACP
// support fall back to the text. Mirrors kimi-agent-sdk
// `go/wire/message.go::DisplayBlock`.
//
// Type tells the client which interpretation to apply. Unknown
// types should fall back to `text` rendering.
type DisplayBlock struct {
	Type DisplayBlockType `json:"type"`

	// Common fields — most types use one or two of these.
	Text     string `json:"text,omitempty"`
	Path     string `json:"path,omitempty"`     // file path for diff / shell tools
	Language string `json:"language,omitempty"` // syntax-highlight hint
	Command  string `json:"command,omitempty"`  // for shell-block

	// Diff fields — populated when Type == "diff".
	OldText string `json:"old_text,omitempty"`
	NewText string `json:"new_text,omitempty"`

	// Todo items — populated when Type == "todo".
	Items []TodoItem `json:"items,omitempty"`

	// Free-form structured data — fallback for tools that need to
	// pass JSON the client can render however it wants. Clients
	// should round-trip Data unchanged in their state.
	Data map[string]any `json:"data,omitempty"`
}

// DisplayBlockType is the tagged-union discriminator.
type DisplayBlockType string

const (
	// DisplayBlockBrief — short status line, e.g. "Read 12 files".
	// Renders as a single muted-color line in most clients.
	DisplayBlockBrief DisplayBlockType = "brief"

	// DisplayBlockDiff — file edit; client renders side-by-side or
	// unified diff. Path identifies the file. OldText + NewText are
	// the before/after contents (or hunks; this protocol doesn't
	// commit to either).
	DisplayBlockDiff DisplayBlockType = "diff"

	// DisplayBlockTodo — list of task items with status. Items field
	// holds them. Renders as an interactive todo list in clients
	// that support it.
	DisplayBlockTodo DisplayBlockType = "todo"

	// DisplayBlockShell — shell command with its output. Command +
	// Text are the command and its captured output.
	DisplayBlockShell DisplayBlockType = "shell"

	// DisplayBlockText — fallback when none of the above fit. Text
	// holds the content; Language can hint at syntax highlighting.
	DisplayBlockText DisplayBlockType = "text"
)

// TodoItem is one row in a DisplayBlockTodo. Mirrors the metis
// tasks package's task statuses.
type TodoItem struct {
	Title  string         `json:"title"`
	Status TodoItemStatus `json:"status"`
}

// TodoItemStatus is the lifecycle state of a TodoItem. Matches
// pkg/tasks's task statuses.
type TodoItemStatus string

const (
	TodoPending    TodoItemStatus = "pending"
	TodoInProgress TodoItemStatus = "in_progress"
	TodoCompleted  TodoItemStatus = "completed"
)
