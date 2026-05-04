package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

// TestAzure_URL_DeploymentRouting checks that Complete posts to the
// Azure-flavored URL (path includes the deployment name; query
// includes api-version). Catches accidental regression to OpenAI's
// flat /chat/completions URL.
func TestAzure_URL_DeploymentRouting(t *testing.T) {
	var gotURL, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("api-key")
		_ = json.NewEncoder(w).Encode(openai.Resp{
			Choices: []openai.Choice{{Message: openai.WireMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer srv.Close()

	a := NewAzure("test-key", srv.URL, "gpt-4o-deployment", "2024-08-01-preview", "gpt-4o", 1024, time.Second)
	a.BaseURL = srv.URL // bypass NewAzure's https:// templating for the test server

	_, err := a.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(gotURL, "/openai/deployments/gpt-4o-deployment/chat/completions") {
		t.Errorf("URL missing deployment routing: %s", gotURL)
	}
	if !strings.Contains(gotURL, "api-version=2024-08-01-preview") {
		t.Errorf("URL missing api-version query: %s", gotURL)
	}
	if gotAuth != "test-key" {
		t.Errorf("api-key header: got %q, want test-key", gotAuth)
	}
}

func TestAzure_AuthMode_AAD_UsesBearer(t *testing.T) {
	var gotAPIKey, gotBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("api-key")
		gotBearer = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(openai.Resp{
			Choices: []openai.Choice{{Message: openai.WireMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer srv.Close()

	a := NewAzure("aad-token-xyz", srv.URL, "depl", "2024-08-01", "model", 1024, time.Second)
	a.BaseURL = srv.URL
	a.AuthMode = "aad"

	_, err := a.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAPIKey != "" {
		t.Errorf("AAD mode shouldn't set api-key header; got %q", gotAPIKey)
	}
	if gotBearer != "Bearer aad-token-xyz" {
		t.Errorf("AAD mode Authorization header: got %q, want Bearer aad-token-xyz", gotBearer)
	}
}

func TestAzure_PreflightChecks(t *testing.T) {
	cases := []struct {
		name   string
		az     *Azure
		expect string
	}{
		{
			name:   "missing API key",
			az:     &Azure{Deployment: "d", BaseURL: "https://x.openai.azure.com", APIVersion: "v"},
			expect: "API key",
		},
		{
			name:   "missing deployment",
			az:     &Azure{APIKey: "k", BaseURL: "https://x.openai.azure.com", APIVersion: "v"},
			expect: "deployment",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.az.httpClient = &http.Client{Timeout: time.Second}
			_, err := tc.az.Complete(context.Background(), Request{})
			if err == nil {
				t.Fatal("expected preflight error")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q should mention %q", err.Error(), tc.expect)
			}
		})
	}
}

func TestAzure_BaseURL_TemplatingFromResource(t *testing.T) {
	// User passes just the resource name (no scheme); NewAzure should
	// expand to https://<resource>.openai.azure.com.
	a := NewAzure("k", "my-resource", "depl", "v", "m", 1024, time.Second)
	if a.BaseURL != "https://my-resource.openai.azure.com" {
		t.Errorf("BaseURL: got %q, want https://my-resource.openai.azure.com", a.BaseURL)
	}
}

func TestAzure_BaseURL_PreservesFullURL(t *testing.T) {
	// User passes a full URL (e.g. private endpoint); leave it alone.
	full := "https://my-private.cognitiveservices.azure.com"
	a := NewAzure("k", full, "depl", "v", "m", 1024, time.Second)
	if a.BaseURL != full {
		t.Errorf("BaseURL: got %q, want %q (full URLs preserved)", a.BaseURL, full)
	}
}

func TestAzure_Name(t *testing.T) {
	a := NewAzure("k", "r", "d", "v", "m", 1024, time.Second)
	if a.Name() != "azure" {
		t.Errorf("Name: got %q, want azure", a.Name())
	}
}
