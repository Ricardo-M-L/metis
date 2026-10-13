package modelinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const codexProfileFixture = `{"slug":"gpt-6.1-sol","context_window":272000,"max_context_window":872000,"max_tokens":128000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}`

func TestCompleteCodexProfileIsMarkedWithoutChangingNumbers(t *testing.T) {
	models, err := Parse([]byte(`{"models":[`+codexProfileFixture+`]}`), SourceEndpointPI)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ContextWindow != 272000 || models[0].MaxContextWindow != 872000 || models[0].MaxOutputTokens != 128000 {
		t.Fatalf("client numbers changed: %+v", models)
	}
	data, err := json.Marshal(models[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"window_kind":"client_profile"`) {
		t.Fatalf("Codex profile was flattened to capacity: %s", data)
	}
}

func TestLegacyWindowCacheMustBeRediscovered(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"models":[`+codexProfileFixture+`]}`)
	}))
	defer s.Close()
	dir := t.TempDir()
	b := Binding{ProviderID: "gateway", BaseURL: s.URL}
	_, key, err := normalizeBinding(b)
	if err != nil {
		t.Fatal(err)
	}
	legacy := cacheRecord{Schema: 1, Binding: b, UpdatedAt: time.Now(), Models: []Model{{ID: "gpt-6.1-sol", ContextWindow: 272000, MaxContextWindow: 872000, MaxOutputTokens: 128000, Source: SourceEndpointPI}}}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(b, "gpt-6.1-sol"); ok {
		t.Fatal("legacy marker-free cache silently treated 272k as model capacity")
	}
	if _, err := c.RefreshIfStale(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests=%d", calls.Load())
	}
}

func profileRecord(t *testing.T) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewBufferString(codexProfileFixture))
	decoder.UseNumber()
	var record map[string]any
	if err := decoder.Decode(&record); err != nil {
		t.Fatal(err)
	}
	return record
}

func parseProfileRecord(t *testing.T, record map[string]any, source string) Model {
	t.Helper()
	data, err := json.Marshal(map[string]any{"models": []any{record}})
	if err != nil {
		t.Fatal(err)
	}
	models, err := Parse(data, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models=%+v", models)
	}
	return models[0]
}

func TestProfileRequiresAllOriginalMarkersWithoutNamesOrMagicNumbers(t *testing.T) {
	for _, field := range []string{"slug", "context_window", "max_context_window", "supported_in_api", "minimal_client_version", "supports_experimental_context"} {
		t.Run("missing-"+field, func(t *testing.T) {
			record := profileRecord(t)
			record["id"] = "gpt-6.1-sol"
			delete(record, field)
			if got := parseProfileRecord(t, record, SourceEndpointPI); got.WindowKind == WindowKindClientProfile {
				t.Fatalf("incomplete profile accepted: %+v", got)
			}
		})
	}
	for _, tt := range []struct {
		name, field string
		value       any
	}{
		{"supported-null", "supported_in_api", nil},
		{"supported-string", "supported_in_api", "true"},
		{"experimental-null", "supports_experimental_context", nil},
		{"experimental-string", "supports_experimental_context", "false"},
		{"empty-version", "minimal_client_version", ""},
		{"version-control", "minimal_client_version", "0.153.0\n"},
		{"version-number", "minimal_client_version", json.Number("1")},
		{"string-default", "context_window", "272000"},
		{"string-maximum", "max_context_window", "872000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record := profileRecord(t)
			record[tt.field] = tt.value
			if got := parseProfileRecord(t, record, SourceEndpointPI); got.WindowKind == WindowKindClientProfile {
				t.Fatalf("malformed marker accepted: %+v", got)
			}
		})
	}
	for _, supported := range []bool{false, true} {
		for _, experimental := range []bool{false, true} {
			t.Run(fmt.Sprintf("bools-%v-%v", supported, experimental), func(t *testing.T) {
				record := profileRecord(t)
				record["supported_in_api"] = supported
				record["supports_experimental_context"] = experimental
				// A name, specific tier size, minimum version, and Pi source are not
				// classifier conditions. The original schema supplies the evidence.
				record["slug"] = "brand-new-route"
				record["context_window"] = json.Number("131072")
				record["max_context_window"] = json.Number("1048576")
				record["minimal_client_version"] = "next-client-v3"
				if got := parseProfileRecord(t, record, SourceEndpointModels); got.WindowKind != WindowKindClientProfile || got.ContextWindow != 131072 {
					t.Fatalf("profile rejected: %+v", got)
				}
			})
		}
	}
	for _, body := range []string{
		`{"models":[{"slug":"gpt-6.1-sol","context_window":272000,"max_context_window":872000,"max_tokens":128000}]}`,
		`{"data":[{"id":"gpt-6.1-sol","context_length":272000,"max_context_window":872000}]}`,
		`{"models":[{"slug":"gpt-6.1-sol"}]}`,
	} {
		models, err := Parse([]byte(body), SourceEndpointPI)
		if err != nil {
			t.Fatal(err)
		}
		if models[0].WindowKind == WindowKindClientProfile {
			t.Fatalf("generic route capacity mislabeled: %+v", models)
		}
	}
	t.Run("total-alias-is-not-original-client-schema", func(t *testing.T) {
		record := profileRecord(t)
		record["contextWindow"] = record["context_window"]
		delete(record, "context_window")
		if got := parseProfileRecord(t, record, SourceEndpointPI); got.WindowKind == WindowKindClientProfile || got.ContextWindow != 272000 {
			t.Fatalf("alias promoted to profile: %+v", got)
		}
	})
}

