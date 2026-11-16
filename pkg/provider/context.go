package provider

// ContextUpdateProvider is optional. A non-empty scope opts into durable,
// ordered context updates encoded at their original position in history.
// Providers must preserve instruction authority and never fold these updates
// back into the request's static prefix. Unknown transports remain unchanged.
type ContextUpdateProvider interface {
	ContextUpdateScope() string
}

// ContextSection is a named runtime snapshot, not a permission grant. Memory
// and retrieved passages remain reference data inside a host-authored envelope.
type ContextSection struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// ContextUpdate carries its complete baseline for deterministic resume and
// its changed/removed fields for the model. Only the rendered Text of the
// containing block goes over the provider wire; this metadata stays local.
type ContextUpdate struct {
	Scope    string           `json:"scope"`
	Revision uint64           `json:"revision"`
	State    []ContextSection `json:"state"`
	Changed  []ContextSection `json:"changed,omitempty"`
	Removed  []string         `json:"removed,omitempty"`
}

func IsMutableContextSection(section SystemSection) bool {
	return section.Volatile || section.Name == "memory_index" || section.Name == "runtime_state"
}

func IsContextUpdateMessage(message Message) bool {
	return message.Role == RoleSystem && len(message.Content) == 1 && message.Content[0].Type == "context_update"
}

// WithoutContextUpdates projects canonical history for legacy providers.
// Current runtime state is still supplied through their existing system path.
func WithoutContextUpdates(messages []Message) []Message {
	var out []Message
	for i, message := range messages {
		if IsContextUpdateMessage(message) {
			if out == nil {
				out = append(make([]Message, 0, len(messages)), messages[:i]...)
			}
			continue
		}
		if out != nil {
			out = append(out, message)
		}
	}
	if out == nil {
		return messages
	}
	return out
}
