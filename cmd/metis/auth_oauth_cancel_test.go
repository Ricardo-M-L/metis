package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

func TestCompleteOAuthLoginDoesNotPersistAfterAcquisitionIsCancelled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())

	original := richOAuthLogin
	t.Cleanup(func() { richOAuthLogin = original })
	ctx, cancel := context.WithCancel(context.Background())
	richOAuthLogin = func(context.Context, string, auth.OAuthOptions) (*auth.OAuthCredential, error) {
		cancel()
		return &auth.OAuthCredential{AccessToken: "must-not-be-persisted"}, nil
	}

	err := completeOAuthLogin(ctx, "openai-codex", "", false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("completeOAuthLogin error = %v, want context cancellation", err)
	}
	credential, loadErr := auth.GetOAuth("openai-codex")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if credential != nil {
		t.Fatal("cancelled OAuth acquisition persisted a credential")
	}
	if _, statErr := os.Lstat(filepath.Join(home, "config.toml")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled OAuth acquisition changed user config: %v", statErr)
	}
}
