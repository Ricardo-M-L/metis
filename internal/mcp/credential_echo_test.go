package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStdioTransportRedactsConfiguredArgAndShortEnvCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to echo configured argv and environment values")
	}
	secrets := []string{"a1", "t2", "s3", "p4", "z5", "c6", "l7", "e8", "k9"}
	transport, err := NewStdioTransportWithEnv(
		context.Background(),
		"/bin/sh",
		[]string{"MCP_TOKEN=e8", "X_KEY_ID=k9", "MODE=dev"},
		"-c",
		`printf 'env=%s key=%s ordinary=%s\n' "$MCP_TOKEN" "$X_KEY_ID" "$MODE" >&2; for arg do printf 'arg=%s\n' "$arg" >&2; done; cat >/dev/null`,
		"mcp-credential-echo",
		"--api-key=a1",
		"--token", "t2",
		"--secret=s3",
		"--password", "p4",
		"--authorization=Bearer z5",
		"--credential", "c6",
		"--license=l7",
		"--mode=dev",
	)
	if err != nil {
		t.Fatalf("start credential echo child: %v", err)
	}
	waitForStderrContains(t, transport, "ordinary=dev")
	if err := transport.Close(); err != nil {
		t.Fatalf("close credential echo child: %v", err)
	}

	got := transport.Stderr()
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Fatalf("stdio echo leaked configured secret %q: %q", secret, got)
		}
	}
	for _, ordinary := range []string{"ordinary=dev", "--mode=dev"} {
		if !strings.Contains(got, ordinary) {
			t.Fatalf("ordinary short value was redacted (%q missing): %q", ordinary, got)
		}
	}
}

func TestHTTPClientRedactsShortCredentialHeadersButKeepsOrdinaryShortHeader(t *testing.T) {
	headers := map[string]string{
		"X-API-Key":       "a1",
		"X-Session-Token": "t2",
		"X-Client-Secret": "s3",
		"X-Password":      "p4",
		"Authorization":   "Bearer z5",
		"X-Credential":    "c6",
		"X-License":       "l7",
		"X-Key-Id":        "k9",
		"X-Mode":          "dev",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request JSONRPCRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any = map[string]any{}
		switch request.Method {
		case "tools/list":
			result = ListToolsResult{Tools: []Tool{{Name: "echo_headers"}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]string{{
				"type": "text",
				"text": fmt.Sprintf("api=%s token=%s secret=%s password=%s auth=%s credential=%s license=%s key=%s ordinary=%s",
					r.Header.Get("X-API-Key"), r.Header.Get("X-Session-Token"),
					r.Header.Get("X-Client-Secret"), r.Header.Get("X-Password"),
					r.Header.Get("Authorization"), r.Header.Get("X-Credential"),
					r.Header.Get("X-License"), r.Header.Get("X-Key-Id"), r.Header.Get("X-Mode")),
			}}}
		}
		raw, _ := json.Marshal(result)
		response, _ := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: request.ID, Result: raw})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewHTTPClient(ctx, server.URL, headers)
	if err != nil {
		t.Fatalf("connect HTTP MCP echo server: %v", err)
	}
	defer client.Close()
	raw, err := client.CallTool(ctx, "echo_headers", nil)
	if err != nil {
		t.Fatalf("call HTTP MCP echo tool: %v", err)
	}
	got := client.RedactText(string(raw))
	for _, secret := range []string{"a1", "t2", "s3", "p4", "z5", "c6", "l7", "k9"} {
		if strings.Contains(got, secret) {
			t.Fatalf("HTTP echo leaked configured secret %q: %q", secret, got)
		}
	}
	if !strings.Contains(got, "ordinary=dev") {
		t.Fatalf("ordinary short header value was redacted: %q", got)
	}
}
