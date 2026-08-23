package slash

import (
	"strings"
	"testing"
)

func TestArtifactHandlerBuildsToolDirectedPrompt(t *testing.T) {
	r := NewRegistry()
	RegisterArtifactCommands(r)
	cmd, ok := r.Get("artifact")
	if !ok {
		t.Fatal("artifact command not registered")
	}

	prompt, signal := cmd.Handler("  update the dashboard with a chart  ")
	if signal != SignalCustomPrompt {
		t.Fatalf("signal = %v, want SignalCustomPrompt", signal)
	}
	for _, want := range []string{"`Artifact` tool", "update the dashboard with a chart", "artifact ID", "version", "export|delete"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "  update the dashboard") {
		t.Error("handler should trim surrounding request whitespace")
	}
}

func TestArtifactHandlerWithoutRequestListsBeforeGuidance(t *testing.T) {
	prompt, signal := func() (string, Signal) {
		r := NewRegistry()
		RegisterArtifactCommands(r)
		cmd, _ := r.Get("artifact")
		return cmd.Handler("")
	}()
	if signal != SignalCustomPrompt {
		t.Fatalf("signal = %v, want SignalCustomPrompt", signal)
	}
	for _, want := range []string{"list artifacts", "create", "update", "read", "metis artifacts"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("empty-request prompt missing %q: %s", want, prompt)
		}
	}
}

func TestArtifactsCommandRequestsCurrentSessionList(t *testing.T) {
	r := NewRegistry()
	RegisterArtifactCommands(r)
	cmd, ok := r.Get("artifacts")
	if !ok {
		t.Fatal("artifacts command not registered")
	}
	prompt, signal := cmd.Handler("ignored")
	if signal != SignalCustomPrompt {
		t.Fatalf("signal = %v, want SignalCustomPrompt", signal)
	}
	for _, want := range []string{"`Artifact` tool", "list action", "current session", "/artifact <request>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("list prompt missing %q: %s", want, prompt)
		}
	}
}

func TestRegisterAllIncludesArtifactCommands(t *testing.T) {
	r := NewRegistry()
	RegisterAll(r, nil)
	for _, name := range []string{"artifact", "artifacts"} {
		cmd, ok := r.Get(name)
		if !ok || cmd.Handler == nil {
			t.Errorf("RegisterAll did not install /%s", name)
		}
	}
}
