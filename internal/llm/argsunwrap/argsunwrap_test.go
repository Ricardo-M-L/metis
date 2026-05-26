package argsunwrap

import (
	"reflect"
	"testing"
)

// TestUnwrap_MinimaxBundle — the canonical MiniMax bug shape from
// session 87e366fa: args wrapped as {"_": "<json-object-string>"}.
// Must unwrap to the embedded object.
func TestUnwrap_MinimaxBundle(t *testing.T) {
	in := map[string]any{"_": `{"x":735,"y":130}`}
	got := Unwrap(in)
	want := map[string]any{"x": float64(735), "y": float64(130)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unwrap(%v) = %v, want %v", in, got, want)
	}
}

// TestUnwrap_MinimaxEmptyArgs — the zero-arg variant from the same
// session: `{"_": ""}`. Should normalise to an empty map (which is
// what the tool's `{}`-schema expects).
func TestUnwrap_MinimaxEmptyArgs(t *testing.T) {
	got := Unwrap(map[string]any{"_": ""})
	if got == nil {
		t.Fatal("Unwrap returned nil; want empty map")
	}
	if len(got) != 0 {
		t.Errorf("Unwrap({\"_\":\"\"}) = %v, want empty map", got)
	}
}

// TestUnwrap_NormalArgsUntouched — every shape that ISN'T the buggy
// MiniMax wrapper must return the input unchanged. These cases pin
// the "保证别影响别的模型" guarantee from the user requirement.
func TestUnwrap_NormalArgsUntouched(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
	}{
		{"empty map", map[string]any{}},
		{"nil map", nil},
		{"normal cu args", map[string]any{"x": 735, "y": 130}},
		{"single non-underscore key", map[string]any{"path": "/tmp/foo"}},
		{"underscore key but multiple fields", map[string]any{
			"_": "irrelevant", "real_arg": 42,
		}},
		{"underscore key but value is non-string", map[string]any{
			"_": map[string]any{"x": 1},
		}},
		{"underscore key but value is integer", map[string]any{"_": 42}},
		{"underscore key but value is array", map[string]any{
			"_": []any{1, 2, 3},
		}},
		{"genuine string field happens to be named _", map[string]any{
			"_": "not json at all",
		}},
		{"json array (not object) in _ string", map[string]any{
			"_": "[1,2,3]",
		}},
		{"json scalar in _ string", map[string]any{
			"_": "42",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Unwrap(tc.in)
			// For unchanged shapes the helper should return the same
			// map; reflect.DeepEqual handles nil and empty correctly.
			if !reflect.DeepEqual(got, tc.in) {
				t.Errorf("Unwrap(%v) = %v, want %v (unchanged)", tc.in, got, tc.in)
			}
		})
	}
}

// TestUnwrap_NestedObjectPreserved — when the embedded JSON has
// nested structure (e.g. an Anthropic-style tool with input_schema
// describing arrays / objects), the unwrap must produce the full
// nested shape, not just a top-level skeleton.
func TestUnwrap_NestedObjectPreserved(t *testing.T) {
	in := map[string]any{"_": `{"command":"open","args":["-a","Safari"],"opts":{"wait":true}}`}
	got := Unwrap(in)
	if got["command"] != "open" {
		t.Errorf("nested object lost top-level field; got %v", got)
	}
	args, ok := got["args"].([]any)
	if !ok || len(args) != 2 {
		t.Errorf("nested object lost args array; got %v", got)
	}
	opts, ok := got["opts"].(map[string]any)
	if !ok || opts["wait"] != true {
		t.Errorf("nested object lost opts struct; got %v", got)
	}
}
