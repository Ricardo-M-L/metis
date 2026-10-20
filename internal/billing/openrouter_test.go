package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func billingTestRequest(t *testing.T, endpoint string) Request {
	t.Helper()
	hash, err := EndpointSHA256(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Provider: "my-openrouter", EndpointSHA256: hash, Model: "test/model", ClientRequestID: strings.Repeat("a", 32), ProviderRequestID: "request-1", ResponseID: "gen-test-1"}
}

const generationTestRecord = `{"data":{"id":"gen-test-1","request_id":"request-1","model":"test/model","finish_reason":"stop","is_byok":false,"native_tokens_prompt":100,"native_tokens_completion":20,"native_tokens_cached":30,"total_cost":0.002}}`

func TestOpenRouterLooksUpActualChargeWithRawEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/generation" || r.URL.Query().Get("id") != "gen-test-1" || r.Header.Get("Authorization") != "Bearer secret-only-to-provider" {
			t.Error("invalid request")
		}
		w.Write([]byte(generationTestRecord))
	}))
	defer server.Close()
	request := billingTestRequest(t, server.URL+"/chat/completions")
	source, err := NewOpenRouter(OpenRouterConfig{APIBaseURL: server.URL, InferenceEndpoint: server.URL + "/chat/completions", LedgerProvider: request.Provider, Token: "secret-only-to-provider"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := source.Lookup(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Receipt.ActualUSD == nil || *observation.Receipt.ActualUSD != .002 || observation.Receipt.Usage != (projectcoord.TokenUsage{Input: 70, Output: 20, CacheRead: 30}) {
		t.Fatalf("wrong bill %+v", observation.Receipt)
	}
	hash := sha256.Sum256(observation.Evidence)
	if observation.Receipt.EvidenceSHA256 != hex.EncodeToString(hash[:]) || string(observation.Evidence) != generationTestRecord {
		t.Fatal("raw evidence was not bound")
	}
	second, err := source.Lookup(context.Background(), request)
	if err != nil || second.Receipt.ReceiptID != observation.Receipt.ReceiptID {
		t.Fatal("unstable receipt identity")
	}
}

func TestOpenRouterDelayedMalformedAndMismatchedBills(t *testing.T) {
	cases := []struct {
		name, raw string
		status    int
		pending   bool
	}{
		{"not indexed", "", 404, true}, {"accepted", "", 202, true}, {"null", "{\"data\":null}", 200, true},
		{"no cost", strings.Replace(generationTestRecord, `"total_cost":0.002`, `"total_cost":null`, 1), 200, true},
		{"no tokens", strings.Replace(generationTestRecord, `"native_tokens_prompt":100`, `"native_tokens_prompt":null`, 1), 200, true},
		{"not final", strings.Replace(generationTestRecord, `"finish_reason":"stop"`, `"finish_reason":""`, 1), 200, true},
		{"negative cost", strings.Replace(generationTestRecord, `0.002`, `-1`, 1), 200, false},
		{"infinite cost", strings.Replace(generationTestRecord, `0.002`, `1e10000`, 1), 200, false},
		{"wrong model", strings.Replace(generationTestRecord, `test/model`, `other/model`, 1), 200, false},
		{"wrong gen", strings.Replace(generationTestRecord, `gen-test-1`, `gen-test-2`, 1), 200, false},
		{"wrong request", strings.Replace(generationTestRecord, `request-1`, `request-2`, 1), 200, false},
		{"over cache", strings.Replace(generationTestRecord, `"native_tokens_cached":30`, `"native_tokens_cached":101`, 1), 200, false},
		{"negative tokens", strings.Replace(generationTestRecord, `"native_tokens_prompt":100`, `"native_tokens_prompt":-1`, 1), 200, false},
		{"duplicate key", strings.Replace(generationTestRecord, `"total_cost":0.002`, `"total_cost":0.002,"TOTAL_COST":0`, 1), 200, false},
		{"trailing JSON", generationTestRecord + `{}`, 200, false},
		{"redacted HTTP", `secret-provider-key unsafe-private-response`, 500, false},
		{"oversize", strings.Repeat("x", EvidenceLimit+1), 200, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); w.Write([]byte(test.raw)) }))
			defer server.Close()
			source, err := NewOpenRouter(OpenRouterConfig{APIBaseURL: server.URL, InferenceEndpoint: server.URL + "/chat/completions", LedgerProvider: "my-openrouter", Token: "secret-provider-key"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.Lookup(context.Background(), billingTestRequest(t, server.URL+"/chat/completions"))
			if err == nil || errors.Is(err, ErrPending) != test.pending {
				t.Fatalf("unexpected bill classification %v", err)
			}
			if strings.Contains(err.Error(), "secret-provider-key") || strings.Contains(err.Error(), "unsafe-private-response") {
				t.Fatal("error leaked provider body")
			}
		})
	}
}

