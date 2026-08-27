package main

// mcp_serve.go — "metis mcp-serve": run Metis as an MCP server over stdio.
//
// Register once in Claude Code:
//
//	claude mcp add metis -- metis mcp-serve
//
// After that, Claude Code sees a "run_task" tool it can delegate to Metis.
// Each call runs a fresh agent loop (same semantics as `metis run`).
//
// Protocol: MCP 2024-11-05, JSON-RPC 2.0 over stdin/stdout.
// One JSON object per line (NDJSON).
//
// Fixes applied vs v1:
//  1. EventPermissionRequest/AskUser — auto-resolved so non-bypass modes
//     never deadlock the event loop.
//  2. Per-call wall-clock timeout — matches cmdRun's 30-min cap
//     (METIS_RUN_MAX_SECONDS env override).
//  3. Heartbeat goroutine — setupRuntime receives the per-call context so
//     the heartbeat is reaped when the call finishes, not at process exit.
//  4. Process-wide singletons — run_task owns one process-wide runtime
//     window at a time, preventing concurrent setup/Cleanup from replacing
//     another call's trace hook, adapter, or tool trace store.
//  5. notifications/cancelled — scanner runs in a goroutine; per-call
//     cancel map lets incoming cancellations reach in-flight loop.Run.
//  6. req.ID null check — mcpIDPresent handles json.RawMessage("null").
//  7. noAuthWizard — always true in MCP mode; never write to stdout.
//  8. Flag parsing — delegates to parseFlags; --provider/--model/--mode
//     at the CLI level are all honoured.
//  9. isError:false — omitted from success responses (omitempty handles it).
// 10. Known limitation: setupRuntime is called per-call (full cold-boot
//     cost). Proper fix requires a session-reset primitive; tracked TODO.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"bufio"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/version"
)

const mcpProtoVersion = "2024-11-05"

// mcpRuntimeRunGate serializes the part of run_task that owns process-wide
// runtime wiring. setupRuntime installs global trace hooks/adapters and
// Cleanup flushes through those same globals, so overlapping runtimes would
// replace or flush one another's trace. The stdio scanner and all non-run MCP
// methods remain concurrent; only setupRuntime -> loop.Run -> Cleanup is gated.
//
// A channel is used instead of sync.Mutex so notifications/cancelled can stop
// a call while it is queued behind another long-running task.
type mcpRuntimeRunGate struct {
	token chan struct{}
}

func newMCPRuntimeRunGate() *mcpRuntimeRunGate {
	return &mcpRuntimeRunGate{token: make(chan struct{}, 1)}
}

