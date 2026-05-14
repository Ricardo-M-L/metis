package tool

// isenabled_test.go pins the Tool.IsEnabled() interface contract added
// 2026-05-14 as the metis equivalent of claude-code Tool.isEnabled
// (restored-src/src/Tool.ts:403). The contract:
//
//   1. Every tool reports availability via IsEnabled() bool.
//   2. Embedding BaseTool gives a zero-cost default of "always enabled".
//   3. Tools with environment dependencies override IsEnabled and skip
//      the BaseTool embed (so a typo can't accidentally hide the
//      override behind the default).
//
// Registry callers (internal/tools/builtin/register.go) filter out
// IsEnabled()==false BEFORE the tool reaches the model — the model
// never sees a tool it can only fail to call.

import "testing"

func TestBaseTool_DefaultIsEnabled(t *testing.T) {
	var b BaseTool
	if !b.IsEnabled() {
		t.Errorf("BaseTool must report IsEnabled() = true by default; got false")
	}
}

// alwaysOffTool simulates a tool that overrides IsEnabled to return
// false (e.g. LSP without gopls). We don't embed BaseTool so the
// override stands.
type alwaysOffTool struct{ fakeTool }

func (alwaysOffTool) IsEnabled() bool { return false }

func TestOverride_HidesTool(t *testing.T) {
	// Compile-time guard: a tool that overrides IsEnabled satisfies the
	// Tool interface without embedding BaseTool, AND its override wins
	// even when the embedded fakeTool would inherit BaseTool's true.
	var x Tool = alwaysOffTool{}
	if x.IsEnabled() {
		t.Errorf("override IsEnabled()=false must win over embedded BaseTool default; got true")
	}
}

func TestEmbedding_InheritsDefault(t *testing.T) {
	// The standard path: a tool embeds BaseTool, gets IsEnabled() = true
	// for free without writing the method.
	var x Tool = fakeTool{name: "demo"} // fakeTool embeds BaseTool
	if !x.IsEnabled() {
		t.Errorf("tool embedding BaseTool must inherit IsEnabled()=true; got false")
	}
}
