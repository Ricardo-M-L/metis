package modelinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	c, err := New(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestColdEndpointDiscoveryAndPersistentExactBinding(t *testing.T) {
	const secret = "a-secret-that-must-never-be-persisted"
	var rich, plain atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("authorization absent")
		}
		if r.URL.Query().Get("client_version") == "pi" {
			rich.Add(1)
			fmt.Fprint(w, `{"models":[{"slug":"brand-new-model","display_name":"New Model","context_window":272000,"max_context_window":872000,"max_tokens":128000,"secret":"a-secret-that-must-never-be-persisted"},{"slug":"different-model","context_window":1048576,"max_tokens":8192}]}`)
		} else {
			plain.Add(1)
			fmt.Fprint(w, `{"data":[{"id":"brand-new-model"}]}`)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{ProviderID: "private-gateway", Transport: "anthropic_messages", BaseURL: server.URL, AuthKind: AuthBearer}
	if _, ok := c.Get(b, "brand-new-model"); ok {
		t.Fatal("cold lookup invented a model")
	}
	models, err := c.Refresh(context.Background(), b, secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || rich.Load() != 1 || plain.Load() != 0 {
		t.Fatalf("models=%v requests rich=%d plain=%d", models, rich.Load(), plain.Load())
	}
	m, ok := c.Get(b, "brand-new-model")
	if !ok || m.ContextWindow != 272000 || m.MaxContextWindow != 872000 || m.MaxOutputTokens != 128000 || m.Source != SourceEndpointPI {
		t.Fatalf("model = %+v ok=%v", m, ok)
	}
	if version := c.Version(b); version != 1 {
		t.Fatalf("version = %d", version)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("cache files %v, error=%v", files, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "Authorization") || strings.Contains(string(data), "client_version") || strings.Contains(string(data), `"secret"`) {
		t.Fatalf("cache contains response or authentication material: %s", data)
	}
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0700 {
		t.Fatalf("dir mode=%v", st.Mode())
	}
	st, _ = os.Stat(filepath.Join(dir, files[0].Name()))
	if st.Mode().Perm() != 0600 {
		t.Fatalf("file mode=%v", st.Mode())
	}
	restarted, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Startup resolves cached data synchronously without contacting the server.
	server.Close()
	b.BaseURL += "/"
	got, ok := restarted.Get(b, "brand-new-model")
	if !ok || got != m {
		t.Fatalf("cold cache lookup=%+v ok=%v", got, ok)
	}
	other := b
	other.ProviderID = "unrelated-route"
	if _, ok := restarted.Get(other, "brand-new-model"); ok {
		t.Fatal("cross-provider capability reuse")
	}
	other = b
	other.BaseURL += "other"
	if _, ok := restarted.Get(other, "brand-new-model"); ok {
		t.Fatal("cross-endpoint capability reuse")
	}
	if _, ok := restarted.Get(b, "Brand-New-Model"); ok {
		t.Fatal("case-insensitive model match")
	}
}

func TestRichToPlainFallbackIsBounded(t *testing.T) {
	for _, tt := range []struct {
		name, rich   string
		status       int
		wantRequests int
		wantErr      bool
		context      int
	}{
		{"unsupported", "", 404, 2, false, 1000000},
		{"unsupported-query", "", 400, 2, false, 1000000},
		{"ID-only", `{"models":[{"slug":"new"}]}`, 200, 2, false, 1000000},
		{"empty", `{"models":[]}`, 200, 2, false, 1000000},
		{"unauthorized", "", 401, 1, true, 0},
		{"rate-limit", "", 429, 1, true, 0},
		{"server-error", "", 500, 1, true, 0},
		{"invalid-json", `{not JSON`, 200, 1, true, 0},
		{"negative", `{"models":[{"slug":"new","context_window":-1}]}`, 200, 1, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var count atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				if r.URL.Query().Get("client_version") == "pi" {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.rich)
				} else {
					fmt.Fprint(w, `{"data":[{"id":"new","context_length":1000000,"top_provider":{"max_completion_tokens":64000}}]}`)
				}
			}))
			defer s.Close()
			models, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL + "/v1"}, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v", err)
			}
			if int(count.Load()) != tt.wantRequests {
				t.Fatalf("requests=%d", count.Load())
			}
			if !tt.wantErr && (len(models) != 1 || models[0].ContextWindow != tt.context || models[0].MaxOutputTokens != 64000 || models[0].Source != SourceEndpointModels) {
				t.Fatalf("models=%+v", models)
			}
		})
	}
}

