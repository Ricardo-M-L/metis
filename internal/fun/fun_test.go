package fun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempHome redirects HOME so FunDir lands in a temp dir per test.
// Restores the original HOME on cleanup.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func TestDispatch_Help(t *testing.T) {
	withTempHome(t)
	out := Dispatch("help")
	if !strings.Contains(out, "/fun lofi") {
		t.Errorf("help should mention /fun lofi; got:\n%s", out)
	}
	if !strings.Contains(out, "mpv") {
		t.Errorf("help should mention mpv dependency; got:\n%s", out)
	}
}

func TestDispatch_EmptyArgsShowsHelp(t *testing.T) {
	withTempHome(t)
	out := Dispatch("")
	if !strings.Contains(out, "/fun lofi") {
		t.Errorf("empty args should show help; got:\n%s", out)
	}
}

func TestDispatch_UnknownSub(t *testing.T) {
	withTempHome(t)
	out := Dispatch("xyzzy")
	if !strings.Contains(out, "unknown") {
		t.Errorf("unknown sub should be flagged; got:\n%s", out)
	}
}

func TestMusicStatus_NoPlayer(t *testing.T) {
	withTempHome(t)
	out := Dispatch("music status")
	if !strings.Contains(out, "no /fun-managed player") {
		t.Errorf("status with no player should say so; got:\n%s", out)
	}
}

func TestMusicStop_NoPlayer(t *testing.T) {
	withTempHome(t)
	out := Dispatch("music stop")
	if !strings.Contains(out, "nothing to stop") {
		t.Errorf("stop with no player should say nothing to stop; got:\n%s", out)
	}
}

func TestPlayerState_RoundTrip(t *testing.T) {
	withTempHome(t)
	in := &PlayerState{
		PID:       os.Getpid(), // always alive
		URL:       "https://example.test/stream",
		Title:     "Test Stream",
		StartedAt: time.Now().Add(-30 * time.Second),
	}
	if err := in.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.Title != in.Title || out.URL != in.URL || out.PID != in.PID {
		t.Errorf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestPlayerState_DeadPidTreatedAsNotRunning(t *testing.T) {
	withTempHome(t)
	// PID 1 (init) is always alive on macOS/Linux — but it's ALMOST
	// certainly not /fun's player. The intent of pidAlive is "the
	// process recorded as ours is alive". For a truly-dead-pid test,
	// pick a wildly high pid that's unlikely to exist.
	in := &PlayerState{PID: 999999, URL: "u", Title: "t", StartedAt: time.Now()}
	if err := in.Save(); err != nil {
		t.Fatal(err)
	}
	_, err := LoadState()
	if err != ErrNotRunning {
		t.Errorf("dead pid should be ErrNotRunning; got %v", err)
	}
	// Stale file must have been cleaned up.
	d, _ := FunDir()
	if _, err := os.Stat(filepath.Join(d, "music_state.json")); !os.IsNotExist(err) {
		t.Errorf("stale state file should be removed; stat err=%v", err)
	}
}

func TestPlayerState_FormatUptime(t *testing.T) {
	cases := []struct {
		ago    time.Duration
		expect string
	}{
		{45 * time.Second, "45s"},
		{2*time.Minute + 14*time.Second, "2m 14s"},
		{1*time.Hour + 3*time.Minute, "1h 03m"},
	}
	for _, c := range cases {
		s := &PlayerState{StartedAt: time.Now().Add(-c.ago)}
		got := s.FormatUptime()
		if got != c.expect {
			t.Errorf("FormatUptime(-%v) = %q; want %q", c.ago, got, c.expect)
		}
	}
}

func TestLofiCommand_UnknownPreset(t *testing.T) {
	withTempHome(t)
	out := Dispatch("lofi sleepydream")
	if !strings.Contains(out, "unknown preset") {
		t.Errorf("unknown preset should be flagged; got:\n%s", out)
	}
}

func TestLofiCommand_StopAlias(t *testing.T) {
	withTempHome(t)
	out := Dispatch("lofi stop")
	if !strings.Contains(out, "nothing to stop") {
		t.Errorf("`/fun lofi stop` should route to musicStop; got:\n%s", out)
	}
}

func TestMusicCommand_PhaseOneB_StubsAreHonest(t *testing.T) {
	withTempHome(t)
	for _, sub := range []string{"play", "pause", "resume", "skip", "vol", "queue", "history"} {
		out := Dispatch("music " + sub)
		if !strings.Contains(out, "Phase 1b") {
			t.Errorf("%q should mention Phase 1b status; got:\n%s", sub, out)
		}
	}
}
