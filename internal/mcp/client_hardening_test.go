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
	"sync/atomic"
	"testing"
	"time"
)

func TestConfiguredCredentialValuesSplitSchemePayloadForEverySecretField(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{
			name: "stdio credential-shaped env",
			values: configuredSecretEnvValues([]string{
				"FIRECRAWL_KEY=Bearer raw-firecrawl-scheme-payload",
			}),
		},
		{
			name: "HTTP credential-shaped header",
			values: configuredSecretHeaderValues(map[string]string{
				"X-API-Key": "PrivateScheme raw-http-scheme-payload",
			}),
		},
		{
			name: "arbitrary explicit header classified by value",
			values: configuredSecretHeaderValues(map[string]string{
				"X-Runtime-Value": "GNAP raw-runtime-scheme-payload",
			}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSensitiveText("server echoed raw-firecrawl-scheme-payload raw-http-scheme-payload raw-runtime-scheme-payload", tc.values)
			for _, payload := range []string{
				"raw-firecrawl-scheme-payload",
				"raw-http-scheme-payload",
				"raw-runtime-scheme-payload",
			} {
				if strings.Contains(strings.Join(tc.values, "\n"), payload) && strings.Contains(got, payload) {
					t.Fatalf("scheme payload %q was not independently redacted: %q", payload, got)
				}
			}
		})
	}
}

func TestCredentialURLRedactionValuesIncludeStandaloneSensitiveComponents(t *testing.T) {
	const (
		password  = "short-db-password"
		signature = "opaque-query-signature-value"
		pathToken = "session0123456789opaqueTOKEN"
		fragment  = "private-fragment-value"
	)
	rawURL := "https://alice:" + password + "@hooks.example.test/session/" + pathToken +
		"?X-Amz-Signature=" + signature + "#" + fragment
	values := credentialURLRedactionValues(rawURL, true)
	got := redactSensitiveText(strings.Join([]string{rawURL, password, signature, pathToken, fragment}, "\n"), values)
	for _, secret := range []string{rawURL, password, signature, pathToken, fragment} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential URL component leaked independently (%q): %q", secret, got)
		}
	}
}

func TestCredentialURLRedactionValuesIncludeSignedPathPayloadWithoutDigits(t *testing.T) {
	const payload = "abcdefghijklmnopqrstuvwxyzABCDEF"
	values := credentialURLRedactionValues("https://api.example.test/download/"+payload, true)
	got := redactSensitiveText("upstream rejected path payload "+payload, values)
	if strings.Contains(got, payload) {
		t.Fatalf("signed path payload leaked independently: %q", got)
	}
}

func TestCredentialURLRedactionValuesIncludeRawEncodedFragment(t *testing.T) {
	const rawFragment = "private%20fragment%2Fvalue"
	values := credentialURLRedactionValues("https://api.example.test/mcp#"+rawFragment, true)
	got := redactSensitiveText("upstream echoed "+rawFragment, values)
	if strings.Contains(got, rawFragment) {
		t.Fatalf("raw encoded fragment leaked independently: %q", got)
	}
}

func TestCredentialURLRedactionValuesIncludeUsernameOnlyUserinfo(t *testing.T) {
	const usernameToken = "opaque-api-token-value"
	values := credentialURLRedactionValues("https://"+usernameToken+"@api.example.test/mcp", true)
	got := redactSensitiveText("upstream echoed "+usernameToken, values)
	if strings.Contains(got, usernameToken) {
		t.Fatalf("username-only userinfo credential leaked independently: %q", got)
	}
}

func TestCredentialURLRedactionValuesIncludeValuelessOpaqueQueryToken(t *testing.T) {
	const queryToken = "abcdefghijklmnopqrstuvwxyzABCDEF"
	values := credentialURLRedactionValues("https://api.example.test/mcp?"+queryToken, true)
	got := redactSensitiveText("upstream echoed "+queryToken, values)
	if strings.Contains(got, queryToken) {
		t.Fatalf("valueless opaque query credential leaked independently: %q", got)
	}
}

