package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// A partial Read must use the complete pinned file as redaction context, not
// only the page returned to the model. Otherwise offset/limit can exclude both
// PEM markers while returning the private-key body verbatim.
func TestBypassReadRedactsPEMWhenPageContainsOnlyBody(t *testing.T) {
	privateKeyBody := strings.Repeat("A", 64)
	path := filepath.Join(t.TempDir(), "ordinary-notes.txt")
	content := "-----BEGIN PRIVATE KEY-----\n" + privateKeyBody + "\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := (Read{gate: permission.New(permission.ModeBypassPermissions)}).Execute(
		context.Background(),
		map[string]any{"path": path, "offset": 2, "limit": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, privateKeyBody) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want successful redacted page", result)
	}
}

func TestDefaultReadRedactsPEMWhenPageContainsOnlyBody(t *testing.T) {
	privateKeyBody := strings.Repeat("B", 64)
	path := filepath.Join(t.TempDir(), "ordinary-notes.txt")
	content := "-----BEGIN PRIVATE KEY-----\n" + privateKeyBody + "\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := (Read{gate: permission.New(permission.ModeDefault)}).Execute(
		context.Background(),
		map[string]any{"path": path, "offset": 2, "limit": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, privateKeyBody) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want successful redacted page", result)
	}
}

// The JSON token-field detector also spans newlines. Keep this regression
// separate from PEM because those two credential families are implemented by
// different scanner layers.
func TestBypassReadRedactsJSONCredentialWhenPageContainsOnlyValue(t *testing.T) {
	secretValue := "json-token-value-must-not-leak"
	path := filepath.Join(t.TempDir(), "ordinary-data.json")
	content := "{\n  \"access_token\":\n  \"" + secretValue + "\"\n}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := (Read{gate: permission.New(permission.ModeBypassPermissions)}).Execute(
		context.Background(),
		map[string]any{"path": path, "offset": 3, "limit": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secretValue) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want successful redacted page", result)
	}
}

func TestReadRedactsTruncatedPEMBodyPage(t *testing.T) {
	privateKeyBody := strings.Repeat("T", 80)
	path := filepath.Join(t.TempDir(), "truncated-key.txt")
	if err := os.WriteFile(path, []byte("-----BEGIN PRIVATE KEY-----\n"+privateKeyBody+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := (Read{gate: permission.New(permission.ModeDefault)}).Execute(
		context.Background(), map[string]any{"path": path, "offset": 2, "limit": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, privateKeyBody) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want truncated PEM body redacted", result)
	}
}

func TestReadRedactsGenericMultilineCredentialValuePage(t *testing.T) {
	secretValue := "generic-password-must-not-leak"
	path := filepath.Join(t.TempDir(), "ordinary-config.yaml")
	content := "title: visible\npassword:\n  " + secretValue + "\nafter: visible\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := (Read{gate: permission.New(permission.ModeBypassPermissions)}).Execute(
		context.Background(), map[string]any{"path": path, "offset": 3, "limit": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secretValue) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want generic credential value redacted", result)
	}
}

func TestReadRedactsCRLFJSONCredentialValuePage(t *testing.T) {
	secretValue := "crlf-token-must-not-leak"
	path := filepath.Join(t.TempDir(), "windows-data.json")
	content := "{\r\n  \"access_token\":\r\n  \"" + secretValue + "\"\r\n}\r\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := (Read{gate: permission.New(permission.ModeDefault)}).Execute(
		context.Background(), map[string]any{"path": path, "offset": 3, "limit": 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secretValue) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want CRLF value page redacted", result)
	}
}

