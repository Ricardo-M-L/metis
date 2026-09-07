package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Cancel while the already-returned HTTP 200 body is being read. This pins the
// client-cancel/server-response race without relying on scheduler ordering.
type cancelRefreshBody struct {
	cancel context.CancelFunc
	err    error
}

func (b cancelRefreshBody) Read([]byte) (int, error) { b.cancel(); return 0, b.err }
func (cancelRefreshBody) Close() error               { return nil }

type cancelRefreshTransport struct {
	cancel   context.CancelFunc
	bodyErr  error
	requests int
}

func (transport *cancelRefreshTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requests++
	var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"access_token":"fresh","expires_in":3600}`))
	if transport.requests == 1 {
		body = cancelRefreshBody{cancel: transport.cancel, err: transport.bodyErr}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: request}, nil
}

func TestResolveOAuthCredentialCancellationDuringResponseDecode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bodyErr error
	}{{"empty_response", io.EOF}, {"body_read_error", io.ErrUnexpectedEOF}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			const provider = "cancel-during-refresh-body"
			previous, existed := KnownProviders[provider]
			t.Cleanup(func() {
				if existed {
					KnownProviders[provider] = previous
				} else {
					delete(KnownProviders, provider)
				}
			})
			KnownProviders[provider] = OAuthProvider{Name: provider, TokenURL: "https://issuer.example.test/token", ClientID: "client"}
			original := OAuthCredential{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}
			if err := PutOAuth(provider, original); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &cancelRefreshTransport{cancel: cancel, bodyErr: tc.bodyErr}
			previousClient := oauthHTTPClient
			oauthHTTPClient = &http.Client{Transport: transport}
			t.Cleanup(func() { oauthHTTPClient = previousClient })
			if _, err := ResolveOAuthCredential(ctx, provider); !errors.Is(err, context.Canceled) {
				t.Fatalf("decode-time canceled refresh = %v", err)
			}
			stored, err := GetOAuth(provider)
			if err != nil || stored == nil || stored.AccessToken != original.AccessToken || stored.RefreshToken != original.RefreshToken {
				t.Fatalf("cancellation changed credentials: %+v, %v", stored, err)
			}
			fresh, err := ResolveOAuthCredential(context.Background(), provider)
			if err != nil || fresh == nil || fresh.AccessToken != "fresh" {
				t.Fatalf("cancellation set failure cooldown: %+v, %v", fresh, err)
			}
			if transport.requests != 2 {
				t.Fatalf("refresh requests = %d, want 2", transport.requests)
			}
		})
	}
}
