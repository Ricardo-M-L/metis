package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDesktopSubagentSlotsBoundAcrossAcquires(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(desktopSubagentSlotDirEnv, dir)
	t.Setenv(desktopSubagentSlotsEnv, "1")
	first, err := AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = AcquireDesktopSubagentSlot(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second slot acquire = %v, want deadline exceeded", err)
	}
	first()
	second, err := AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second()
}

func TestTeammateReleasesDesktopSlotWhenUnregistered(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(desktopSubagentSlotDirEnv, dir)
	t.Setenv(desktopSubagentSlotsEnv, "1")
	release, err := AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roster := NewRoster(1)
	teammate := &Teammate{Name: "worker"}
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	teammate.SetResourceRelease(release)
	roster.UnregisterTeammate(teammate)
	second, err := AcquireDesktopSubagentSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second()
}