func (g *mcpRuntimeRunGate) run(ctx context.Context, fn func() (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case g.token <- struct{}{}:
		defer func() { <-g.token }()
		// Cancellation and gate release can become ready together. Recheck so a
		// cancelled queued request never boots a runtime after winning the select.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return fn()
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var mcpRuntimeRuns = newMCPRuntimeRunGate()

// mcpMsg is a single JSON-RPC 2.0 envelope used for both inbound requests
// and outbound responses. Fields not relevant to the direction are zero-valued
// and suppressed by omitempty.
type mcpMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpMsgError    `json:"error,omitempty"`
}

type mcpMsgError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpIDPresent reports whether the message carries a real request ID.
// json.RawMessage is []byte; "id":null unmarshals to []byte("null") — a
// non-nil slice — so we must check the string value, not just nil.
func mcpIDPresent(id json.RawMessage) bool {
	return len(id) > 0 && string(id) != "null"
}

// mcpRunCap returns the per-call wall-clock limit, mirroring cmdRun.
func mcpRunCap() time.Duration {
	if s := os.Getenv("METIS_RUN_MAX_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 30 * time.Minute
}

func cmdMCPServe(ctx context.Context, args []string) error {
	// Use parseFlags so --provider, --model, --mode, --bare, etc. all work.
	flags, _, err := parseFlags(args)
	if err != nil {
		return fmt.Errorf("mcp-serve: %w", err)
	}
	if flags.mode == "" {
		// Automated callers cannot respond to permission prompts.
		flags.mode = string(permission.ModeBypassPermissions)
	}
	// Never show the interactive auth wizard on the MCP stdio transport —
	// any wizard output written to stdout would corrupt the JSON-RPC stream.
	flags.noAuthWizard = true

	// Mutex-protected encoder: tools/call responses are sent from goroutines.
	var outMu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	send := func(id json.RawMessage, result any, mcpErr *mcpMsgError) {
		outMu.Lock()
		defer outMu.Unlock()
		_ = enc.Encode(mcpMsg{JSONRPC: "2.0", ID: id, Result: result, Error: mcpErr})
	}
	ok := func(id json.RawMessage, result any) { send(id, result, nil) }
	fail := func(id json.RawMessage, code int, msg string) {
		send(id, nil, &mcpMsgError{Code: code, Message: msg})
	}

	// Scanner goroutine: reads stdin continuously so notifications/cancelled
	// can arrive while a tools/call goroutine is executing.
	scanCh := make(chan mcpMsg, 8)
	go func() {
		defer close(scanCh)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var msg mcpMsg
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			select {
			case scanCh <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	// activeCancels tracks in-flight tools/call requests by JSON-encoded ID
	// so notifications/cancelled can reach the right goroutine.
	var activeMu sync.Mutex
	activeCancels := map[string]context.CancelFunc{}

	for {
		select {
		case <-ctx.Done():
			return nil

		case req, open := <-scanCh:
			if !open {
				return nil // stdin closed — client disconnected
			}

			switch req.Method {
			case "initialize":
				ok(req.ID, map[string]any{
					"protocolVersion": mcpProtoVersion,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "metis", "version": version.Version},
				})

			case "notifications/initialized":
				// notification — no response required

			case "notifications/cancelled":
				// Cancel the matching in-flight call if it's still running.
				// Use json.RawMessage for RequestID so integer IDs (e.g. 1)
				// and string IDs both produce the same raw bytes we stored in
				// activeCancels via string(req.ID) — comparing decoded strings
				// would mismatch because string(req.ID)="1" != decoded "1".
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				_ = json.Unmarshal(req.Params, &p)
				activeMu.Lock()
				if cancel, exists := activeCancels[string(p.RequestID)]; exists {
					cancel()
				}
				activeMu.Unlock()

			case "ping":
				ok(req.ID, map[string]any{})

			case "tools/list":
				ok(req.ID, map[string]any{"tools": []any{mcpRunTaskSchema()}})

			case "tools/call":
				var p struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				}
				if err := json.Unmarshal(req.Params, &p); err != nil {
					fail(req.ID, -32602, "invalid params: "+err.Error())
					continue
				}
				if p.Name != "run_task" {
					fail(req.ID, -32602, "unknown tool: "+p.Name)
					continue
				}
				prompt, _ := p.Arguments["prompt"].(string)
				if prompt == "" {
					fail(req.ID, -32602, "prompt is required")
					continue
				}
				callProvider, _ := p.Arguments["provider"].(string)
				callModel, _ := p.Arguments["model"].(string)

				// Per-call context wires together:
				//   — wall-clock timeout (matches cmdRun's 30 min cap)
				//   — notifications/cancelled (via activeCancels map)
				//   — heartbeat goroutine lifetime (setupRuntime receives this ctx)
				callCtx, callCancel := context.WithTimeout(ctx, mcpRunCap())
				reqIDStr := string(req.ID)
				activeMu.Lock()
				activeCancels[reqIDStr] = callCancel
				activeMu.Unlock()

				// Run in a goroutine so the scanner loop stays live and can
				// receive notifications/cancelled during a long agent run.
				go func(req mcpMsg, callCtx context.Context, reqIDStr string) {
					defer func() {
						callCancel()
						activeMu.Lock()
						delete(activeCancels, reqIDStr)
						activeMu.Unlock()
					}()

					// Shallow-copy flags and apply per-call overrides.
					callFlags := *flags
					if callProvider != "" {
						callFlags.provider = callProvider
					}
					if callModel != "" {
						callFlags.model = callModel
					}

					text, runErr := mcpDoRunTask(callCtx, &callFlags, prompt)
					if runErr != nil {
						ok(req.ID, map[string]any{
							"content": []any{map[string]any{"type": "text", "text": runErr.Error()}},
							"isError": true,
						})
						return
					}
					// isError omitted on success (omitempty on Result handles it)
					ok(req.ID, map[string]any{
						"content": []any{map[string]any{"type": "text", "text": text}},
					})
				}(req, callCtx, reqIDStr)

			default:
				// Only respond to requests (have a real ID), not notifications.
				if mcpIDPresent(req.ID) {
					fail(req.ID, -32601, "method not found: "+req.Method)
				}
			}
		}
	}
}

// mcpRunTaskSchema returns the JSON Schema for the run_task tool.
func mcpRunTaskSchema() map[string]any {
	return map[string]any{
		"name":        "run_task",
		"description": "Delegate a task to the Metis agent. Metis runs its full agent loop (reads files, executes tools, reasons) and returns the final response text.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task or question for Metis to handle.",
				},
				"provider": map[string]any{
					"type":        "string",
					"description": "LLM provider override (e.g. deepseek, kimi, minimax). Uses Metis default if omitted.",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Model name override. Uses the provider's default model if omitted.",
				},
			},
			"required": []string{"prompt"},
		},
	}
}