func TestEndpointPathsMatchConfiguredTransport(t *testing.T) {
	for _, tt := range []struct{ base, transport, want string }{
		{"http://localhost:8317", "anthropic_messages", "http://localhost:8317/v1/models?client_version=pi"},
		{"http://localhost:8317/proxy", "anthropic", "http://localhost:8317/proxy/v1/models?client_version=pi"},
		{"https://api.anthropic.com", "anthropic_messages", "https://api.anthropic.com/v1/models?client_version=pi"},
		{"https://api.openai.com/v1", "openai_chat", "https://api.openai.com/v1/models?client_version=pi"},
		{"https://openrouter.ai/api/v1", "openai_chat", "https://openrouter.ai/api/v1/models?client_version=pi"},
		{"http://localhost:8317/proxy", "openai_chat", "http://localhost:8317/proxy/models?client_version=pi"},
	} {
		t.Run(tt.base+tt.transport, func(t *testing.T) {
			b, _, err := normalizeBinding(Binding{ProviderID: "route", Transport: tt.transport, BaseURL: tt.base})
			if err != nil {
				t.Fatal(err)
			}
			if got := modelsURL(b, true); got != tt.want {
				t.Fatalf("got=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestModelSchemasAndUnknownLimits(t *testing.T) {
	for _, tt := range []struct {
		name, body           string
		context, max, output int
	}{
		{"pi", `{"models":[{"slug":"new","context_window":272000,"max_context_window":872000,"max_tokens":128000}]}`, 272000, 872000, 128000},
		{"data", `{"data":[{"id":"new","context_length":1000000,"max_output_tokens":32000}]}`, 1000000, 0, 32000},
		{"map", `{"models":{"new":{"limits":{"context_window":"1000000","max_output_tokens":"16000"},"irrelevant":{"api_key":"ignored"}}}}`, 1000000, 0, 16000},
		{"alias-map", `{"models":{"new":{"id":"different-upstream","slug":"different-slug","context_window":1000000}}}`, 1000000, 0, 0},
		{"array", `[{"id":"new","limit":{"context":128000,"output":4096}}]`, 128000, 0, 4096},
		{"nested", `{"data":[{"id":"new","context":{"input_token_limit":1000000,"output_token_limit":64000}}]}`, 0, 0, 64000},
		{"ID-only", `{"data":[{"id":"new"}]}`, 0, 0, 0},
		{"max-only", `{"models":[{"slug":"new","max_context_window":872000}]}`, 0, 872000, 0},
		{"nullable", `{"data":[{"id":"new","context_window":null,"max_tokens":null}]}`, 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			models, err := parseModels([]byte(tt.body), SourceEndpointPI)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 || models[0].ID != "new" || models[0].ContextWindow != tt.context || models[0].MaxContextWindow != tt.max || models[0].MaxOutputTokens != tt.output {
				t.Fatalf("models=%+v", models)
			}
		})
	}
}

func TestTotalContextAndInputLimitAreDifferentCapabilities(t *testing.T) {
	models, err := parseModels([]byte(`{"data":[{"id":"new","limit":{"context":1050000,"input":922000,"output":128000}}]}`), SourceEndpointModels)
	if err != nil {
		t.Fatalf("valid independent total/input/output limits rejected: %v", err)
	}
	if len(models) != 1 || models[0].ContextWindow != 1050000 || models[0].MaxOutputTokens != 128000 {
		t.Fatalf("limits=%+v", models)
	}
	encoded, err := json.Marshal(models[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"max_input_tokens":922000`) {
		t.Fatalf("input limit was lost: %s", encoded)
	}
}

func TestPartialRichCatalogEnrichesSelectedModelFromPlain(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, `{"models":[{"slug":"other","context_window":272000},{"slug":"selected"}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"id":"selected","context_length":128000}]}`)
		}
	}))
	defer s.Close()
	models, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL}, "")
	if err != nil {
		t.Fatal(err)
	}
	selected := Model{}
	for _, m := range models {
		if m.ID == "selected" {
			selected = m
		}
	}
	if selected.ContextWindow != 128000 || len(models) != 2 || calls.Load() != 2 {
		t.Fatalf("partial catalog not enriched: models=%+v requests=%d", models, calls.Load())
	}
}

func TestValidPartialRichSurvivesPlainFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"unsupported", 404, `not found`},
		{"invalid-json", 200, `{"data":`},
		{"invalid-limit", 200, `{"data":[{"id":"selected","context_length":-1}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("client_version") == "pi" {
					fmt.Fprint(w, `{"models":[{"slug":"selected","context_window":272000},{"slug":"other"}]}`)
				} else {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, tt.body)
				}
			}))
			defer s.Close()
			c := newTestClient(t)
			b := Binding{ProviderID: "route", BaseURL: s.URL}
			models, err := c.Refresh(context.Background(), b, "")
			if err != nil {
				t.Fatalf("valid rich metadata discarded: %v", err)
			}
			if got, ok := c.Get(b, "selected"); !ok || got.ContextWindow != 272000 || got.Source != SourceEndpointPI || len(models) != 2 {
				t.Fatalf("rich snapshot=%+v ok=%v models=%+v", got, ok, models)
			}
		})
	}
}

func TestPartialDiscoveryTimeoutPreservesRichButCallerCancellationWins(t *testing.T) {
	for _, tt := range []struct {
		name           string
		cancelCaller   bool
		parentDeadline bool
	}{
		{"internal-deadline-retains-rich", false, false},
		{"caller-cancellation", true, false},
		{"caller-deadline", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plainStarted := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("client_version") == "pi" {
					fmt.Fprint(w, `{"models":[{"slug":"selected","context_window":272000},{"slug":"other"}]}`)
					return
				}
				close(plainStarted)
				<-r.Context().Done()
			}))
			defer s.Close()
			clientTimeout := 60 * time.Millisecond
			ctx := context.Background()
			var cancel context.CancelFunc
			if tt.cancelCaller {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				go func() { <-plainStarted; cancel() }()
			}
			if tt.parentDeadline {
				clientTimeout = 2 * time.Second
				ctx, cancel = context.WithTimeout(ctx, 60*time.Millisecond)
				defer cancel()
			}
			c := newTestClient(t, WithTimeout(clientTimeout))
			b := Binding{ProviderID: "route", BaseURL: s.URL}
			_, err := c.Refresh(ctx, b, "")
			if tt.cancelCaller || tt.parentDeadline {
				want := error(context.Canceled)
				if tt.parentDeadline {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Fatalf("caller cancellation err=%v", err)
				}
				if _, ok := c.Get(b, "selected"); ok {
					t.Fatal("caller canceled but snapshot published")
				}
			} else {
				if err != nil {
					t.Fatalf("internal enrichment timeout discarded validated model: %v", err)
				}
				if got, ok := c.Get(b, "selected"); !ok || got.ContextWindow != 272000 || got.Source != SourceEndpointPI {
					t.Fatalf("validated snapshot=%+v ok=%v", got, ok)
				}
			}
		})
	}
}

