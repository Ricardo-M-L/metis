package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

func TestBuildSlashModeFullAccessIsSideEffectFreeSignal(t *testing.T) {
	rt := &runtime{
		cfg:  &config.Config{},
		gate: permission.New(permission.ModeDefault),
	}
	r := buildSlash(rt)

	handled, _, sig, _ := r.Parse("/mode fullAccess")
	if !handled {
		t.Fatal("/mode fullAccess was not handled")
	}
	if sig != slash.SignalFullAccess {
		t.Fatalf("/mode fullAccess signal = %v, want SignalFullAccess", sig)
	}
	if got := rt.gate.Mode(); got != permission.ModeDefault {
		t.Fatalf("parsing /mode fullAccess changed gate to %q", got)
	}
}
