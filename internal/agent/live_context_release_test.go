package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	metisruntime "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Explicit opt-in only. No runtime/session/memory bootstrap: every message and
// memory below is synthetic, and credentials remain in the normal config path.
// Run once, without retries: METIS_LIVE_CONTEXT_RELEASE=1 go test ./internal/agent -run '^TestLiveContextRelease$' -count=1 -v
func TestLiveContextRelease(t *testing.T) {
	if os.Getenv("METIS_LIVE_CONTEXT_RELEASE") != "1" {
		t.Skip("requires explicit paid-provider opt-in")
	}
	for _, key := range []string{"METIS_DUMP_PROMPTS", "DUMP_PROMPTS", "METIS_DEBUG", "DEBUG", "METIS_DEBUG_OPENAI"} {
		t.Setenv(key, "0")
	}
	t.Setenv("METIS_RECOVERY_MAX_ATTEMPTS", "1")
	cfg, _, err := config.Load()
	if err != nil || cfg == nil {
		t.Fatal("normal provider configuration unavailable (details suppressed)")
	}
	name := cfg.Provider.Default
	raw, custom := cfg.Provider.Custom[name]
	t.Logf("provider=%s custom=%v transport=%s; no endpoint or credential logging", name, custom, raw.Transport)
	built, err := metisruntime.BuildProviderWithoutPreconnect(cfg, name, "")
	if err != nil || built == nil || built.Provider == nil {
		t.Fatal("provider build failed (details suppressed)")
	}
	p := &liveReleaseProvider{Provider: built.Provider, limit: 2}
	p.before = func(req llm.Request) error {
		payload := liveReleaseText(req.Messages)
		t.Logf("summary preflight head=%v middle=%v tail=%v", strings.Contains(payload, "HEAD_SOURCE_START"), strings.Contains(payload, "MIDDLE_SYNTHETIC_FACT_COMPLETELY_EXCLUDED"), strings.Contains(payload, "TAIL_SOURCE_END"))
		if strings.Contains(payload, "MIDDLE_SYNTHETIC_FACT_COMPLETELY_EXCLUDED") || !strings.Contains(payload, "TAIL_SOURCE_END") {
			return errors.New("synthetic truncation preflight failed; real request blocked")
		}
		return nil
	}
	defer func() {
		for i, u := range p.usage {
			t.Logf("request=%d actual_usage input=%d output=%d cache_read=%d cache_write=%d", i+1, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	mm, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal("synthetic memory initialization failed")
	}
	specs := []memory.Passage{
		{ID: "head", Content: "HEAD_SOURCE_START " + strings.Repeat("alpha evidence ", 80) + " HEAD_SOURCE_END", SourceSessionID: "head-origin"},
		{ID: "middle", Content: "MIDDLE_SYNTHETIC_FACT_COMPLETELY_EXCLUDED", SourceSessionID: "middle-origin"},
		{ID: "tail", Content: "TAIL_SOURCE_START " + strings.Repeat("omega evidence ", 80) + " TAIL_SOURCE_END", SourceSessionID: "tail-origin"},
	}
	for _, spec := range specs {
		if err := mm.Archival().Insert(spec); err != nil {
			t.Fatal("synthetic memory insert failed")
		}
	}
	stored, err := mm.Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil {
		t.Fatal("synthetic memory lookup failed")
	}
	byID := map[string]memory.Passage{}
	for _, passage := range stored {
		byID[passage.ID] = passage
	}
	var fences []string
	for _, spec := range specs {
		passage := byID[spec.ID]
		data, _ := json.Marshal([]string{passage.Content, passage.Scope, passage.SourceSessionID})
		refs, _ := json.Marshal([]struct{ ID, Hash string }{{passage.ID, fmt.Sprintf("%x", sha256.Sum256(data))}})
		body := memory.FormatRetrieveSection([]memory.Passage{passage})
		fences = append(fences, strings.Replace(body, "<auto-retrieve>", `<auto-retrieve refs="`+base64.RawURLEncoding.EncodeToString(refs)+`">`, 1))
	}
	message := func(role llm.Role, body string) llm.Message {
		return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: body}}}
	}
	history := []llm.Message{message(llm.RoleUser, "ancient synthetic greeting")}
	history[0].Content = append(history[0].Content, llm.ContentBlock{Type: "text", Text: strings.Join(fences, "\n\n"), Synthetic: true})
	for i := 0; i < 10; i++ {
		history = append(history, message(llm.RoleAssistant, strings.Repeat("old answer ", 12)), message(llm.RoleUser, fmt.Sprintf("old synthetic follow-up %d", i)))
	}
	history = append(history, message(llm.RoleUser, "current synthetic task"), message(llm.RoleAssistant, "working"), message(llm.RoleUser, "continue with the current task"), message(llm.RoleAssistant, "latest answer"))
	cc := agent.DefaultCompactionConfig()
	cc.RetainTokens = 200
	cc.RetainMinMessages = 2
	cc.RetainMinUserMessages = 1
	cc.MaxSummarizeInputTokens = 1400
	cc.MaxSummaryTokens = 1024
	cc.MaxSummaryRetries = 0
	l := agent.NewLoop(p, tools.NewRegistry(), nil, nil, "Synthetic release verification. No tools needed. Answer concisely.", 1)
	l.Model = built.Model
	l.Memory = mm
	l.Messages = history
	l.Compactor = agent.NewCompactor(cc, built.Model, built.Provider.MaxContextTokens(), p)
	result, err := l.CompactNow(ctx, agent.CompactOptions{Force: true})
	if err != nil || !result.Applied {
		if p.calls == 0 && err != nil {
			t.Logf("local pre-network failure: %s", err)
		}
		t.Fatalf("live summary failed or was not applied (provider details suppressed); err_type=%T applied=%v real_requests=%d", err, result.Applied, p.calls)
	}
	if len(p.requests) != 1 {
		t.Fatalf("summary requests=%d want=1", len(p.requests))
	}
	payload := liveReleaseText(p.requests[0].Messages)
	if strings.Contains(payload, specs[1].Content) {
		t.Fatal("fixture middle memory was retained; will not spend another request")
	}
	if !strings.Contains(payload, "TAIL_SOURCE_END") || strings.Contains(payload, specs[2].Content) {
		t.Fatal("fixture did not retain truncated head/tail evidence")
	}
	var summary string
	var referenceIDs []string
	for _, m := range l.History() {
		for _, b := range m.Content {
			if raw := b.ProviderHint["metis.summary.memory.v1"]; raw != "" {
				var deps struct {
					References   []struct{ ID, Hash string } `json:"references"`
					Unverifiable bool                        `json:"unverifiable"`
				}
				if json.Unmarshal([]byte(raw), &deps) != nil || deps.Unverifiable {
					t.Fatal("summary dependencies unverifiable")
				}
				for _, ref := range deps.References {
					referenceIDs = append(referenceIDs, ref.ID)
					if ref.ID == "middle" {
						t.Fatal("omitted middle memory incorrectly became summary dependency")
					}
				}
				summary = b.Text
			}
		}
	}
	if summary == "" || len(referenceIDs) == 0 {
		t.Fatal("missing real summary or retained memory provenance")
	}
	if err := mm.DeleteSession("middle-origin"); err != nil {
		t.Fatal("synthetic source deletion failed")
	}
	// The second real call exercises next-request sanitization after withdrawal.
	p.before = func(req llm.Request) error {
		if !strings.Contains(liveReleaseText(req.Messages), summary) {
			return errors.New("unrelated withdrawal invalidated live summary")
		}
		return nil
	}
	l.AppendUser("Synthetic check only: reply with LIVE_CONTEXT_OK. Do not call tools.")
	events := make(chan agent.Event, 128)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range events {
		}
	}()
	err = l.Run(ctx, events)
	close(events)
	<-drained
	if err != nil {
		t.Fatal("post-withdrawal real query failed (details suppressed)")
	}
	if p.calls != 2 {
		t.Fatalf("real requests=%d want=2", p.calls)
	}
	if !strings.Contains(p.outputs[1], "LIVE_CONTEXT_OK") {
		t.Fatal("post-withdrawal query did not return expected synthetic marker")
	}
	t.Logf("sameblock truncation verified; omitted source revoked; actual live summary preserved; retained_refs=%v real_requests=%d", referenceIDs, p.calls)
	if !custom || raw.Transport != "openai_responses" {
		t.Log("BOUNDARY: default provider is not explicitly configured for Responses; real Responses/local dynamic-plan test NOT attempted; no endpoint guessed")
	}
}