func TestOpenRouterBYOKCannotEstablishProviderInvoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(generationTestRecord, `"is_byok":false`, `"is_byok":true`, 1)))
	}))
	defer server.Close()
	source, _ := NewOpenRouter(OpenRouterConfig{APIBaseURL: server.URL, InferenceEndpoint: server.URL + "/chat/completions", LedgerProvider: "my-openrouter", Token: "secret"})
	observation, err := source.Lookup(context.Background(), billingTestRequest(t, server.URL+"/chat/completions"))
	if err != nil || observation.Receipt.ActualUSD != nil || observation.Receipt.Usage.Input != 70 {
		t.Fatal("BYOK upstream invoice was inferred from router fee")
	}
}

func TestOpenRouterFreeChargeIsKnownAndScopeMismatchNeverMakesRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(strings.Replace(generationTestRecord, `0.002`, `0`, 1)))
	}))
	defer server.Close()
	source, err := NewOpenRouter(OpenRouterConfig{APIBaseURL: server.URL, InferenceEndpoint: server.URL + "/chat/completions", LedgerProvider: "my-openrouter", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	request := billingTestRequest(t, server.URL+"/chat/completions")
	bill, err := source.Lookup(context.Background(), request)
	if err != nil || bill.Receipt.ActualUSD == nil || *bill.Receipt.ActualUSD != 0 {
		t.Fatal("free charge became unknown")
	}
	request.Provider = "another-account"
	if _, err := source.Lookup(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("mismatched account queried provider")
	}
	request.Provider = "my-openrouter"
	request.EndpointSHA256 = strings.Repeat("0", 64)
	if _, err := source.Lookup(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("mismatched endpoint queried provider")
	}
	request = billingTestRequest(t, server.URL+"/chat/completions")
	request.ResponseID = ""
	if _, err := source.Lookup(context.Background(), request); !errors.Is(err, ErrPending) || calls != 1 {
		t.Fatal("guessed generation identity")
	}
}

func TestOpenRouterDoesNotFollowCredentialRedirects(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer server.Close()
	source, _ := NewOpenRouter(OpenRouterConfig{APIBaseURL: server.URL, InferenceEndpoint: server.URL + "/chat/completions", LedgerProvider: "my-openrouter", Token: "secret"})
	if _, err := source.Lookup(context.Background(), billingTestRequest(t, server.URL+"/chat/completions")); err == nil || leaked {
		t.Fatal("followed provider redirect")
	}
}

func TestBillingConfigurationDoesNotCallArbitraryCompatibleGatewayOfficial(t *testing.T) {
	for _, base := range []string{"https://other.example/api/v1", "https://openrouter.ai.evil.example/api/v1", "http://openrouter.ai/api/v1", "http://localhost/api/v1", "https://u:p@openrouter.ai/api/v1"} {
		if _, err := NewOpenRouter(OpenRouterConfig{APIBaseURL: base, InferenceEndpoint: "https://openrouter.ai/api/v1/chat/completions", LedgerProvider: "openrouter", Token: "secret"}); err == nil {
			t.Fatalf("accepted untrusted source %s", base)
		}
	}
}
