package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Exercise the real launcher rather than a stub: the MCP name boundary,
// initialize, tools/list, and tools/call must all accept a valid plugin.
func TestLoadPluginsRealHTTPMCPHandshakeAndToolCall(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	var initializations, lists, calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		var req mcpsdk.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			initializations.Add(1)
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "plugin-fixture", "version": "1"}}
		case "tools/list":
			lists.Add(1)
			result = mcpsdk.ListToolsResult{Tools: []mcpsdk.Tool{{Name: "probe", Description: "local fixture", InputSchema: map[string]any{"type": "object"}}}}
		case "tools/call":
			calls.Add(1)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "plugin-fixture-ok"}}}
		}
		body, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcpsdk.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: body})
	}))
	defer server.Close()
	t.Setenv("METIS_PLUGIN_FIXTURE_ENDPOINT", server.URL)
	writePluginManifest(t, "real-mcp", "name = \"real-mcp\"\n[mcp_server]\nurl = \"${METIS_PLUGIN_FIXTURE_ENDPOINT}\"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry := tools.NewRegistry()
	plugins, errs := LoadPlugins(ctx, registry)
	defer plugins.Close()
	if len(errs) > 0 || len(plugins.All()) != 1 {
		t.Fatalf("real plugin load failed: %v, plugins=%d", errs, len(plugins.All()))
	}
	registered := plugins.All()[0].MCP.Tools()
	if len(registered) != 1 {
		t.Fatalf("tools = %d", len(registered))
	}
	result, err := registered[0].Execute(ctx, map[string]any{})
	if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "plugin-fixture-ok") {
		t.Fatalf("tool execution failed: result=%#v err=%v", result, err)
	}
	if initializations.Load() != 1 || lists.Load() < 1 || calls.Load() != 1 {
		t.Fatalf("protocol calls initialize=%d list=%d call=%d", initializations.Load(), lists.Load(), calls.Load())
	}
}

func pluginWireMCPFixture(t *testing.T, names []string) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var called []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		var req mcpsdk.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "wire-fixture", "version": "1"}}
		case "tools/list":
			listed := make([]mcpsdk.Tool, 0, len(names))
			for _, name := range names {
				listed = append(listed, mcpsdk.Tool{Name: name, Description: "fixture " + name, InputSchema: map[string]any{"type": "object"}})
			}
			result = mcpsdk.ListToolsResult{Tools: listed}
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				http.Error(w, "invalid tool call", http.StatusBadRequest)
				return
			}
			mu.Lock()
			called = append(called, params.Name)
			mu.Unlock()
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "wire-fixture-ok"}}}
		}
		body, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcpsdk.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: body})
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), called...)
	}
}

func TestLoadPluginsFunctionNamesFitProviderWire(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	remoteNames := []string{"probe", strings.Repeat("long-remote-name-", 8), "搜索/文件?"}
	fixtureNames := append(append([]string(nil), remoteNames...), "disabled")
	endpoint, calls := pluginWireMCPFixture(t, fixtureNames)
	allow, _ := json.Marshal(fixtureNames)
	writePluginManifest(t, "wire-mcp", "name = \"wire-mcp\"\n[mcp_server]\nurl = \""+endpoint+"\"\nenabled_tools = "+string(allow)+"\ndisabled_tools = [\"disabled\"]\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry := tools.NewRegistry()
	plugins, errs := LoadPlugins(ctx, registry)
	defer plugins.Close()
	if len(errs) != 0 || len(plugins.All()) != 1 {
		t.Fatalf("load wire fixture: %v", errs)
	}
	if len(registry.All()) != len(remoteNames) {
		t.Fatalf("remote-name filters lost: registered=%d", len(registry.All()))
	}
	var wireMu sync.Mutex
	var wireNames []string
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		wireMu.Lock()
		for _, tool := range request.Tools {
			wireNames = append(wireNames, tool.Function.Name)
		}
		wireMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer providerServer.Close()
	provider := openai.New("local-fixture-key", providerServer.URL, "local-fixture-model", 128, 5*time.Second, 0)
	loop := agent.NewLoop(provider, registry, nil, nil, "fixture", 1)
	specs := loop.ToolSpecsSnapshot()
	if _, err := provider.Complete(ctx, llm.Request{Tools: specs, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "fixture"}}}}}); err != nil {
		t.Fatalf("local provider wire: %v", err)
	}
	wireMu.Lock()
	actualWireNames := append([]string(nil), wireNames...)
	wireMu.Unlock()
	if len(actualWireNames) != len(remoteNames) {
		t.Fatalf("wire tools=%d", len(actualWireNames))
	}
	validFunction := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	for _, name := range actualWireNames {
		if !validFunction.MatchString(name) {
			t.Errorf("provider rejected function name shape: bytes=%d name=%q", len(name), name)
		}
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("wire alias not callable through registry: %q", name)
		}
		result, err := tool.Execute(ctx, map[string]any{})
		if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "wire-fixture-ok") {
			t.Fatalf("registry call failed: result=%#v err=%v", result, err)
		}
	}
	for _, entry := range registry.ModelEntriesForCache() {
		if entry.Exposure != tools.ToolExposureDeferred {
			t.Fatalf("MCP deferred exposure lost: %v", entry.Exposure)
		}
	}
	actualRemote := calls()
	sort.Strings(actualRemote)
	sort.Strings(remoteNames)
	if strings.Join(actualRemote, "\x00") != strings.Join(remoteNames, "\x00") {
		t.Fatalf("aliases changed remote tool routing: got=%q want=%q", actualRemote, remoteNames)
	}
}

func TestLoadPluginsRejectsToolAliasCollisionWithoutPublishing(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	long := strings.Repeat("long-remote-name-", 8)
	hash := sha256.Sum256([]byte(long))
	alias := fmt.Sprintf("tool-%x", hash[:10])
	endpoint, calls := pluginWireMCPFixture(t, []string{long, alias})
	writePluginManifest(t, "collision-mcp", "name = \"collision-mcp\"\n[mcp_server]\nurl = \""+endpoint+"\"\n")
	registry := tools.NewRegistry()
	plugins, errs := LoadPlugins(context.Background(), registry)
	defer plugins.Close()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "collision") || len(plugins.All()) != 0 || len(registry.All()) != 0 || len(calls()) != 0 {
		t.Fatalf("alias collision accepted or leaked tools: errs=%v plugins=%d tools=%d calls=%d", errs, len(plugins.All()), len(registry.All()), len(calls()))
	}
}

func TestPluginMCPServerNameStableAndUnambiguous(t *testing.T) {
	first := pluginMCPServerName("a-b", "c")
	if first != pluginMCPServerName("a-b", "c") {
		t.Fatal("namespace is not stable")
	}
	if first == pluginMCPServerName("a", "b-c") || first == pluginMCPServerName("a-b", "other") {
		t.Fatal("different plugin identities share a namespace")
	}
	long := pluginMCPServerName(strings.Repeat("plugin", 100), strings.Repeat("server", 100))
	if len(long) != 31 || strings.ContainsAny(long, ":/\\ ") {
		t.Fatalf("namespace is not a portable MCP identifier")
	}
}
