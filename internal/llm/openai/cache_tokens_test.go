package openai

// cache_tokens_test.go — covers the three wire shapes that OpenAI-
// compat upstreams use to report cached-prefix tokens, so a flip on
// any one provider's response shape doesn't silently drop the
// cache-hit metric on the floor.

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestCacheReadTokens_OpenAINestedShape(t *testing.T) {
	// OpenAI / GLM / Zhipu emit prompt_tokens_details.cached_tokens.
	body := `{
		"prompt_tokens": 1234,
		"completion_tokens": 56,
		"prompt_tokens_details": {"cached_tokens": 1000}
	}`
	var u oaiUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cacheReadTokens(u); got != 1000 {
		t.Errorf("OpenAI nested cached_tokens: got %d want 1000", got)
	}
}

func TestCacheReadTokens_DeepSeekFlatShape(t *testing.T) {
	// DeepSeek emits prompt_cache_hit_tokens flat alongside prompt_tokens.
	body := `{
		"prompt_tokens": 1234,
		"completion_tokens": 56,
		"prompt_cache_hit_tokens": 900,
		"prompt_cache_miss_tokens": 334
	}`
	var u oaiUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cacheReadTokens(u); got != 900 {
		t.Errorf("DeepSeek prompt_cache_hit_tokens: got %d want 900", got)
	}
}

func TestCacheReadTokens_KimiFlatShape(t *testing.T) {
	// Some Kimi variants emit cached_tokens flat.
	body := `{
		"prompt_tokens": 1234,
		"completion_tokens": 56,
		"cached_tokens": 800
	}`
	var u oaiUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cacheReadTokens(u); got != 800 {
		t.Errorf("Kimi flat cached_tokens: got %d want 800", got)
	}
}

func TestCacheReadTokens_ZeroWhenNoCache(t *testing.T) {
	// Cold prompt — upstream emits no cached fields at all. This is
	// the common case below cache thresholds (OpenAI 1024 / DeepSeek 64).
	body := `{
		"prompt_tokens": 1234,
		"completion_tokens": 56
	}`
	var u oaiUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cacheReadTokens(u); got != 0 {
		t.Errorf("cold prompt: got %d want 0", got)
	}
}

func TestCacheReadTokens_PrefersNestedOverFlat(t *testing.T) {
	// Defensive: if a provider sends BOTH shapes (e.g. a future
	// version of OpenAI also reflects a flat field for legacy
	// clients), we pick the canonical nested OpenAI form first
	// because that's the documented one.
	body := `{
		"prompt_tokens": 1234,
		"completion_tokens": 56,
		"prompt_tokens_details": {"cached_tokens": 1000},
		"prompt_cache_hit_tokens": 999,
		"cached_tokens": 888
	}`
	var u oaiUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cacheReadTokens(u); got != 1000 {
		t.Errorf("nested form should win: got %d want 1000", got)
	}
}

func TestFromOpenAIChoice_PopulatesCacheReadField(t *testing.T) {
	// End-to-end through the non-streaming path: usage with a DeepSeek-
	// style cache hit lands in Response.CacheReadInputTokens.
	body := `{
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "hi"},
			"finish_reason": "stop"
		}],
		"usage": {
			"prompt_tokens": 1234,
			"completion_tokens": 5,
			"prompt_cache_hit_tokens": 900
		}
	}`
	var r oaiResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp := fromOpenAIChoice(r.Choices[0], r.Usage)
	if resp.InputTokens != 334 {
		t.Errorf("InputTokens: got %d want 334", resp.InputTokens)
	}
	if resp.CacheReadInputTokens != 900 {
		t.Errorf("CacheReadInputTokens: got %d want 900", resp.CacheReadInputTokens)
	}
}

func TestOpenAIStream_EmitsCacheReadOnUsageFrame(t *testing.T) {
	// Streaming path: the final frame carries usage with cached_tokens;
	// the resulting message_delta event must reflect it in
	// CacheReadInputTokens (same field Anthropic populates so the
	// metrics line speaks one language across providers).
	payload := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"ok","role":"assistant"},"index":0}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":2000,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1800}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	s := newOpenAIStream(io.NopCloser(strings.NewReader(payload)))
	defer s.Close()

	var sawDelta bool
	for {
		ev, err := s.Recv()
		if ev.Type == "message_delta" {
			if ev.CacheReadInputTokens != 1800 {
				t.Errorf("message_delta CacheReadInputTokens: got %d want 1800", ev.CacheReadInputTokens)
			}
			if ev.InputTokens != 200 {
				t.Errorf("message_delta InputTokens: got %d want 200", ev.InputTokens)
			}
			sawDelta = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
	}
	if !sawDelta {
		t.Error("never saw a message_delta event with usage")
	}
}

func TestNormalizeInputUsage_CachedExceedsTotal(t *testing.T) {
	input, cached := normalizeInputUsage(10, 12)
	if input != 0 || cached != 10 {
		t.Fatalf("normalizeInputUsage(10, 12) = (%d, %d), want (0, 10)", input, cached)
	}
}
