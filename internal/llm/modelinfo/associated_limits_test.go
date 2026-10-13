package modelinfo

import (
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

func TestUniqueAliasCapacityWindowHasSeparateTotalEvidence(t *testing.T) {
	models, calls := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, `{"data":[{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","context_window":200000,"input_token_limit":128000,"output_token_limit":64000}]}`)
	m := findOwnerModel(t, models, "kimi-k3")
	if calls != 2 || m.ContextWindow != 272000 || m.MaxContextWindow != 272000 || m.MaxInputTokens != 0 || m.MaxOutputTokens != 0 {
		t.Fatalf("raw profile changed: %+v calls=%d", m, calls)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	limits, ok := record["assoc_limits"].(map[string]any)
	if !ok || limits["context_window"] != float64(200000) || limits["evidence"] != "capacity_window" || limits["source"] != SourceEndpointModels || limits["model_id"] != "wire-k3" {
		t.Fatalf("real alias total was lost: %s", data)
	}
	if _, present := limits["max_input_tokens"]; present {
		t.Fatalf("bare input promoted to known limit: %s", data)
	}
	if _, present := limits["max_output_tokens"]; present {
		t.Fatalf("bare output promoted to known limit: %s", data)
	}
}

func TestAssociatedLimitsUseFieldSpecificEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, peer string
		limits     AssociatedLimits
	}{
		{"bare-limits-unknown", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","input_token_limit":200000,"output_token_limit":64000}`, AssociatedLimits{}},
		{"total-proves-only-total", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","context_window":200000,"max_context_window":512000,"input_token_limit":128000,"output_token_limit":64000}`, AssociatedLimits{ContextWindow: 200000, MaxContextWindow: 512000, Source: SourceEndpointModels, ModelID: "wire-k3", Evidence: AssocEvidenceCapacityWindow}},
		{"max-total-only", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","max_context_window":200000,"input_token_limit":128000}`, AssociatedLimits{MaxContextWindow: 200000, Source: SourceEndpointModels, ModelID: "wire-k3", Evidence: AssocEvidenceCapacityWindow}},
		{"explicit-small-input-output", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","limit_evidence":"served_capacity","input_token_limit":32000,"output_token_limit":4096}`, AssociatedLimits{MaxInputTokens: 32000, MaxOutputTokens: 4096, Source: SourceEndpointModels, ModelID: "wire-k3", Evidence: AssocEvidenceDeclaredServed}},
		{"explicit-group", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","limit_evidence":"served_capacity","context_window":200000,"max_context_window":512000,"input_token_limit":128000,"output_token_limit":64000}`, AssociatedLimits{ContextWindow: 200000, MaxContextWindow: 512000, MaxInputTokens: 128000, MaxOutputTokens: 64000, Source: SourceEndpointModels, ModelID: "wire-k3", Evidence: AssocEvidenceDeclaredServed}},
		{"unknown-marker", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","limit_evidence":"capacity_window","input_token_limit":32000,"output_token_limit":4096}`, AssociatedLimits{}},
		{"wrong-marker-type", `{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","limit_evidence":true,"input_token_limit":32000}`, AssociatedLimits{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			models, calls := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, `{"data":[`+tt.peer+`]}`)
			m := findOwnerModel(t, models, "kimi-k3")
			if calls != 2 || m.AssocLimits != tt.limits || m.ContextWindow != 272000 || m.MaxContextWindow != 272000 || m.MaxInputTokens != 0 || m.MaxOutputTokens != 0 || m.ID != "kimi-k3" {
				t.Fatalf("field evidence/profile: %+v calls=%d want=%+v", m, calls, tt.limits)
			}
		})
	}
}

func TestSameIDServingTotalPreservesProfileAndTypedTokenLimits(t *testing.T) {
	models, _ := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, `{"data":[{"id":"kimi-k3","owned_by":"kimi","context_window":200000,"max_context_window":256000,"input_token_limit":128000,"output_token_limit":64000}]}`)
	m := findOwnerModel(t, models, "kimi-k3")
	want := AssociatedLimits{ContextWindow: 200000, MaxContextWindow: 256000, Source: SourceEndpointModels, ModelID: "kimi-k3", Evidence: AssocEvidenceCapacityWindow}
	if m.AssocLimits != want || m.WindowKind != WindowKindClientProfile || m.ContextWindow != 272000 || m.MaxContextWindow != 272000 || m.MaxInputTokens != 128000 || m.MaxOutputTokens != 64000 {
		t.Fatalf("same-ID contract regressed: %+v", m)
	}
	models, _ = ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, `{"data":[{"id":"kimi-k3","owned_by":"kimi","limit_evidence":"served_capacity","input_token_limit":32000,"output_token_limit":4096}]}`)
	m = findOwnerModel(t, models, "kimi-k3")
	if m.AssocLimits.MaxInputTokens != 32000 || m.AssocLimits.Evidence != AssocEvidenceDeclaredServed || m.LimitEvidence != LimitEvidenceServedCapacity || m.ContextWindow != 272000 {
		t.Fatalf("same-ID explicit provenance lost: %+v", m)
	}
}

func TestKnownOwnerStillEnrichesServingLimitsWithoutIdentityOverride(t *testing.T) {
	rich := strings.Replace(kimiProfileFixture, `"slug":"kimi-k3"`, `"slug":"kimi-k3","owned_by":"kimi"`, 1)
	models, calls := ownerModels(t, `{"models":[`+rich+`]}`, `{"data":[{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","context_window":200000}]}`)
	m := findOwnerModel(t, models, "kimi-k3")
	if calls != 2 || m.OwnerSource != SourceEndpointPI || m.OwnerModelID != m.ID || m.OwnedBy != "kimi" || m.AssocLimits.ContextWindow != 200000 || m.AssocLimits.ModelID != "wire-k3" {
		t.Fatalf("known issuer skipped or overwritten: %+v calls=%d", m, calls)
	}
	models, _ = ownerModels(t, `{"models":[`+rich+`]}`, `{"data":[{"id":"wire-k3","display_name":"kimi-k3","owned_by":"other","context_window":200000}]}`)
	if m := findOwnerModel(t, models, "kimi-k3"); m.AssocLimits != (AssociatedLimits{}) || m.OwnedBy != "kimi" {
		t.Fatalf("foreign issuer supplied serving limits: %+v", m)
	}
}

func TestAmbiguousAliasesCannotSupplyServingConstraints(t *testing.T) {
	for _, plain := range []string{
		`{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi","context_window":200000},{"id":"wire-b","display_name":"kimi-k3","owned_by":"kimi","context_window":160000}]}`,
		`{"data":[{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi","context_window":200000},{"id":"wire-a","display_name":"kimi-k3","owned_by":"kimi","context_window":200000}]}`,
		`{"data":[{"id":"wire-a","name":"kimi-k3","owned_by":"kimi","context_window":200000}]}`,
	} {
		models, _ := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, plain)
		if m := findOwnerModel(t, models, "kimi-k3"); m.AssocLimits != (AssociatedLimits{}) {
			t.Fatalf("ambiguous mapping supplied limit: %+v", m)
		}
	}
	models, calls := ownerModels(t, `{"models":[{"slug":"ordinary","context_window":64000,"max_tokens":4096}]}`, `{"data":[{"id":"wire","display_name":"ordinary","owned_by":"kimi","context_window":32000}]}`)
	if m := findOwnerModel(t, models, "ordinary"); calls != 1 || m.ContextWindow != 64000 || m.MaxOutputTokens != 4096 || m.AssocLimits != (AssociatedLimits{}) {
		t.Fatalf("ordinary capacity contract changed: %+v calls=%d", m, calls)
	}
}

func TestAssociatedLimitsCache4WarmAndLegacy3Rediscovery(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(fmt.Sprint(sameID), func(t *testing.T) {
			wireID := "wire-k3"
			if sameID {
				wireID = "kimi-k3"
			}
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("client_version") == "pi" {
					fmt.Fprint(w, `{"models":[`+kimiProfileFixture+`]}`)
				} else {
					fmt.Fprintf(w, `{"data":[{"id":%q,"display_name":"kimi-k3","owned_by":"kimi","limit_evidence":"served_capacity","context_window":200000,"input_token_limit":128000,"output_token_limit":32000}]}`, wireID)
				}
			}))
			defer s.Close()
			dir := t.TempDir()
			b := Binding{ProviderID: "gateway", BaseURL: s.URL}
			reader, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			writer, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Refresh(context.Background(), b, "secret-not-in-limit-cache"); err != nil {
				t.Fatal(err)
			}
			if err := reader.Reload(b); err != nil {
				t.Fatal(err)
			}
			warm, err := New(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := warm.RefreshIfStale(context.Background(), b, ""); err != nil {
				t.Fatal(err)
			}
			for _, c := range []*Client{reader, warm} {
				m, ok := c.Get(b, "kimi-k3")
				if !ok || m.AssocLimits.ContextWindow != 200000 || m.AssocLimits.MaxInputTokens != 128000 || m.AssocLimits.MaxOutputTokens != 32000 || m.AssocLimits.ModelID != wireID || m.AssocLimits.Evidence != AssocEvidenceDeclaredServed || m.ContextWindow != 272000 {
					t.Fatalf("warm constraint/proof lost: %+v ok=%v", m, ok)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("warm cache requested again: %d", calls.Load())
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
			if record.Schema != 4 || strings.Contains(string(data), "secret-not-in-limit-cache") {
				t.Fatalf("schema/privacy=%s", data)
			}
			oldDir := t.TempDir()
			record.Schema = 3
			record.UpdatedAt = time.Now()
			for i := range record.Models {
				record.Models[i].AssocLimits = AssociatedLimits{}
				record.Models[i].LimitEvidence = ""
			}
			data, _ = json.Marshal(record)
			if err := os.WriteFile(filepath.Join(oldDir, key+".json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			old, err := New(oldDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := old.Get(b, "kimi-k3"); ok {
				t.Fatal("schema3 stale constraints accepted")
			}
			if _, err := old.RefreshIfStale(context.Background(), b, ""); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 4 {
				t.Fatalf("schema3 was not rediscovered: %d", calls.Load())
			}
		})
	}
}

func TestAssociatedCacheEvidenceAndPeerCannotBeForged(t *testing.T) {
	models, _ := ownerModels(t, `{"models":[`+kimiProfileFixture+`]}`, `{"data":[{"id":"wire-k3","display_name":"kimi-k3","owned_by":"kimi","context_window":200000}]}`)
	if !validCachedModels(models) {
		t.Fatal("valid snapshot rejected")
	}
	for _, tt := range []struct {
		name   string
		mutate func([]Model)
	}{
		{"input-with-only-total-evidence", func(ms []Model) {
			for i := range ms {
				if ms[i].ID == "kimi-k3" {
					ms[i].AssocLimits.MaxInputTokens = 128000
				}
			}
		}},
		{"wrong-peer-field", func(ms []Model) {
			for i := range ms {
				if ms[i].ID == "kimi-k3" {
					ms[i].AssocLimits.ContextWindow = 128000
				}
			}
		}},
		{"unknown-evidence", func(ms []Model) {
			for i := range ms {
				if ms[i].ID == "kimi-k3" {
					ms[i].AssocLimits.Evidence = "claimed-by-label"
				}
			}
		}},
		{"missing-peer", func(ms []Model) {
			for i := range ms {
				if ms[i].ID == "kimi-k3" {
					ms[i].AssocLimits.ModelID = "unlisted-wire"
				}
			}
		}},
		{"unknown-raw-marker", func(ms []Model) {
			for i := range ms {
				if ms[i].ID == "wire-k3" {
					ms[i].LimitEvidence = "inferred"
				}
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidate := append([]Model(nil), models...)
			tt.mutate(candidate)
			if validCachedModels(candidate) {
				t.Fatal("unsafe limit graph accepted")
			}
		})
	}
}
