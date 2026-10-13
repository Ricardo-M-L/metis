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
	"sync/atomic"
	"testing"
	"time"
)

const kimiProfileFixture = `{"slug":"kimi-k3","context_window":272000,"max_context_window":272000,"supported_in_api":true,"minimal_client_version":"0.153.0","supports_experimental_context":false}`
const kimiPlainFixture = `{"data":[{"id":"claude-fable-5-dd-3k-imik","display_name":"kimi-k3","owned_by":"kimi","input_token_limit":200000,"output_token_limit":64000}]}`

func TestProfileOwnerDiscoveryReadsPlainWithoutCopyingAliasLimits(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, `{"models":[`+kimiProfileFixture+`]}`)
		} else {
			fmt.Fprint(w, kimiPlainFixture)
		}
	}))
	defer s.Close()
	c := newTestClient(t)
	b := Binding{ProviderID: "gateway", Transport: "anthropic_messages", BaseURL: s.URL}
	if _, err := c.Refresh(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("profile context hid ordinary issuer metadata: requests=%d", calls.Load())
	}
	m, ok := c.Get(b, "kimi-k3")
	if !ok || m.ID != "kimi-k3" || m.ContextWindow != 272000 || m.MaxInputTokens != 0 || m.MaxOutputTokens != 0 || m.Source != SourceEndpointPI {
		t.Fatalf("alias limits or selection were changed: %+v ok=%v", m, ok)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record["owned_by"] != "kimi" || record["owner_source"] != SourceEndpointModels || record["owner_model_id"] != "claude-fable-5-dd-3k-imik" {
		t.Fatalf("issuer association lost: %s", data)
	}
	wire, ok := c.Get(b, "claude-fable-5-dd-3k-imik")
	if !ok || wire.ID != "claude-fable-5-dd-3k-imik" || wire.MaxInputTokens != 200000 || wire.MaxOutputTokens != 64000 {
		t.Fatalf("wire alias limits lost: %+v ok=%v", wire, ok)
	}
}

func ownerModels(t *testing.T, rich, plain string) ([]Model, int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, rich)
		} else {
			fmt.Fprint(w, plain)
		}
	}))
	defer s.Close()
	models, err := newTestClient(t).Discover(context.Background(), Binding{ProviderID: "gateway", BaseURL: s.URL}, "")
	if err != nil {
		t.Fatal(err)
	}
	return models, calls.Load()
}

func findOwnerModel(t *testing.T, models []Model, id string) Model {
	t.Helper()
	for _, m := range models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("model %s missing: %+v", id, models)
	return Model{}
}

