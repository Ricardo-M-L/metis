package runtime

import "testing"

func TestProfileSnapshotPreservesTrustAndIndependentSlices(t *testing.T) {
	p := &AgentProfile{Name: "role", Source: AgentProfileSourceProject, PermissionMode: "fullAccess", Model: "bound-model", Tools: []string{"Read"}, Skills: []string{"review"}, SystemPrompt: "fixed role"}
	snapshot := SnapshotAgentProfile(p)
	p.Tools[0] = "Bash"
	p.Skills[0] = "write"
	restored := AgentProfileFromSnapshot(snapshot)
	if restored.Source != AgentProfileSourceProject || restored.Tools[0] != "Read" || restored.Skills[0] != "review" || restored.SystemPrompt != "fixed role" {
		t.Fatalf("snapshot lost identity or trust: %+v", restored)
	}
	restored.Tools[0] = "Edit"
	if snapshot.Tools[0] != "Read" {
		t.Fatal("restored profile mutates frozen snapshot")
	}
}
