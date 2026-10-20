package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func learningFixtureFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLearningProviderConfigCopiesOnlySelectedRoute(t *testing.T) {
	for _, selected := range []string{"anthropic", "openai", "gemini", "custom-target"} {
		t.Run(selected, func(t *testing.T) {
			cfg := &config.Config{
				Provider: config.ProviderSet{
					Default:     selected,
					Anthropic:   config.ProviderAnthropic{APIKey: "anthropic-secret-fixture", APIKeyEnv: "HOST_ANTHROPIC_KEY", BaseURL: "https://anthropic.test", Model: "anthropic-fixture", MaxTokens: 1024},
					OpenAI:      config.ProviderOpenAI{APIKey: "openai-secret-fixture", APIKeyEnv: "HOST_OPENAI_KEY", BaseURL: "https://openai.test", Model: "openai-fixture", MaxTokens: 2048, HostedTools: []string{"web_search"}, ResponsesStateMode: "provider"},
					Gemini:      config.ProviderGemini{APIKey: "gemini-secret-fixture", APIKeyEnv: "HOST_GEMINI_KEY", BaseURL: "https://gemini.test", Model: "gemini-fixture", MaxTokens: 512},
					OpenAICodex: config.ProviderOpenAICodex{Model: "subscription-fixture"},
					Custom: map[string]config.ProviderRaw{
						"custom-target": {Transport: "anthropic_messages", APIKey: "custom-secret-fixture", APIKeyEnv: "HOST_CUSTOM_KEY", BaseURL: "https://custom.test", Model: "custom-fixture", MaxTokens: 4096, HostedTools: []string{"web_search"}, ResponsesStateMode: "provider"},
						"unrelated":     {Transport: "openai_chat", APIKey: "unrelated-secret-fixture", Model: "unrelated-fixture"},
					},
				},
				Hooks: config.HooksConfig{SessionStart: []config.HookSpec{{Command: "echo hook-secret-fixture"}}},
			}
			before, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, model, err := learningProviderConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got.Default != selected {
				t.Fatalf("default = %q", got.Default)
			}
			want := config.ProviderSet{Default: selected}
			switch selected {
			case "anthropic":
				want.Anthropic = cfg.Provider.Anthropic
				want.Anthropic.APIKey = ""
				want.Anthropic.APIKeyEnv = "METIS_LEARNING_QUERY_API_KEY"
				if model != want.Anthropic.Model {
					t.Fatalf("model = %q", model)
				}
			case "openai":
				want.OpenAI = cfg.Provider.OpenAI
				want.OpenAI.APIKey = ""
				want.OpenAI.APIKeyEnv = "METIS_LEARNING_QUERY_API_KEY"
				want.OpenAI.HostedTools = nil
				want.OpenAI.ResponsesStateMode = "local"
				if model != want.OpenAI.Model {
					t.Fatalf("model = %q", model)
				}
			case "gemini":
				want.Gemini = cfg.Provider.Gemini
				want.Gemini.APIKey = ""
				want.Gemini.APIKeyEnv = "METIS_LEARNING_QUERY_API_KEY"
				if model != want.Gemini.Model {
					t.Fatalf("model = %q", model)
				}
			case "custom-target":
				p := cfg.Provider.Custom[selected]
				p.APIKey = ""
				p.APIKeyEnv = "METIS_LEARNING_QUERY_API_KEY"
				p.HostedTools = nil
				p.ResponsesStateMode = "local"
				want.Custom = map[string]config.ProviderRaw{selected: p}
				if model != p.Model {
					t.Fatalf("model = %q", model)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("copied unrelated provider settings: got %#v, want %#v", got, want)
			}
			var serialized bytes.Buffer
			if err := toml.NewEncoder(&serialized).Encode(got); err != nil {
				t.Fatal(err)
			}
			for _, marker := range []string{"secret-fixture", "HOST_", "unrelated-fixture", "subscription-fixture", "web_search", "[hooks"} {
				if strings.Contains(serialized.String(), marker) {
					t.Fatalf("isolated config retained %q", marker)
				}
			}
			after, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("selected-provider isolation mutated parent config")
			}
		})
	}
}

func TestLearningProviderConfigRejectsNonAPIKeyRoutes(t *testing.T) {
	for _, provider := range []string{"openai-codex", "missing", "unsupported"} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{Provider: config.ProviderSet{Default: provider, Custom: map[string]config.ProviderRaw{"unsupported": {Transport: "bedrock_anthropic", Model: "fixture"}}}}
			if _, _, err := learningProviderConfig(cfg); err == nil {
				t.Fatal("accepted unsupported evaluator route")
			}
		})
	}
}

