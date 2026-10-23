package desktopipc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestContextPressurePreservesCapabilitiesAndReadsOldWorker(t *testing.T) {
	pressure := ContextPressure{Used: 30488, Window: 872000, WindowKind: "total",
		ProfileDefaultWindow: 500000, ProfileMaxWindow: 872000,
		MaxWindow: 872000, MaxInputTokens: 744000, MaxOutputTokens: 128000, Source: "configured_clamped",
		ModelWindow: 1050000, ModelMaxInputTokens: 922000, ModelMaxOutputTokens: 128000, ModelSource: "catalog_model_spec",
		RequestedWindow: 1050000, LimitSource: "endpoint_models", BudgetSource: "per_model",
		ProviderID: "bound-provider", Model: "exact-model", RouteKey: "route-bound-to-account",
		CompactThreshold: 0.8, CompactAtTokens: 595200}
	var wire bytes.Buffer
	if err := NewEncoder(&wire).Encode(Message{Version: Version, Type: TypeStatus, Status: &Status{Context: &pressure}}); err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		Status struct {
			Context map[string]any `json:"context"`
		} `json:"status"`
	}
	if err := json.Unmarshal(wire.Bytes(), &encoded); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"windowKind": "total", "modelWindow": float64(1050000), "modelMaxInputTokens": float64(922000),
		"modelMaxOutputTokens": float64(128000), "modelSource": "catalog_model_spec", "requestedWindow": float64(1050000),
		"limitSource": "endpoint_models", "budgetSource": "per_model", "providerId": "bound-provider",
		"model": "exact-model", "routeKey": "route-bound-to-account",
	} {
		if encoded.Status.Context[key] != want {
			t.Fatalf("context JSON contract %q=%v; want %v", key, encoded.Status.Context[key], want)
		}
	}
	message, err := NewDecoder(&wire).Decode()
	if err != nil || message.Status == nil || message.Status.Context == nil || *message.Status.Context != pressure {
		t.Fatalf("capacity IPC round trip = %+v err=%v", message, err)
	}
	old := `{"version":1,"type":"status","decision":0,"accepted":false,"status":{"subAgents":0,"namedAgents":0,"backgroundTasks":0,"agents":[],"jobs":[],"context":{"used":1000,"window":200000,"compactThreshold":0.8,"compactAtTokens":160000}}}`
	message, err = NewDecoder(strings.NewReader(old + "\n")).Decode()
	wantOld := ContextPressure{Used: 1000, Window: 200000, CompactThreshold: 0.8, CompactAtTokens: 160000}
	if err != nil || message.Status == nil || message.Status.Context == nil || *message.Status.Context != wantOld {
		t.Fatalf("old worker compatibility = %+v err=%v", message, err)
	}
}

func TestContextBreakdownOptionalWireContract(t *testing.T) {
	parts := &ContextBreakdown{SystemTokens: 0, ToolTokens: 0, MessageTokens: 29, Estimated: true}
	pressure := ContextPressure{Used: 901, Window: 1000000, Breakdown: parts}
	var wire bytes.Buffer
	if err := NewEncoder(&wire).Encode(Message{Version: Version, Type: TypeStatus, Status: &Status{Context: &pressure}}); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"contextBreakdown":`, `"systemTokens":0`, `"toolTokens":0`, `"messageTokens":29`, `"estimated":true`} {
		if !strings.Contains(wire.String(), field) {
			t.Fatalf("missing wire field %s: %s", field, wire.String())
		}
	}
	decoded, err := NewDecoder(&wire).Decode()
	if err != nil || decoded.Status == nil || decoded.Status.Context == nil || decoded.Status.Context.Breakdown == nil || *decoded.Status.Context.Breakdown != *parts || decoded.Status.Context.Used != 901 {
		t.Fatalf("breakdown IPC=%+v err=%v", decoded, err)
	}
	for _, invalid := range []*ContextBreakdown{nil, {}, {SystemTokens: -1, Estimated: true}, {ToolTokens: -1, Estimated: true}, {MessageTokens: -1, Estimated: true}} {
		if invalid.Valid() {
			t.Fatalf("invalid categories accepted: %+v", invalid)
		}
	}
	if !parts.Valid() {
		t.Fatal("zero-valued categories are valid")
	}
}