func TestReadRedactsAdditionalCrossLineCredentialForms(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		offset int
		secret string
	}{
		{name: "quoted YAML key", body: "\"password\":\n  quoted-yaml-secret\n", offset: 2, secret: "quoted-yaml-secret"},
		{name: "comment before value", body: "password:\n  # injected later\n  after-comment-secret\n", offset: 3, secret: "after-comment-secret"},
		{name: "block scalar blank line", body: "password: |\n  first\n\n  block-after-blank-secret\nordinary: keep\n", offset: 4, secret: "block-after-blank-secret"},
		{name: "camelCase JSON", body: "{\n  \"clientSecret\":\n  \"camel-read-secret\"\n}\n", offset: 3, secret: "camel-read-secret"},
		{name: "TOML triple quote", body: "password = \"\"\"\ntriple-read-secret\n\"\"\"\n", offset: 2, secret: "triple-read-secret"},
		{name: "physical multiline quote", body: "password: \"first\nmultiline-read-secret\"\n", offset: 2, secret: "multiline-read-secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ordinary-config.txt")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := (Read{gate: permission.New(permission.ModeDefault)}).Execute(
				context.Background(), map[string]any{"path": path, "offset": tt.offset, "limit": 1},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || result.IsError || strings.Contains(result.Output, tt.secret) ||
				!strings.Contains(result.Output, "[REDACTED]") {
				t.Fatalf("Read result = %+v, want %q redacted", result, tt.secret)
			}
		})
	}
}

