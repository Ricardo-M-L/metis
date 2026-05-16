package transport

import (
	"regexp"
	"strconv"
)

// FloorOutputTokens is the minimum completion budget we'll accept when
// auto-reducing max_tokens on a context-overflow retry. Below this and
// the model effectively can't say anything useful, so we'd rather fail
// loud with a hint than silently produce a useless one-token response.
const FloorOutputTokens = 512

// SafetyBuffer is reserved on top of the parsed input size when we
// compute "how much room is left for completion." Without it, a tiny
// timing drift between client and server token counts trips the same
// 400 on the retry.
const SafetyBuffer = 1000

// reMaxContextLen — OpenAI / OpenAI-compatible gateways (DeepSeek, Together,
// Groq, ...) all use the canonical phrasing
// "This model's maximum context length is N tokens".
var reMaxContextLen = regexp.MustCompile(`(?i)maximum context length is (\d+)`)

// reRequested — companion phrase "However, you requested N tokens (...)".
var reRequested = regexp.MustCompile(`(?i)you requested (\d+) tokens`)

// reMessagesIn — when present (OpenAI canonical), this is the actual
// input-token figure stripped of the completion budget. Prefer it over
// reRequested so the retry math reflects what we actually need to fit.
var reMessagesIn = regexp.MustCompile(`(?i)\((\d+) in the messages`)

// reAnthropicTooLong — Anthropic's classic 400, distinct from OpenAI's.
// "prompt is too long: N tokens > M maximum"
var reAnthropicTooLong = regexp.MustCompile(`(?i)prompt is too long:\s*(\d+)\s*tokens?\s*>\s*(\d+)`)

// ParseContextOverflow tries to extract (inputTokens, contextLimit)
// from a 4xx body. Returns ok=false when the body isn't a recognised
// overflow shape — caller should fall back to surfacing the raw error.
//
// Mirrors CC's parseMaxTokensContextOverflowError + the Anthropic
// "prompt is too long" path. Used to drive the one-shot max_tokens
// reduction on the retry, identical in spirit to
// services/api/withRetry.ts:384–426.
func ParseContextOverflow(body string) (inputTokens, contextLimit int, ok bool) {
	if m := reAnthropicTooLong.FindStringSubmatch(body); len(m) >= 3 {
		in, err1 := strconv.Atoi(m[1])
		cap, err2 := strconv.Atoi(m[2])
		if err1 == nil && err2 == nil {
			return in, cap, true
		}
	}

	capMatch := reMaxContextLen.FindStringSubmatch(body)
	if len(capMatch) < 2 {
		return 0, 0, false
	}
	cap, err := strconv.Atoi(capMatch[1])
	if err != nil {
		return 0, 0, false
	}

	// Prefer the "(N in the messages" subgroup — that's the real input
	// size. "you requested N tokens" sums messages + completion budget,
	// which would cause the retry to reserve less room than necessary.
	if m := reMessagesIn.FindStringSubmatch(body); len(m) >= 2 {
		if in, err := strconv.Atoi(m[1]); err == nil {
			return in, cap, true
		}
	}
	if m := reRequested.FindStringSubmatch(body); len(m) >= 2 {
		if req, err := strconv.Atoi(m[1]); err == nil {
			return req, cap, true
		}
	}
	return 0, 0, false
}

// BuildOverflowHint produces the user-/model-facing message when the
// retry path can't recover (available < FloorOutputTokens). Includes
// the Fork-specific hint because Fork's warm-spawn pattern is the most
// common producer of unrecoverable overflow — copying the parent's
// 1M-token history outright leaves no room for a completion.
func BuildOverflowHint(inputTokens, contextLimit int) string {
	return "context overflow: input " + strconv.Itoa(inputTokens) +
		" tokens > provider cap " + strconv.Itoa(contextLimit) +
		" tokens. If this came from Fork, the parent history is too large to warm-spawn — " +
		"use Agent({prompt: \"...\"}) for a cold sub-agent instead, or wait for compaction. " +
		"For other paths, the conversation needs to be compacted before retrying."
}

// ComputeAdjustedMaxTokens returns the max_tokens value to use on the
// retry, or (0, false) if no useful retry budget is available.
// Mirrors CC's `Math.max(FLOOR_OUTPUT_TOKENS, availableContext, minRequired)`
// guard (services/api/withRetry.ts:411–415).
func ComputeAdjustedMaxTokens(inputTokens, contextLimit int) (int, bool) {
	available := contextLimit - inputTokens - SafetyBuffer
	if available < FloorOutputTokens {
		return 0, false
	}
	return available, true
}
