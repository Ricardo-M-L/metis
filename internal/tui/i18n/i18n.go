// Package i18n provides a minimal localisation layer for metis's TUI.
// Mirrors opencode's per-locale dict pattern (`packages/app/src/
// i18n/{en,zh-CN}.ts`): each locale is a flat string→string map, the
// runtime picks one based on $METIS_LANG / $LANG / config, and a
// parity test keeps locales from drifting (every locale must define
// every key the en locale defines).
//
// Usage:
//
//	import "github.com/Ricardo-M-L/metis/internal/tui/i18n"
//
//	msg := i18n.T("input.placeholder")  // returns localised string
//
// Falls back to en when:
//   - METIS_LANG is unset
//   - the requested locale is unknown
//   - the requested locale is missing the key
//
// Today only en + zh-CN ship. Adding a locale = adding one file with
// the same keys + dropping it into Locales{}. The parity test in
// i18n_test.go guarantees the key set is complete.
package i18n

import (
	"os"
	"strings"
	"sync"
)

// Locales is the registered dictionary set, indexed by locale tag
// (BCP 47 short form). Add new entries via Register so test code can
// validate parity.
var (
	mu      sync.RWMutex
	locales = map[string]map[string]string{
		"en":    en,
		"zh-CN": zhCN,
	}
	current = "" // resolved on first T() call from env / config
)

// Register adds a locale dict at runtime. Used by 3rd-party plugins
// that want to ship translations without forking metis.
func Register(tag string, dict map[string]string) {
	mu.Lock()
	defer mu.Unlock()
	locales[tag] = dict
}

// SetLocale forces a specific locale. Empty restores auto-detect on
// next T() call. Test-friendly + lets a slash command (`/lang zh`)
// switch live.
func SetLocale(tag string) {
	mu.Lock()
	defer mu.Unlock()
	current = tag
}

// resolveLocale picks the active locale based on (in order):
//   - explicit SetLocale
//   - METIS_LANG env var
//   - LANG env var (matched loosely: "zh_CN.UTF-8" → "zh-CN")
//   - "en" fallback
func resolveLocale() string {
	mu.RLock()
	if current != "" {
		c := current
		mu.RUnlock()
		return c
	}
	mu.RUnlock()
	if v := os.Getenv("METIS_LANG"); v != "" {
		return normalize(v)
	}
	if v := os.Getenv("LANG"); v != "" {
		return normalize(v)
	}
	return "en"
}

// normalize maps POSIX locale strings to BCP 47-ish tags. We only
// handle the exact tags we ship today; unknown values get returned
// unchanged so T()'s fallback path drops to en.
func normalize(s string) string {
	s = strings.SplitN(s, ".", 2)[0]
	s = strings.ReplaceAll(s, "_", "-")
	switch s {
	case "zh", "zh-CN", "zh-Hans", "zh-Hans-CN":
		return "zh-CN"
	case "en", "en-US", "en-GB":
		return "en"
	}
	return s
}

// T returns the localised string for `key`. Falls back to the en
// dict when the active locale is missing or doesn't have the key;
// returns the key verbatim as the very last fallback so a missed
// translation surfaces visibly rather than as an empty string.
func T(key string) string {
	tag := resolveLocale()
	mu.RLock()
	defer mu.RUnlock()
	if d, ok := locales[tag]; ok {
		if s, ok := d[key]; ok {
			return s
		}
	}
	if s, ok := locales["en"][key]; ok {
		return s
	}
	return key
}

// Keys returns the en dict's key set — what every locale must
// implement. Used by the parity test.
func Keys() []string {
	out := make([]string, 0, len(en))
	for k := range en {
		out = append(out, k)
	}
	return out
}
