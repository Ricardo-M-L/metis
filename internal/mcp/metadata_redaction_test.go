package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResourceHandleMap_IsOpaqueConcurrentBoundedAndClearedOnClose(t *testing.T) {
	client := NewClient(context.Background(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			const uri = "postgres://user:password@example.test/private/table"
			handle, ok := client.resourceHandle(uri)
			if !ok || strings.Contains(handle, "password") || !strings.HasPrefix(handle, resourceHandlePrefix) {
				t.Errorf("non-opaque resource handle: %q ok=%v", handle, ok)
				return
			}
			if got, err := client.resolveResourceHandle(handle); err != nil || got != uri {
				t.Errorf("resolve handle: got=%q err=%v", got, err)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < maxResourceHandles+1; i++ {
		if _, ok := client.resourceHandle(fmt.Sprintf("resource://bounded/%d", i)); !ok {
			t.Fatalf("failed to allocate bounded handle %d", i)
		}
	}
	client.resourceMu.RLock()
	if got := len(client.resourceByHandle); got != maxResourceHandles {
		client.resourceMu.RUnlock()
		t.Fatalf("resource handle map size = %d, want %d", got, maxResourceHandles)
	}
	client.resourceMu.RUnlock()

	handle, _ := client.resourceHandle("resource://close-check")
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if _, err := client.resolveResourceHandle(handle); err == nil {
		t.Fatal("resource handle survived client Close")
	}
	if _, ok := client.resourceHandle("resource://after-close"); ok {
		t.Fatal("closed client accepted a new resource URI")
	}
}

func TestClientRedactsConfiguredCredentialAcrossModelVisibleMCPMetadata(t *testing.T) {
	const token = "opaque-custom-x-auth-credential-0123456789"
	const blob = "BINARY_BLOB_BYTES_MUST_REMAIN_UNCHANGED"
	server := httptest.NewServer(metadataRedactionHandler(t, token, blob))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Deliberately use a non-standard name: exact-value redaction must not
	// depend on the header containing token/key/auth/secret.
	client, err := NewHTTPClient(ctx, server.URL, map[string]string{"X-Runtime-Value": token})
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer client.Close()

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if cap(tools) != len(tools) {
		t.Fatalf("filtered tool slice retains sensitive tail: len=%d cap=%d", len(tools), cap(tools))
	}
	if tools[0].Name != "safe_tool" {
		t.Fatalf("sensitive tool identifier was rewritten instead of dropped: %#v", tools)
	}
	assertDoesNotContainCredential(t, tools[0].Description, token, "tool description")
	schemaJSON, _ := json.Marshal(tools[0].InputSchema)
	assertDoesNotContainCredential(t, string(schemaJSON), token, "nested tool input schema")
	properties, _ := tools[0].InputSchema["properties"].(map[string]any)
	if _, exists := properties["credential_"+token]; exists {
		t.Fatal("credential-bearing schema map key was retained")
	}
	if _, rewritten := properties["credential_[REDACTED]"]; rewritten {
		t.Fatal("credential-bearing schema identifier was rewritten instead of dropped")
	}
	required, _ := tools[0].InputSchema["required"].([]any)
	if len(required) != 1 || required[0] != "value" {
		t.Fatalf("credential-bearing required identifier was not dropped: %#v", required)
	}
	if cap(required) != len(required) {
		t.Fatalf("filtered required identifiers retain sensitive tail: len=%d cap=%d", len(required), cap(required))
	}

	prompts, err := client.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0].Name != "safe_prompt" || len(prompts[0].Arguments) != 1 {
		t.Fatalf("unexpected prompts: %#v", prompts)
	}
	if cap(prompts) != len(prompts) || cap(prompts[0].Arguments) != len(prompts[0].Arguments) {
		t.Fatalf("filtered prompt slices retain sensitive tails: prompts=%d/%d args=%d/%d",
			len(prompts), cap(prompts), len(prompts[0].Arguments), cap(prompts[0].Arguments))
	}
	if prompts[0].Arguments[0].Name != "safe_argument" {
		t.Fatalf("credential-bearing prompt argument identifier was rewritten: %#v", prompts[0].Arguments)
	}
	assertDoesNotContainCredential(t, prompts[0].Description, token, "prompt description")
	assertDoesNotContainCredential(t, prompts[0].Arguments[0].Description, token, "prompt argument description")

	prompt, err := client.GetPrompt(ctx, "safe_prompt", nil)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	assertDoesNotContainCredential(t, prompt.Description, token, "rendered prompt description")
	assertDoesNotContainCredential(t, prompt.Messages[0].Content[0].Text, token, "rendered prompt text")

	resources, err := client.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(resources))
	}
	if !strings.HasPrefix(resources[0].URI, resourceHandlePrefix) || strings.Contains(resources[0].URI, token) {
		t.Fatalf("resource URI was not replaced with an opaque handle: %q", resources[0].URI)
	}
	for label, value := range map[string]string{
		"resource name":        resources[0].Name,
		"resource description": resources[0].Description,
		"resource MIME type":   resources[0].MimeType,
	} {
		assertDoesNotContainCredential(t, value, token, label)
	}

	resource, err := client.ReadResource(ctx, resources[0].URI)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(resource.Contents) != 1 {
		t.Fatalf("resource contents = %d, want 1", len(resource.Contents))
	}
	if !strings.HasPrefix(resource.Contents[0].URI, resourceHandlePrefix) || strings.Contains(resource.Contents[0].URI, token) {
		t.Fatalf("resource content URI was not replaced with an opaque handle: %q", resource.Contents[0].URI)
	}
	for label, value := range map[string]string{
		"resource content MIME type": resource.Contents[0].MimeType,
		"resource text":              resource.Contents[0].Text,
	} {
		assertDoesNotContainCredential(t, value, token, label)
	}
	if resource.Contents[0].Blob != blob {
		t.Fatalf("resource blob bytes changed: got %q want %q", resource.Contents[0].Blob, blob)
	}
	if _, err := client.ReadResource(ctx, "resource://"+token); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("raw resource URI was accepted or echoed in an error: %v", err)
	}
}

