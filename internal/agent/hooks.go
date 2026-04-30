package agent

// This file used to host the hook types + Registry. They moved to
// pkg/hook so 3rd-party plugins can write Pre/PostToolUse handlers
// without depending on Metis internals (Round 11 split).
//
// All internal callers (agent.Loop, cmd/metis) keep using the old
// names through these aliases — same types, just reachable from this
// package.

import (
	"context"

	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

// HookEvent re-exports.
type HookEvent = pubhook.Event

const (
	HookPreToolUse   = pubhook.EventPreToolUse
	HookPostToolUse  = pubhook.EventPostToolUse
	HookSessionStart = pubhook.EventSessionStart
	HookSessionEnd   = pubhook.EventSessionEnd
	HookTurnStart    = pubhook.EventTurnStart
	HookTurnEnd      = pubhook.EventTurnEnd
	HookLoopEnd      = pubhook.EventLoopEnd
	HookError        = pubhook.EventError
)

// Data struct re-exports.
type (
	HookContext        = pubhook.Context
	PreToolUseHook     = pubhook.PreToolUse
	ModifiedPreToolUse = pubhook.ModifiedPreToolUse
	HookOutput         = pubhook.Output
	PostToolUseHook    = pubhook.PostToolUse
)

// Handler typedef re-exports.
type (
	PreToolUseHandler   = pubhook.PreToolUseHandler
	PostToolUseHandler  = pubhook.PostToolUseHandler
	SessionStartHandler = pubhook.SessionStartHandler
	SessionEndHandler   = pubhook.SessionEndHandler
	TurnStartHandler    = pubhook.TurnStartHandler
	TurnEndHandler      = pubhook.TurnEndHandler
	LoopEndHandler      = pubhook.LoopEndHandler
	ErrorHandler        = pubhook.ErrorHandler
)

// HookRegistry is the in-process registry. Re-exported here so existing
// `*agent.HookRegistry` references in cmd/metis/main.go and the loop
// stay valid without changes.
type HookRegistry = pubhook.Registry

// NewHookRegistry constructs a fresh registry.
func NewHookRegistry() *HookRegistry {
	return pubhook.NewRegistry()
}

// Tiny smoke that pkg/hook is reachable from the agent's own code path
// without import cycles. The blank assignment is compiled away — its
// only purpose is to fail compilation loud-and-early if a future
// refactor turns the alias into a wrapper.
var _ = func(ctx context.Context) {
	r := NewHookRegistry()
	_ = r.EmitPreToolUse
	_ = ctx
}
