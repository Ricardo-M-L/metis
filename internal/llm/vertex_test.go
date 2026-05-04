package llm

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/cloud"
)

// genTestKey is duplicated from internal/llm/cloud/gcp_test.go for
// the transient state where vertex still lives in package llm but
// uses cloud's ServiceAccountKey. Phase 2 moves vertex to its own
// subpackage and this duplicate goes away.
func genTestKey(t *testing.T) (*rsa.PrivateKey, *cloud.ServiceAccountKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return priv, &cloud.ServiceAccountKey{
		Type:        "service_account",
		ClientEmail: "test-sa@example.iam.gserviceaccount.com",
		PrivateKey:  string(pemBytes),
		TokenURI:    "https://oauth2.googleapis.com/token",
	}
}

// writeTestSAFile dumps a generated service-account JSON to a temp
// file, returns its path. Lets us exercise the file-loading code path
// in NewVertex.
func writeTestSAFile(t *testing.T) string {
	t.Helper()
	_, key := genTestKey(t)
	data, _ := json.Marshal(key)
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write SA file: %v", err)
	}
	return path
}

func TestNewVertex_RequiresProject(t *testing.T) {
	saPath := writeTestSAFile(t)
	_, err := NewVertex(saPath, "", "us-central1", "claude-sonnet-4-5", 1024, time.Second)
	if err == nil || !strings.Contains(err.Error(), "project") {
		t.Errorf("expected project-required error; got %v", err)
	}
}

func TestNewVertex_RequiresModel(t *testing.T) {
	saPath := writeTestSAFile(t)
	_, err := NewVertex(saPath, "my-proj", "us-central1", "", 1024, time.Second)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("expected model-required error; got %v", err)
	}
}

func TestNewVertex_RegionDefault(t *testing.T) {
	saPath := writeTestSAFile(t)
	v, err := NewVertex(saPath, "p", "", "claude-sonnet-4-5", 1024, time.Second)
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	if v.Region != "us-central1" {
		t.Errorf("default region: got %q, want us-central1", v.Region)
	}
}

func TestVertex_Endpoint_StreamingVariant(t *testing.T) {
	saPath := writeTestSAFile(t)
	v, err := NewVertex(saPath, "my-project", "europe-west4", "claude-sonnet-4-5@20250514", 1024, time.Second)
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	stream := v.endpoint(true)
	sync := v.endpoint(false)
	if !strings.HasSuffix(stream, ":streamRawPredict") {
		t.Errorf("stream endpoint: got %q", stream)
	}
	if !strings.HasSuffix(sync, ":rawPredict") {
		t.Errorf("sync endpoint: got %q", sync)
	}
	if !strings.Contains(stream, "europe-west4-aiplatform.googleapis.com") {
		t.Errorf("region not in URL: %s", stream)
	}
	if !strings.Contains(stream, "/projects/my-project/locations/europe-west4/") {
		t.Errorf("project/location segment missing: %s", stream)
	}
	if !strings.Contains(stream, "/anthropic/models/claude-sonnet-4-5@20250514") {
		t.Errorf("model segment missing: %s", stream)
	}
}

// TestVertexBody_StripsModel_AddsAnthropicVersion: the body that goes
// to Vertex must NOT carry the Anthropic-style "model" field (Vertex
// routes by URL) AND must include "anthropic_version":"vertex-...".
func TestVertexBody_StripsModel_AddsAnthropicVersion(t *testing.T) {
	body, err := vertexBody(Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	}, 1024)
	if err != nil {
		t.Fatalf("vertexBody: %v", err)
	}
	if _, has := body["model"]; has {
		t.Errorf("vertex body must drop 'model'; got %v", body["model"])
	}
	if v := body["anthropic_version"]; v != "vertex-2023-10-16" {
		t.Errorf("anthropic_version: got %v, want vertex-2023-10-16", v)
	}
	// max_tokens must still be there — Vertex requires it.
	if v := body["max_tokens"]; v != float64(1024) {
		t.Errorf("max_tokens: got %v, want 1024", v)
	}
}

// TestVertex_E2E_MockedAuth: full request flow (token fetch + predict
// call). Two servers — one for OAuth2 token, one for Vertex predict —
// chained via the Vertex client.
func TestVertex_E2E_MockedAuth(t *testing.T) {
	// Token server
	tokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokSrv.Close()

	// Vertex predict server
	var gotAuth, gotURL string
	vertexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotURL = r.URL.Path
		_, _ = w.Write([]byte(`{
            "id":"msg_x", "type":"message", "role":"assistant",
            "content":[{"type":"text","text":"hello from vertex"}],
            "model":"claude", "stop_reason":"end_turn",
            "usage":{"input_tokens":10,"output_tokens":3}
        }`))
	}))
	defer vertexSrv.Close()

	saPath := writeTestSAFile(t)
	v, err := NewVertex(saPath, "p", "us-central1", "claude-sonnet-4-5", 1024, 5*time.Second)
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	// Redirect the token source to our mock.
	v.tokenSource.Key.TokenURI = tokSrv.URL
	// Redirect the predict endpoint by overriding the http client's
	// transport: every request goes to vertexSrv regardless of Host.
	v.httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteHostTransport{base: http.DefaultTransport, target: vertexSrv.URL},
	}

	resp, err := v.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer ya29.test" {
		t.Errorf("Authorization header: got %q", gotAuth)
	}
	if !strings.HasSuffix(gotURL, ":rawPredict") {
		t.Errorf("URL path doesn't end with :rawPredict; got %q", gotURL)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "hello from vertex" {
		t.Errorf("response text: got %+v", resp.Content)
	}
}

// rewriteHostTransport redirects every Request to a fixed target
// host — the cheap way to mock cloud endpoints whose URLs are baked
// into the provider code without running a fake DNS server.
type rewriteHostTransport struct {
	base   http.RoundTripper
	target string // full base, e.g. "http://127.0.0.1:5432"
}

func (t *rewriteHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	parsedTarget, _ := r.URL.Parse(t.target)
	r.URL.Scheme = parsedTarget.Scheme
	r.URL.Host = parsedTarget.Host
	return t.base.RoundTrip(r)
}

func TestVertex_Name(t *testing.T) {
	saPath := writeTestSAFile(t)
	v, err := NewVertex(saPath, "p", "us-east1", "m", 1024, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if v.Name() != "vertex" {
		t.Errorf("Name: got %q", v.Name())
	}
}
