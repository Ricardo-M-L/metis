package transport

import (
	"regexp"
	"strconv"
	"strings"
)

// suffixRe captures the canonical size hint that vendors stamp into
// model ids when they ship multiple windows of the same family.
// Examples it must catch:
//
//	deepseek-v3.2-32k          → 32,000
//	moonshot-v1-128k           → 128,000
//	kimi-k1.5-200k             → 200,000
//	deepseek-v4-1m             → 1,000,000
//	gemini-2.0-flash-2m        → 2,000,000
//	qwen3-235b-a22b-thinking-2507-256k → 256,000
//
// Matched at end-of-string only — a "k" or "m" inside the body of
// the model id is part of the family name, not a window declaration.
var suffixRe = regexp.MustCompile(`(?i)-(\d+)([km])$`)

// ParseModelWindowSuffix extracts the context window from a model id
// that carries a vendor-convention `-Nk` / `-Nm` suffix. Returns
// (0, false) when no suffix matches.
//
// This is the LAST tier in the provider.MaxContextTokens fallback
// chain — used when:
//  1. user didn't set context_window in config.toml,
//  2. the models.dev catalog hasn't loaded yet or doesn't know this
//     model (self-hosted forks, brand-new releases), AND
//  3. no hardcoded prefix entry covers the model id.
//
// Mirrors DeepSeek-TUI's tui/src/models.rs convention so users who
// publish a new variant only need to encode the window in the name
// to get correct accounting — no metis-side change required.
func ParseModelWindowSuffix(modelID string) (int, bool) {
	m := suffixRe.FindStringSubmatch(modelID)
	if len(m) != 3 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "k":
		return n * 1_000, true
	case "m":
		return n * 1_000_000, true
	}
	return 0, false
}
