package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOAuthTokenRequestsRejectCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		for _, grant := range []string{"authorization_code", "refresh_token"} {
			t.Run(fmt.Sprintf("%s_%d", grant, status), func(t *testing.T) {
				const redirectSecret = "redirect-location-must-not-leak"
				var reached atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					reached.Add(1)
					_, _ = fmt.Fprint(w, `{"access_token":"redirected"}`)
				}))
				t.Cleanup(target.Close)

				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL+"/?secret="+redirectSecret, status)
				}))
				t.Cleanup(source.Close)

				provider := OAuthProvider{TokenURL: source.URL, ClientID: "client", UsePKCE: true}
				var err error
				if grant == "authorization_code" {
					_, err = exchangeCodeForTokenFullContext(context.Background(), provider, "private-code", "redirect", "verifier")
				} else {
					_, err = RefreshTokenContext(context.Background(), provider, "private-refresh-token")
				}
				if err == nil {
					t.Fatal("cross-origin token redirect was followed")
				}
				if reached.Load() != 0 {
					t.Fatalf("cross-origin destination received %d request(s)", reached.Load())
				}
				if strings.Contains(err.Error(), redirectSecret) {
					t.Fatalf("redirect destination leaked through error: %v", err)
				}
			})
		}
	}
}

func TestOAuthTokenRequestsPreserveSameOriginRedirectBehavior(t *testing.T) {
	for _, status := range []int{
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		for _, grant := range []string{"authorization_code", "refresh_token"} {
			t.Run(fmt.Sprintf("%s_%d", grant, status), func(t *testing.T) {
				mux := http.NewServeMux()
				server := httptest.NewServer(mux)
				t.Cleanup(server.Close)
				mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "/final", status)
				})
				mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
					if status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect {
						if err := r.ParseForm(); err != nil {
							t.Errorf("ParseForm: %v", err)
						}
						if got := r.Form.Get("grant_type"); got != grant {
							t.Errorf("redirected grant_type = %q, want %q", got, grant)
						}
					}
					_, _ = fmt.Fprint(w, `{"access_token":"same-origin"}`)
				})

				provider := OAuthProvider{TokenURL: server.URL + "/start", ClientID: "client", UsePKCE: true}
				var err error
				if grant == "authorization_code" {
					_, err = exchangeCodeForTokenFullContext(context.Background(), provider, "private-code", "redirect", "verifier")
				} else {
					_, err = RefreshTokenContext(context.Background(), provider, "private-refresh-token")
				}
				if err != nil {
					t.Fatalf("same-origin redirect failed: %v", err)
				}
			})
		}
	}
}