func TestCredentialURLRedactionValuesIncludeCanonicalPercentEncodingVariants(t *testing.T) {
	const (
		decoded      = "opaque/secret-value-0123456789"
		lowerEscaped = "opaque%2fsecret-value-0123456789"
		upperEscaped = "opaque%2Fsecret-value-0123456789"
	)
	for _, rawURL := range []string{
		"https://api.example.test/mcp?token=" + lowerEscaped,
		"https://api.example.test/token/" + lowerEscaped,
	} {
		values := credentialURLRedactionValues(rawURL, true)
		got := redactSensitiveText(strings.Join([]string{decoded, lowerEscaped, upperEscaped}, " "), values)
		for _, secret := range []string{decoded, lowerEscaped, upperEscaped} {
			if strings.Contains(got, secret) {
				t.Fatalf("canonicalized escape %q leaked for %q: %q", secret, rawURL, got)
			}
		}
	}
}

func TestExactCredentialRedactionRunsBeforeGenericPrefixRules(t *testing.T) {
	credential := "ghp_" + strings.Repeat("a", 36) + "-private-tail"
	got := redactSensitiveText("server echoed "+credential, []string{credential})
	if strings.Contains(got, "private-tail") || strings.Contains(got, credential) {
		t.Fatalf("exact credential suffix leaked after generic prefix redaction: %q", got)
	}
}

func TestCombinedResourceURISecretsAreRedactedLongestFirst(t *testing.T) {
	const longToken = "abcDEF0123456789opaque-tail"
	values := append(
		credentialURLRedactionValues("https://api.example.test/parent?token=abc", true),
		credentialURLRedactionValues("https://api.example.test/content?token="+longToken, true)...,
	)
	got := redactSensitiveText("server echoed "+longToken, values)
	if strings.Contains(got, longToken) || strings.Contains(got, "DEF0123456789opaque-tail") {
		t.Fatalf("overlapping URI credentials were only partially redacted: %q", got)
	}
}

func TestRedactJSONRawStringsFailsClosedAndSupportsLargeJSONNumbers(t *testing.T) {
	const token = "opaque-json-redaction-token"
	c := NewClient(context.Background(), &StdioTransport{redactValues: []string{token}})
	if got, err := c.redactJSONRawStrings(json.RawMessage(`{"text":"` + token + `"`)); err == nil || got != nil {
		t.Fatalf("invalid JSON did not fail closed: got=%s err=%v", got, err)
	}

	got, err := c.redactJSONRawStrings(json.RawMessage(`{"huge":1e100000,"text":"` + token + `"}`))
	if err != nil {
		t.Fatalf("valid JSON with a large number was rejected: %v", err)
	}
	if strings.Contains(string(got), token) || !strings.Contains(string(got), "[REDACTED]") {
		t.Fatalf("large-number JSON bypassed string redaction: %s", got)
	}
}

func TestMCPMessageReadersRejectOversizePayloads(t *testing.T) {
	oversize := strings.Repeat("x", maxMCPMessageBytes+1)
	if _, err := readBoundedJSONRPC(strings.NewReader(oversize)); err == nil {
		t.Fatal("oversize HTTP JSON-RPC body was accepted")
	}

	scanner := newMCPMessageScanner(strings.NewReader(oversize + "\n"))
	if scanner.Scan() || scanner.Err() == nil {
		t.Fatalf("oversize stdio JSON-RPC line was accepted: scan=%v err=%v", scanner.Scan(), scanner.Err())
	}

	c := NewClient(context.Background(), nil)
	if err := c.parseSSE(strings.NewReader("data: " + oversize + "\n\n")); err == nil {
		t.Fatal("oversize SSE frame was accepted")
	}
}

func TestPrepareResourcesCapsCatalogAndReturnsOnlyLiveOpaqueHandles(t *testing.T) {
	c := NewClient(context.Background(), nil)
	resources := make([]Resource, 0, maxResourceHandles+1)
	for i := 0; i < maxResourceHandles+1; i++ {
		resources = append(resources, Resource{URI: fmt.Sprintf("resource://catalog/%d", i)})
	}
	safe := c.prepareResources(resources)
	if len(safe) != maxResourceHandles {
		t.Fatalf("resource list length = %d, want cap %d", len(safe), maxResourceHandles)
	}
	if cap(safe) != len(safe) {
		t.Fatalf("resource result exposes filtered backing-array tail: len=%d cap=%d", len(safe), cap(safe))
	}
	for i, resource := range safe {
		if strings.Contains(resource.URI, "resource://catalog/") {
			t.Fatalf("resource %d exposed raw URI: %q", i, resource.URI)
		}
		if _, err := c.resolveResourceHandle(resource.URI); err != nil {
			t.Fatalf("returned resource %d has an immediately expired handle: %v", i, err)
		}
	}
	c.resourceMu.RLock()
	bytesUsed := c.resourceURIBytes
	c.resourceMu.RUnlock()
	if bytesUsed > maxResourceURITotalBytes {
		t.Fatalf("resource URI catalog uses %d bytes, cap %d", bytesUsed, maxResourceURITotalBytes)
	}

	tooLong := []Resource{{URI: "resource://" + strings.Repeat("x", maxResourceURIBytes+1)}}
	if got := c.prepareResources(tooLong); len(got) != 0 {
		t.Fatalf("oversize resource URI was retained: %#v", got)
	}
}

