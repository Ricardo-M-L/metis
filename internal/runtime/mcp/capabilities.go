package mcp

// HasEnabledServer reports whether the merged MCP configuration contains an
// enabled server with the given logical name. It is intentionally based on
// configuration rather than live process state so prompt capability assembly
// can happen before the asynchronous launcher starts.
func (r *Registry) HasEnabledServer(name string) bool {
	if r == nil || name == "" {
		return false
	}
	for _, server := range r.Servers {
		if server.Name == name && !server.Disabled {
			return true
		}
	}
	return false
}
