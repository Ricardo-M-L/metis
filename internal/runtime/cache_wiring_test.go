package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestBuildAgentLoopWiresCacheDiagnostics(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(builtin.NewMetisInfo(nil, nil, nil, nil, reg))
	cfg := &config.Config{}
	p := openai.NewResponses("", "https://api.openai.com/v1", "fixture", 64, 0, 0)
	loop := BuildAgentLoop(cfg, AgentLoopOptions{Provider: p, Registry: reg, Model: "fixture"})
	loop.CacheStats.Add(agent.CacheStat{CacheRead: 900, Input: 100})
	tool, _ := reg.Get("MetisInfo")
	result, err := tool.Execute(context.Background(), map[string]any{"section": "cache"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, "actual_cache_read = 900") {
		t.Fatal("runtime did not attach live cache ring")
	}
}