func TestOwnerAliasAmbiguityAndDisplayOnlyRestrictions(t *testing.T) {
	for _, tt := range []struct{ name, plain string }{
		{"two-wire-IDs", `{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi"},{"id":"wire-b","display_name":"kimi-k3","owned_by":"kimi"}]}`},
		{"two-different-owners", `{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi"},{"id":"wire-b","display_name":"kimi-k3","owned_by":"other"}]}`},
		{"one-unknown-owner-match", `{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi"},{"id":"wire-b","display_name":"kimi-k3"}]}`},
		{"duplicate-identical-wire-record", `{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi"},{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi"}]}`},
		{"generic-name-is-not-display-identity", `{"data":[{"id":"wire-a","name":"kimi-k3","owned_by":"kimi"}]}`},
		{"substring-is-not-identity", `{"data":[{"id":"wire-a","display_name":"kimi-k3-preview","owned_by":"kimi"}]}`},
		{"different-case-is-not-identity", `{"data":[{"id":"wire-a","display_name":"Kimi-K3","owned_by":"kimi"}]}`},
		{"missing-owner", `{"data":[{"id":"wire-a","display_name":"kimi-k3"}]}`},
		{"empty-owner", `{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":""}]}`},
		{"null-owner", `{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":null}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			models, calls := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, tt.plain)
			m := findOwnerModel(t, models, "kimi-k3")
			if calls != 2 || m.OwnedBy != "" || m.OwnerSource != "" || m.OwnerModelID != "" || m.ID != "kimi-k3" || m.MaxInputTokens != 0 || m.MaxOutputTokens != 0 {
				t.Fatalf("unsafe association: %+v calls=%d", m, calls)
			}
		})
	}
	t.Run("duplicate-rich-ID", func(t *testing.T) {
		models, _ := ownerModels(t, `{"models":[`+kimiProfileFixture+`,`+kimiProfileFixture+`]}`, kimiPlainFixture)
		if m := findOwnerModel(t, models, "kimi-k3"); m.OwnedBy != "" {
			t.Fatalf("duplicate profile established issuer: %+v", m)
		}
	})
	t.Run("wire-ID-already-a-rich-model", func(t *testing.T) {
		models, _ := ownerModels(t, `{"models":[`+kimiProfileFixture+`,{"slug":"claude-fable-5-dd-3k-imik"}]}`, kimiPlainFixture)
		if m := findOwnerModel(t, models, "kimi-k3"); m.OwnedBy != "" {
			t.Fatalf("occupied wire ID established issuer: %+v", m)
		}
	})
}

func TestDirectOwnerAndCapacityMergeRemainIndependent(t *testing.T) {
	t.Run("same-ID-owner-is-direct", func(t *testing.T) {
		models, _ := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, `{"data":[{"id":"kimi-k3","owned_by":"kimi","input_token_limit":64000,"output_token_limit":8192}]}`)
		m := findOwnerModel(t, models, "kimi-k3")
		if m.OwnedBy != "kimi" || m.OwnerSource != SourceEndpointModels || m.OwnerModelID != m.ID || m.MaxInputTokens != 64000 || m.MaxOutputTokens != 8192 || m.ContextWindow != 272000 {
			t.Fatalf("direct owner/real limits lost: %+v", m)
		}
	})
	t.Run("rich-existing-owner-is-not-label-overridden", func(t *testing.T) {
		rich := strings.Replace(kimiProfileFixture, `"slug":"kimi-k3"`, `"slug":"kimi-k3","owned_by":"original"`, 1)
		models, calls := ownerModels(t, `{"models":[`+rich+`]}`, kimiPlainFixture)
		m := findOwnerModel(t, models, "kimi-k3")
		if calls != 2 || m.OwnedBy != "original" || m.OwnerSource != SourceEndpointPI || m.OwnerModelID != m.ID {
			t.Fatalf("original issuer overridden: %+v calls=%d", m, calls)
		}
	})
	t.Run("same-ID-owner-conflict-cannot-use-alias-backdoor", func(t *testing.T) {
		rich := strings.Replace(kimiProfileFixture, `"slug":"kimi-k3"`, `"slug":"kimi-k3","owned_by":"original"`, 1)
		models, _ := ownerModels(t, `{"models":[`+rich+`,{"slug":"partial"}]}`, `{"data":[{"id":"kimi-k3","owned_by":"kimi","input_token_limit":64000},{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi"}]}`)
		m := findOwnerModel(t, models, "kimi-k3")
		if m.OwnedBy != "" || m.OwnerSource != "" || m.OwnerModelID != "" || m.MaxInputTokens != 64000 {
			t.Fatalf("conflicting issuer reintroduced or capacity erased: %+v", m)
		}
	})
	t.Run("normal-capacity-does-not-infer-owner-from-label", func(t *testing.T) {
		models, calls := ownerModels(t, `{"models":[{"slug":"kimi-k3","context_window":64000,"max_tokens":8192}]}`, kimiPlainFixture)
		m := findOwnerModel(t, models, "kimi-k3")
		if calls != 1 || m.OwnedBy != "" || m.ContextWindow != 64000 || m.MaxOutputTokens != 8192 {
			t.Fatalf("normal endpoint capacity changed: %+v calls=%d", m, calls)
		}
	})
	for _, tt := range []struct{ name, value string }{{"null", "null"}, {"empty", `""`}, {"valid", `"kimi"`}, {"number", "1"}, {"bool", "true"}, {"control", `"ki\nmi"`}, {"spaces", `" kimi "`}} {
		t.Run("parse-"+tt.name, func(t *testing.T) {
			models, err := Parse([]byte(`{"data":[{"id":"callable","display_name":"human label","owned_by":`+tt.value+`}]}`), SourceEndpointModels)
			wantInvalid := tt.name == "number" || tt.name == "bool" || tt.name == "control" || tt.name == "spaces"
			if wantInvalid {
				if !errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("invalid owner err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m := models[0]
			if tt.name == "valid" {
				if m.OwnedBy != "kimi" || m.OwnerSource != SourceEndpointModels || m.OwnerModelID != "callable" || m.DisplayName != "human label" {
					t.Fatalf("direct statement lost: %+v", m)
				}
			} else if m.OwnedBy != "" || m.OwnerSource != "" || m.OwnerModelID != "" {
				t.Fatalf("unknown owner fabricated: %+v", m)
			}
		})
	}
}

func TestOwnerAssociationSurvivesCurrentCacheAndReload(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, `{"models":[`+kimiProfileFixture+`]}`)
		} else {
			fmt.Fprint(w, kimiPlainFixture)
		}
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
	if _, err := ui.Refresh(context.Background(), b, "secret-not-in-owner-cache"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Reload(b); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RefreshIfStale(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*Client{worker, restarted} {
		m, ok := c.Get(b, "kimi-k3")
		if !ok || m.OwnedBy != "kimi" || m.OwnerSource != SourceEndpointModels || m.OwnerModelID != "claude-fable-5-dd-3k-imik" || m.Source != SourceEndpointPI || m.MaxInputTokens != 0 || m.MaxOutputTokens != 0 {
			t.Fatalf("warm association lost or aliased limits copied: %+v ok=%v", m, ok)
		}
		wire, ok := c.Get(b, m.OwnerModelID)
		if !ok || wire.DisplayName != "kimi-k3" || wire.ID != m.OwnerModelID {
			t.Fatalf("original wire statement lost: %+v ok=%v", wire, ok)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("warm cache called endpoint again: %d", calls.Load())
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
	if record.Schema != cacheSchema || strings.Contains(string(data), "secret-not-in-owner-cache") {
		t.Fatalf("schema/privacy: %s", data)
	}
	legacyDir := t.TempDir()
	legacy := cacheRecord{Schema: 2, Binding: b, UpdatedAt: time.Now(), Models: []Model{{ID: "kimi-k3", ContextWindow: 272000, MaxContextWindow: 272000, WindowKind: WindowKindClientProfile, Source: SourceEndpointPI}}}
	data, _ = json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(legacyDir, key+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	old, err := New(legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := old.Get(b, "kimi-k3"); ok {
		t.Fatal("schema2 marker-free issuer silently accepted")
	}
	if _, err := old.RefreshIfStale(context.Background(), b, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("schema2 did not rediscover both channels: %d", calls.Load())
	}
}
