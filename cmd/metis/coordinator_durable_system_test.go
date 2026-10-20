package main

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCoordinatorPolicyReachesBothStringAndActualTypedProviderSystem(t *testing.T) {
	for _, typed := range []bool{false, true} {
		loop := &agent.Loop{System: "CUSTOM_ROLE_SENTINEL"}
		if typed {
			loop.SystemSections = []llm.SystemSection{{Name: "base", Body: "CUSTOM_ROLE_SENTINEL", Cache: true}}
		}
		applyCoordinatorDurableSystem(loop)
		applyCoordinatorDurableSystem(loop)
		if strings.Count(loop.System, coordinatorDurableSystemBody) != 1 || !strings.Contains(loop.System, "CUSTOM_ROLE_SENTINEL") {
			t.Fatal("string system lost role or duplicated durability policy")
		}
		if typed {
			if len(loop.SystemSections) != 2 || loop.SystemSections[0].Body != "CUSTOM_ROLE_SENTINEL" || loop.SystemSections[1].Body != coordinatorDurableSystemBody || !loop.SystemSections[1].Cache {
				t.Fatalf("provider sections omit policy or overwrite profile: %+v", loop.SystemSections)
			}
		} else if len(loop.SystemSections) != 0 {
			t.Fatal("string-only provider was switched to partial typed system")
		}
	}
}