func TestConfiguredRedactionValues_CatchArbitraryNamesButKeepObviousNonSecrets(t *testing.T) {
	const token = "opaque-firecrawl-credential-0123456789"
	const workspacePath = "/Users/example/workspaces/project-with-a-long-name"
	const userAgent = "metis-desktop-integration-test"

	envValues := configuredSecretEnvValues([]string{
		"FIRECRAWL_KEY=" + token,
		"WORKSPACE_ROOT=" + workspacePath,
	})
	headerValues := configuredSecretHeaderValues(map[string]string{
		"X-Auth":     token,
		"User-Agent": userAgent,
	})
	got := redactSensitiveText(
		"credential="+token+" workspace="+workspacePath+" agent="+userAgent,
		append(envValues, headerValues...),
	)
	assertDoesNotContainCredential(t, got, token, "arbitrarily named explicit credential")
	if !strings.Contains(got, workspacePath) {
		t.Fatalf("obvious workspace path was unexpectedly redacted: %q", got)
	}
	if !strings.Contains(got, userAgent) {
		t.Fatalf("non-credential User-Agent was unexpectedly redacted: %q", got)
	}
}

func TestClientRedactsSamplingRequestStrings(t *testing.T) {
	const token = "opaque-sampling-credential-0123456789"
	client := NewClient(context.Background(), &StdioTransport{
		redactValues: configuredSecretEnvValues([]string{"CUSTOM_VALUE=" + token}),
	})
	raw := json.RawMessage(`{"messages":[{"role":"user","content":"echo ` + token + `"}]}`)
	redacted, err := client.redactJSONRawStrings(raw)
	if err != nil {
		t.Fatalf("redact sampling request: %v", err)
	}
	assertDoesNotContainCredential(t, string(redacted), token, "sampling request")
	if !json.Valid(redacted) {
		t.Fatalf("redacted sampling request is invalid JSON: %s", redacted)
	}
}

func metadataRedactionHandler(t *testing.T, token, blob string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read MCP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req JSONRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode MCP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result any
		switch req.Method {
		case "tools/list":
			result = ListToolsResult{Tools: []Tool{{
				Name:        "safe_tool",
				Description: "tool description " + token,
				InputSchema: map[string]any{
					"type":     "object",
					"required": []any{"credential_" + token, "value"},
					"properties": map[string]any{
						"credential_" + token: map[string]any{"type": "string"},
						"value": map[string]any{
							"description": "nested schema " + token,
							"enum":        []any{"safe", token},
						},
					},
				},
			}, {
				Name: "unsafe_" + token, Description: "must be dropped",
				InputSchema: map[string]any{"type": "object"},
			}}}
		case "prompts/list":
			result = ListPromptsResult{Prompts: []Prompt{{
				Name:        "safe_prompt",
				Description: "prompt description " + token,
				Arguments: []PromptArgument{{
					Name:        "safe_argument",
					Description: "argument description " + token,
				}, {
					Name: "unsafe_" + token, Description: "must be dropped",
				}},
			}, {
				Name: "unsafe_" + token, Description: "must be dropped",
			}}}
		case "prompts/get":
			result = GetPromptResult{
				Description: "rendered prompt description " + token,
				Messages: []PromptMessage{{
					Role: "user",
					Content: []PromptContentRaw{{
						Type: "text",
						Text: "rendered prompt body " + token,
					}},
				}},
			}
		case "resources/list":
			result = ListResourcesResult{Resources: []Resource{{
				URI:         "resource://" + token,
				Name:        "resource name " + token,
				Description: "resource description " + token,
				MimeType:    "text/" + token,
			}}}
		case "resources/read":
			var params struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.URI != "resource://"+token {
				t.Errorf("resources/read did not resolve opaque handle: params=%s err=%v", req.Params, err)
			}
			result = ReadResourceResult{Contents: []ResourceContent{{
				URI:      "resource://" + token,
				MimeType: "text/" + token,
				Text:     "resource body " + token,
				Blob:     blob,
			}}}
		default:
			result = map[string]any{}
		}
		raw, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: raw})
	})
}

func assertDoesNotContainCredential(t *testing.T, got, credential, label string) {
	t.Helper()
	if strings.Contains(got, credential) {
		t.Fatalf("%s leaked configured credential: %q", label, got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("%s did not mark credential redaction: %q", label, got)
	}
}
