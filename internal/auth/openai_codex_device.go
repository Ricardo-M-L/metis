package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	openAICodexDeviceVerificationURI = "https://auth.openai.com/codex/device"
	openAICodexDeviceRedirectURI     = "https://auth.openai.com/deviceauth/callback"
	openAICodexDeviceTimeout         = 15 * time.Minute
	openAICodexDeviceDefaultInterval = 5 * time.Second
	openAICodexDeviceMinimumInterval = time.Second
	openAICodexDeviceSlowDown        = 5 * time.Second
)

// OpenAICodexDeviceCode is safe, short-lived information the CLI may show to
// the user. It deliberately excludes the device_auth_id, authorization code,
// PKCE verifier, and resulting OAuth tokens.
type OpenAICodexDeviceCode struct {
	UserCode         string
	VerificationURI  string
	IntervalSeconds  int
	ExpiresInSeconds int
}

// OpenAICodexDeviceOptions controls the headless ChatGPT subscription login.
// Endpoint overrides are internal test seams; callers normally set only
// Notify. They remain fields rather than package globals so concurrent tests
// and multiple login attempts cannot race over mutable process state.
type OpenAICodexDeviceOptions struct {
	Notify          func(OpenAICodexDeviceCode) error
	UserCodeURL     string
	DeviceTokenURL  string
	VerificationURI string
}

type openAICodexDeviceStart struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	Interval     json.RawMessage `json:"interval"`
}

type openAICodexDevicePoll struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	Error             any    `json:"error"`
}

// AcquireOpenAICodexDeviceCode performs Pi-compatible OpenAI device-code
// login and returns a refreshable credential without persisting it.
func AcquireOpenAICodexDeviceCode(ctx context.Context, opts OpenAICodexDeviceOptions) (*OAuthCredential, error) {
	return acquireOpenAICodexDeviceCodeWithProvider(ctx, KnownProviders[openAICodexProviderID], opts)
}

