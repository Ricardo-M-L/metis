package slash

import (
	"strings"
	"testing"
)

func TestCheckpointCatalogOffersSessionSwitchOnly(t *testing.T) {
	registry := newRegistryWithBuiltins(t)
	command, ok := registry.Resolve("checkpoint")
	if !ok {
		t.Fatal("checkpoint command is missing")
	}
	if command.ArgumentHint != "[status|on|off]" {
		t.Fatalf("checkpoint hint must offer only the session switch: %q", command.ArgumentHint)
	}
	if strings.Contains(command.Description, "项目范围") || !strings.Contains(command.Description, "当前会话") {
		t.Fatalf("checkpoint description must explain session ownership: %q", command.Description)
	}
}

func TestAutomaticCheckpointControlIsAdvancedButStillDispatches(t *testing.T) {
	registry := newRegistryWithBuiltins(t)
	command, _ := registry.Resolve("checkpoint")
	if command.IsVisible() || strings.Contains(registry.HelpText(), "/checkpoint") {
		t.Fatal("automatic checkpoint controls should not compete with Rewind in discovery")
	}
	handled, _, signal, args := registry.Parse("/checkpoint off")
	if !handled || signal != SignalCheckpoint || args != "off" {
		t.Fatalf("advanced compatibility control did not dispatch: %v %v %q", handled, signal, args)
	}
	rewind, ok := registry.Resolve("rewind")
	if !ok || !rewind.IsVisible() {
		t.Fatal("Rewind must remain discoverable")
	}
}
