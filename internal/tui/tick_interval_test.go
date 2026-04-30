package tui

import (
	"testing"
	"time"
)

// TestTickInterval covers the override precedence order:
// reduced-motion > METIS_TICK_MS env > config > default.
func TestTickInterval(t *testing.T) {
	// Save + restore the package-level reduced-motion flag and config.
	oldRM := reducedMotionEnabled
	oldCfg := perfConfig()
	defer func() {
		reducedMotionEnabled = oldRM
		SetPerfConfig(oldCfg)
	}()
	// Tests below override per-case; reset to defaults between runs so
	// a config-set value from one case doesn't leak into the next.
	resetCfg := func() {
		SetPerfConfig(PerfConfig{TickMs: 40, EventBufferSize: 256, MouseWheelLines: 1})
	}
	resetCfg()

	cases := []struct {
		name        string
		reducedMode bool
		env         string
		want        time.Duration
	}{
		{"default", false, "", 40 * time.Millisecond},
		{"reduced motion wins", true, "16", 500 * time.Millisecond},
		{"power-user 16ms", false, "16", 16 * time.Millisecond},
		{"power-user 100ms", false, "100", 100 * time.Millisecond},
		{"out of range high → default", false, "9999", 40 * time.Millisecond},
		{"out of range low → default", false, "0", 40 * time.Millisecond},
		{"non-numeric → default", false, "fast", 40 * time.Millisecond},
		{"upper bound 1000ms accepted", false, "1000", 1000 * time.Millisecond},
		{"lower bound 1ms accepted", false, "1", 1 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCfg()
			reducedMotionEnabled = tc.reducedMode
			t.Setenv("METIS_TICK_MS", tc.env)
			if got := tickInterval(); got != tc.want {
				t.Errorf("tickInterval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTickInterval_ConfigPrecedence verifies the new precedence chain:
// reduced-motion > env > config > default. Locks in the contract so a
// future refactor doesn't accidentally swap env and config priority.
func TestTickInterval_ConfigPrecedence(t *testing.T) {
	oldRM := reducedMotionEnabled
	oldCfg := perfConfig()
	defer func() {
		reducedMotionEnabled = oldRM
		SetPerfConfig(oldCfg)
	}()
	reducedMotionEnabled = false

	// Config value with no env → config wins over default.
	t.Setenv("METIS_TICK_MS", "")
	SetPerfConfig(PerfConfig{TickMs: 80})
	if got := tickInterval(); got != 80*time.Millisecond {
		t.Errorf("config value should be honored; got %v", got)
	}

	// Env wins over config.
	t.Setenv("METIS_TICK_MS", "20")
	SetPerfConfig(PerfConfig{TickMs: 80})
	if got := tickInterval(); got != 20*time.Millisecond {
		t.Errorf("env should override config; got %v", got)
	}

	// Config-set ReducedMotion still wins over env.
	SetPerfConfig(PerfConfig{TickMs: 20, ReducedMotion: true})
	if got := tickInterval(); got != 500*time.Millisecond {
		t.Errorf("config reduced_motion should win over env; got %v", got)
	}

	// Out-of-range config falls through to default.
	reducedMotionEnabled = false
	t.Setenv("METIS_TICK_MS", "")
	SetPerfConfig(PerfConfig{TickMs: 99999})
	if got := tickInterval(); got != 40*time.Millisecond {
		t.Errorf("out-of-range config should fall through to default; got %v", got)
	}
}

// TestEventBufferSize covers the same env > config > default chain
// for the agent.Event channel depth.
func TestEventBufferSize(t *testing.T) {
	oldCfg := perfConfig()
	defer SetPerfConfig(oldCfg)

	t.Setenv("METIS_EVENT_BUFFER", "")
	SetPerfConfig(PerfConfig{EventBufferSize: 0})
	if got := eventBufferSize(); got != 256 {
		t.Errorf("default = 256, got %d", got)
	}

	SetPerfConfig(PerfConfig{EventBufferSize: 1024})
	if got := eventBufferSize(); got != 1024 {
		t.Errorf("config 1024 → got %d", got)
	}

	t.Setenv("METIS_EVENT_BUFFER", "512")
	if got := eventBufferSize(); got != 512 {
		t.Errorf("env should override config; got %d", got)
	}

	// Out-of-range env (too small) falls through to config.
	t.Setenv("METIS_EVENT_BUFFER", "5")
	if got := eventBufferSize(); got != 1024 {
		t.Errorf("env=5 below floor → expected config 1024; got %d", got)
	}
}

// TestMouseWheelLines locks in the wheel-step tunable.
func TestMouseWheelLines(t *testing.T) {
	oldCfg := perfConfig()
	defer SetPerfConfig(oldCfg)

	t.Setenv("METIS_MOUSE_WHEEL_LINES", "")
	SetPerfConfig(PerfConfig{MouseWheelLines: 0})
	if got := mouseWheelLines(); got != 1 {
		t.Errorf("default = 1, got %d", got)
	}

	SetPerfConfig(PerfConfig{MouseWheelLines: 3})
	if got := mouseWheelLines(); got != 3 {
		t.Errorf("config 3 → got %d", got)
	}

	t.Setenv("METIS_MOUSE_WHEEL_LINES", "5")
	if got := mouseWheelLines(); got != 5 {
		t.Errorf("env should override config; got %d", got)
	}
}