func validLearningSuiteFixture() learning.Suite {
	return learning.Suite{Name: "host-suite", Cases: []learning.Case{{ID: "case-1", Prompt: "compute 17 times 19", Expected: "323"}}, MaxTokens: 100000, MaxDurationSeconds: 120}
}

func TestLoadLearningSuiteStrictHostInput(t *testing.T) {
	suite := validLearningSuiteFixture()
	data, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	got, digest, err := loadLearningSuite(learningFixtureFile(t, string(data)+"\n"))
	if err != nil || !reflect.DeepEqual(got, suite) || digest != learning.Digest(append(data, '\n')) {
		t.Fatalf("valid suite = %#v hash=%q err=%v", got, digest, err)
	}
	for _, tc := range []struct{ name, data string }{
		{"unknown-top-level", strings.TrimSuffix(string(data), "}") + `,"host_secret":true}`},
		{"unknown-case-field", `{"name":"host-suite","cases":[{"id":"case-1","prompt":"test","expected":"ok","command":"ignored"}],"max_tokens":100,"max_duration_seconds":30}`},
		{"second-object", string(data) + "\n{}"},
		{"trailing-invalid-json", string(data) + " trailing"},
		{"null", "null"},
		{"array", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := loadLearningSuite(learningFixtureFile(t, tc.data)); err == nil {
				t.Fatal("accepted invalid host suite")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*learning.Suite)
	}{
		{"empty-name", func(s *learning.Suite) { s.Name = " " }},
		{"no-cases", func(s *learning.Suite) { s.Cases = nil }},
		{"duplicate-case", func(s *learning.Suite) { s.Cases = append(s.Cases, s.Cases[0]) }},
		{"too-many-cases", func(s *learning.Suite) {
			for i := 1; i < 33; i++ {
				c := s.Cases[0]
				c.ID = strings.Repeat("x", i+1)
				s.Cases = append(s.Cases, c)
			}
		}},
		{"empty-case-id", func(s *learning.Suite) { s.Cases[0].ID = "" }},
		{"empty-prompt", func(s *learning.Suite) { s.Cases[0].Prompt = "\n\t" }},
		{"oversized-prompt", func(s *learning.Suite) { s.Cases[0].Prompt = strings.Repeat("x", 65537) }},
		{"no-assertion", func(s *learning.Suite) { s.Cases[0].Expected = "" }},
		{"two-assertions", func(s *learning.Suite) { s.Cases[0].ExpectedJSON = json.RawMessage(`{"answer":323}`) }},
		{"zero-token-budget", func(s *learning.Suite) { s.MaxTokens = 0 }},
		{"oversized-token-budget", func(s *learning.Suite) { s.MaxTokens = 10000001 }},
		{"zero-deadline", func(s *learning.Suite) { s.MaxDurationSeconds = 0 }},
		{"oversized-deadline", func(s *learning.Suite) { s.MaxDurationSeconds = 3601 }},
		{"negative-cost-ratio", func(s *learning.Suite) { s.MaxCostRatio = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validLearningSuiteFixture()
			tc.mutate(&s)
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadLearningSuite(learningFixtureFile(t, string(b))); err == nil {
				t.Fatal("accepted invalid host suite case or bounds")
			}
		})
	}
}

func TestReadLearningMetricsAggregatesReportedUsage(t *testing.T) {
	body := `{"turn_number":1,"tokens":{"in":17,"out":19,"cache_read":23,"cache_create":29}}` + "\n" +
		`{"turn_number":2,"tokens":{"in":0,"out":31,"cache_read":37,"cache_create":0}}` + "\n"
	got, reported, err := readLearningMetrics(learningFixtureFile(t, body))
	want := learning.Usage{Input: 17, Output: 50, CacheRead: 60, CacheCreate: 29}
	if err != nil || !reported || got != want {
		t.Fatalf("usage=%#v reported=%v err=%v", got, reported, err)
	}
	got, reported, err = readLearningMetrics(learningFixtureFile(t, ""))
	if err != nil || reported || got != (learning.Usage{}) {
		t.Fatalf("empty usage=%#v reported=%v err=%v", got, reported, err)
	}
}

func TestReadLearningMetricsRejectsAbsentNegativeAndMalformed(t *testing.T) {
	for _, field := range []string{"in", "out", "cache_read", "cache_create"} {
		for _, mode := range []string{"absent", "null", "negative"} {
			t.Run(field+"-"+mode, func(t *testing.T) {
				values := map[string]any{"in": 1, "out": 2, "cache_read": 3, "cache_create": 4}
				switch mode {
				case "absent":
					delete(values, field)
				case "null":
					values[field] = nil
				case "negative":
					values[field] = -1
				}
				b, err := json.Marshal(map[string]any{"tokens": values})
				if err != nil {
					t.Fatal(err)
				}
				if _, reported, err := readLearningMetrics(learningFixtureFile(t, string(b))); err == nil || reported {
					t.Fatalf("accepted %s %s: reported=%v err=%v", field, mode, reported, err)
				}
			})
		}
	}
	for _, body := range []string{`{}`, `{"tokens":null}`, `{"tokens":{"in":"1","out":2,"cache_read":3,"cache_create":4}}`, `not-json`} {
		if _, reported, err := readLearningMetrics(learningFixtureFile(t, body)); err == nil || reported {
			t.Fatalf("accepted malformed row %q: reported=%v err=%v", body, reported, err)
		}
	}
	if _, reported, err := readLearningMetrics(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil || reported {
		t.Fatalf("missing metric file became reported zero usage: reported=%v err=%v", reported, err)
	}
}

func learningTraceFixture(t *testing.T, events ...session.TraceEvent) (string, []byte) {
	t.Helper()
	var data bytes.Buffer
	for _, event := range events {
		if err := json.NewEncoder(&data).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return learningFixtureFile(t, data.String()), data.Bytes()
}

func TestReadLearningTracePairsConcreteCallIDs(t *testing.T) {
	const name = "test-skill"
	path, data := learningTraceFixture(t,
		session.TraceEvent{ID: "start-a", Kind: "tool_start", ToolName: "Skill", ToolUseID: "reused-provider-id", TraceCallID: "call-a", TraceInvocationID: "invoke-a", Text: `{"action":"invoke","name":"test-skill"}`},
		session.TraceEvent{ID: "start-b", Kind: "tool_start", ToolName: "Skill", ToolUseID: "reused-provider-id", TraceCallID: "call-b", TraceInvocationID: "invoke-b", Text: `{"action":"invoke","name":"test-skill"}`},
		session.TraceEvent{ID: "result-b", Kind: "tool_result", ToolName: "Skill", ToolUseID: "reused-provider-id", TraceCallID: "call-b", TraceInvocationID: "invoke-b", Text: "body-b"},
		session.TraceEvent{ID: "tokens-a", Kind: "tokens", TraceInvocationID: "invoke-a", Text: "input=3 output=4 cache_write=5 cache_read=6"},
		session.TraceEvent{ID: "result-a", Kind: "tool_result", ToolName: "Skill", ToolUseID: "reused-provider-id", TraceCallID: "call-a", TraceInvocationID: "invoke-a", Text: "body-a"},
		session.TraceEvent{ID: "duplicate-result", Kind: "tool_result", ToolName: "Skill", TraceCallID: "call-a", Text: "body-a"},
	)
	got, err := readLearningTrace(path, name)
	if err != nil {
		t.Fatal(err)
	}
	if got.TraceSHA256 != learning.Digest(data) || !reflect.DeepEqual(got.TokenEventIDs, []string{"tokens-a"}) || len(got.SkillInvocations) != 2 {
		t.Fatalf("trace evidence=%#v", got)
	}
	for i, want := range []struct{ call, id, body string }{{"call-b", "invoke-b", "body-b"}, {"call-a", "invoke-a", "body-a"}} {
		call := got.SkillInvocations[i]
		if call.CallID != want.call || call.InvocationID != want.id || call.Result != want.body || call.ResultSHA256 != learning.Digest([]byte(want.body)) {
			t.Fatalf("paired wrong call: %#v want %#v", call, want)
		}
	}
}

func TestReadLearningTraceRejectsUnsuccessfulOrUnmatchedSkillCalls(t *testing.T) {
	start := session.TraceEvent{ID: "start", Kind: "tool_start", ToolName: "Skill", ToolUseID: "tool-1", TraceCallID: "call-1", TraceInvocationID: "invoke-1", Text: `{"action":"invoke","name":"test-skill"}`}
	result := session.TraceEvent{ID: "result", Kind: "tool_result", ToolName: "Skill", ToolUseID: "tool-1", TraceCallID: "call-1", TraceInvocationID: "invoke-1", Text: "body"}
	for _, tc := range []struct {
		name   string
		mutate func(*session.TraceEvent, *session.TraceEvent)
	}{
		{"wrong-skill", func(s, r *session.TraceEvent) { s.Text = `{"action":"invoke","name":"other-skill"}` }},
		{"get-not-invoke", func(s, r *session.TraceEvent) { s.Text = `{"action":"get","name":"test-skill"}` }},
		{"invalid-arguments", func(s, r *session.TraceEvent) { s.Text = `{` }},
		{"missing-call-id", func(s, r *session.TraceEvent) { s.TraceCallID = ""; r.TraceCallID = "" }},
		{"error-result", func(s, r *session.TraceEvent) { r.IsError = true }},
		{"foreign-call-id", func(s, r *session.TraceEvent) { r.TraceCallID = "call-2" }},
		{"foreign-tool-result", func(s, r *session.TraceEvent) { r.ToolName = "Read" }},
		{"result-before-start", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := start, result
			if tc.mutate != nil {
				tc.mutate(&s, &r)
			}
			events := []session.TraceEvent{s, r}
			if tc.name == "result-before-start" {
				events = []session.TraceEvent{r, s}
			}
			path, _ := learningTraceFixture(t, events...)
			got, err := readLearningTrace(path, "test-skill")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.SkillInvocations) != 0 {
				t.Fatalf("accepted invalid invocation: %#v", got.SkillInvocations)
			}
		})
	}
	if _, err := readLearningTrace(learningFixtureFile(t, "not-json"), "test-skill"); err == nil {
		t.Fatal("accepted corrupt trace")
	}
}

func TestLearningChildEnvironmentPinsPrivateQueryInputs(t *testing.T) {
	parent := []string{"PATH=/bin", "HOME=/host/home", "METIS_HOME=/host/metis", "METIS_API_KEY=host-secret", "METIS_SIMPLE=1", "METIS_RUN_CACHE=1", "METIS_LEARNING_QUERY_API_KEY=old-secret", "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.test", "OTEL_RESOURCE_ATTRIBUTES=host-data", "HTTPS_PROXY=http://proxy.test", "LANG=en_US.UTF-8"}
	original := append([]string(nil), parent...)
	home := filepath.Join(t.TempDir(), "metis")
	got := learningChildEnvironment(parent, home, "private-query-key")
	values := map[string]string{}
	for _, entry := range got {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed env: %q", entry)
		}
		if _, duplicate := values[name]; duplicate {
			t.Fatalf("duplicate env: %s", name)
		}
		values[name] = value
	}
	for name, want := range map[string]string{"HOME": filepath.Dir(home), "METIS_HOME": home, "METIS_LEARNING_QUERY_API_KEY": "private-query-key", "METIS_AUTO_MEMORY": "0", "METIS_NO_UPDATE_CHECK": "1", "ENABLE_TOOL_SEARCH": "false", "PATH": "/bin", "HTTPS_PROXY": "http://proxy.test", "LANG": "en_US.UTF-8"} {
		if values[name] != want {
			t.Fatalf("env %s=%q want %q", name, values[name], want)
		}
	}
	for _, name := range []string{"METIS_API_KEY", "METIS_SIMPLE", "METIS_RUN_CACHE", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_RESOURCE_ATTRIBUTES"} {
		if _, ok := values[name]; ok {
			t.Fatalf("inherited %s", name)
		}
	}
	if !reflect.DeepEqual(parent, original) {
		t.Fatal("environment sanitization mutated parent")
	}
}

func TestLearningProviderProfileDigestBindsRouteAndGenerationPolicy(t *testing.T) {
	cfg := &config.Config{Provider: config.ProviderSet{Default: "openai", OpenAI: config.ProviderOpenAI{BaseURL: "https://first.invalid/v1", Model: "unit", MaxTokens: 4096, APIKey: "one-secret"}}}
	one, _, err := learningProviderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest := learningProfileDigest(one)
	cfg.Provider.OpenAI.APIKey = "different-secret"
	same, _, _ := learningProviderConfig(cfg)
	if learningProfileDigest(same) != digest {
		t.Fatal("secret entered published route digest")
	}
	cfg.Provider.OpenAI.BaseURL = "https://second.invalid/v1"
	route, _, _ := learningProviderConfig(cfg)
	if learningProfileDigest(route) == digest {
		t.Fatal("endpoint change reused evaluator identity")
	}
	cfg.Provider.OpenAI.BaseURL = "https://first.invalid/v1"
	cfg.Provider.OpenAI.MaxTokens = 8192
	budget, _, _ := learningProviderConfig(cfg)
	if learningProfileDigest(budget) == digest {
		t.Fatal("generation policy change reused evaluator identity")
	}
}