func TestRedactedFullReadRemainsPartialForWriteButAllowsTargetedEdit(t *testing.T) {
	githubToken := "ghp_" + strings.Repeat("G", 36)
	pemBody := strings.Repeat("K", 80)
	tests := []struct {
		name   string
		secret string
		body   string
	}{
		{
			name:   "single-line token",
			secret: githubToken,
			body:   "editable unique text\ntoken=" + githubToken + "\n",
		},
		{
			name:   "multiline PEM",
			secret: pemBody,
			body: "editable unique text\n-----BEGIN PRIVATE KEY-----\n" + pemBody +
				"\n-----END PRIVATE KEY-----\n",
		},
		{
			name:   "multiline JSON",
			secret: "oauth-state-secret",
			body:   "editable unique text\n{\n  \"access_token\":\n  \"oauth-state-secret\"\n}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ordinary.txt")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			gate := permission.New(permission.ModeBypassPermissions)
			state := NewReadFileState()
			read := Read{gate: gate, state: state}

			result, err := read.Execute(context.Background(), map[string]any{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || result.IsError || strings.Contains(result.Output, tt.secret) ||
				!strings.Contains(result.Output, "[REDACTED]") {
				t.Fatalf("Read result = %+v, want redacted full view", result)
			}
			entry, ok := state.Get(path)
			if !ok || !entry.IsPartialView {
				t.Fatalf("redacted Read state = %+v, want partial view", entry)
			}

			writeResult, err := (Write{gate: gate, state: state}).Execute(
				context.Background(), map[string]any{"path": path, "content": "overwrite\n"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if writeResult == nil || !writeResult.IsError || !strings.Contains(writeResult.Output, "partial view") {
				t.Fatalf("Write result = %+v, want partial-view denial", writeResult)
			}

			editResult, err := (Edit{gate: gate, state: state}).Execute(
				context.Background(),
				map[string]any{"path": path, "old": "editable unique text", "new": "edited unique text"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if editResult == nil || editResult.IsError {
				t.Fatalf("targeted Edit result = %+v, want success", editResult)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), "edited unique text") || !strings.Contains(string(got), tt.secret) {
				t.Fatalf("targeted Edit did not preserve hidden credential bytes: %q", got)
			}

			writeResult, err = (Write{gate: gate, state: state}).Execute(
				context.Background(), map[string]any{"path": path, "content": "overwrite after edit\n"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if writeResult == nil || !writeResult.IsError || !strings.Contains(writeResult.Output, "partial view") {
				t.Fatalf("post-Edit Write result = %+v, want partial-view denial", writeResult)
			}
		})
	}
}

func TestRedactedReadLexicalAliasEditKeepsPartialWriteGuard(t *testing.T) {
	secret := "ghp_" + strings.Repeat("L", 36)
	dir := t.TempDir()
	path := filepath.Join(dir, "ordinary.txt")
	alias := dir + string(os.PathSeparator) + "." + string(os.PathSeparator) + "ordinary.txt"
	if err := os.WriteFile(path, []byte("editable unique text\ntoken="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	if result, err := (Read{gate: gate, state: state}).Execute(
		context.Background(), map[string]any{"path": path},
	); err != nil || result == nil || result.IsError {
		t.Fatalf("Read = %+v, %v", result, err)
	}
	if result, err := (Edit{gate: gate, state: state}).Execute(
		context.Background(), map[string]any{"path": alias, "old": "editable", "new": "edited"},
	); err != nil || result == nil || result.IsError {
		t.Fatalf("alias Edit = %+v, %v", result, err)
	}
	for _, writePath := range []string{path, alias} {
		result, err := (Write{gate: gate, state: state}).Execute(
			context.Background(), map[string]any{"path": writePath, "content": "overwrite\n"},
		)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || !result.IsError || !strings.Contains(result.Output, "partial view") {
			t.Fatalf("Write(%q) = %+v, want partial-view denial", writePath, result)
		}
	}
}

func TestRedactedReadSymlinkAliasEditKeepsPartialWriteGuard(t *testing.T) {
	secret := "ghp_" + strings.Repeat("S", 36)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	alias := filepath.Join(dir, "alias.txt")
	if err := os.WriteFile(target, []byte("editable unique text\ntoken="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, tt := range []struct {
		name, readPath, editPath string
	}{
		{name: "target to alias", readPath: target, editPath: alias},
		{name: "alias to target", readPath: alias, editPath: target},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(target, []byte("editable unique text\ntoken="+secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gate := permission.New(permission.ModeBypassPermissions)
			state := NewReadFileState()
			if result, err := (Read{gate: gate, state: state}).Execute(
				context.Background(), map[string]any{"path": tt.readPath},
			); err != nil || result == nil || result.IsError {
				t.Fatalf("Read = %+v, %v", result, err)
			}
			if result, err := (Edit{gate: gate, state: state}).Execute(
				context.Background(), map[string]any{"path": tt.editPath, "old": "editable", "new": "edited"},
			); err != nil || result == nil || result.IsError {
				t.Fatalf("Edit = %+v, %v", result, err)
			}
			for _, writePath := range []string{target, alias} {
				result, err := (Write{gate: gate, state: state}).Execute(
					context.Background(), map[string]any{"path": writePath, "content": "overwrite\n"},
				)
				if err != nil {
					t.Fatal(err)
				}
				if result == nil || !result.IsError || !strings.Contains(result.Output, "partial view") {
					t.Fatalf("Write(%q) = %+v, want partial-view denial", writePath, result)
				}
			}
		})
	}
}

func TestEditWithStateRequiresPriorRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("old value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := (Edit{
		gate:  permission.New(permission.ModeBypassPermissions),
		state: NewReadFileState(),
	}).Execute(context.Background(), map[string]any{
		"path": path, "old": "old value", "new": "new value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || !strings.Contains(result.Output, "has not been Read") {
		t.Fatalf("Edit result = %+v, want read-first denial", result)
	}
}

func TestWriteThroughSymlinkParentRecordsCanonicalState(t *testing.T) {
	realDir := t.TempDir()
	linkRoot := t.TempDir()
	linkedDir := filepath.Join(linkRoot, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(linkedDir, "new.txt")
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	result, err := (Write{gate: gate, state: state}).Execute(
		context.Background(), map[string]any{"path": path, "content": "old value\n"},
	)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Write = %+v, %v", result, err)
	}
	result, err = (Edit{gate: gate, state: state}).Execute(
		context.Background(), map[string]any{"path": path, "old": "old value", "new": "new value"},
	)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("Edit after symlink-parent Write = %+v, %v", result, err)
	}
}

func TestRedactedReadRejectsEditAll(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	path := filepath.Join(t.TempDir(), "ordinary.txt")
	if err := os.WriteFile(path, []byte("visible visible\ntoken="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	state := NewReadFileState()
	if result, err := (Read{gate: gate, state: state}).Execute(
		context.Background(), map[string]any{"path": path},
	); err != nil || result == nil || result.IsError {
		t.Fatalf("Read = %+v, %v", result, err)
	}
	result, err := (Edit{gate: gate, state: state}).Execute(
		context.Background(), map[string]any{"path": path, "old": "visible", "new": "edited", "all": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || !strings.Contains(result.Output, "all=true") {
		t.Fatalf("Edit all result = %+v, want partial-view denial", result)
	}
}
