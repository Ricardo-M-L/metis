package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEndpointBindingCanonicalizesOnlySafeSpellingDifferences(t *testing.T) {
	a, err := NormalizeEndpointBinding(" GOOGLE ", " Gemini ", "https://EXAMPLE.COM:443/v1/")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeEndpointBinding("gemini", "gemini_native", "https://example.com/v1")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("equivalent bindings differ: %+v != %+v", a, b)
	}
	if a.Provider != "gemini" || a.Transport != "gemini_native" || a.BaseURL != "https://example.com/v1" {
		t.Fatalf("normalized binding = %+v", a)
	}
}

func TestNormalizeEndpointBindingLegacyEmptyTransportMatchesAnthropic(t *testing.T) {
	legacy, err := NormalizeEndpointBinding("route", "", "https://api.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := NormalizeEndpointBinding("route", "anthropic_messages", "https://api.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	if legacy != explicit {
		t.Fatalf("legacy empty transport binding = %+v, want %+v", legacy, explicit)
	}
}

func TestNormalizeEndpointBindingPreservesRepeatedSeparatorsAndIsIdempotent(t *testing.T) {
	for _, path := range []string{"", "/", "//", "///", "/tenant", "/tenant/", "/tenant//", "/tenant///", "/tenant//v1/", "/tenant%2F/", "/tenant%2F//"} {
		t.Run(path, func(t *testing.T) {
			raw := "https://gateway.example" + path
			first, err := NormalizeEndpointBinding("route", "openai", raw)
			if err != nil {
				t.Fatal(err)
			}
			second, err := NormalizeEndpointBinding(first.Provider, first.Transport, first.BaseURL)
			if err != nil || first != second {
				t.Fatalf("normalization is not idempotent: %+v -> %+v, %v", first, second, err)
			}
			if strings.HasSuffix(path, "//") && first.BaseURL != raw {
				t.Fatalf("routing-significant separator changed: %q -> %q", raw, first.BaseURL)
			}
		})
	}
}

func TestActivateAPIKeyBoundRepeatedSeparatorsRoundTripAndIsolation(t *testing.T) {
	withTempHome(t)
	const base = "https://gateway.example/tenant//"
	if err := ActivateAPIKeyBound("route", "fixture-managed-key", "openai", base); err != nil {
		t.Fatal(err)
	}
	if key, err := GetAPIKeyForEndpoint("route", "openai_chat", base, false); err != nil || key != "fixture-managed-key" {
		t.Errorf("original endpoint lookup = %q, %v", key, err)
	}
	for _, path := range []string{"/tenant", "/tenant/", "/tenant///", "/tenant%2F", "/tenant//v1"} {
		key, err := GetAPIKeyForEndpoint("route", "openai_chat", "https://gateway.example"+path, false)
		if key != "" || !errors.Is(err, ErrEndpointBindingMismatch) {
			t.Errorf("different path %q returned %q, %v", path, key, err)
		}
	}
}

func TestNormalizeEndpointBindingAzureAlias(t *testing.T) {
	alias, err := NormalizeEndpointBinding("route", "azure", "https://azure.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := NormalizeEndpointBinding("route", "azure_openai", "https://azure.example/v1")
	if err != nil || alias != canonical {
		t.Fatalf("azure alias = %+v, want %+v, %v", alias, canonical, err)
	}
}

func TestEndpointBindingDistinguishesSecurityBoundaries(t *testing.T) {
	base, err := NormalizeEndpointBinding("route", "openai_chat", "https://api.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		provider  string
		transport string
		baseURL   string
	}{
		"provider":  {"other", "openai_chat", "https://api.example/v1"},
		"transport": {"route", "openai_responses", "https://api.example/v1"},
		"host":      {"route", "openai_chat", "https://other.example/v1"},
		"path":      {"route", "openai_chat", "https://api.example/v2"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeEndpointBinding(tc.provider, tc.transport, tc.baseURL)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatalf("changed %s unexpectedly matched %+v", name, base)
			}
		})
	}
}

