package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestBypassReadRedactsSingleLineCredentialInInnocuousFile(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("token="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := (Read{gate: permission.New(permission.ModeBypassPermissions)}).Execute(
		context.Background(), map[string]any{"path": path},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secret) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Read result = %+v, want successful redacted result", result)
	}
}

func TestBypassGrepRedactsSingleLineCredentialInInnocuousFile(t *testing.T) {
	secret := "ghp_" + strings.Repeat("B", 36)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("token="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := NewGrep(permission.New(permission.ModeBypassPermissions)).Execute(
		context.Background(), map[string]any{"root": root, "pattern": "token="},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secret) ||
		!strings.Contains(result.Output, filepath.Join(root, "notes.txt")) ||
		!strings.Contains(result.Output, "[REDACTED]") ||
		strings.Contains(result.Output, "credential file(s) skipped") {
		t.Fatalf("Grep did not return a safely redacted match: %+v", result)
	}
}

func TestGrepDoesNotExposePEMBodyFromInnocuousFile(t *testing.T) {
	for _, mode := range []permission.Mode{permission.ModeDefault, permission.ModeBypassPermissions} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			root := t.TempDir()
			privateKeyBody := strings.Repeat("A", 64)
			ordinaryBase64 := strings.Repeat("B", 64)
			privateKey := "-----BEGIN PRIVATE KEY-----\n" + privateKeyBody + "\n-----END PRIVATE KEY-----\n"
			if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(privateKey), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "ordinary.txt"), []byte(ordinaryBase64+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := NewGrep(permission.New(mode)).Execute(
				context.Background(), map[string]any{"root": root, "pattern": `^[A-Z0-9+/=]{64}$`},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || result.IsError {
				t.Fatal("Grep did not return the ordinary match")
			}
			if strings.Contains(result.Output, privateKeyBody) {
				t.Fatal("Grep result exposed PEM file content")
			}
			if !strings.Contains(result.Output, filepath.Join(root, "notes.txt")) ||
				!strings.Contains(result.Output, filepath.Join(root, "ordinary.txt")) ||
				!strings.Contains(result.Output, "[REDACTED]") ||
				strings.Contains(result.Output, "credential file(s) skipped") {
				t.Fatalf("Grep result = %q, want redacted PEM body and ordinary base64 match", result.Output)
			}
		})
	}
}

func TestGrepRedactsTruncatedPEMBody(t *testing.T) {
	root := t.TempDir()
	privateKeyBody := strings.Repeat("P", 80)
	if err := os.WriteFile(
		filepath.Join(root, "truncated-key.txt"),
		[]byte("-----BEGIN PRIVATE KEY-----\n"+privateKeyBody+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	result, err := NewGrep(permission.New(permission.ModeDefault)).Execute(
		context.Background(), map[string]any{"root": root, "pattern": `^P{80}$`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, privateKeyBody) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Grep result = %+v, want truncated PEM body redacted", result)
	}
}

func TestGrepRedactsGenericMultilineCredentialValue(t *testing.T) {
	root := t.TempDir()
	secret := "nested-grep-password-value"
	content := "metadata:\n  password:\n    " + secret + "\nordinary: visible\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := NewGrep(permission.New(permission.ModeBypassPermissions)).Execute(
		context.Background(), map[string]any{"root": root, "pattern": "nested-grep-password"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secret) ||
		!strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("Grep result = %+v, want generic multiline value redacted", result)
	}
}

func TestGrepRedactsAdditionalCrossLineCredentialForms(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		secret string
	}{
		{name: "quoted YAML key", body: "\"password\":\n  quoted-grep-secret\n", secret: "quoted-grep-secret"},
		{name: "comment before value", body: "password:\n  # injected later\n  comment-grep-secret\n", secret: "comment-grep-secret"},
		{name: "block scalar blank line", body: "password: |\n  first\n\n  block-grep-secret\nordinary: keep\n", secret: "block-grep-secret"},
		{name: "camelCase JSON", body: "{\n  \"clientSecret\":\n  \"camel-grep-secret\"\n}\n", secret: "camel-grep-secret"},
		{name: "TOML triple quote", body: "password = \"\"\"\ntriple-grep-secret\n\"\"\"\n", secret: "triple-grep-secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := NewGrep(permission.New(permission.ModeDefault)).Execute(
				context.Background(), map[string]any{"root": root, "pattern": tt.secret},
			)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || result.IsError || strings.Contains(result.Output, tt.secret) ||
				!strings.Contains(result.Output, "[REDACTED]") {
				t.Fatalf("Grep result = %+v, want %q redacted", result, tt.secret)
			}
		})
	}
}

func TestGrepSkipsKnownCredentialFilesInsideBroadRoot(t *testing.T) {
	root := t.TempDir()
	secret := "not-a-prefixed-secret"
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("API_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("API_KEY=public-example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := NewGrep(permission.New(permission.ModeBypassPermissions)).Execute(
		context.Background(), map[string]any{"root": root, "pattern": "API_KEY="},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || strings.Contains(result.Output, secret) {
		t.Fatalf("Grep result = %+v, want ordinary match without .env secret", result)
	}
	if !strings.Contains(result.Output, "credential file(s) skipped") {
		t.Fatalf("Grep result = %q, want auditable credential-skip footer", result.Output)
	}
}