func TestInvalidRichCatalogDoesNotFallBackToPlain(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, `{"models":[{"slug":"selected","context_window":272000},{"slug":"other","context_window":-1}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"id":"selected","context_length":1050000}]}`)
		}
	}))
	defer s.Close()
	if _, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL}, ""); !errors.Is(err, ErrInvalidResponse) || calls.Load() != 1 {
		t.Fatalf("invalid rich response downgraded: err=%v calls=%d", err, calls.Load())
	}
}

func TestInputLimitOnlyDoesNotInventTotalAndAliasesRemainStrict(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"id":"new","max_input_tokens":922000,"max_output_tokens":128000}]}`,
		`{"models":[{"slug":"new","context":{"input_token_limit":922000,"output_token_limit":128000}}]}`,
		`{"models":[{"slug":"new","limit":{"input":922000,"output":128000}}]}`,
	} {
		models, err := parseModels([]byte(body), SourceEndpointModels)
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 1 || models[0].ContextWindow != 0 || models[0].MaxContextWindow != 0 || models[0].MaxInputTokens != 922000 || models[0].MaxOutputTokens != 128000 {
			t.Fatalf("independent input limits=%+v", models)
		}
	}
	for _, body := range []string{
		`{"models":[{"slug":"new","max_input_tokens":922000,"limit":{"input":921000}}]}`,
		`{"models":[{"slug":"new","context_window":1050000,"limit":{"context":1049999,"input":922000}}]}`,
		`{"models":[{"slug":"new","max_input_tokens":-1}]}`,
		`{"models":[{"slug":"new","max_input_tokens":0}]}`,
		`{"models":[{"slug":"new","max_input_tokens":true}]}`,
	} {
		if _, err := parseModels([]byte(body), SourceEndpointModels); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("invalid semantic alias accepted: %s err=%v", body, err)
		}
	}
}

func TestPartialCatalogMergePreservesRichCapabilitiesAndSources(t *testing.T) {
	for _, tt := range []struct {
		name, rich, plain string
		want              Model
		wantErr           bool
	}{
		{"output-only", `{"models":[{"slug":"new","max_tokens":128000}]}`, `{"data":[{"id":"new","context_length":1050000}]}`, Model{ID: "new", ContextWindow: 1050000, MaxOutputTokens: 128000, Source: SourceEndpointMerged}, false},
		{"input-and-output-only", `{"models":[{"slug":"new","max_input_tokens":922000,"max_tokens":128000}]}`, `{"data":[{"id":"new"}]}`, Model{ID: "new", MaxInputTokens: 922000, MaxOutputTokens: 128000, Source: SourceEndpointPI}, false},
		{"rich-field-wins", `{"models":[{"slug":"new","context_window":272000,"max_context_window":872000},{"slug":"other"}]}`, `{"data":[{"id":"new","context_length":1050000,"max_output_tokens":128000}]}`, Model{ID: "new", ContextWindow: 272000, MaxContextWindow: 872000, MaxOutputTokens: 128000, Source: SourceEndpointMerged}, false},
		{"plain-IDs-do-not-erase", `{"models":[{"slug":"new","context_window":272000,"max_context_window":872000,"max_tokens":128000},{"slug":"other"}]}`, `{"data":[{"id":"new"}]}`, Model{ID: "new", ContextWindow: 272000, MaxContextWindow: 872000, MaxOutputTokens: 128000, Source: SourceEndpointPI}, false},
		{"max-only-keeps-default-unknown", `{"models":[{"slug":"new","max_context_window":872000}]}`, `{"data":[{"id":"new"}]}`, Model{ID: "new", MaxContextWindow: 872000, Source: SourceEndpointPI}, false},
		{"merged-default-exceeds-rich-max", `{"models":[{"slug":"new","max_context_window":872000}]}`, `{"data":[{"id":"new","context_length":1050000}]}`, Model{ID: "new", MaxContextWindow: 872000, Source: SourceEndpointPI}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("client_version") == "pi" {
					fmt.Fprint(w, tt.rich)
				} else {
					fmt.Fprint(w, tt.plain)
				}
			}))
			defer s.Close()
			models, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL}, "")
			if (err != nil) != tt.wantErr || calls.Load() != 2 {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
			if tt.wantErr {
				return
			}
			got := Model{}
			for _, m := range models {
				if m.ID == "new" {
					got = m
				}
			}
			if got != tt.want {
				t.Fatalf("got=%+v want=%+v", got, tt.want)
			}
		})
	}
}

func TestInputAndMergedSourceSurviveSecretlessCacheRestart(t *testing.T) {
	dir := t.TempDir()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, `{"models":[{"slug":"new","limit":{"input":922000,"output":128000}}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"id":"new","context_length":1050000}]}`)
		}
	}))
	defer s.Close()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{ProviderID: "route", BaseURL: s.URL}
	if _, err := c.Refresh(context.Background(), b, "secret-never-written"); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := restarted.Get(b, "new"); !ok || got.MaxInputTokens != 922000 || got.ContextWindow != 1050000 || got.MaxOutputTokens != 128000 || got.Source != SourceEndpointMerged {
		t.Fatalf("cached=%+v ok=%v", got, ok)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret-never-written") {
			t.Fatal("secret in cache")
		}
	}
}

func TestDualAuthForCustomAnthropicModelCatalogs(t *testing.T) {
	for _, authHeader := range []string{"Authorization", "X-Api-Key"} {
		t.Run(authHeader, func(t *testing.T) {
			const secret = "dual-header-secret-not-in-cache"
			dir := t.TempDir()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := secret
				if authHeader == "Authorization" {
					want = "Bearer " + secret
				}
				if r.Header.Get(authHeader) != want {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Api-Key") != secret || r.Header.Get("Anthropic-Version") != "2023-06-01" {
					t.Error("dual headers missing")
				}
				fmt.Fprint(w, `{"models":[{"slug":"new","context_window":272000}]}`)
			}))
			defer s.Close()
			c, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			b := Binding{ProviderID: "custom", Transport: "anthropic_messages", BaseURL: s.URL, AuthKind: AuthAPIKeyBearer}
			if _, err := c.Refresh(context.Background(), b, secret); err != nil {
				t.Fatal(err)
			}
			files, _ := os.ReadDir(dir)
			for _, f := range files {
				data, _ := os.ReadFile(filepath.Join(dir, f.Name()))
				if strings.Contains(string(data), secret) || strings.Contains(string(data), "Authorization") || strings.Contains(string(data), "auth_kind") {
					t.Fatalf("auth persisted: %s", data)
				}
			}
		})
	}
}

func TestRejectsMalformedCapabilitiesAndConflicts(t *testing.T) {
	for _, body := range []string{
		`{"models":[{"slug":"new","context_window":0}]}`,
		`{"models":[{"slug":"new","context_window":1.5}]}`,
		`{"models":[{"slug":"new","context_window":1e6}]}`,
		`{"models":[{"slug":"new","context_window":"-1"}]}`,
		`{"models":[{"slug":"new","context_window":100000001}]}`,
		`{"models":[{"slug":"new","context_window":999999999999999999999}]}`,
		`{"models":[{"slug":"new","context_window":true}]}`,
		`{"models":[{"slug":"new","context_window":"100,000"}]}`,
		`{"models":[{"slug":"new","context_window":900000,"max_context_window":800000}]}`,
		`{"models":[{"slug":"new","context_window":100000,"context_length":200000}]}`,
		`{"models":[{"slug":"new","context_window":100000},{"slug":"new","context_window":200000}]}`,
		`{"models":[{"slug":"new","context_window":100000},{"slug":"new","max_tokens":1000}]}`,
		`{"models":[{"slug":"new\n","context_window":100000}]}`,
		`{"models":[{}]}`,
		`{"data":{},"models":"invalid"}`,
		`{"data":{}}`,
		`{"data":[]}{"data":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := parseModels([]byte(body), SourceEndpointPI); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	models, err := parseModels([]byte(`{"data":[{"id":"new","context_length":100000},{"id":"new","context_length":100000}]}`), SourceEndpointModels)
	if err != nil || len(models) != 1 {
		t.Fatalf("identical duplicates = %v err=%v", models, err)
	}
}

