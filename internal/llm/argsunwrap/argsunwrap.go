// Package argsunwrap normalises tool-call argument maps that arrive in
// non-standard "bundled" shapes from buggy providers, so the downstream
// tool dispatcher sees the same flat object shape every model is
// supposed to produce.
//
// Why this package exists. Session 87e366fa (2026-05-26) showed
// MiniMax-M2.7 emitting cu tool calls like this:
//
//	"input": {"_": "{\"x\":735,\"y\":130}"}
//
// instead of the spec-compliant
//
//	"input": {"x":735, "y":130}
//
// Every cu mouse_move / left_click / type call then failed with
// `invalid params: missing required field: x`, even though the model
// HAD the right coordinates. The bug appears to live in MiniMax's
// Anthropic-shim tool_use serialiser — it wraps the entire args JSON
// into a sentinel "_" key, presumably as a fallback when its function-
// calling backend can't faithfully round-trip an object.
//
// Design constraint from the user ("修 minimax，别影响别的模型"):
// detection MUST be structural and conservative enough that a non-
// buggy provider never trips it. The check:
//
//  1. map has exactly ONE key
//  2. that key is literally "_"
//  3. value is a string
//  4. string parses as a JSON OBJECT (not array / scalar)
//
// No realistic tool schema declares a single argument named "_" of
// type string-containing-JSON, so steps 1-3 already filter almost
// everything; step 4 protects against the diagonal case where a tool
// genuinely takes a string blob and the user happened to name the
// argument "_". When all four pass we unmarshal and substitute; when
// anything else, we return the input unchanged.
//
// Package lives in internal/llm/ rather than inside one provider so
// both the streaming dispatcher (internal/agent/streaming.go) and the
// non-streaming anthropic.go path can share the same heuristic without
// either taking a dependency on the other.
package argsunwrap

import "encoding/json"

// Unwrap returns a normalised copy of `in` when it matches the buggy
// `{"_": "<json-object-string>"}` shape, or `in` itself otherwise.
// Safe to call on any tool-call argument map — non-matching shapes
// are a no-op and the input pointer is returned unchanged so the
// caller's existing reference stays valid.
//
// nil / empty input is returned unchanged (no allocation).
func Unwrap(in map[string]any) map[string]any {
	if len(in) != 1 {
		return in
	}
	raw, ok := in["_"]
	if !ok {
		return in
	}
	str, ok := raw.(string)
	if !ok {
		return in
	}
	if str == "" {
		// Empty string is the MiniMax "no-arg tool" wrapper variant
		// (saw `screenshot input={"_":""}` in the same session). The
		// tool's schema accepts {} for zero-arg calls, so unwrapping
		// to an empty map is the correct restoration.
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(str), &parsed); err != nil {
		// Not a JSON object — keep the input as-is so a tool that
		// genuinely takes a string field named "_" (unlikely but
		// legal) still receives its value untouched.
		return in
	}
	return parsed
}
