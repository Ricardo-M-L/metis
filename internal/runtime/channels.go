package runtime

import (
	"os"

	"github.com/Ricardo-M-L/metis/internal/channels"
	"github.com/Ricardo-M-L/metis/internal/channels/dingtalk"
	"github.com/Ricardo-M-L/metis/internal/channels/discord"
	"github.com/Ricardo-M-L/metis/internal/channels/feishu"
	"github.com/Ricardo-M-L/metis/internal/channels/slack"
	"github.com/Ricardo-M-L/metis/internal/channels/telegram"
	"github.com/Ricardo-M-L/metis/internal/channels/wechat"
	"github.com/Ricardo-M-L/metis/internal/config"
)

// BuildChannelRegistry assembles the chat-platform adapter set used by the
// SendMessage tool. Each adapter wakes up only when its env-var credentials
// are present, so an unconfigured user never sees a "missing token" error
// from a platform they didn't ask for.
//
// Extracted from cmd/metis/main.go as part of the bootstrap-pkg cleanup
// (Round 7 took MCP, this round takes channels). main.go shouldn't know
// the order of platforms or the env-var conventions — it just needs an
// already-registered chReg.
//
// The Registry returned is empty when no platforms are configured; callers
// can still register a SendMessage tool against it (it will surface
// "no channels available" at call time rather than blowing up at boot).
func BuildChannelRegistry(cfg *config.Channels) *channels.Registry {
	chReg := channels.NewRegistry()
	if cfg == nil {
		return chReg
	}
	if env := os.Getenv(cfg.Slack.BotTokenEnv); env != "" {
		chReg.Register(slack.New(env, cfg.Slack.DefaultChannel))
	}
	if env := os.Getenv(cfg.Telegram.BotTokenEnv); env != "" {
		chReg.Register(telegram.New(env, os.Getenv(cfg.Telegram.DefaultChatIDEnv)))
	}
	if env := os.Getenv(cfg.Discord.WebhookURLEnv); env != "" {
		chReg.Register(discord.New(env))
	}
	if env := os.Getenv(cfg.Dingtalk.WebhookURLEnv); env != "" {
		chReg.Register(dingtalk.New(env, os.Getenv(cfg.Dingtalk.SecretEnv)))
	}
	if env := os.Getenv(cfg.Feishu.WebhookURLEnv); env != "" {
		chReg.Register(feishu.New(env, os.Getenv(cfg.Feishu.SecretEnv)))
	}
	if env := os.Getenv(cfg.Wechat.WebhookURLEnv); env != "" {
		chReg.Register(wechat.New(env))
	}
	return chReg
}