func TestCancellationTimeoutAndResponseLimits(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		started := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
		defer s.Close()
		c := newTestClient(t)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, err := c.Discover(ctx, Binding{ProviderID: "route", BaseURL: s.URL}, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("client deadline", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer s.Close()
		_, err := newTestClient(t, WithTimeout(40*time.Millisecond)).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL}, "")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("body size", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
			fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1))
		}))
		defer s.Close()
		_, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL}, "")
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("chunked size", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1))
		}))
		defer s.Close()
		_, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL}, "")
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("model count", func(t *testing.T) {
		items := make([]string, maxModels+1)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":"new-%d"}`, i)
		}
		if _, err := parseModels([]byte("["+strings.Join(items, ",")+"]"), SourceEndpointPI); !errors.Is(err, ErrTooManyModels) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestNoRedirectAndNativeAPIKeyHeaders(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "native-secret" || r.Header.Get("Authorization") != "" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Error("incorrect native headers")
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer s.Close()
	_, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "route", BaseURL: s.URL, AuthKind: AuthAPIKey}, "native-secret")
	if err == nil || targetCalls.Load() != 0 || strings.Contains(err.Error(), "native-secret") || strings.Contains(err.Error(), s.URL) {
		t.Fatalf("err=%v target calls=%d", err, targetCalls.Load())
	}
}