func TestActivateAPIKeyBoundRoundTripAndRejectsChangedEndpoint(t *testing.T) {
	withTempHome(t)
	if err := ActivateAPIKeyBound("route", "managed-secret", "openai", "https://API.EXAMPLE:443/v1/"); err != nil {
		t.Fatal(err)
	}
	key, err := GetAPIKeyForEndpoint("route", "openai_chat", "https://api.example/v1", false)
	if err != nil || key != "managed-secret" {
		t.Fatalf("bound lookup = %q, %v", key, err)
	}
	for _, endpoint := range []struct {
		transport string
		baseURL   string
	}{
		{"openai_responses", "https://api.example/v1"},
		{"openai_chat", "https://other.example/v1"},
		{"openai_chat", "https://api.example/v2"},
	} {
		key, err := GetAPIKeyForEndpoint("route", endpoint.transport, endpoint.baseURL, false)
		if key != "" || !errors.Is(err, ErrEndpointBindingMismatch) {
			t.Fatalf("changed endpoint lookup = %q, %v; want binding mismatch", key, err)
		}
	}
}

func TestGetAPIKeyForEndpointLegacyUnboundPolicy(t *testing.T) {
	withTempHome(t)
	if err := Set("anthropic", "legacy-secret"); err != nil {
		t.Fatal(err)
	}
	if key, err := GetAPIKeyForEndpoint("anthropic", "anthropic_messages", "https://api.anthropic.com", true); err != nil || key != "legacy-secret" {
		t.Fatalf("official legacy lookup = %q, %v", key, err)
	}
	if key, err := GetAPIKeyForEndpoint("anthropic", "anthropic_messages", "https://gateway.example/v1", false); key != "" || !errors.Is(err, ErrEndpointBindingRequired) {
		t.Fatalf("third-party legacy lookup = %q, %v; want binding required", key, err)
	}
}

func TestActivateAPIKeyBoundPersistsNormalizedMetadata(t *testing.T) {
	withTempHome(t)
	if err := ActivateAPIKeyBound("GOOGLE", "secret", "gemini", "https://GenerativeLanguage.Googleapis.com:443/"); err != nil {
		t.Fatal(err)
	}
	file, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := file["gemini"]
	if !ok || entry.Endpoint == nil {
		t.Fatalf("bound entry missing: %+v", file)
	}
	want := EndpointBinding{Provider: "gemini", Transport: "gemini_native", BaseURL: "https://generativelanguage.googleapis.com"}
	if *entry.Endpoint != want {
		t.Fatalf("persisted endpoint = %+v, want %+v", *entry.Endpoint, want)
	}
}

func TestStoredAPIKeyEndpointReturnsMetadataWithoutSecret(t *testing.T) {
	withTempHome(t)
	if err := ActivateAPIKeyBound("route", "managed-secret", "openai_chat", "https://api.example/v1"); err != nil {
		t.Fatal(err)
	}
	binding, present, err := StoredAPIKeyEndpoint("route")
	if err != nil || !present || binding == nil {
		t.Fatalf("StoredAPIKeyEndpoint = %+v, %v, %v", binding, present, err)
	}
	if binding.Provider != "route" || binding.Transport != "openai_chat" || binding.BaseURL != "https://api.example/v1" {
		t.Fatalf("unexpected endpoint metadata: %+v", binding)
	}
	if rendered := binding.Provider + binding.Transport + binding.BaseURL; strings.Contains(rendered, "managed-secret") {
		t.Fatal("endpoint metadata exposed the stored secret")
	}
}

func TestNormalizeEndpointBindingRejectsAmbiguousURLs(t *testing.T) {
	for _, raw := range []string{
		"", "api.example/v1", "ftp://api.example/v1",
		"http://api.example/v1", "https://user:pass@api.example/v1", "https://api.example/v1?q=x", "https://api.example/v1#x",
	} {
		if _, err := NormalizeEndpointBinding("route", "openai_chat", raw); err == nil {
			t.Fatalf("NormalizeEndpointBinding accepted %q", raw)
		}
	}
	for _, raw := range []string{"http://localhost:11434/v1", "http://127.0.0.1:9000/v1", "http://[::1]:9000/v1"} {
		if _, err := NormalizeEndpointBinding("route", "openai_chat", raw); err != nil {
			t.Fatalf("NormalizeEndpointBinding rejected loopback %q: %v", raw, err)
		}
	}
}

func TestGetAPIKeyForEndpointNeverReturnsSearchCredential(t *testing.T) {
	withTempHome(t)
	if err := SetSearchKey("tavily", "search-only-secret"); err != nil {
		t.Fatal(err)
	}
	key, err := GetAPIKeyForEndpoint("search:tavily", "openai_chat", "https://api.example/v1", false)
	if key != "" || err == nil {
		t.Fatalf("search credential lookup = %q, %v; want fail-closed", key, err)
	}
}
