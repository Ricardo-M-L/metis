// Package channels hosts Metis's built-in chat-platform adapters
// (Slack / Telegram / Discord / 钉钉 / 飞书 / 企业微信). Each platform
// lives in its own subpackage; this file re-exports the public Adapter
// + Registry types from pkg/channel so existing in-tree imports
// (`channels.NewRegistry`, `channels.Message`, `channels.Adapter`) keep
// working.
//
// Round 12 split: types moved to pkg/channel so 3rd-party platform
// adapters can implement Adapter without depending on Metis internals.
package channels

import pubchan "github.com/Ricardo-M-L/metis/pkg/channel"

// Public-API re-exports.
type (
	Message  = pubchan.Message
	Adapter  = pubchan.Adapter
	Registry = pubchan.Registry
)

// NewRegistry returns a fresh registry. Convenience pass-through so
// callers can keep using `channels.NewRegistry()`.
func NewRegistry() *Registry { return pubchan.NewRegistry() }

// ParseChannel re-export (rare external use; exposed for completeness).
var ParseChannel = pubchan.ParseChannel
