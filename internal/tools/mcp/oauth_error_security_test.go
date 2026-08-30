package mcp_tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	clientmcp "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/mcpoauth"
)

func TestLazyMCPToolDoesNotExposeRefreshTokenFromOAuthFailure(t *testing.T) {
	const refreshToken = "opaque lazy refresh/+== ?&value"
	t.Setenv("METIS_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		echo := r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": echo, "error_description": echo + " / " + url.QueryEscape(echo),
		})
	}))
	t.Cleanup(server.Close)

	serverURL := server.URL + "/mcp"
	store := mcpoauth.NewTokenStore()
	if err := store.PutEntry("secure", &mcpoauth.TokenEntry{
		ServerURL: serverURL, ResourceURL: serverURL, Issuer: server.URL,
		AuthURL: server.URL + "/authorize", TokenURL: server.URL,
		ClientID: "client", Token: &auth.Token{
			AccessToken: "expired", RefreshToken: refreshToken, ExpiresAt: time.Now().Add(-time.Hour),
		},
	}); err != nil {
		t.Fatal(err)
	}

	lazy := NewLazyServer("secure", []clientmcp.Tool{{
		Name: "cached", Description: "cached OAuth tool",
		InputSchema: map[string]any{"type": "object"},
	}}, func(ctx context.Context) (*clientmcp.Client, error) {
		_, err := store.EnsureToken(ctx, "secure", serverURL, false)
		return nil, err
	})
	result, err := lazy.Tools()[0].Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("lazy tool Execute: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("lazy OAuth failure result = %#v", result)
	}
	for _, candidate := range []string{refreshToken, url.QueryEscape(refreshToken), url.PathEscape(refreshToken)} {
		if candidate != "" && strings.Contains(result.Output, candidate) {
			t.Fatalf("lazy tool-visible OAuth error leaked refresh credential: %q", result.Output)
		}
	}
}
