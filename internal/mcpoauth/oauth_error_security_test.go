package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

type untrustedOAuthDiagnostic struct {
	status int
	code   string
}

func (e *untrustedOAuthDiagnostic) Error() string          { return "untrusted remote prose" }
func (e *untrustedOAuthDiagnostic) OAuthStatusCode() int   { return e.status }
func (e *untrustedOAuthDiagnostic) OAuthErrorCode() string { return e.code }

func TestEnsureTokenAutonomousRefreshHidesProviderEcho(t *testing.T) {
	const refreshToken = "opaque refresh/+== ?&value"
	tests := []struct {
		name        string
		status      int
		encodedEcho bool
	}{
		{name: "200_json_error", status: http.StatusOK},
		{name: "200_url_encoded_json_error", status: http.StatusOK, encodedEcho: true},
		{name: "non_2xx_body", status: http.StatusUnauthorized},
		{name: "same_origin_redirect", status: http.StatusTemporaryRedirect},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			var base string
			mux := http.NewServeMux()
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			base = server.URL

			writeEcho := func(w http.ResponseWriter, status int, echo string) {
				if test.encodedEcho {
					echo = url.QueryEscape(echo)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":             echo,
					"error_description": "provider echoed " + echo + " / " + url.QueryEscape(echo),
				})
			}
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("ParseForm: %v", err)
					http.Error(w, "bad form", http.StatusBadRequest)
					return
				}
				echo := r.Form.Get("refresh_token")
				switch test.status {
				case http.StatusTemporaryRedirect:
					w.Header().Set("Location", base+"/final?echo="+url.QueryEscape(echo))
					w.WriteHeader(http.StatusTemporaryRedirect)
				default:
					writeEcho(w, test.status, echo)
				}
			})
			mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
				writeEcho(w, http.StatusOK, r.URL.Query().Get("echo"))
			})

			serverURL := server.URL + "/mcp"
			store := NewTokenStore()
			if err := store.PutEntry("srv", &TokenEntry{
				ServerURL: serverURL, ResourceURL: serverURL, Issuer: server.URL,
				AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token",
				ClientID: "client", Token: &auth.Token{
					AccessToken: "expired", RefreshToken: refreshToken,
					ExpiresAt: time.Now().Add(-time.Hour),
				},
			}); err != nil {
				t.Fatal(err)
			}

			_, err := store.EnsureToken(context.Background(), "srv", serverURL, false)
			if !errors.Is(err, ErrCredentialReauthRequired) {
				t.Fatalf("EnsureToken error = %v, want ErrCredentialReauthRequired", err)
			}
			if !strings.Contains(err.Error(), "OAuth refresh failed") {
				t.Fatalf("autonomous refresh error lacks stable classification: %v", err)
			}
			assertMCPRefreshSecretHidden(t, err.Error(), refreshToken)
		})
	}
}

func TestEnsureTokenAutonomousRefreshRetainsBoundedOAuthCode(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"arbitrary remote explanation"}`)
	}))
	t.Cleanup(server.Close)

	serverURL := server.URL + "/mcp"
	store := NewTokenStore()
	if err := store.PutEntry("srv", &TokenEntry{
		ServerURL: serverURL, ResourceURL: serverURL, Issuer: server.URL,
		AuthURL: server.URL + "/authorize", TokenURL: server.URL,
		ClientID: "client", Token: &auth.Token{
			AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour),
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := store.EnsureToken(context.Background(), "srv", serverURL, false)
	if !errors.Is(err, ErrCredentialReauthRequired) {
		t.Fatalf("EnsureToken error = %v, want reauthentication classification", err)
	}
	for _, want := range []string{"HTTP 400", "invalid_grant"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("autonomous error %q omitted %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "arbitrary remote explanation") {
		t.Fatalf("autonomous error exposed provider prose: %v", err)
	}
}

func TestAutonomousRefreshFailureRevalidatesDiagnosticFields(t *testing.T) {
	got := autonomousRefreshFailure(&untrustedOAuthDiagnostic{
		status: 999999,
		code:   strings.Repeat("x", 65) + " remote prose",
	})
	if got != "OAuth refresh failed" {
		t.Fatalf("unbounded diagnostic reached autonomous error: %q", got)
	}
}

func assertMCPRefreshSecretHidden(t *testing.T, output, secret string) {
	t.Helper()
	for _, candidate := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
		if candidate != "" && strings.Contains(output, candidate) {
			t.Fatalf("refresh credential leaked through error: %q contains %q", output, candidate)
		}
	}
}
