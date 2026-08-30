package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOAuthResourceIndicatorIsSentOnAuthorizationCodeAndRefresh(t *testing.T) {
	const resourceURL = "https://resource.example.test/tenant/api/"
	p := OAuthProvider{
		AuthURL:     "https://issuer.example.test/authorize",
		ClientID:    "client",
		ResourceURL: resourceURL,
	}
	authURL, err := url.Parse(buildAuthURL(p, "http://127.0.0.1/callback", "state", "challenge", false))
	if err != nil {
		t.Fatal(err)
	}
	if got := authURL.Query().Get("resource"); got != resourceURL {
		t.Fatalf("authorization resource = %q, want %q", got, resourceURL)
	}

	requests := make(chan url.Values, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		requests <- r.Form
		_, _ = fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	p.TokenURL = server.URL

	if _, err := exchangeCodeForTokenFullContext(context.Background(), p, "code", "redirect", "verifier"); err != nil {
		t.Fatalf("authorization-code exchange: %v", err)
	}
	if _, err := RefreshTokenContext(context.Background(), p, "refresh"); err != nil {
		t.Fatalf("refresh exchange: %v", err)
	}

	for _, grant := range []string{"authorization_code", "refresh_token"} {
		form := <-requests
		if got := form.Get("grant_type"); got != grant {
			t.Fatalf("grant type = %q, want %q", got, grant)
		}
		if got := form.Get("resource"); got != resourceURL {
			t.Fatalf("%s resource = %q, want %q", grant, got, resourceURL)
		}
	}
}

func TestOAuthTokenResponsesAreSizeBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"access_token":"access"}`)
		_, _ = fmt.Fprint(w, strings.Repeat(" ", 2<<20))
	}))
	t.Cleanup(server.Close)
	p := OAuthProvider{TokenURL: server.URL, ClientID: "client"}

	if _, err := exchangeCodeForTokenFullContext(context.Background(), p, "code", "redirect", "verifier"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "large") {
		t.Fatalf("oversized authorization-code response accepted: %v", err)
	}
	if _, err := RefreshTokenContext(context.Background(), p, "refresh"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "large") {
		t.Fatalf("oversized refresh response accepted: %v", err)
	}
}

func TestOAuthCallbackHandlerDoesNotBlockOnRepeatedDelivery(t *testing.T) {
	resultCh := make(chan oauthCallbackResult, 10)
	server := newCallbackServer("state", resultCh)

	const requests = 10
	done := make(chan struct{}, requests)
	for i := 0; i < requests; i++ {
		go func() {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/callback?state=state&code=code", nil)
			server.Handler.ServeHTTP(recorder, request)
			done <- struct{}{}
		}()
	}
	for i := 0; i < requests; i++ {
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("callback request %d/%d blocked", i+1, requests)
		}
	}
	if got := len(resultCh); got != 1 {
		t.Fatalf("callback result count = %d, want exactly one", got)
	}
}

func TestOAuthCallbackWritesSuccessBeforePublishingResult(t *testing.T) {
	resultCh := make(chan oauthCallbackResult, 1)
	server := newCallbackServer("state", resultCh)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/callback?state=state&code=code", nil)
	done := make(chan struct{})
	go func() {
		server.Handler.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case result := <-resultCh:
		if result.err != nil || result.code != "code" {
			t.Fatalf("callback result = %+v", result)
		}
		if !strings.Contains(recorder.Body.String(), "Authorized") {
			t.Fatalf("callback published before success body was written: %q", recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("callback result was not published")
	}
	<-done
}

func TestRunOAuthManualContextCancellationInterruptsPasteCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	p := OAuthProvider{
		AuthURL: "https://issuer.example.test/authorize", TokenURL: "https://issuer.example.test/token",
		ManualRedirectURL: "https://issuer.example.test/manual",
	}

	go func() {
		_, err := runOAuthManualContext(ctx, p, "verifier", "challenge", "state", OAuthOptions{
			SkipPersist:    true,
			AuthURLHandler: func(string) error { return nil },
			PasteCode: func(string) (string, error) {
				close(started)
				<-release
				return "code", nil
			},
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("manual paste callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		close(release)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("manual cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(release)
		<-done
		t.Fatal("manual paste callback ignored context cancellation")
	}
}

func TestRunOAuthManualPassesCancellationToContextPasteCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	p := OAuthProvider{
		AuthURL: "https://issuer.example.test/authorize", TokenURL: "https://issuer.example.test/token",
		ManualRedirectURL: "https://issuer.example.test/manual",
	}
	go func() {
		_, err := runOAuthManualContext(ctx, p, "verifier", "challenge", "state", OAuthOptions{
			SkipPersist:    true,
			AuthURLHandler: func(string) error { return nil },
			PasteCodeContext: func(callbackCtx context.Context, _ string) (string, error) {
				close(started)
				<-callbackCtx.Done()
				return "", callbackCtx.Err()
			},
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("context paste cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context-aware paste callback did not observe cancellation")
	}
}
