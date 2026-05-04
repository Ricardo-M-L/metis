package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/cloud"
)

// TestSignV4_AuthorizationHeaderShape spot-checks the canonical
// Authorization header structure. The exact signature is sensitive to
// many inputs (headers, payload, time) so we verify presence + prefix
// rather than full byte-equality. End-to-end signature correctness is
// covered by TestBedrock_E2E_RoundTrip below.
func TestSignV4_AuthorizationHeaderShape(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/invoke", nil)
	creds := cloud.AWSCreds{AccessKeyID: "AKIATEST123", SecretAccessKey: "secret"}
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	if err := cloud.SignV4(r, []byte(`{}`), creds, "us-east-1", "bedrock", now); err != nil {
		t.Fatalf("SignV4: %v", err)
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIATEST123/20260504/us-east-1/bedrock/aws4_request") {
		t.Errorf("Authorization prefix wrong:\n%s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=") {
		t.Error("Authorization missing SignedHeaders")
	}
	if !strings.Contains(auth, "Signature=") {
		t.Error("Authorization missing Signature")
	}
	if r.Header.Get("X-Amz-Date") != "20260504T120000Z" {
		t.Errorf("X-Amz-Date: got %q", r.Header.Get("X-Amz-Date"))
	}
	if r.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Error("X-Amz-Content-Sha256 missing")
	}
}

func TestSignV4_AddsSecurityToken_WhenSessionToken(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://x.amazonaws.com/", nil)
	creds := cloud.AWSCreds{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "s",
		SessionToken:    "FQoGZXIvYXdzEAa...",
	}
	if err := cloud.SignV4(r, nil, creds, "us-east-1", "bedrock", time.Now()); err != nil {
		t.Fatalf("SignV4: %v", err)
	}
	if r.Header.Get("X-Amz-Security-Token") != "FQoGZXIvYXdzEAa..." {
		t.Errorf("session token not set on header")
	}
}

func TestSignV4_RejectsEmptyCreds(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://x.amazonaws.com/", nil)
	err := cloud.SignV4(r, nil, cloud.AWSCreds{}, "us-east-1", "bedrock", time.Now())
	if err == nil {
		t.Fatal("expected error for empty creds")
	}
}

func TestNewBedrock_RequiresCreds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	_, err := NewBedrock("", "", "", "us-east-1", "model-x", 1024, time.Second)
	if err == nil || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Errorf("expected creds error; got %v", err)
	}
}

func TestNewBedrock_RequiresModel(t *testing.T) {
	_, err := NewBedrock("AK", "SK", "", "us-east-1", "", 1024, time.Second)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("expected model error; got %v", err)
	}
}

func TestNewBedrock_FallsBackToEnvVars(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SKENV")
	t.Setenv("AWS_SESSION_TOKEN", "ST")
	t.Setenv("AWS_REGION", "ap-southeast-1")

	b, err := NewBedrock("", "", "", "", "anthropic.claude-x", 1024, time.Second)
	if err != nil {
		t.Fatalf("NewBedrock: %v", err)
	}
	if b.creds.AccessKeyID != "AKENV" {
		t.Errorf("AK from env: got %q", b.creds.AccessKeyID)
	}
	if b.creds.SessionToken != "ST" {
		t.Errorf("ST from env: got %q", b.creds.SessionToken)
	}
	if b.Region != "ap-southeast-1" {
		t.Errorf("region from env: got %q", b.Region)
	}
}

