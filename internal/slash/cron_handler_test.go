package slash

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func newSlashCronCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Session.Dir = t.TempDir()
	return cfg
}

func TestCron_EmptyShowsUsage(t *testing.T) {
	cfg := newSlashCronCfg(t)
	out, ok := handleCronCommand(cfg, "")
	if !ok {
		t.Error("/cron with no args should still be handled (usage hint)")
	}
	if !strings.Contains(out, "usage") {
		t.Errorf("expected usage hint, got %q", out)
	}
}

func TestCron_AddAndList(t *testing.T) {
	cfg := newSlashCronCfg(t)
	out, _ := handleCronCommand(cfg, "add 5m do the thing")
	if !strings.Contains(out, "created cron job") {
		t.Fatalf("expected creation confirmation, got %q", out)
	}
	out, _ = handleCronCommand(cfg, "list")
	if !strings.Contains(out, "do the thing") {
		t.Errorf("list should show the new job, got %q", out)
	}
}

func TestCron_AddRejectsBadDuration(t *testing.T) {
	cfg := newSlashCronCfg(t)
	out, _ := handleCronCommand(cfg, "add nonsense do something")
	if !strings.Contains(out, "cron:") {
		t.Errorf("bad duration should yield cron: error message; got %q", out)
	}
}

func TestCron_AddNeedsBothFields(t *testing.T) {
	cfg := newSlashCronCfg(t)
	out, _ := handleCronCommand(cfg, "add 5m")
	if !strings.Contains(out, "usage") {
		t.Errorf("missing prompt should print usage; got %q", out)
	}
}

func TestCron_RemoveMissingID(t *testing.T) {
	cfg := newSlashCronCfg(t)
	out, _ := handleCronCommand(cfg, "rm")
	if !strings.Contains(out, "usage") {
		t.Errorf("rm without id should print usage; got %q", out)
	}
}

func TestCron_UnknownSubcommand(t *testing.T) {
	cfg := newSlashCronCfg(t)
	out, _ := handleCronCommand(cfg, "weird-thing")
	if !strings.Contains(out, "usage") {
		t.Errorf("unknown subcommand should print usage; got %q", out)
	}
}
