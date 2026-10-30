package main

import (
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

// agentContinuationRuntime resolves a child's saved provider identity using
// current credentials, without putting credentials into its transcript. Live
// dial callbacks keep newly spawned children aligned with /effort and /fast.
func agentContinuationRuntime(cfg *config.Config, loop *agent.Loop) builtin.AgentRuntimePromptState {
	return builtin.AgentRuntimePromptState{
		ResolveProvider: func(name, model string) (llm.Provider, error) {
			built, err := rtpkg.BuildProviderWithoutPreconnect(cfg, name, model)
			if err != nil {
				return nil, err
			}
			return built.Provider, nil
		},
		Effort: loop.EffortValue,
		Fast:   loop.FastEnabled,
	}
}
