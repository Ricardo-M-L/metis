package agent

import (
	"errors"
	"testing"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ErrUnknown},
		{"context overflow anthropic", errors.New("prompt is too long: 250000 tokens > 200000"), ErrContextOverflow},
		{"context overflow minimax", errors.New("server returned error: code 2013"), ErrContextOverflow},
		{"context overflow openai", errors.New("context_length_exceeded: please reduce the length"), ErrContextOverflow},
		{"rate limit 429", errors.New("HTTP 429: Too Many Requests"), ErrRateLimit},
		{"rate limit phrasing", errors.New("rate limit reached, please wait"), ErrRateLimit},
		{"billing 402", errors.New("HTTP 402 payment required"), ErrBilling},
		{"billing quota", errors.New("monthly quota exceeded for your plan"), ErrBilling},
		{"auth 401", errors.New("HTTP 401 unauthorized"), ErrAuth},
		{"auth invalid key", errors.New("invalid_api_key: please check"), ErrAuth},
		{"server 500", errors.New("HTTP 500 internal server error"), ErrServerError},
		{"server overloaded", errors.New("model is overloaded, please retry"), ErrServerError},
		{"network refused", errors.New("dial tcp 1.2.3.4:443: connection refused"), ErrNetwork},
		{"network DNS", errors.New("no such host: api.example.com"), ErrNetwork},
		{"invalid request", errors.New("HTTP 400 bad request: invalid tool schema"), ErrInvalidRequest},
		{"cancelled", errors.New("context canceled"), ErrCancelled},
		{"unknown", errors.New("some weird thing happened"), ErrUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRecovery_StrategyForEachClass(t *testing.T) {
	cases := []struct {
		c    ErrorClass
		want RecoveryStrategy
	}{
		{ErrContextOverflow, RecoveryCompactRetry},
		{ErrRateLimit, RecoveryRetry},
		{ErrServerError, RecoveryRetry},
		{ErrNetwork, RecoveryRetry},
		{ErrBilling, RecoveryFailUser},
		{ErrAuth, RecoveryFailUser},
		{ErrInvalidRequest, RecoveryFailUser},
		{ErrCancelled, RecoveryNone},
		{ErrUnknown, RecoveryNone},
	}
	for _, tc := range cases {
		if got := tc.c.Recovery(); got != tc.want {
			t.Errorf("%s.Recovery() = %v, want %v", tc.c.String(), got, tc.want)
		}
	}
}

func TestUserFacingMessage_FailUserClassesOnly(t *testing.T) {
	if msg := UserFacingMessage(ErrBilling, errors.New("over quota")); msg == "" {
		t.Error("billing should produce a user-facing message")
	}
	if msg := UserFacingMessage(ErrAuth, errors.New("expired")); msg == "" {
		t.Error("auth should produce a user-facing message")
	}
	if msg := UserFacingMessage(ErrRateLimit, errors.New("429")); msg != "" {
		t.Errorf("rate-limit should NOT produce a user-facing message (auto-recovers); got %q", msg)
	}
}
