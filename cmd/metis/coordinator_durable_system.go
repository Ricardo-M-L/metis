package main

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

const coordinatorDurableSystemName = "durable_coordinator_execution"
const coordinatorDurableSystemBody = "This worker uses synchronous durable operation receipts. Run every Bash command and finite test in the foreground; omit run_in_background, await_completion and required_for_completion or set them to false. The general background-job and finite-background-check instructions do not apply here. Use timeout_ms for a bounded command. Never replace an uncertain external action with rewritten arguments or another tool; the host must resolve its receipt."

// Anthropic uses the typed sections when present. Updating only Loop.System
// would mark the policy locally while omitting it from the actual request.
func applyCoordinatorDurableSystem(loop *agent.Loop) {
	if loop == nil {
		return
	}
	if !strings.Contains(loop.System, coordinatorDurableSystemBody) {
		loop.System += "\n\n<" + coordinatorDurableSystemName + ">\n" + coordinatorDurableSystemBody + "\n</" + coordinatorDurableSystemName + ">"
	}
	if len(loop.SystemSections) == 0 {
		return
	}
	for _, section := range loop.SystemSections {
		if section.Name == coordinatorDurableSystemName && section.Body == coordinatorDurableSystemBody {
			return
		}
	}
	loop.SystemSections = append(append([]llm.SystemSection(nil), loop.SystemSections...), llm.SystemSection{Name: coordinatorDurableSystemName, Body: coordinatorDurableSystemBody, Cache: true})
}
