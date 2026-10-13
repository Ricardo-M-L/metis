package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

const kimiOwnerPeerID = "claude-fable-5-dd-3k-imik"

func kimiOwnerProjectionFixture() []modelinfo.Model {
	return []modelinfo.Model{
		{ID: "kimi-k3", Name: "kimi-k3", DisplayName: "kimi-k3", WindowKind: modelinfo.WindowKindClientProfile,
			ContextWindow: 272000, MaxContextWindow: 272000, Source: modelinfo.SourceEndpointPI,
			OwnedBy: "kimi", OwnerSource: modelinfo.SourceEndpointModels, OwnerModelID: kimiOwnerPeerID},
		{ID: kimiOwnerPeerID, Name: "kimi-k3", DisplayName: "kimi-k3", Source: modelinfo.SourceEndpointModels,
			OwnedBy: "kimi", OwnerSource: modelinfo.SourceEndpointModels, OwnerModelID: kimiOwnerPeerID,
			MaxInputTokens: 200000, MaxOutputTokens: 64000},
	}
}

func TestProviderModelOwnerProjectionHidesOnlyKimiIssuerPeer(t *testing.T) {
	models := kimiOwnerProjectionFixture()
	before := append([]modelinfo.Model(nil), models...)
	got := discoveredProviderModels(models)
	if len(got) != 1 || got[0].ID != "kimi-k3" || got[0].WindowKind != modelinfo.WindowKindClientProfile || got[0].Source != modelinfo.SourceEndpointPI || got[0].ContextWindow != 272000 || got[0].MaxContextWindow != 272000 || got[0].MaxInputTokens != 0 || got[0].MaxOutputTokens != 0 {
		t.Fatalf("issuer association created duplicate/masked choices or copied their limits: %+v", got)
	}
	if !reflect.DeepEqual(models, before) {
		t.Fatal("candidate projection changed the ownership evidence")
	}
}

func TestProviderModelOwnerProjectionPreservesUnverifiedAndIndependentPeers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]modelinfo.Model) []modelinfo.Model
	}{
		{"not a client profile", func(m []modelinfo.Model) []modelinfo.Model { m[0].WindowKind = modelinfo.WindowKindCapacity; return m }},
		{"owner absent", func(m []modelinfo.Model) []modelinfo.Model { m[0].OwnedBy = ""; return m }},
		{"owner came from rich route", func(m []modelinfo.Model) []modelinfo.Model { m[0].OwnerSource = modelinfo.SourceEndpointPI; return m }},
		{"owner peer absent", func(m []modelinfo.Model) []modelinfo.Model { m[0].OwnerModelID = ""; return m }},
		{"self owner", func(m []modelinfo.Model) []modelinfo.Model { m[0].OwnerModelID = m[0].ID; return m }},
		{"wrong peer identity", func(m []modelinfo.Model) []modelinfo.Model { m[0].OwnerModelID = "unrelated-wire-id"; return m }},
		{"rich peer advertised independently", func(m []modelinfo.Model) []modelinfo.Model { m[1].Source = modelinfo.SourceEndpointPI; return m }},
		{"merged peer advertised independently", func(m []modelinfo.Model) []modelinfo.Model { m[1].Source = modelinfo.SourceEndpointMerged; return m }},
		{"only Name matches", func(m []modelinfo.Model) []modelinfo.Model { m[1].DisplayName = "different-original-name"; return m }},
		{"peer owner differs", func(m []modelinfo.Model) []modelinfo.Model { m[1].OwnedBy = "different-issuer"; return m }},
		{"peer owner identity differs", func(m []modelinfo.Model) []modelinfo.Model { m[1].OwnerModelID = "different-peer"; return m }},
		{"ambiguous profile references", func(m []modelinfo.Model) []modelinfo.Model {
			second := m[0]
			second.ID = "second-client-profile"
			return append(m, second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := tc.change(kimiOwnerProjectionFixture())
			before := append([]modelinfo.Model(nil), models...)
			got := discoveredProviderModels(models)
			if len(got) != len(models) {
				t.Fatalf("unverified/independent candidates were hidden: %+v", got)
			}
			if !reflect.DeepEqual(models, before) {
				t.Fatal("candidate projection mutated its input")
			}
		})
	}
	models := append(kimiOwnerProjectionFixture(),
		modelinfo.Model{ID: "independent-wire", Name: "kimi-k3", DisplayName: "different-model", OwnedBy: "kimi", OwnerSource: modelinfo.SourceEndpointModels, OwnerModelID: "independent-wire", Source: modelinfo.SourceEndpointModels},
		modelinfo.Model{ID: "independent-rich", Name: "kimi-k3", DisplayName: "kimi-k3", OwnedBy: "kimi", OwnerSource: modelinfo.SourceEndpointPI, OwnerModelID: "independent-rich", Source: modelinfo.SourceEndpointPI})
	got := discoveredProviderModels(models)
	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	if !reflect.DeepEqual(ids, []string{"kimi-k3", "independent-wire", "independent-rich"}) {
		t.Fatalf("generic Name/issuer deduplication removed independent routes: %v", ids)
	}
}

func TestProviderModelOwnerProjectionKeepsCachedKimiOwnershipEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") == "pi" {
			fmt.Fprint(w, `{"models":[{"slug":"kimi-k3","display_name":"kimi-k3","context_window":272000,"max_context_window":272000,"supported_in_api":true,"minimal_client_version":"0.124.0","supports_experimental_context":false}]}`)
		} else {
			fmt.Fprint(w, `{"data":[{"id":"claude-fable-5-dd-3k-imik","display_name":"kimi-k3","owned_by":"kimi","max_input_tokens":200000,"max_tokens":64000}]}`)
		}
	}))
	defer upstream.Close()
	client, err := modelinfo.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binding := modelinfo.Binding{ProviderID: "owner-projection-test", Transport: "anthropic_messages", BaseURL: upstream.URL}
	models, err := client.Refresh(context.Background(), binding, "")
	if err != nil {
		t.Fatal(err)
	}
	before := client.Models(binding)
	if got := discoveredProviderModels(models); len(got) != 1 || got[0].ID != "kimi-k3" {
		t.Fatalf("K3 rich selection was replaced or duplicated: %+v", got)
	}
	if !reflect.DeepEqual(client.Models(binding), before) || !reflect.DeepEqual(models, before) {
		t.Fatal("projection rewrote the cached union")
	}
	rich, ok := client.Get(binding, "kimi-k3")
	if !ok || rich.OwnedBy != "kimi" || rich.OwnerSource != modelinfo.SourceEndpointModels || rich.OwnerModelID != kimiOwnerPeerID || rich.MaxInputTokens != 0 || rich.MaxOutputTokens != 0 {
		t.Fatalf("rich owner identity or independent capacity changed: %+v ok=%v", rich, ok)
	}
	peer, ok := client.Get(binding, kimiOwnerPeerID)
	if !ok || peer.DisplayName != "kimi-k3" || peer.OwnedBy != "kimi" || peer.MaxInputTokens != 200000 || peer.MaxOutputTokens != 64000 {
		t.Fatalf("runtime owner-validation evidence was removed: %+v ok=%v", peer, ok)
	}
}