// LoginOpenAICodexDeviceCode preserves the historical library contract by
// persisting the acquired credential. The CLI uses Acquire... + ActivateOAuth
// so switching from an API key is one cross-store transaction.
func LoginOpenAICodexDeviceCode(ctx context.Context, opts OpenAICodexDeviceOptions) (*OAuthCredential, error) {
	credential, err := AcquireOpenAICodexDeviceCode(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := PutOAuth(openAICodexProviderID, *credential); err != nil {
		return nil, err
	}
	return cloneOAuthCredential(credential), nil
}

func acquireOpenAICodexDeviceCodeWithProvider(ctx context.Context, provider OAuthProvider, opts OpenAICodexDeviceOptions) (*OAuthCredential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userCodeURL := strings.TrimSpace(opts.UserCodeURL)
	if userCodeURL == "" {
		userCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	}
	deviceTokenURL := strings.TrimSpace(opts.DeviceTokenURL)
	if deviceTokenURL == "" {
		deviceTokenURL = "https://auth.openai.com/api/accounts/deviceauth/token"
	}
	verificationURI := strings.TrimSpace(opts.VerificationURI)
	if verificationURI == "" {
		verificationURI = openAICodexDeviceVerificationURI
	}

	start, interval, err := startOpenAICodexDeviceAuth(ctx, provider.ClientID, userCodeURL)
	if err != nil {
		return nil, err
	}
	info := OpenAICodexDeviceCode{
		UserCode:         start.UserCode,
		VerificationURI:  verificationURI,
		IntervalSeconds:  int(interval / time.Second),
		ExpiresInSeconds: int(openAICodexDeviceTimeout / time.Second),
	}
	if opts.Notify != nil {
		if err := opts.Notify(info); err != nil {
			return nil, fmt.Errorf("openai codex device login notification: %w", err)
		}
	}

	poll, err := pollOpenAICodexDeviceAuth(ctx, deviceTokenURL, start, interval)
	if err != nil {
		return nil, err
	}
	token, err := exchangeCodeForTokenFullWithStateContext(ctx, provider, poll.AuthorizationCode, "", openAICodexDeviceRedirectURI, poll.CodeVerifier)
	if err != nil {
		return nil, err
	}
	credential, err := credentialFromToken(openAICodexProviderID, token)
	if err != nil {
		return nil, err
	}
	return cloneOAuthCredential(credential), nil
}

func loginOpenAICodexDeviceCodeWithProvider(ctx context.Context, provider OAuthProvider, opts OpenAICodexDeviceOptions) (*OAuthCredential, error) {
	credential, err := acquireOpenAICodexDeviceCodeWithProvider(ctx, provider, opts)
	if err != nil {
		return nil, err
	}
	if err := PutOAuth(openAICodexProviderID, *credential); err != nil {
		return nil, err
	}
	return cloneOAuthCredential(credential), nil
}

func startOpenAICodexDeviceAuth(ctx context.Context, clientID, endpoint string) (openAICodexDeviceStart, time.Duration, error) {
	requestBody, err := json.Marshal(map[string]string{"client_id": clientID})
	if err != nil {
		return openAICodexDeviceStart{}, 0, newOAuthEndpointError("device authorization", 0, "invalid_request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return openAICodexDeviceStart{}, 0, newOAuthEndpointError("device authorization", 0, "invalid_request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return openAICodexDeviceStart{}, 0, safeOAuthRequestError(ctx, "device authorization", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return openAICodexDeviceStart{}, 0, decodeOpenAIDeviceEndpointError(resp, "device authorization", nil)
	}
	var start openAICodexDeviceStart
	if err := decodeBoundedOAuthJSON(resp.Body, &start); err != nil {
		return openAICodexDeviceStart{}, 0, newOAuthEndpointError("device authorization", resp.StatusCode, "invalid_response")
	}
	start.DeviceAuthID = strings.TrimSpace(start.DeviceAuthID)
	start.UserCode = strings.TrimSpace(start.UserCode)
	if start.DeviceAuthID == "" || start.UserCode == "" || len(start.DeviceAuthID) > 16<<10 || len(start.UserCode) > 1024 || strings.ContainsAny(start.DeviceAuthID+start.UserCode, "\x00\r\n") {
		return openAICodexDeviceStart{}, 0, newOAuthEndpointError("device authorization", resp.StatusCode, "invalid_response")
	}
	interval := parseOpenAIDeviceInterval(start.Interval)
	return start, interval, nil
}

func pollOpenAICodexDeviceAuth(ctx context.Context, endpoint string, start openAICodexDeviceStart, interval time.Duration) (openAICodexDevicePoll, error) {
	if interval < openAICodexDeviceMinimumInterval {
		interval = openAICodexDeviceMinimumInterval
	}
	deadline := time.NewTimer(openAICodexDeviceTimeout)
	defer deadline.Stop()
	for {
		poll, status, code, hasOAuthError, err := requestOpenAICodexDeviceToken(ctx, endpoint, start)
		if err != nil {
			return openAICodexDevicePoll{}, err
		}
		if hasOAuthError {
			// Parse the OAuth envelope before interpreting the HTTP status. Some
			// endpoints return terminal OAuth errors with 403/404 (and a few
			// compatibility shims even use 200); only the two RFC polling states
			// are allowed to keep the loop alive.
			switch code {
			case "deviceauth_authorization_pending", "authorization_pending":
			case "slow_down":
				interval += openAICodexDeviceSlowDown
			default:
				return openAICodexDevicePoll{}, newOAuthEndpointError("device token", status, code, start.DeviceAuthID, start.UserCode)
			}
		} else if status >= 200 && status < 300 {
			if strings.TrimSpace(poll.AuthorizationCode) == "" || strings.TrimSpace(poll.CodeVerifier) == "" || len(poll.AuthorizationCode) > 16<<10 || len(poll.CodeVerifier) > 16<<10 || strings.ContainsAny(poll.AuthorizationCode+poll.CodeVerifier, "\x00\r\n") {
				return openAICodexDevicePoll{}, newOAuthEndpointError("device token", status, "invalid_response", start.DeviceAuthID, start.UserCode)
			}
			return poll, nil
		} else if status == http.StatusForbidden || status == http.StatusNotFound {
			// OpenAI's device endpoint uses bare/non-OAuth 403 and 404 responses
			// while authorization has not propagated. Preserve that explicit
			// compatibility behavior, but never apply it when an OAuth error field
			// was present (handled above).
		} else {
			return openAICodexDevicePoll{}, newOAuthEndpointError("device token", status, code, start.DeviceAuthID, start.UserCode)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return openAICodexDevicePoll{}, ctx.Err()
		case <-deadline.C:
			timer.Stop()
			return openAICodexDevicePoll{}, errors.New("openai codex device login timed out")
		case <-timer.C:
		}
	}
}

func requestOpenAICodexDeviceToken(ctx context.Context, endpoint string, start openAICodexDeviceStart) (openAICodexDevicePoll, int, string, bool, error) {
	requestBody, err := json.Marshal(map[string]string{
		"device_auth_id": start.DeviceAuthID,
		"user_code":      start.UserCode,
	})
	if err != nil {
		return openAICodexDevicePoll{}, 0, "", false, newOAuthEndpointError("device token", 0, "invalid_request", start.DeviceAuthID, start.UserCode)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return openAICodexDevicePoll{}, 0, "", false, newOAuthEndpointError("device token", 0, "invalid_request", start.DeviceAuthID, start.UserCode)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := doOAuthHTTPRequest(req)
	if err != nil {
		return openAICodexDevicePoll{}, 0, "", false, safeOAuthRequestError(ctx, "device token", err, start.DeviceAuthID, start.UserCode)
	}
	defer resp.Body.Close()
	var poll openAICodexDevicePoll
	if err := decodeBoundedOAuthJSON(resp.Body, &poll); err != nil {
		// A malformed success field or trailing JSON must not turn a decoded
		// terminal OAuth error into a bare pending status.
		if poll.Error != nil {
			return poll, resp.StatusCode, openAIDeviceErrorCode(poll.Error, []string{start.DeviceAuthID, start.UserCode}), true, nil
		}
		return openAICodexDevicePoll{}, resp.StatusCode, "invalid_response", false, nil
	}
	code := openAIDeviceErrorCode(poll.Error, []string{start.DeviceAuthID, start.UserCode})
	return poll, resp.StatusCode, code, poll.Error != nil, nil
}

func decodeOpenAIDeviceEndpointError(resp *http.Response, operation string, secrets []string) error {
	var body struct {
		Error any `json:"error"`
	}
	if err := decodeBoundedOAuthJSON(resp.Body, &body); err != nil {
		return newOAuthEndpointError(operation, resp.StatusCode, "request_failed", secrets...)
	}
	return newOAuthEndpointError(operation, resp.StatusCode, openAIDeviceErrorCode(body.Error, secrets), secrets...)
}

func openAIDeviceErrorCode(value any, secrets []string) string {
	var raw string
	switch value := value.(type) {
	case string:
		raw = value
	case map[string]any:
		raw, _ = value["code"].(string)
	}
	code := sanitizeOAuthProviderCode(raw, secrets)
	switch code {
	case "deviceauth_authorization_pending", "authorization_pending", "slow_down", "expired_token", "access_denied", "invalid_grant", "invalid_request", "server_error", "temporarily_unavailable":
		return code
	default:
		return "request_failed"
	}
}

func decodeBoundedOAuthJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, oauthMaxJSONResponseBytes+1))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func parseOpenAIDeviceInterval(raw json.RawMessage) time.Duration {
	seconds := 0.0
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &seconds); err != nil {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				_, _ = fmt.Sscanf(strings.TrimSpace(text), "%f", &seconds)
			}
		}
	}
	if seconds <= 0 {
		return openAICodexDeviceDefaultInterval
	}
	interval := time.Duration(seconds * float64(time.Second))
	if interval < openAICodexDeviceMinimumInterval {
		return openAICodexDeviceMinimumInterval
	}
	if interval > time.Minute {
		return time.Minute
	}
	return interval
}
