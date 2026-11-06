package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCacheProjectionDistinguishesLocalChangesFromActualUsage(t *testing.T) {
	r := NewCacheStatsRing(2)
	req := llm.Request{Model: "test", SystemSections: []llm.SystemSection{{Name: "base", Body: strings.Repeat("stable ", 30)}, {Name: "runtime_state", Body: "cwd=/one"}}, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
	r.RecordRequest(req, CacheStat{Input: 100, CacheRead: 900})
	req.SystemSections[1].Body = "cwd=/two"
	r.RecordRequest(req, CacheStat{Input: 100, CacheRead: 900})
	s := r.Snapshot()[1]
	if s.Projection.Source != "local_request_estimate" || s.Projection.FirstChange != "dynamic_system" || s.Projection.SharedPrefixTokens == 0 {
		t.Fatalf("bad local projection: %+v", s.Projection)
	}
	if s.CacheRead != 900 || s.HitRate() != 0.9 {
		t.Fatal("fingerprint change incorrectly changed actual usage")
	}
	summary := r.CacheSummary()
	if !strings.Contains(summary, "do not imply a full cache miss") || strings.Contains(summary, "cwd=") {
		t.Fatal("diagnostics leak prompt or imply cache miss")
	}
	r.Reset()
	r.RecordRequest(req, CacheStat{})
	if r.Snapshot()[0].Projection.FirstChange != "initial" {
		t.Fatal("reset retained previous session projection")
	}
}

func TestCacheProjectionHashesAttachmentsAndResetDropsState(t *testing.T) {
	r := NewCacheStatsRing(2)
	payload := strings.Repeat("base64-private-fixture", 10000)
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "image", MediaType: "image/png", Data: payload}, {Type: "redacted_thinking", Data: payload}}}}}
	r.RecordRequest(req, CacheStat{})
	for _, part := range r.previousRequest {
		if strings.Contains(part.body, "base64-private-fixture") || len(part.body) > 2000 {
			t.Fatal("attachment retained in diagnostics")
		}
	}
	want := estimateActiveHistoryTokens(req.Messages)
	if r.Snapshot()[0].Projection.HistoryTokens != want {
		t.Fatal("history tokens count encoded data instead of native estimate")
	}
	r.Reset()
	if r.previousRequest != nil || len(r.Snapshot()) != 0 {
		t.Fatal("reset retained prompt/attachment identities")
	}
}

func TestCacheProjectionSchemaAndHistoryChanges(t *testing.T) {
	r := NewCacheStatsRing(4)
	req := llm.Request{Model: "test", System: "base"}
	r.RecordRequest(req, CacheStat{})
	req.Tools = []llm.ToolSpec{{Name: "Deferred", Description: "schema"}}
	r.RecordRequest(req, CacheStat{})
	if r.Snapshot()[1].Projection.FirstChange != "tool_schema" {
		t.Fatal("schema addition not classified")
	}
	req.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}
	r.RecordRequest(req, CacheStat{})
	if r.Snapshot()[2].Projection.FirstChange != "history" {
		t.Fatal("history addition not classified")
	}
}
