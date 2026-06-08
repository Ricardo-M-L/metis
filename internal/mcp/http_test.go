package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMCPServer stands up a minimal MCP-over-Streamable-HTTP server.
// POSTs are dispatched on `method`; we hand-roll a pair of canned
// responses (initialize + tools/list + tools/call) so the handshake
// path runs end-to-end. The two tunable behaviors `useSSE` and
// `requireHeader` cover the per-test variations.
type fakeMCPServer struct {
	useSSE        bool
	requireHeader string // when set, POST must include this Authorization header
	gotHeader     atomic.Pointer[string]
	postCount     atomic.Int64
}

func (f *fakeMCPServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Notification channel — empty stream is fine for these
			// tests; the handshake doesn't depend on it.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		f.postCount.Add(1)
		if f.requireHeader != "" {
			h := r.Header.Get("Authorization")
			f.gotHeader.Store(&h)
			if h != f.requireHeader {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		body, _ := io.ReadAll(r.Body)
		var req JSONRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var result interface{}
		switch req.Method {
		case "tools/list":
			result = ListToolsResult{Tools: []Tool{
				{Name: "echo", Description: "echo back the input"},
			}}
		case "tools/call":
			var p struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result = map[string]interface{}{
				"content": fmt.Sprintf("ok:%s", p.Name),
			}
		case "resources/list":
			result = ListResourcesResult{Resources: []Resource{
				{URI: "file:///x.md", Name: "x", Description: "doc x", MimeType: "text/markdown"},
			}}
		case "resources/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			result = ReadResourceResult{Contents: []ResourceContent{
				{URI: p.URI, MimeType: "text/markdown", Text: "hello from " + p.URI},
			}}
		default:
			result = map[string]interface{}{}
		}

		resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
		raw, _ := json.Marshal(result)
		resp.Result = raw
		respBytes, _ := json.Marshal(&resp)

		if f.useSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: %s\n\n", respBytes)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(respBytes)
	})
}

// TestHTTPClient_HandshakeJSON connects to a JSON-only fake server and
// verifies tools/list comes back during the New constructor.
func TestHTTPClient_HandshakeJSON(t *testing.T) {
	fake := &fakeMCPServer{}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewHTTPClient(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer c.Close()

	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
}

// TestHTTPClient_HandshakeSSE confirms the SSE response branch parses
// `data:` frames and routes the response to the right pending entry.
func TestHTTPClient_HandshakeSSE(t *testing.T) {
	fake := &fakeMCPServer{useSSE: true}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewHTTPClient(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient (SSE): %v", err)
	}
	defer c.Close()

	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
}

// TestHTTPClient_AuthHeaders forwards the optHeaders map on every
// request — needed for Bearer-token MCP servers.
func TestHTTPClient_AuthHeaders(t *testing.T) {
	fake := &fakeMCPServer{requireHeader: "Bearer test123"}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewHTTPClient(ctx, srv.URL, map[string]string{"Authorization": "Bearer test123"})
	if err != nil {
		t.Fatalf("NewHTTPClient with auth: %v", err)
	}
	defer c.Close()

	got := fake.gotHeader.Load()
	if got == nil || *got != "Bearer test123" {
		t.Fatalf("Authorization header not forwarded; got %v", got)
	}
}

// TestHTTPClient_CallTool walks the request → JSON-response path past
// the handshake to verify general request-response works.
func TestHTTPClient_CallTool(t *testing.T) {
	fake := &fakeMCPServer{}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewHTTPClient(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer c.Close()

	raw, err := c.CallTool(ctx, "echo", map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(raw), "ok:echo") {
		t.Fatalf("unexpected result: %s", raw)
	}
}

// TestHTTPClient_Resources covers resources/list + resources/read over
// the HTTP transport against the fake server.
func TestHTTPClient_Resources(t *testing.T) {
	fake := &fakeMCPServer{}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewHTTPClient(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer c.Close()

	res, err := c.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res) != 1 || res[0].URI != "file:///x.md" || res[0].MimeType != "text/markdown" {
		t.Fatalf("unexpected resources: %#v", res)
	}

	rr, err := c.ReadResource(ctx, "file:///x.md")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(rr.Contents) != 1 || rr.Contents[0].Text != "hello from file:///x.md" {
		t.Fatalf("unexpected contents: %#v", rr.Contents)
	}
}