func TestTTLRefreshConcurrencyAndVersion(t *testing.T) {
	var count atomic.Int32
	var window atomic.Int32
	window.Store(272000)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		time.Sleep(10 * time.Millisecond)
		fmt.Fprintf(w, `{"models":[{"slug":"new","context_window":%d}]}`, window.Load())
	}))
	defer s.Close()
	c := newTestClient(t)
	b := Binding{ProviderID: "route", BaseURL: s.URL}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.RefreshIfStale(context.Background(), b, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 || c.Version(b) != 1 {
		t.Fatalf("calls=%d version=%d", count.Load(), c.Version(b))
	}
	if _, err := c.Refresh(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	if c.Version(b) != 1 {
		t.Fatal("identical capabilities changed version")
	}
	window.Store(872000)
	if _, err := c.Refresh(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	if c.Version(b) != 2 {
		t.Fatal("capability refresh did not advance version")
	}
	m, _ := c.Get(b, "new")
	if m.ContextWindow != 872000 {
		t.Fatalf("stale window=%d", m.ContextWindow)
	}
	models := c.Models(b)
	models[0].ContextWindow = 1
	if got, _ := c.Get(b, "new"); got.ContextWindow != 872000 {
		t.Fatal("caller mutated snapshot")
	}
}

func TestFailedRefreshRetainsPreviousSnapshot(t *testing.T) {
	var invalid atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if invalid.Load() {
			fmt.Fprint(w, `{"models":[{"slug":"new","context_window":-1}]}`)
		} else {
			fmt.Fprint(w, `{"models":[{"slug":"new","context_window":272000}]}`)
		}
	}))
	defer s.Close()
	c := newTestClient(t)
	b := Binding{ProviderID: "route", BaseURL: s.URL}
	if _, err := c.Refresh(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	invalid.Store(true)
	if _, err := c.Refresh(context.Background(), b, ""); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err=%v", err)
	}
	if got, ok := c.Get(b, "new"); !ok || got.ContextWindow != 272000 || c.Version(b) != 1 {
		t.Fatalf("snapshot=%+v ok=%v", got, ok)
	}
}

