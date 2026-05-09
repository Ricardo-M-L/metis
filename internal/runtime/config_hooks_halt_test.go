package runtime

// config_hooks_halt_test.go pins the three new behaviors landed for
// SUMMARY #1 (Hook 三态决议):
//
//   1. JSON `{"decision":"halt"}` lifts ModifiedPreToolUse.Halt = true.
//   2. JSON top-level `{"halt":true}` flag does the same (so users
//      can attach halt to an existing decision form like
//      `{"decision":"deny","halt":true}`).
//   3. Claude-code envelope (`hookSpecificOutput.permissionDecision`)
//      is parsed equivalently to the flat form.
//
// These are unit tests for parsePreToolUseResponse / preToolFromDecision;
// the exit-code-49 path is covered separately because it requires an
// actual subprocess.

import (
	"testing"

	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestParsePreTool_HaltDecision(t *testing.T) {
	got := parsePreToolUseResponse([]byte(`{"decision":"halt","reason":"forbidden path"}`))
	if got == nil {
		t.Fatal("expected non-nil mod")
	}
	if !got.Halt {
		t.Error("Halt should be true")
	}
	if got.HaltReason != "forbidden path" {
		t.Errorf("HaltReason = %q, want %q", got.HaltReason, "forbidden path")
	}
	// halt MUST also emit an Output so the model gets a tool_result
	// for the call it just issued — without this the transcript ends
	// on a dangling tool_use.
	if got.Output == nil {
		t.Error("halt decision should also synthesize an Output for the dangling tool_use")
	}
}

func TestParsePreTool_HaltFlagAlongsideDeny(t *testing.T) {
	got := parsePreToolUseResponse([]byte(`{"decision":"deny","reason":"no","halt":true}`))
	if got == nil || !got.Halt {
		t.Fatalf("halt flag should propagate alongside deny; got %+v", got)
	}
	if got.Output == nil || !got.Output.IsError {
		t.Error("deny + halt should produce an error Output")
	}
}

func TestParsePreTool_DefaultHaltReasonWhenBlank(t *testing.T) {
	got := parsePreToolUseResponse([]byte(`{"decision":"halt"}`))
	if got == nil || got.HaltReason == "" {
		t.Fatalf("blank halt reason should default; got %+v", got)
	}
}

func TestParsePreTool_ClaudeEnvelopeDeny(t *testing.T) {
	body := []byte(`{
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": "blocked by policy"
        }
    }`)
	got := parsePreToolUseResponse(body)
	if got == nil {
		t.Fatal("expected non-nil mod for envelope")
	}
	if got.Output == nil || !got.Output.IsError {
		t.Error("claude-code envelope deny should map to error Output")
	}
	if got.Output.Content != "blocked by policy" {
		t.Errorf("reason not preserved; got %q", got.Output.Content)
	}
}

func TestParsePreTool_ClaudeEnvelopeModifiedInput(t *testing.T) {
	body := []byte(`{
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "allow",
            "modifiedInput": {"command": "ls /safe"}
        }
    }`)
	got := parsePreToolUseResponse(body)
	if got == nil {
		t.Fatal("modifiedInput in envelope should produce a mod")
	}
	if got.ModifiedInput["command"] != "ls /safe" {
		t.Errorf("ModifiedInput.command = %v, want %q", got.ModifiedInput["command"], "ls /safe")
	}
}

func TestParsePreTool_ClaudeEnvelopeAllowIsNoOp(t *testing.T) {
	body := []byte(`{
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "allow"
        }
    }`)
	got := parsePreToolUseResponse(body)
	if got != nil {
		t.Errorf("plain allow with no modifiedInput should be a no-op (return nil); got %+v", got)
	}
}

func TestParsePreTool_FlatAllowStillNoOp(t *testing.T) {
	got := parsePreToolUseResponse([]byte(`{"decision":"allow"}`))
	if got != nil {
		t.Errorf("flat allow with nothing else should remain no-op; got %+v", got)
	}
}

func TestParsePreTool_EmptyBody(t *testing.T) {
	if got := parsePreToolUseResponse(nil); got != nil {
		t.Errorf("empty body should be nil; got %+v", got)
	}
	if got := parsePreToolUseResponse([]byte("   ")); got != nil {
		t.Errorf("whitespace-only should be nil; got %+v", got)
	}
}

func TestParsePreTool_MalformedJSONIsNoOp(t *testing.T) {
	if got := parsePreToolUseResponse([]byte("{not json")); got != nil {
		t.Errorf("malformed JSON must NOT block the agent; got %+v", got)
	}
}

// TestParsePreTool_DenyDefaultReason — a deny with blank reason should
// still produce a usable Output so the model sees something on the
// tool_result side. Otherwise the model gets just `IsError=true` with
// empty content and no idea why.
func TestParsePreTool_DenyDefaultReason(t *testing.T) {
	got := parsePreToolUseResponse([]byte(`{"decision":"deny"}`))
	if got == nil || got.Output == nil || got.Output.Content == "" {
		t.Fatalf("deny with blank reason should default to a non-empty message; got %+v", got)
	}
}

// TestPreToolFromDecision_HaltOverridesAllow — if a hook returns
// `{"decision":"allow","halt":true}` the halt flag wins, even though
// allow itself is a no-op decision. Otherwise a partial-allow-with-halt
// would silently drop the halt.
func TestPreToolFromDecision_HaltOverridesAllow(t *testing.T) {
	got := preToolFromDecision("allow", "stop here", nil, true)
	if got == nil || !got.Halt {
		t.Fatalf("halt flag must propagate through allow; got %+v", got)
	}
}

// TestModifiedPreToolUse_FieldsAreReachable — sanity that the
// downstream consumer (dispatch.go) can read the new Halt fields
// off the public type without reaching into runtime internals.
func TestModifiedPreToolUse_FieldsAreReachable(t *testing.T) {
	mod := pubhook.ModifiedPreToolUse{Halt: true, HaltReason: "x"}
	if !mod.Halt || mod.HaltReason != "x" {
		t.Errorf("public field surface broken: %+v", mod)
	}
}
