// Package llm hosts Metis's built-in LLM transport adapters
// (Anthropic, OpenAI). The Provider contract + wire types moved to
// pkg/provider so 3rd-party plugins can implement custom backends
// without depending on Metis internals.
//
// This file re-exports everything as type aliases so existing in-tree
// imports (`llm.Provider`, `llm.Request`, `llm.RoleUser`, etc.) keep
// working. Plugin authors should import pkg/provider directly.
package llm

import (
	pubLLM "github.com/Ricardo-M-L/metis/pkg/llm"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

// --- Effort re-exports (originally Round 2's pkg/llm split) ---

type Effort = pubLLM.Effort

const (
	EffortDefault = pubLLM.EffortDefault
	EffortLow     = pubLLM.EffortLow
	EffortMedium  = pubLLM.EffortMedium
	EffortHigh    = pubLLM.EffortHigh
)

var ParseEffort = pubLLM.ParseEffort

// --- Provider re-exports (Round 19's pkg/provider split) ---

// Wire-format type aliases. Existing call sites use these unchanged.
type (
	Role                 = pubprov.Role
	ContentBlock         = pubprov.ContentBlock
	Message              = pubprov.Message
	ToolSpec             = pubprov.ToolSpec
	ResponseFormat       = pubprov.ResponseFormat
	Request              = pubprov.Request
	SystemSection        = pubprov.SystemSection
	Response             = pubprov.Response
	StreamEvent          = pubprov.StreamEvent
	Provider             = pubprov.Provider
	ContextHistoryPolicy = pubprov.ContextHistoryPolicy
	StreamReader         = pubprov.StreamReader
	VisionCapability     = pubprov.VisionCapability
)

// Role constants. pkg/provider declares the canonical values.
const (
	RoleSystem        = pubprov.RoleSystem
	RoleUser          = pubprov.RoleUser
	RoleAssistant     = pubprov.RoleAssistant
	RoleTool          = pubprov.RoleTool
	VisionUnknown     = pubprov.VisionUnknown
	VisionUnsupported = pubprov.VisionUnsupported
	VisionSupported   = pubprov.VisionSupported
)
