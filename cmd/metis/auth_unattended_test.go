package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestDisableAuthWizardForBypassPermissions(t *testing.T) {
	if !disableAuthWizard(false, permission.ModeBypassPermissions) {
		t.Fatal("bypassPermissions must never launch the credential wizard")
	}
	if !disableAuthWizard(false, permission.ModeFullAccess) {
		t.Fatal("fullAccess must never launch the credential wizard")
	}
	if disableAuthWizard(false, permission.ModeDefault) {
		t.Fatal("interactive default mode should retain first-run wizard behavior")
	}
	if disableAuthWizard(false, permission.ModeDontAsk) {
		t.Fatal("dontAsk controls tool approvals, not whether an interactive credential wizard may run")
	}
	if !disableAuthWizard(true, permission.ModeDefault) {
		t.Fatal("explicit --no-auth-wizard must remain authoritative")
	}
}
