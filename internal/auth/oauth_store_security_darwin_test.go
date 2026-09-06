//go:build darwin

package auth

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testInheritableCredentialACL = "everyone allow list,search,read,readattr,readextattr,readsecurity,file_inherit,directory_inherit"

func addCredentialTestACL(t *testing.T, path, acl string) {
	t.Helper()
	if output, err := exec.Command("/bin/chmod", "+a", acl, path).CombinedOutput(); err != nil {
		t.Fatalf("add test ACL: %v: %s", err, output)
	}
}

func credentialTestACL(t *testing.T, path string) string {
	t.Helper()
	output, err := exec.Command("/bin/ls", "-lde", path).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect test ACL: %v: %s", err, output)
	}
	_, acl, _ := strings.Cut(string(output), "\n")
	return acl
}

func TestOAuthStoreRemovesInheritedDarwinACL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	addCredentialTestACL(t, home, testInheritableCredentialACL)
	parentACL := credentialTestACL(t, home)
	if !strings.Contains(parentACL, "group:everyone") {
		t.Fatal("test precondition: parent lacks the everyone ACL")
	}
	if err := PutOAuth("test-provider", OAuthCredential{AccessToken: "fake-access", RefreshToken: "fake-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := Set("test-provider", "fake-api-key"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{CredentialDirectory(), OAuthPath(), Path()} {
		if acl := credentialTestACL(t, path); strings.TrimSpace(acl) != "" {
			t.Errorf("private credential path retains ACL: %s: %s", path, acl)
		}
	}
	if got := credentialTestACL(t, home); got != parentACL {
		t.Errorf("shared parent ACL changed: got %q, want %q", got, parentACL)
	}
}

func TestCredentialStoreReadRemovesExistingDarwinACL(t *testing.T) {
	for _, filename := range []string{"auth.json", "llm-oauth.json"} {
		t.Run(filename, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), filename)
			if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			addCredentialTestACL(t, path, "everyone allow read,readattr,readextattr,readsecurity")
			file, found, err := openCredentialStoreFile(path, authStoreMaxJSONBytes, false)
			if err != nil || !found {
				t.Fatalf("open credential store = found %v, err %v", found, err)
			}
			defer file.Close()
			if acl := credentialTestACL(t, path); strings.TrimSpace(acl) != "" {
				t.Errorf("opened credential inode retains ACL: %s", acl)
			}
		})
	}
}

func TestCredentialStoreDarwinACLRemovalUsesOpenedInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	held := filepath.Join(dir, "opened.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	addCredentialTestACL(t, path, "everyone allow read")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	addCredentialTestACL(t, path, "everyone allow read")
	replacementACL := credentialTestACL(t, path)
	if err := validateOpenedCredentialStore(path, file, authStoreMaxJSONBytes, false); err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("replacement rejection = %v", err)
	}
	if acl := credentialTestACL(t, held); strings.TrimSpace(acl) != "" {
		t.Errorf("opened inode retains ACL: %s", acl)
	}
	if acl := credentialTestACL(t, path); acl != replacementACL {
		t.Errorf("replacement inode ACL changed: got %q, want %q", acl, replacementACL)
	}
}
