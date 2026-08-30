package mcp_tools

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	clientmcp "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const redactionTestImageData = "BASE64_IMAGE_DATA_MUST_REMAIN_UNCHANGED"

func TestMCPToolExecute_RedactsStdioCredentialFromSuccessfulText(t *testing.T) {
	const payload = "opaque-stdio-success-token"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv, err := NewServerWithEnv(
		ctx,
		"stdio-redaction",
		os.Args[0],
		[]string{
			"GO_WANT_MCP_REDACTION_HELPER=1",
			"MCP_AUTHORIZATION=Bearer " + payload,
		},
		"-test.run=^TestMCPStdioRedactionHelper$",
	)
	if err != nil {
		t.Fatalf("start stdio MCP helper: %v", err)
	}
	defer srv.Close()

	assertRedactedMCPToolResult(t, executeOnlyMCPTool(t, ctx, srv), payload)
}

func TestMCPStdioRedactionHelper(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_REDACTION_HELPER") != "1" {
		return
	}
	authFields := strings.Fields(os.Getenv("MCP_AUTHORIZATION"))
	if len(authFields) != 2 {
		t.Fatalf("helper received malformed MCP_AUTHORIZATION")
	}
	payload := authFields[1]

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req clientmcp.JSONRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		if err := encoder.Encode(redactionHelperResponse(req, payload)); err != nil {
			return
		}
	}
}

func TestStdioMCPServerOutlivesLaunchOperationContext(t *testing.T) {
	const payload = "opaque-stdio-lifecycle-token"
	launchCtx, cancelLaunch := context.WithCancel(context.Background())
	srv, err := NewServerWithEnv(
		launchCtx,
		"stdio-lifecycle",
		os.Args[0],
		[]string{
			"GO_WANT_MCP_REDACTION_HELPER=1",
			"MCP_AUTHORIZATION=Bearer " + payload,
		},
		"-test.run=^TestMCPStdioRedactionHelper$",
	)
	if err != nil {
		cancelLaunch()
		t.Fatalf("start stdio MCP helper: %v", err)
	}
	defer srv.Close()
	// /mcp start and /cu enable use a bounded launch context. Completing the
	// command must not terminate the successfully adopted long-lived server.
	cancelLaunch()

	ctx, cancelCall := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCall()
	result := executeOnlyMCPTool(t, ctx, srv)
	if result == nil || result.IsError {
		t.Fatalf("tool failed after launch context completed: %#v", result)
	}
}

func TestMCPToolExecute_RedactsHTTPHeaderCredentialFromSuccessfulText(t *testing.T) {
	const payload = "opaque-http-success-token"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read HTTP MCP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req clientmcp.JSONRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode HTTP MCP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(redactionHelperResponse(req, payload))
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv, err := NewHTTPServer(ctx, "http-redaction", httpServer.URL, map[string]string{
		"Authorization": "Bearer " + payload,
	})
	if err != nil {
		t.Fatalf("start HTTP MCP helper: %v", err)
	}
	defer srv.Close()

	assertRedactedMCPToolResult(t, executeOnlyMCPTool(t, ctx, srv), payload)
}

func redactionHelperResponse(req clientmcp.JSONRPCRequest, payload string) clientmcp.JSONRPCResponse {
	var result any
	switch req.Method {
	case "tools/list":
		result = clientmcp.ListToolsResult{Tools: []clientmcp.Tool{{
			Name:        "echo_secret",
			Description: "echo a configured credential",
			InputSchema: map[string]any{"type": "object"},
		}}}
	case "tools/call":
		result = map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "successful result echoed " + payload},
				{"type": "image", "data": redactionTestImageData, "mimeType": "image/png"},
			},
			"isError": false,
		}
	default:
		result = map[string]any{}
	}
	raw, _ := json.Marshal(result)
	return clientmcp.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: raw}
}

func executeOnlyMCPTool(t *testing.T, ctx context.Context, srv *Server) *tools.Result {
	t.Helper()
	registered := srv.Tools()
	if len(registered) != 1 {
		t.Fatalf("registered MCP tools = %d, want 1", len(registered))
	}
	result, err := registered[0].Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("execute MCP tool: %v", err)
	}
	return result
}

func assertRedactedMCPToolResult(t *testing.T, result *tools.Result, payload string) {
	t.Helper()
	if result == nil {
		t.Fatal("MCP tool returned nil result")
	}
	if strings.Contains(result.Output, payload) {
		t.Fatalf("successful MCP output leaked configured credential: %q", result.Output)
	}
	if !strings.Contains(result.Output, "[REDACTED]") {
		t.Fatalf("successful MCP output did not mark credential redaction: %q", result.Output)
	}
	if len(result.Images) != 1 {
		t.Fatalf("MCP images = %d, want 1", len(result.Images))
	}
	if result.Images[0].Data != redactionTestImageData {
		t.Fatalf("MCP image bytes were altered: %q", result.Images[0].Data)
	}
}

func TestParseMCPResponse_DropsUnsafeImageMIMEWithoutEchoingIt(t *testing.T) {
	const data = "IMAGE_BYTES_MUST_NOT_BE_REWRITTEN"
	unsafeMIME := "image/png\r\nX-Secret: opaque-value"
	raw, err := json.Marshal(map[string]any{
		"content": []map[string]any{{
			"type": "image", "data": data, "mimeType": unsafeMIME,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseMCPResponse(raw)
	if !ok || got == nil {
		t.Fatal("valid MCP envelope was not parsed")
	}
	if len(got.Images) != 0 {
		t.Fatalf("unsafe image MIME reached an attachment: %#v", got.Images)
	}
	if strings.Contains(got.Output, unsafeMIME) || strings.Contains(got.Output, data) {
		t.Fatalf("unsafe MIME or image bytes leaked into text output: %q", got.Output)
	}
	if !strings.Contains(got.Output, "unsupported image MIME") {
		t.Fatalf("missing safe omission marker: %q", got.Output)
	}
}

func TestParseMCPResponse_NormalizesAllowedImageMIMEAndPreservesData(t *testing.T) {
	const data = "IMAGE_BYTES_MUST_REMAIN_IDENTICAL"
	raw := []byte(`{"content":[{"type":"image","data":"` + data + `","mimeType":"IMAGE/PNG"}]}`)
	got, ok := parseMCPResponse(raw)
	if !ok || got == nil || len(got.Images) != 1 {
		t.Fatalf("allowed image was not retained: ok=%v result=%#v", ok, got)
	}
	if got.Images[0].MediaType != "image/png" || got.Images[0].Data != data {
		t.Fatalf("image changed: %#v", got.Images[0])
	}
}