func TestSyntheticMapSlugCannotCreateProfile(t *testing.T) {
	for _, hasOriginalSlug := range []bool{false, true} {
		t.Run(fmt.Sprint(hasOriginalSlug), func(t *testing.T) {
			record := profileRecord(t)
			if !hasOriginalSlug {
				delete(record, "slug")
			}
			data, err := json.Marshal(map[string]any{"models": map[string]any{"callable-alias": record}})
			if err != nil {
				t.Fatal(err)
			}
			models, err := Parse(data, SourceEndpointPI)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 || models[0].ID != "callable-alias" {
				t.Fatalf("alias lost: %+v", models)
			}
			if got := models[0].WindowKind == WindowKindClientProfile; got != hasOriginalSlug {
				t.Fatalf("synthetic slug participated in classification: original=%v model=%+v", hasOriginalSlug, models[0])
			}
		})
	}
}

func TestProfileSemanticsSurviveWarmCacheAndSharedReload(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"models":[`+codexProfileFixture+`]}`)
	}))
	defer s.Close()
	dir := t.TempDir()
	b := Binding{ProviderID: "gateway", BaseURL: s.URL}
	worker, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ui, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ui.Refresh(context.Background(), b, "secret-not-in-profile-cache"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Reload(b); err != nil {
		t.Fatal(err)
	}
	if got, ok := worker.Get(b, "gpt-6.1-sol"); !ok || got.WindowKind != WindowKindClientProfile || got.ContextWindow != 272000 {
		t.Fatalf("shared semantic lost: %+v ok=%v", got, ok)
	}
	restarted, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RefreshIfStale(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("warm-cache unexpectedly rediscovered: calls=%d", calls.Load())
	}
	if got, ok := restarted.Get(b, "gpt-6.1-sol"); !ok || got.WindowKind != WindowKindClientProfile || got.MaxContextWindow != 872000 || got.MaxOutputTokens != 128000 {
		t.Fatalf("cached semantic lost: %+v ok=%v", got, ok)
	}
	_, key, _ := normalizeBinding(b)
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record cacheRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Schema != cacheSchema || strings.Contains(string(data), "secret-not-in-profile-cache") || strings.Contains(string(data), "minimal_client_version") {
		t.Fatalf("cache schema/privacy=%s", data)
	}
}

func TestPartialMergeRetainsWindowDonorSemantics(t *testing.T) {
	profile := parseProfileRecord(t, profileRecord(t), SourceEndpointPI)
	for _, tt := range []struct {
		name                           string
		rich, plain                    Model
		kind                           string
		window, maximum, input, output int
	}{
		{"profile-not-capacity", profile, Model{ID: profile.ID, ContextWindow: 272000, MaxContextWindow: 272000, MaxInputTokens: 922000, Source: SourceEndpointModels}, WindowKindClientProfile, 272000, 872000, 922000, 128000},
		{"generic-window-does-not-adopt-profile-maximum", Model{ID: profile.ID, ContextWindow: 64000, Source: SourceEndpointPI}, profile, "", 64000, 0, 0, 128000},
		{"input-only-adopts-profile-window-kind", Model{ID: profile.ID, MaxInputTokens: 922000, Source: SourceEndpointPI}, profile, WindowKindClientProfile, 272000, 872000, 922000, 128000},
		{"generic-max-only-does-not-adopt-profile-default", Model{ID: profile.ID, MaxContextWindow: 1050000, Source: SourceEndpointPI}, profile, "", 0, 1050000, 0, 128000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			models, err := mergeCapabilities([]Model{tt.rich}, []Model{tt.plain})
			if err != nil {
				t.Fatal(err)
			}
			m := models[0]
			if m.WindowKind != tt.kind || m.ContextWindow != tt.window || m.MaxContextWindow != tt.maximum || m.MaxInputTokens != tt.input || m.MaxOutputTokens != tt.output {
				t.Fatalf("mixed semantic=%+v", m)
			}
		})
	}
}