func TestRejectsSecretBearingBindingsAndUnsafeCache(t *testing.T) {
	for _, raw := range []string{"http://user:pass@127.0.0.1:8317", "http://127.0.0.1:8317?api_key=secret", "http://127.0.0.1:8317?", "http://127.0.0.1:8317#secret", "file:///tmp/models", "ftp://localhost/models", "http://localhost/a/../v1", "http://localhost/a%2Fb", "http://localhost/a%0Ab"} {
		if _, err := NormalizeBinding(Binding{ProviderID: "route", BaseURL: raw}); !errors.Is(err, ErrInvalidBinding) {
			t.Fatalf("binding accepted %s", raw)
		}
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "cache")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(link); err == nil {
		t.Fatal("symlink cache directory accepted")
	}
}

func TestRefreshWaitCancellationAndNonblockingGet(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			close(blocked)
			<-release
		}
		fmt.Fprint(w, `{"models":[{"slug":"new","context_window":272000}]}`)
	}))
	defer s.Close()
	c := newTestClient(t)
	b := Binding{ProviderID: "route", BaseURL: s.URL}
	if _, err := c.Refresh(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := c.Refresh(context.Background(), b, ""); finished <- err }()
	<-blocked
	// The last validated snapshot stays readable while a refresh is blocked.
	if model, ok := c.Get(b, "new"); !ok || model.ContextWindow != 272000 {
		t.Fatalf("model=%+v ok=%v", model, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Refresh(ctx, b, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait err=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("canceled waiter started new request: calls=%d", calls.Load())
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := c.RefreshIfStale(canceled, b, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached canceled refresh err=%v", err)
	}
}

func TestColdCacheIgnoresCorruptionUnsafeModeAndForeignBinding(t *testing.T) {
	b := Binding{ProviderID: "route", BaseURL: "http://127.0.0.1:8317"}
	normalized, key, err := normalizeBinding(b)
	if err != nil {
		t.Fatal(err)
	}
	record := cacheRecord{Schema: cacheSchema, Binding: normalized, UpdatedAt: time.Now().UTC(), Models: []Model{{ID: "new", ContextWindow: 272000, Source: SourceEndpointPI}}}
	for _, tt := range []struct {
		name    string
		change  func(*cacheRecord)
		mode    os.FileMode
		corrupt bool
	}{
		{"invalid-json", nil, 0600, true},
		{"future", func(r *cacheRecord) { r.UpdatedAt = time.Now().Add(2 * time.Hour) }, 0600, false},
		{"schema", func(r *cacheRecord) { r.Schema = cacheSchema + 1 }, 0600, false},
		{"foreign-provider", func(r *cacheRecord) { r.Binding.ProviderID = "other" }, 0600, false},
		{"foreign-endpoint", func(r *cacheRecord) { r.Binding.BaseURL = "http://127.0.0.1:9999" }, 0600, false},
		{"secret-url", func(r *cacheRecord) { r.Binding.BaseURL += "?key=secret" }, 0600, false},
		{"negative", func(r *cacheRecord) { r.Models[0].ContextWindow = -1 }, 0600, false},
		{"unknown-source", func(r *cacheRecord) { r.Models[0].Source = "provider-default" }, 0600, false},
		{"unknown-window-kind", func(r *cacheRecord) { r.Models[0].WindowKind = "unproven-profile" }, 0600, false},
		{"profile-without-maximum", func(r *cacheRecord) { r.Models[0].WindowKind = WindowKindClientProfile }, 0600, false},
		{"world-readable", nil, 0644, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := record
			candidate.Models = append([]Model(nil), record.Models...)
			if tt.change != nil {
				tt.change(&candidate)
			}
			data, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if tt.corrupt {
				data = []byte("not JSON")
			}
			if err := os.WriteFile(filepath.Join(dir, key+".json"), data, tt.mode); err != nil {
				t.Fatal(err)
			}
			client, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := client.Get(b, "new"); ok {
				t.Fatal("unsafe cache was trusted")
			}
		})
	}
	t.Run("cache file symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "record.json")
		data, _ := json.Marshal(record)
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, key+".json")); err != nil {
			t.Fatal(err)
		}
		client, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := client.Get(b, "new"); ok {
			t.Fatal("symlink cache record was trusted")
		}
	})
}