func TestPrepareResourcesCapsTotalURIBytesWithoutReturningExpiredHandles(t *testing.T) {
	c := NewClient(context.Background(), nil)
	count := maxResourceURITotalBytes/(maxResourceURIBytes-1) + 2
	resources := make([]Resource, 0, count)
	for i := 0; i < count; i++ {
		prefix := fmt.Sprintf("resource://bulk/%04d/", i)
		resources = append(resources, Resource{
			URI: prefix + strings.Repeat("x", maxResourceURIBytes-len(prefix)),
		})
	}
	safe := c.prepareResources(resources)
	if len(safe) >= len(resources) {
		t.Fatalf("URI byte cap did not truncate catalog: got=%d input=%d", len(safe), len(resources))
	}
	c.resourceMu.RLock()
	bytesUsed := c.resourceURIBytes
	c.resourceMu.RUnlock()
	if bytesUsed > maxResourceURITotalBytes {
		t.Fatalf("resource URI catalog uses %d bytes, cap %d", bytesUsed, maxResourceURITotalBytes)
	}
	for i, resource := range safe {
		if _, err := c.resolveResourceHandle(resource.URI); err != nil {
			t.Fatalf("returned resource %d expired at total-byte boundary: %v", i, err)
		}
	}
}

func TestPrepareResourcesRedactsSiblingURISecret(t *testing.T) {
	const token = "sibling-resource-secret-value"
	c := NewClient(context.Background(), nil)
	safe := c.prepareResources([]Resource{
		{URI: "https://api.example.test/private?token=" + token, Name: "first"},
		{URI: "resource://public", Description: "sibling echoed " + token},
	})
	if len(safe) != 2 {
		t.Fatalf("prepareResources returned %d resources", len(safe))
	}
	if strings.Contains(safe[1].Description, token) || !strings.Contains(safe[1].Description, "[REDACTED]") {
		t.Fatalf("sibling URI secret leaked through metadata: %q", safe[1].Description)
	}
}

func TestPrepareResourcesManyCredentialComponentsRemainsBounded(t *testing.T) {
	c := NewClient(context.Background(), nil)
	resources := make([]Resource, maxResourceHandles)
	for i := range resources {
		token := fmt.Sprintf("catalog-token-%04d-opaque-value", i)
		resources[i] = Resource{
			URI:         "https://api.example.test/resource?token=" + token,
			Description: "echoed " + token,
		}
	}
	start := time.Now()
	safe := c.prepareResources(resources)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("catalog-wide credential redaction took %s", elapsed)
	}
	if len(safe) != len(resources) {
		t.Fatalf("prepareResources returned %d resources, want %d", len(safe), len(resources))
	}
	for i := range safe {
		if strings.Contains(safe[i].Description, "catalog-token-") || !strings.Contains(safe[i].Description, "[REDACTED]") {
			t.Fatalf("resource %d was not redacted: %q", i, safe[i].Description)
		}
	}
}