// mcpDoRunTask runs prompt through a fresh agent loop and returns the
// collected assistant text. callCtx must carry a timeout and will be
// cancelled by the caller when the call is done (heartbeat cleanup).
//
// TODO: replace per-call setupRuntime with a single init + loop.Reset()
// between calls to avoid the full cold-boot cost on every invocation.
func mcpDoRunTask(callCtx context.Context, flags *cliFlags, prompt string) (string, error) {
	return mcpRuntimeRuns.run(callCtx, func() (string, error) {
		return mcpDoRunTaskExclusive(callCtx, flags, prompt)
	})
}

// mcpDoRunTaskExclusive owns the process-wide runtime wiring for the duration
// of one MCP call. Call only through mcpRuntimeRuns.
func mcpDoRunTaskExclusive(callCtx context.Context, flags *cliFlags, prompt string) (string, error) {
	// setupRuntime receives callCtx so the heartbeat goroutine it spawns
	// is reaped when callCtx is cancelled (at call end), not at process exit.
	rt, err := setupRuntime(callCtx, flags)
	if err != nil {
		return "", fmt.Errorf("metis mcp-serve: setup: %w", err)
	}
	defer rt.Cleanup()

	if alive := rt.WaitForMCP(15 * time.Second); !alive {
		fmt.Fprintln(os.Stderr, "metis mcp-serve: MCP servers still loading after 15s — continuing")
	}

	rt.loop.AppendUser(prompt)

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- rtpkg.RunWithTraceTurn(callCtx, rt.sessionID, func(turnCtx context.Context) error {
			return rt.loop.Run(turnCtx, events)
		})
		close(events)
	}()

	var sb strings.Builder
	for ev := range events {
		switch ev.Kind {
		case agent.EventTextDelta:
			sb.WriteString(ev.TextDelta)

		case agent.EventPermissionRequest:
			// No interactive approval is possible in MCP server context.
			// Auto-deny to unblock the loop. Under the default bypass mode
			// this branch is never reached; it guards explicit --mode ask/acceptEdits.
			ev.PermissionReply <- agent.PermissionDecisionDeny

		case agent.EventAskUser:
			// Return empty answer so AskUser tool completes without hanging.
			if ev.AskUserReply != nil {
				ev.AskUserReply <- ""
			}
		}
	}

	if err := <-done; err != nil {
		return "", err
	}
	// Each MCP request owns a fresh serialized runtime. Scope the durability
	// barrier to that request's session ID so it can never join or flush a
	// different request if the process serves multiple calls over its lifetime.
	if err := rt.persistHeadlessMemoryBoundary("metis mcp-serve run_task", runtimeDistillationShutdownGrace); err != nil {
		return "", err
	}
	return strings.TrimSpace(sb.String()), nil
}