func liveReleaseText(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

type liveReleaseProvider struct {
	llm.Provider
	limit, calls int
	requests     []llm.Request
	usage        []llm.StreamEvent
	outputs      []string
	before       func(llm.Request) error
}

func (p *liveReleaseProvider) DynamicContextCheckpoints() bool {
	capability, ok := p.Provider.(interface{ DynamicContextCheckpoints() bool })
	return ok && capability.DynamicContextCheckpoints()
}
func (p *liveReleaseProvider) ContextCapabilities() llm.ContextCapabilities {
	if capability, ok := p.Provider.(interface {
		ContextCapabilities() llm.ContextCapabilities
	}); ok {
		return capability.ContextCapabilities()
	}
	return llm.ContextCapabilities{Window: p.Provider.MaxContextTokens()}
}
func (p *liveReleaseProvider) ContextIncludesAssistantBlock(b llm.ContentBlock) bool {
	if policy, ok := p.Provider.(llm.ContextHistoryPolicy); ok {
		return policy.ContextIncludesAssistantBlock(b)
	}
	return true
}
func (p *liveReleaseProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	if p.calls >= p.limit {
		return nil, errors.New("live test request budget exhausted")
	}
	if p.before != nil {
		if err := p.before(req); err != nil {
			return nil, err
		}
	}
	p.calls++
	// Cap output and remove all tools. Never log request bodies or provider errors.
	req.MaxTokens = 1024
	req.Tools = nil
	p.requests = append(p.requests, req)
	p.usage = append(p.usage, llm.StreamEvent{})
	p.outputs = append(p.outputs, "")
	stream, err := p.Provider.Stream(ctx, req)
	if err != nil {
		return nil, errors.New("live stream failed (details suppressed)")
	}
	return &liveReleaseStream{StreamReader: stream, p: p, index: p.calls - 1}, nil
}
func (p *liveReleaseProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected completion blocked by live test")
}

type liveReleaseStream struct {
	llm.StreamReader
	p     *liveReleaseProvider
	index int
}

func (s *liveReleaseStream) Recv() (llm.StreamEvent, error) {
	e, err := s.StreamReader.Recv()
	s.p.outputs[s.index] += e.TextDelta
	u := &s.p.usage[s.index]
	if e.HasInputTokens || e.InputTokens > 0 {
		u.InputTokens = e.InputTokens
	}
	if e.HasOutputTokens || e.OutputTokens > 0 {
		u.OutputTokens = e.OutputTokens
	}
	if e.HasCacheReadInputTokens || e.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = e.CacheReadInputTokens
	}
	if e.HasCacheCreationInputTokens || e.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = e.CacheCreationInputTokens
	}
	if e.Err != nil {
		e.Err = errors.New("provider event error (details suppressed)")
	}
	return e, err
}
