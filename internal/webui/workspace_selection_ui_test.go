package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestWorkspaceSelectionBrowserInteractions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sources := map[string]string{}
	for _, name := range []string{"app.js", "sessions.js", "chat.js"} {
		content, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(content)
	}
	payload, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "../../scripts/e2e/workspace_selection_ui_check.cjs")
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("workspace selection browser checks: %v\n%s", err, out)
	}
}