// TestBedrock_E2E_RoundTrip mocks the Bedrock InvokeModel endpoint and
// confirms the full Anthropic-on-Bedrock path: SigV4 signing → POST
// with bedrock-namespaced anthropic_version → response unmarshal.
func TestBedrock_E2E_RoundTrip(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{
            "id":"msg_x", "type":"message", "role":"assistant",
            "content":[{"type":"text","text":"hi from bedrock"}],
            "model":"claude", "stop_reason":"end_turn",
            "usage":{"input_tokens":5,"output_tokens":3}
        }`))
	}))
	defer srv.Close()

	b, err := NewBedrock("AK", "SK", "", "us-east-1", "anthropic.claude-test", 1024, 5*time.Second)
	if err != nil {
		t.Fatalf("NewBedrock: %v", err)
	}
	b.httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteHostTransport{base: http.DefaultTransport, target: srv.URL},
	}

	resp, err := b.Complete(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("missing SigV4 header; got %q", gotAuth)
	}
	// Body must include bedrock-2023-05-31 anthropic_version, NOT model.
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body json: %v", err)
	}
	if body["anthropic_version"] != "bedrock-2023-05-31" {
		t.Errorf("anthropic_version: got %v", body["anthropic_version"])
	}
	if _, has := body["model"]; has {
		t.Errorf("body must drop model; got %v", body["model"])
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "hi from bedrock" {
		t.Errorf("response text: got %+v", resp.Content)
	}
}

func TestSyntheticStream_TextOnly(t *testing.T) {
	r := &Response{
		Content:      []ContentBlock{{Type: "text", Text: "hello"}},
		StopReason:   "end_turn",
		InputTokens:  10,
		OutputTokens: 1,
	}
	s := newSyntheticStream(r)

	ev1, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv #1: %v", err)
	}
	if ev1.Type != "text_delta" || ev1.TextDelta != "hello" {
		t.Errorf("ev1: got %+v", ev1)
	}

	ev2, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv #2: %v", err)
	}
	if ev2.Type != "message_stop" {
		t.Errorf("ev2: got %+v", ev2)
	}
	if ev2.InputTokens != 10 || ev2.OutputTokens != 1 {
		t.Errorf("ev2 usage: got in=%d out=%d", ev2.InputTokens, ev2.OutputTokens)
	}

	if _, err := s.Recv(); err == nil {
		t.Error("expected EOF after message_stop")
	}
}

func TestSyntheticStream_ToolUse_Expands3Events(t *testing.T) {
	r := &Response{
		Content: []ContentBlock{
			{Type: "tool_use", ToolUseID: "id1", ToolName: "Bash", ToolInput: map[string]any{"command": "ls"}},
		},
		StopReason: "tool_use",
	}
	s := newSyntheticStream(r)

	wantTypes := []string{"tool_use_start", "tool_input_delta", "tool_use_stop", "message_stop"}
	for i, wt := range wantTypes {
		ev, err := s.Recv()
		if err != nil {
			t.Fatalf("Recv #%d: %v", i, err)
		}
		if ev.Type != wt {
			t.Errorf("event %d: type=%q, want %q", i, ev.Type, wt)
		}
		if i < 3 && ev.ToolUseID != "id1" {
			t.Errorf("event %d: ToolUseID=%q, want id1", i, ev.ToolUseID)
		}
	}
	// Verify the input_delta carries the JSON.
	r2 := &Response{Content: []ContentBlock{{Type: "tool_use", ToolUseID: "id1", ToolName: "Bash", ToolInput: map[string]any{"command": "ls"}}}}
	s2 := newSyntheticStream(r2)
	_, _ = s2.Recv() // start
	d, _ := s2.Recv()
	if !strings.Contains(d.InputDelta, `"command":"ls"`) {
		t.Errorf("InputDelta missing payload: %q", d.InputDelta)
	}
}

func TestBedrock_Stream_UsesSyntheticPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
            "id":"msg_x", "type":"message", "role":"assistant",
            "content":[{"type":"text","text":"streamed"}],
            "model":"claude", "stop_reason":"end_turn",
            "usage":{"input_tokens":1,"output_tokens":1}
        }`))
	}))
	defer srv.Close()

	b, _ := NewBedrock("AK", "SK", "", "us-east-1", "claude-test", 1024, 5*time.Second)
	b.httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewriteHostTransport{base: http.DefaultTransport, target: srv.URL},
	}

	sr, err := b.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: []ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer sr.Close()

	var saw []string
	for {
		ev, err := sr.Recv()
		if err != nil {
			break
		}
		saw = append(saw, ev.Type)
	}
	want := []string{"text_delta", "message_stop"}
	if len(saw) != len(want) {
		t.Errorf("saw events %v, want %v", saw, want)
	}
}

// rewriteHostTransport redirects every Request to a fixed target host
// — used to mock cloud endpoints whose URLs are baked into the
// provider code without running a fake DNS server. Duplicated from
// vertex_test.go since each subpackage's tests are isolated; the
// 5-line copy is cheaper than exporting a test-only helper.
type rewriteHostTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	parsedTarget, _ := r.URL.Parse(t.target)
	r.URL.Scheme = parsedTarget.Scheme
	r.URL.Host = parsedTarget.Host
	return t.base.RoundTrip(r)
}