func TestCrossProcessCacheReloadBeforeTTLAndRouteIsolation(t *testing.T) {
	var calls, window atomic.Int32
	window.Store(272000)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"models":[{"slug":"new","context_window":%d,"max_context_window":872000,"max_tokens":128000}]}`, window.Load())
	}))
	defer s.Close()
	dir := t.TempDir()
	worker, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ui, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{ProviderID: "route", BaseURL: s.URL}
	if _, err := ui.Refresh(context.Background(), b, "not-persisted"); err != nil {
		t.Fatal(err)
	}
	if _, ok := worker.Get(b, "new"); ok {
		t.Fatal("pure Get must not perform disk discovery")
	}
	if _, err := worker.RefreshIfStale(context.Background(), b, "not-persisted"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("worker ignored another process's fresh cache, requests=%d", calls.Load())
	}
	if got, ok := worker.Get(b, "new"); !ok || got.ContextWindow != 272000 || worker.Version(b) != 1 {
		t.Fatalf("first reload=%+v ok=%v version=%d", got, ok, worker.Version(b))
	}
	window.Store(500000)
	if _, err := ui.Refresh(context.Background(), b, "not-persisted"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Reload(b); err != nil {
		t.Fatal(err)
	}
	if got, ok := worker.Get(b, "new"); !ok || got.ContextWindow != 500000 || worker.Version(b) != 2 {
		t.Fatalf("updated reload=%+v ok=%v version=%d", got, ok, worker.Version(b))
	}
	if err := worker.Reload(b); err != nil {
		t.Fatal(err)
	}
	if worker.Version(b) != 2 || calls.Load() != 2 {
		t.Fatalf("unchanged reload version=%d requests=%d", worker.Version(b), calls.Load())
	}
	other := b
	other.BaseURL += "/other-endpoint"
	if err := worker.Reload(other); err != nil {
		t.Fatal(err)
	}
	if _, ok := worker.Get(other, "new"); ok {
		t.Fatal("cache crossed endpoint binding")
	}
	// A new filesystem mtime cannot make older metadata override a snapshot.
	_, key, _ := normalizeBinding(b)
	old := cacheRecord{Schema: cacheSchema, Binding: b, UpdatedAt: time.Now().Add(-time.Hour), Models: []Model{{ID: "new", ContextWindow: 200000, Source: SourceEndpointPI}}}
	data, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, key+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := worker.Reload(b); err != nil {
		t.Fatal(err)
	}
	if got, _ := worker.Get(b, "new"); got.ContextWindow != 500000 || worker.Version(b) != 2 {
		t.Fatalf("cache timestamp rollback=%+v version=%d", got, worker.Version(b))
	}
}
