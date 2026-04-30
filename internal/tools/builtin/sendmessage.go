package builtin

import (
	"context"
	"errors"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// SendMessage exposes Metis's channels Registry as a single agent-facing
// tool. The agent picks a destination via "<platform>:<target>" prefix,
// e.g. "slack:#general" or "telegram:@chatid". Implementations are in
// internal/channels/<platform>/.
type SendMessage struct {
	gate     *permission.Gate
	registry *channels.Registry
	defaultPlatform string
}

func NewSendMessage(gate *permission.Gate, reg *channels.Registry, defaultPlatform string) *SendMessage {
	return &SendMessage{gate: gate, registry: reg, defaultPlatform: defaultPlatform}
}

func (SendMessage) Name() string { return "SendMessage" }

func (m *SendMessage) Description() string {
	platforms := m.registry.Names()
	if len(platforms) == 0 {
		return "Send a message to a chat platform. (No platforms configured — set channel tokens in config.toml.)"
	}
	return "Send a message to a chat platform. Available: " + strings.Join(platforms, ", ") +
		". Use channel = \"<platform>:<target>\" (e.g. \"slack:#general\", \"telegram:@chatid\")."
}

func (SendMessage) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"channel", "text"},
		"properties": map[string]any{
			"channel":  map[string]any{"type": "string", "description": "Format: \"<platform>:<target>\" or just \"<target>\" to use the default platform."},
			"text":     map[string]any{"type": "string"},
			"title":    map[string]any{"type": "string", "description": "Optional title for platforms that support it (Discord embed / 钉钉 markdown title)."},
			"markdown": map[string]any{"type": "boolean", "description": "Render as markdown when the platform supports it."},
		},
	}
}

func (SendMessage) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }

func (m *SendMessage) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := m.gate.Check(context.Background(), "SendMessage", strFromAny(in["channel"]))
	return mapDecision(d), src
}

func (m *SendMessage) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	channel, _ := in["channel"].(string)
	text, _ := in["text"].(string)
	if channel == "" || text == "" {
		return nil, errors.New("channel and text are required")
	}
	title, _ := in["title"].(string)
	markdown, _ := in["markdown"].(bool)

	if err := m.registry.Send(ctx, channel, m.defaultPlatform, channels.Message{
		Text: text, Title: title, Markdown: markdown,
	}); err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	return &tools.Result{Output: "sent to " + channel}, nil
}
