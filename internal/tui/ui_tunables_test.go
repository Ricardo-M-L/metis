package tui

import (
	"testing"
	"time"
)

// TestSetPermissionTimeout_OverridesAndDefaults — verify the 3
// runtime tunables added 2026-05-22 (permission timeout / voice max
// record / status line refresh) both honor a positive override AND
// reject a zero/negative one (keeping the default in place).
//
// These vars previously were `const`. Without these tests a future
// edit could silently break the config wiring by accidentally
// resetting permissionTimeout etc. inside one of the tui_*.go files.
func TestSetPermissionTimeout_OverridesAndDefaults(t *testing.T) {
	// Snapshot + restore so this test doesn't leak state into others.
	orig := permissionTimeout
	t.Cleanup(func() { permissionTimeout = orig })

	SetPermissionTimeout(45 * time.Second)
	if permissionTimeout != 45*time.Second {
		t.Errorf("SetPermissionTimeout(45s) = %v, want 45s", permissionTimeout)
	}

	// Zero / negative should NOT override — that would auto-deny
	// every permission prompt instantly, which is the bug the
	// guard is meant to prevent.
	SetPermissionTimeout(0)
	if permissionTimeout != 45*time.Second {
		t.Errorf("SetPermissionTimeout(0) silently overrode to %v; should have kept 45s", permissionTimeout)
	}
	SetPermissionTimeout(-1 * time.Second)
	if permissionTimeout != 45*time.Second {
		t.Errorf("SetPermissionTimeout(-1s) silently overrode to %v; should have kept 45s", permissionTimeout)
	}
}

func TestSetVoiceMaxRecord_OverridesAndDefaults(t *testing.T) {
	orig := voiceMaxRecord
	t.Cleanup(func() { voiceMaxRecord = orig })

	SetVoiceMaxRecord(120 * time.Second)
	if voiceMaxRecord != 120*time.Second {
		t.Errorf("SetVoiceMaxRecord(120s) = %v, want 120s", voiceMaxRecord)
	}
	SetVoiceMaxRecord(0)
	if voiceMaxRecord != 120*time.Second {
		t.Errorf("SetVoiceMaxRecord(0) leaked through; voiceMaxRecord=%v", voiceMaxRecord)
	}
}

func TestSetStatusLineRefresh_OverridesAndDefaults(t *testing.T) {
	orig := statusLineRefresh
	t.Cleanup(func() { statusLineRefresh = orig })

	SetStatusLineRefresh(10 * time.Second)
	if statusLineRefresh != 10*time.Second {
		t.Errorf("SetStatusLineRefresh(10s) = %v, want 10s", statusLineRefresh)
	}
	SetStatusLineRefresh(0)
	if statusLineRefresh != 10*time.Second {
		t.Errorf("SetStatusLineRefresh(0) leaked through; statusLineRefresh=%v", statusLineRefresh)
	}
}
