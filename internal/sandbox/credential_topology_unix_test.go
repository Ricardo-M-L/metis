//go:build !windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialTopologyRejectsHardLinkedPrivateCredential(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".metis")
	dir := filepath.Join(root, metisCredentialDirectoryName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(credential, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "innocent.txt")
	if err := os.Link(credential, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	err := validateCredentialTopology(home, root)
	if err == nil || !strings.Contains(err.Error(), "multiple hard links") {
		t.Fatalf("hard-linked credential topology error = %v", err)
	}
}

func TestCredentialTopologyAllowsOrdinaryPrivateStore(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".metis")
	dir := filepath.Join(root, metisCredentialDirectoryName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "future-secret.bin"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialTopology(home, root); err != nil {
		t.Fatal(err)
	}
}