func TestReadResourceReturnsOnlyLiveHandlesWithoutEvictingListedResources(t *testing.T) {
	const parentURI = "resource://catalog/parent"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req JSONRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "tools/list":
			result = ListToolsResult{}
		case "resources/list":
			result = ListResourcesResult{Resources: []Resource{{URI: parentURI}}}
		case "resources/read":
			contents := make([]ResourceContent, maxResourceHandles+1)
			for i := range contents {
				token := fmt.Sprintf("read-token-%04d-opaque-value", i)
				contents[i].URI = fmt.Sprintf("https://api.example.test/read/%d?token=%s", i, token)
				contents[i].Text = "echoed " + token
			}
			result = ReadResourceResult{Contents: contents}
		}
		raw, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: raw})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewHTTPClient(ctx, server.URL)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer client.Close()
	resources, err := client.ListResources(ctx)
	if err != nil || len(resources) != 1 {
		t.Fatalf("ListResources = %#v, %v", resources, err)
	}
	listedHandle := resources[0].URI
	read, err := client.ReadResource(ctx, listedHandle)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	for i, content := range read.Contents {
		if _, err := client.resolveResourceHandle(content.URI); err != nil {
			t.Fatalf("returned content %d has an expired handle %q: %v", i, content.URI, err)
		}
		if strings.Contains(content.Text, "read-token-") || !strings.Contains(content.Text, "[REDACTED]") {
			t.Fatalf("returned content %d was not credential-redacted: %q", i, content.Text)
		}
	}
	if got, err := client.resolveResourceHandle(listedHandle); err != nil || got != parentURI {
		t.Fatalf("resources/read evicted the listed parent: got=%q err=%v", got, err)
	}
}

func TestStdioStderrDropsPartialExactSecretAtTruncationBoundary(t *testing.T) {
	const secret = "opaque-configured-secret-value"
	buffer := newBoundedBuffer(64)
	_, _ = buffer.Write([]byte(strings.Repeat("d", 50) + secret))
	transport := &StdioTransport{stderrBuf: buffer, redactValues: []string{secret}}
	got := transport.Stderr()
	if strings.Contains(got, secret) || strings.Contains(got, secret[:10]) {
		t.Fatalf("truncated stderr exposed a partial exact credential: %q", got)
	}
	if !strings.Contains(got, "bytes elided") {
		t.Fatalf("truncated stderr lost its diagnostic marker: %q", got)
	}
}

func TestSetRootsClonesCallerSliceAndIsRaceSafe(t *testing.T) {
	c := NewClient(context.Background(), nil)
	input := []Root{{URI: "file:///first", Name: "first"}}
	c.SetRoots(input)
	input[0] = Root{URI: "file:///mutated", Name: "mutated"}
	c.mu.RLock()
	got := append([]Root(nil), c.roots...)
	c.mu.RUnlock()
	if len(got) != 1 || got[0].URI != "file:///first" {
		t.Fatalf("SetRoots retained caller-owned backing array: %#v", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.SetRoots([]Root{{URI: fmt.Sprintf("file:///root-%d", i)}})
		}(i)
		go func(i int) {
			defer wg.Done()
			c.handleServerRequest(json.RawMessage(fmt.Sprintf("%d", i+1)), "roots/list", nil)
		}(i)
	}
	wg.Wait()
}

func TestDispatchJSONRPCResponseNeverBlocksOnFullPendingChannel(t *testing.T) {
	c := NewClient(context.Background(), nil)
	ch := make(chan *JSONRPCResponse, 1)
	ch <- &JSONRPCResponse{ID: "already-full"}
	c.pending["1"] = ch
	done := make(chan struct{})
	go func() {
		c.dispatchJSONRPC([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("SSE response dispatcher blocked on a full pending channel")
	}
}

func TestServerRequestDispatchHasHardConcurrencyLimit(t *testing.T) {
	c := NewClient(context.Background(), nil)
	release := make(chan struct{})
	var active, peak atomic.Int32
	c.sampler = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		active.Add(-1)
		return json.RawMessage(`{"role":"assistant","content":{"type":"text","text":"ok"}}`), nil
	}

	for i := 0; i < maxConcurrentServerRequests*4; i++ {
		c.dispatchJSONRPC([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"sampling/createMessage","params":{}}`, i+1)))
	}
	deadline := time.Now().Add(time.Second)
	for peak.Load() < maxConcurrentServerRequests && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := peak.Load(); got != maxConcurrentServerRequests {
		close(release)
		t.Fatalf("peak server request concurrency = %d, want %d", got, maxConcurrentServerRequests)
	}
	close(release)
}

func TestHTTPTransportCancelIsSynchronizedAndIdempotent(t *testing.T) {
	tpt := &HTTPTransport{}
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tpt.setCancelGET(func() { calls.Add(1) })
		}()
		go func() {
			defer wg.Done()
			_ = tpt.Close()
		}()
	}
	wg.Wait()
	if calls.Load() == 0 {
		t.Fatal("HTTP notification cancel was never invoked")
	}
}

type hardeningNopWriteCloser struct{ io.Writer }

func (hardeningNopWriteCloser) Close() error { return nil }
