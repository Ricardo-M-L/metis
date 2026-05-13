//go:build darwin

package notify

// notify_apple_test.go — exercises the Apple Terminal Bell probe
// without spinning up Terminal.app. Drives scanBellInBlock with
// canned `defaults read` output to pin its parsing rules.
//
// Why not test the full isAppleTerminalAudibleBellDisabled path:
// it shells out to osascript + defaults, which makes tests
// non-deterministic in CI and hits the user's actual Terminal
// preferences. The interesting logic is in scanBellInBlock — that's
// what we pin here.

import "testing"

const sampleDefaultsOutput = `{
    Basic =     {
        ANSIBlueColor = {length = 1014, bytes = 0x6270;};
        Bell = 0;
        FontAntialias = 1;
        name = Basic;
        type = "Window Settings";
    };
    "Pro" =     {
        ANSIBlueColor = {length = 274, bytes = 0x6270;};
        Bell = 1;
        Font = {
            length = 277;
            bytes = 0x62706c69;
        };
        name = Pro;
    };
    "Solarized Dark" =     {
        ANSIBlackColor = {length = 273, bytes = 0x6270;};
        FontAntialias = 1;
        name = "Solarized Dark";
    };
}`

func TestScanBellInBlock_ExplicitOff(t *testing.T) {
	if got := scanBellInBlock(sampleDefaultsOutput, "Basic"); got != "0" {
		t.Errorf("Basic profile has Bell=0 in fixture; got %q", got)
	}
}

func TestScanBellInBlock_ExplicitOn(t *testing.T) {
	if got := scanBellInBlock(sampleDefaultsOutput, "Pro"); got != "1" {
		t.Errorf("Pro profile has Bell=1 in fixture; got %q", got)
	}
}

func TestScanBellInBlock_FieldMissing(t *testing.T) {
	// Solarized Dark's block has no Bell field — typical of profiles
	// where the user never touched the Bell setting (default applies).
	if got := scanBellInBlock(sampleDefaultsOutput, "Solarized Dark"); got != "" {
		t.Errorf("missing Bell field should return empty; got %q", got)
	}
}

func TestScanBellInBlock_UnknownProfile(t *testing.T) {
	if got := scanBellInBlock(sampleDefaultsOutput, "DoesNotExist"); got != "" {
		t.Errorf("unknown profile should return empty; got %q", got)
	}
}

func TestScanBellInBlock_NestedDictsHandledCorrectly(t *testing.T) {
	// The Pro profile contains a nested {…} for Font. Brace depth
	// tracking must NOT exit the Pro block early when it sees the
	// Font block's closing `}` — otherwise it'd never reach Bell.
	// The fixture above places Bell BEFORE the Font nested dict,
	// which is the typical case; this test pins the nested-dict
	// behavior with a fresh fixture where Bell is AFTER the nested
	// dict.
	in := `{
    Foo =     {
        Inner = {
            length = 100;
        };
        Bell = 0;
    };
}`
	if got := scanBellInBlock(in, "Foo"); got != "0" {
		t.Errorf("Bell after nested dict should still be found; got %q", got)
	}
}

func TestScanBellInBlock_ProfileNameWithSpaces(t *testing.T) {
	// Quoted profile names (those with spaces or special chars) are
	// what defaults emits with double-quotes around the heading.
	// scanBellInBlock should match either quoted or bare form.
	in := `{
    "Solar Dark" =     {
        Bell = 0;
    };
}`
	if got := scanBellInBlock(in, "Solar Dark"); got != "0" {
		t.Errorf("quoted profile name should match; got %q", got)
	}
}
