package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContinuationMatchesCanonicalWorkspaceIdentity(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	job := &CronJob{Kind: CronKindContinuation, OriginSessionID: "session", WorkDir: real, Provider: "provider", Model: "model"}
	identity := ContinuationIdentity{SessionID: "session", WorkDir: alias, Provider: "provider", Model: "model"}
	if !ContinuationMatches(job, identity) {
		t.Fatal("same workspace rejected")
	}
	identity.WorkDir = t.TempDir()
	if ContinuationMatches(job, identity) {
		t.Fatal("different workspace accepted")
	}
	identity.WorkDir = alias
	identity.SessionID = "other"
	if ContinuationMatches(job, identity) {
		t.Fatal("other session accepted")
	}
}
