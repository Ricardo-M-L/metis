package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func codexTestAccessToken(t *testing.T, accountID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		openAIAuthClaim: map[string]any{"chatgpt_account_id": accountID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestLoginOpenAICodexDeviceCodePersistsRefreshableCredential(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	access := codexTestAccessToken(t, "account-123")
	var userCodeCalls, pollCalls, exchangeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/usercode":
			userCodeCalls++
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("user-code request = %s %q", r.Method, r.Header.Get("Content-Type"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "device-id",
				"user_code":      "ABCD-EFGH",
				"interval":       0,
			})
		case "/device/token":
			pollCalls++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode poll request: %v", err)
			}
			if body["device_auth_id"] != "device-id" || body["user_code"] != "ABCD-EFGH" {
				t.Errorf("poll body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"authorization_code": "authorization-code",
				"code_verifier":      "server-code-verifier",
			})
		case "/oauth/token":
			exchangeCalls++
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse exchange: %v", err)
			}
			if r.Form.Get("code") != "authorization-code" || r.Form.Get("code_verifier") != "server-code-verifier" {
				t.Errorf("exchange form = %#v", r.Form)
			}
			if r.Form.Get("redirect_uri") != "https://auth.openai.com/deviceauth/callback" {
				t.Errorf("redirect_uri = %q", r.Form.Get("redirect_uri"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  access,
				"refresh_token": "refresh-secret",
				"expires_in":    3600,
				"token_type":    "Bearer",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := KnownProviders[openAICodexProviderID]
	provider.TokenURL = server.URL + "/oauth/token"
	var notice OpenAICodexDeviceCode
	credential, err := loginOpenAICodexDeviceCodeWithProvider(context.Background(), provider, OpenAICodexDeviceOptions{
		UserCodeURL:     server.URL + "/device/usercode",
		DeviceTokenURL:  server.URL + "/device/token",
		VerificationURI: "https://auth.openai.com/codex/device",
		Notify: func(info OpenAICodexDeviceCode) error {
			notice = info
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if userCodeCalls != 1 || pollCalls != 1 || exchangeCalls != 1 {
		t.Fatalf("calls = user-code:%d poll:%d exchange:%d", userCodeCalls, pollCalls, exchangeCalls)
	}
	if notice.UserCode != "ABCD-EFGH" || notice.VerificationURI == "" {
		t.Fatalf("notice = %#v", notice)
	}
	if credential == nil || credential.AccountID != "account-123" || credential.RefreshToken != "refresh-secret" {
		t.Fatalf("credential = %#v", credential)
	}
	stored, err := GetOAuth(openAICodexProviderID)
	if err != nil || stored == nil || stored.AccountID != "account-123" {
		t.Fatalf("stored credential = %#v, err=%v", stored, err)
	}
}

func TestLoginOpenAICodexDeviceCodePendingHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/usercode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "device-id",
				"user_code":      "ABCD-EFGH",
				"interval":       0,
			})
		case "/device/token":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	provider := KnownProviders[openAICodexProviderID]
	_, err := loginOpenAICodexDeviceCodeWithProvider(ctx, provider, OpenAICodexDeviceOptions{
		UserCodeURL:    server.URL + "/device/usercode",
		DeviceTokenURL: server.URL + "/device/token",
	})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestPollOpenAICodexDeviceAuthFailsImmediatelyForTerminalOAuthErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "access denied on forbidden", status: http.StatusForbidden, body: `{"error":"access_denied"}`, code: "access_denied"},
		{name: "unknown error on forbidden", status: http.StatusForbidden, body: `{"error":{"code":"unknown_error"}}`, code: "request_failed"},
		{name: "denied with malformed success fields", status: http.StatusForbidden, body: `{"error":"access_denied","authorization_code":123}`, code: "access_denied"},
		{name: "expired token on not found", status: http.StatusNotFound, body: `{"error":{"code":"expired_token"}}`, code: "expired_token"},
		{name: "invalid grant on not found", status: http.StatusNotFound, body: `{"error":"invalid_grant"}`, code: "invalid_grant"},
		{name: "error envelope before success status", status: http.StatusOK, body: `{"error":"access_denied"}`, code: "access_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			_, err := pollOpenAICodexDeviceAuth(ctx, server.URL, openAICodexDeviceStart{
				DeviceAuthID: "device-id",
				UserCode:     "ABCD-EFGH",
			}, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("terminal OAuth error classification = %v", err)
			}
			if strings.Contains(err.Error(), "context deadline exceeded") {
				t.Fatal("terminal OAuth error was incorrectly polled until cancellation")
			}
		})
	}
}

func TestPollOpenAICodexDeviceAuthKeepsBarePendingCompatibility(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		for _, body := range []string{"", "not authorized yet", `{}`} {
			t.Run(http.StatusText(status)+"/"+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()

				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
				defer cancel()
				_, err := pollOpenAICodexDeviceAuth(ctx, server.URL, openAICodexDeviceStart{
					DeviceAuthID: "device-id",
					UserCode:     "ABCD-EFGH",
				}, time.Second)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("bare endpoint %d no longer behaves as pending: %v", status, err)
				}
			})
		}
	}
}

func TestLoginOpenAICodexDeviceCodeDoesNotEchoServerSecrets(t *testing.T) {
	const secret = "server-echoed-device-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + secret + `","error_description":"` + secret + `"}`))
	}))
	defer server.Close()

	provider := KnownProviders[openAICodexProviderID]
	_, err := loginOpenAICodexDeviceCodeWithProvider(context.Background(), provider, OpenAICodexDeviceOptions{
		UserCodeURL: server.URL,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("server-controlled secret leaked in error: %v", err)
	}
}

func TestPollOpenAICodexDeviceAuthBare403ThenSuccess(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_code": "fake-authorization-code",
			"code_verifier":      "fake-verifier",
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	poll, err := pollOpenAICodexDeviceAuth(ctx, server.URL, openAICodexDeviceStart{
		DeviceAuthID: "fake-device-id", UserCode: "ABCD-EFGH",
	}, time.Second)
	if err != nil || poll.AuthorizationCode != "fake-authorization-code" || poll.CodeVerifier != "fake-verifier" || calls != 2 {
		t.Fatalf("pending then success = %#v, calls %d, err %v", poll, calls, err)
	}
}
