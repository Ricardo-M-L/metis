package tui

// cron_chip_test.go — pins the two status-bar cron chips against a
// scratch ~/.metis/cron directory. The chip helpers (wakeupChip,
// silentFiresChip) read the same on-disk format the cron service
// writes; this test plants the right shape and confirms the chip
// text without booting the full cron daemon.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withScratchMetisHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	// Invalidate caches so this test sees fresh state from the
	// scratch home — the chip cache TTL otherwise keeps a previous
	// test's "no wakeup" answer for 5s.
	cronChipMu.Lock()
	cronChipCheckedAt = time.Time{}
	cronChipWakeup = ""
	cronChipSilentFires24h = 0
	cronChipMu.Unlock()
	return dir
}

func writeJobFile(t *testing.T, dir string, j cronJobOnDisk) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(j)
	if err := os.WriteFile(filepath.Join(dir, j.ID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWakeupChip_NoCronDirReturnsEmpty(t *testing.T) {
	withScratchMetisHome(t)
	if got := wakeupChip(); got != "" {
		t.Errorf("missing ~/.metis/cron should yield empty chip; got %q", got)
	}
}

func TestWakeupChip_RendersSoonestActiveWakeup(t *testing.T) {
	home := withScratchMetisHome(t)
	cronDir := filepath.Join(home, "cron")
	// Plant 3 jobs: one wakeup 18m away, one wakeup 60m away, one
	// non-wakeup cron job. Chip should show the 18m one.
	writeJobFile(t, cronDir, cronJobOnDisk{
		ID: "j1", Name: "wakeup: check ci",
		Enabled: true, NextRun: time.Now().Add(18 * time.Minute),
	})
	writeJobFile(t, cronDir, cronJobOnDisk{
		ID: "j2", Name: "wakeup: tomorrow report",
		Enabled: true, NextRun: time.Now().Add(60 * time.Minute),
	})
	writeJobFile(t, cronDir, cronJobOnDisk{
		ID: "j3", Name: "daily news digest", // NOT a wakeup
		Enabled: true, NextRun: time.Now().Add(2 * time.Minute),
	})
	got := wakeupChip()
	if !strings.Contains(got, "wake") {
		t.Errorf("chip should mention 'wake'; got %q", got)
	}
	// `compactDuration` truncates fractional minutes so an 18-minute
	// NextRun reads as "17m" by the time we re-measure. Accept any
	// minute in 16-19 to keep the test stable on slow CI.
	if !strings.ContainsAny(got, "1") || (!strings.Contains(got, "16m") &&
		!strings.Contains(got, "17m") && !strings.Contains(got, "18m") &&
		!strings.Contains(got, "19m")) {
		t.Errorf("chip should show soonest wakeup (~18m); got %q", got)
	}
	if strings.Contains(got, "59m") || strings.Contains(got, "60m") || strings.Contains(got, "1h") {
		t.Errorf("chip must surface only the SOONEST wake; got %q", got)
	}
}

func TestWakeupChip_IgnoresPausedAndDisabled(t *testing.T) {
	home := withScratchMetisHome(t)
	cronDir := filepath.Join(home, "cron")
	writeJobFile(t, cronDir, cronJobOnDisk{
		ID: "paused", Name: "wakeup: paused-x",
		Enabled: true, Paused: true,
		NextRun: time.Now().Add(5 * time.Minute),
	})
	writeJobFile(t, cronDir, cronJobOnDisk{
		ID: "off", Name: "wakeup: disabled-x",
		Enabled: false,
		NextRun: time.Now().Add(5 * time.Minute),
	})
	if got := wakeupChip(); got != "" {
		t.Errorf("paused/disabled wakeups must not appear; got %q", got)
	}
}

func TestWakeupChip_IgnoresPastWakeups(t *testing.T) {
	home := withScratchMetisHome(t)
	cronDir := filepath.Join(home, "cron")
	writeJobFile(t, cronDir, cronJobOnDisk{
		ID: "old", Name: "wakeup: yesterday",
		Enabled: true,
		NextRun: time.Now().Add(-30 * time.Minute),
	})
	if got := wakeupChip(); got != "" {
		t.Errorf("past wakeups should be skipped; got %q", got)
	}
}

func TestSilentFiresChip_ZeroFiresHidden(t *testing.T) {
	withScratchMetisHome(t)
	if got := silentFiresChip(); got != "" {
		t.Errorf("no fires should hide chip; got %q", got)
	}
}

func TestSilentFiresChip_CountsLast24h(t *testing.T) {
	home := withScratchMetisHome(t)
	auditDir := filepath.Join(home, "cron", "audit", "j1")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two recent files (today), one stale (older than 24h).
	recent := filepath.Join(auditDir, "2026-05-13T01-00-00Z.jsonl")
	older := filepath.Join(auditDir, "2026-05-10T01-00-00Z.jsonl")
	if err := os.WriteFile(recent, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(older, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the stale one.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(older, old, old); err != nil {
		t.Fatal(err)
	}
	got := silentFiresChip()
	if !strings.Contains(got, "1/24h") {
		t.Errorf("expected '1/24h' (only the recent file); got %q", got)
	}
}

func TestCompactDuration_Buckets(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{59 * time.Second, "<1m"},
		{1 * time.Minute, "1m"},
		{18 * time.Minute, "18m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h"},
		{90 * time.Minute, "1h30m"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := compactDuration(c.d); got != c.want {
			t.Errorf("compactDuration(%v) = %q; want %q", c.d, got, c.want)
		}
	}
}
