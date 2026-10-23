package session

import (
	"errors"
	"os"
	"testing"
)

func TestCheckpointPermissionStateDurableAndRollback(t *testing.T) {
	s, id := checkpointStore(t)
	if err := s.CheckpointPermissionState(id, "plan", "fullAccess"); err != nil {
		t.Fatal(err)
	}
	other, _ := NewStore(s.Dir)
	h, _, err := other.Load(id)
	if err != nil || h.Mode != "plan" || h.PrePlanMode != "fullAccess" {
		t.Fatalf("not durable: %+v %v", h, err)
	}
	before := checkpointBody(t, s, id)
	s.syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	if err := s.CheckpointPermissionState(id, "fullAccess", ""); err == nil {
		t.Fatal("failed fsync approved permission")
	}
	if string(checkpointBody(t, s, id)) != string(before) {
		t.Fatal("failed permission append was not rolled back")
	}
	h, _, err = other.Load(id)
	if err != nil || h.Mode != "plan" {
		t.Fatalf("failed write elevated resume mode: %+v %v", h, err)
	}
}

func TestCheckpointPermissionStateRejectsIncompletePosture(t *testing.T) {
	s, id := checkpointStore(t)
	for _, state := range [][2]string{{"", ""}, {"plan", ""}, {"plan", "plan"}, {"fullAccess", "default"}, {"unknown", ""}} {
		if err := s.CheckpointPermissionState(id, state[0], state[1]); err == nil {
			t.Errorf("accepted invalid state: %q", state)
		}
	}
}
