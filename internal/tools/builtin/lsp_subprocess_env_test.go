//go:build !windows

package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestGoplsDoesNotInheritProviderSecrets(t *testing.T) {
	secret := "gopls-provider-secret"
	t.Setenv("OPENAI_API_KEY", secret)
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-env")
	gopls := filepath.Join(dir, "gopls")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"${OPENAI_API_KEY-unset}\" > %q\nexit 0\n", captured)
	if err := os.WriteFile(gopls, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, err := runGoplsQuery(context.Background(), "definition", filepath.Join(dir, "main.go"), 1, 1)
	if err != nil {
		t.Fatalf("runGoplsQuery: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("runGoplsQuery result = %#v", res)
	}
	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("fake gopls did not run: %v", err)
	}
	if string(got) != "unset" {
		t.Fatalf("gopls inherited provider secret: %q", got)
	}
}
