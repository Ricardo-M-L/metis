package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type echoingOAuthTransport struct{}

func (echoingOAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	return nil, fmt.Errorf("remote transport echoed request body: %s", body)
}

func TestOAuthTokenEndpointErrorsDoNotExposeGrantCredentials(t *testing.T) {
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
	grants := []struct {
		name     string
		secrets  []string
		exchange func(OAuthProvider) error
	}{
		{
			name:    "authorization_code",
			secrets: []string{"opaque code/+== ?&value", "opaque verifier/+== ?&value"},
			exchange: func(provider OAuthProvider) error {
				_, err := exchangeCodeForTokenFullContext(
					context.Background(), provider,
					"opaque code/+== ?&value", "http://127.0.0.1/callback",
					"opaque verifier/+== ?&value",
				)
				return err
			},
		},
		{
			name:    "refresh_token",
			secrets: []string{"opaque refresh/+== ?&value"},
			exchange: func(provider OAuthProvider) error {
				_, err := RefreshTokenContext(context.Background(), provider, "opaque refresh/+== ?&value")
				return err
			},
		},
	}

	for _, grant := range grants {
		grant := grant
		for _, test := range tests {
			test := test
			t.Run(grant.name+"/"+test.name, func(t *testing.T) {
				var base string
				mux := http.NewServeMux()
				server := httptest.NewServer(mux)
				t.Cleanup(server.Close)
				base = server.URL

				writeEcho := func(w http.ResponseWriter, r *http.Request, status int) {
					if err := r.ParseForm(); err != nil {
						t.Errorf("ParseForm: %v", err)
						http.Error(w, "bad form", http.StatusBadRequest)
						return
					}
					echo := strings.Join(grant.secrets, "::")
					if queryEcho := r.URL.Query().Get("echo"); queryEcho != "" {
						echo = queryEcho
					}
					if test.encodedEcho {
						echo = url.QueryEscape(echo)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error":             echo,
						"error_description": echo + " / " + url.QueryEscape(echo),
					})
				}

				mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
					switch test.status {
					case http.StatusTemporaryRedirect:
						echo := strings.Join(grant.secrets, "::")
						w.Header().Set("Location", base+"/final?echo="+url.QueryEscape(echo))
						w.WriteHeader(http.StatusTemporaryRedirect)
					case http.StatusOK:
						writeEcho(w, r, http.StatusOK)
					default:
						writeEcho(w, r, test.status)
					}
				})
				mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
					writeEcho(w, r, http.StatusOK)
				})

				err := grant.exchange(OAuthProvider{
					TokenURL: server.URL + "/token", ClientID: "client", UsePKCE: true,
				})
				if err == nil {
					t.Fatal("credential-echoing token endpoint unexpectedly succeeded")
				}
				assertOAuthSecretsHidden(t, err.Error(), grant.secrets...)
			})
		}
	}
}

func TestOAuthTokenEndpointErrorRetainsBoundedCodeAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"error":"temporarily_unavailable","error_description":"remote prose must not be returned"}`)
	}))
	t.Cleanup(server.Close)

	_, err := RefreshTokenContext(
		context.Background(), OAuthProvider{TokenURL: server.URL, ClientID: "client"}, "refresh",
	)
	if err == nil {
		t.Fatal("provider rejection unexpectedly succeeded")
	}
	for _, want := range []string{"HTTP 429", "temporarily_unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("provider error %q omitted %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "remote prose") {
		t.Fatalf("provider error description reached caller: %v", err)
	}
}

func TestOAuthTransportErrorsDoNotExposeGrantCredentials(t *testing.T) {
	priorClient := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: echoingOAuthTransport{}}
	t.Cleanup(func() { oauthHTTPClient = priorClient })

	const code = "opaque authorization code/+== ?&value"
	const verifier = "opaque PKCE verifier/+== ?&value"
	_, exchangeErr := exchangeCodeForTokenFullContext(
		context.Background(), OAuthProvider{TokenURL: "https://issuer.example.test/token", UsePKCE: true},
		code, "http://127.0.0.1/callback", verifier,
	)
	if exchangeErr == nil {
		t.Fatal("echoing authorization-code transport unexpectedly succeeded")
	}
	assertOAuthSecretsHidden(t, exchangeErr.Error(), code, verifier)

	const refresh = "opaque refresh token/+== ?&value"
	_, refreshErr := RefreshTokenContext(
		context.Background(), OAuthProvider{TokenURL: "https://issuer.example.test/token"}, refresh,
	)
	if refreshErr == nil {
		t.Fatal("echoing refresh transport unexpectedly succeeded")
	}
	assertOAuthSecretsHidden(t, refreshErr.Error(), refresh)
}

func TestOAuthProviderErrorCodeIsStrictlyBounded(t *testing.T) {
	const secret = "opaque refresh/+== ?&value"
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "standard", raw: "invalid_grant", want: "invalid_grant"},
		{name: "maximum_length", raw: strings.Repeat("a", oauthMaxProviderCodeBytes), want: strings.Repeat("a", oauthMaxProviderCodeBytes)},
		{name: "too_long", raw: strings.Repeat("a", oauthMaxProviderCodeBytes+1)},
		{name: "remote_prose", raw: "invalid_grant: retry with another credential"},
		{name: "raw_secret", raw: secret},
		{name: "encoded_secret", raw: url.QueryEscape(secret)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeOAuthProviderCode(test.raw, []string{secret}); got != test.want {
				t.Fatalf("sanitizeOAuthProviderCode(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func assertOAuthSecretsHidden(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		for _, candidate := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			if candidate != "" && strings.Contains(output, candidate) {
				t.Fatalf("OAuth credential leaked through error: %q contains %q", output, candidate)
			}
		}
	}
}
