package permission

import (
	"context"
	"testing"
)

// Reading a credential file must be gated to ASK even in modes that
// otherwise auto-allow read-only tools (ask / acceptEdits / bypass).
func TestGate_SecretReadGatedAcrossModes(t *testing.T) {
	secrets := []string{
		"/Users/x/.ssh/id_ed25519",
		"/Users/x/.ssh/id_rsa",
		"/home/y/.aws/credentials",
		"/root/.kube/config",
		"/Users/x/.gnupg/secring.gpg",
	}
	normal := []string{
		"/Users/x/project/main.go",
		"/Users/x/notes/.env.example",
		"/etc/hosts",
	}
	for _, mode := range []Mode{ModeAsk, ModeAcceptEdits, ModeBypass} {
		g := New(mode)
		for _, p := range secrets {
			if d, _ := g.Check(context.Background(), "Read", p); d != DecisionAsk {
				t.Errorf("mode=%v Read(%q): want ASK, got %v", mode, p, d)
			}
		}
		for _, p := range normal {
			if d, _ := g.Check(context.Background(), "Read", p); d == DecisionAsk {
				t.Errorf("mode=%v Read(%q): normal file should not be gated, got ASK", mode, p)
			}
		}
	}
}
