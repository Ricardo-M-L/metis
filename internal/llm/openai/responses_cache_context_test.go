package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func cacheContextFixture() provider.Request {
	return provider.Request{
		SystemSections: []provider.SystemSection{
			{Name: "base", Body: "stable", Cache: true},
			{Name: "env", Body: "environment", Volatile: true},
			{Name: "memory_index", Body: "memory-before", Cache: true},
			{Name: "addendum", Body: "addendum", Cache: true},
			{Name: "runtime_state", Body: "runtime-before", Cache: true},
			{Name: "plan_mode", Body: "plan overlay", Volatile: true},
			{Name: "project_context", Body: "project"},
		},
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: "text", Text: "continue"}}}},
	}
}

func TestResponsesLocalCacheContextUpdateAndRemoval(t *testing.T) {
	p := NewResponses("fixture", "https://api.openai.com/v1", "gpt-5.5", 256, time.Second, 0)
	req := cacheContextFixture()
	snapshot, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.buildResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Instructions != "stable\n\naddendum\n\nproject" {
		t.Fatalf("stable instructions = %q", first.Instructions)
	}
	if len(first.Input) != 2 || first.Input[1].Role != "developer" {
		t.Fatalf("input = %+v", first.Input)
	}
	parts := first.Input[1].Content
	want := []responsesContentPart{
		{Type: "input_text", Text: "environment"},
		{Type: "input_text", Text: "memory-before"},
		{Type: "input_text", Text: "runtime-before"},
		{Type: "input_text", Text: "plan overlay"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("non-deterministic context ordering: %+v", parts)
	}
	for _, remove := range []bool{false, true} {
		next := req
		next.SystemSections = nil
		for _, sec := range req.SystemSections {
			if sec.Name == "memory_index" || sec.Name == "runtime_state" {
				if remove {
					continue
				}
				sec.Body = ""
			}
			next.SystemSections = append(next.SystemSections, sec)
		}
		body, err := p.buildResponsesRequest(next)
		if err != nil {
			t.Fatal(err)
		}
		if body.Instructions != first.Instructions || body.PromptCacheKey == "" || body.PromptCacheKey != first.PromptCacheKey {
			t.Fatal("removing/emptying mutable context invalidated stable instructions/cache key")
		}
		raw, err := json.Marshal(body.Input)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "memory-before") || strings.Contains(string(raw), "runtime-before") {
			t.Fatal("deleted context survived into next request")
		}
	}
	unchanged, err := json.Marshal(req)
	if err != nil || string(unchanged) != string(snapshot) {
		t.Fatal("Responses builder mutated caller-owned context/history")
	}
	if !reflect.DeepEqual(first.Input[1].Content, want) {
		t.Fatal("next request mutated a previously constructed context snapshot")
	}
}

func TestResponsesStoredCacheContextPlacementUnchanged(t *testing.T) {
	p := NewResponses("fixture", "https://api.openai.com/v1", "gpt-5.5", 256, time.Second, 0)
	p.StateMode = ResponsesStateProvider
	req := cacheContextFixture()
	req.Messages = stateRecoveryHistory(p)
	for _, build := range []struct {
		name string
		fn   func(provider.Request) (*responsesRequest, error)
	}{
		{"continuation", p.buildResponsesRequest},
		{"missing_id_recovery", p.buildStateRecoveryRequest},
	} {
		t.Run(build.name, func(t *testing.T) {
			body, err := build.fn(req)
			if err != nil {
				t.Fatal(err)
			}
			// Preserve the previous exact ordering/key for stored state. The
			// new implicit-cache layout is deliberately local-replay only.
			if !body.Store || body.Instructions != "stable\n\nmemory-before\n\naddendum\n\nruntime-before\n\nproject\n\nenvironment\n\nplan overlay" {
				t.Fatalf("stored instructions changed: %q", body.Instructions)
			}
			if build.name == "continuation" && body.PreviousResponseID == "" {
				t.Fatal("provider continuation lost its previous response ID")
			}
			if build.name == "missing_id_recovery" && body.PreviousResponseID != "" {
				t.Fatal("recovery retained a missing response ID")
			}
			for _, item := range body.Input {
				if item.Role == "developer" {
					t.Fatal("runtime snapshot leaked into provider-stored conversation input")
				}
			}
			next := req
			next.SystemSections = []provider.SystemSection{{Name: "base", Body: "stable", Cache: true}}
			cleared, err := build.fn(next)
			if err != nil || cleared.Instructions != "stable" {
				t.Fatal("stored-state request retained removed context")
			}
		})
	}
}

func TestResponsesCacheContextLegacyStringUnchanged(t *testing.T) {
	p := NewResponses("fixture", "https://api.openai.com/v1", "gpt-5.5", 256, time.Second, 0)
	req := provider.Request{System: "legacy memory_index runtime_state", Messages: cacheContextFixture().Messages}
	body, err := p.buildResponsesRequest(req)
	if err != nil || body.Instructions != req.System || len(body.Input) != 1 {
		t.Fatal("legacy string-only requests must not be reclassified by text matching")
	}
}
