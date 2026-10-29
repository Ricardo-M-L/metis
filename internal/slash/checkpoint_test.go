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
