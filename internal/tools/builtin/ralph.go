package builtin

// ralph.go — fresh-agent iterative execution loop (DSH ralph parity).
//
// Ralph runs N rounds toward ONE immutable objective. Each round opens
// a brand-new child agent with NO parent conversation and NO prior
// child session — the only things that cross rounds are:
//
//   1. the shared workspace (cwd) — files the children write are the
//      long-term memory;
//   2. one bounded structured report file per round under
//      .metis/ralph/<run>/round-N.md.
//
// A round ends when its child returns. The next round's child sees the
// previous report verbatim (plus the objective) and nothing else. The
// loop stops when a child's report declares `status: complete` (or
// `status: blocked <reason>`), or when maxRounds is exhausted.
//
// The tool returns a run summary: rounds executed, final status, and
// the tail of the last report.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const (
	ralphDefaultRounds = 5
	ralphMaxRounds     = 20
	ralphReportCap     = 400 // lines a report is capped to in the next round's prompt
)

// Ralph is the LLM-facing fresh-agent loop tool.
type Ralph struct {
	tools.BaseTool
	gate     *permission.Gate
	provider llm.Provider
	registry *tools.Registry
	model    string
	system   string

	// spawn runs one round. Swappable so tests can drive the loop
	// without a real provider. The default builds a COLD Agent with
	// the same deps — every call is a fresh history by construction.
	spawn func(ctx context.Context, prompt string) (string, error)
}

// NewRalph builds the tool with the default (fresh-Agent) spawner.
func NewRalph(gate *permission.Gate, prov llm.Provider, reg *tools.Registry, model, system string) *Ralph {
	r := &Ralph{gate: gate, provider: prov, registry: reg, model: model, system: system}
	r.spawn = func(ctx context.Context, prompt string) (string, error) {
		child := Agent{gate: gate, provider: prov, registry: reg, model: model, system: system}
		res, err := child.Execute(ctx, map[string]any{"prompt": prompt})
		if err != nil {
			return "", err
		}
		return res.Output, nil
	}
	return r
}

// WithSpawner replaces the round runner (tests).
func (r *Ralph) WithSpawner(fn func(ctx context.Context, prompt string) (string, error)) *Ralph {
	r.spawn = fn
	return r
}

func (Ralph) Name() string { return "Ralph" }

func (Ralph) Description() string {
	return `Run a fresh-agent iterative loop toward ONE immutable objective.

Each round spawns a brand-new agent with no memory of this conversation or previous rounds. The ONLY things that survive across rounds: (1) the shared workspace — files children write are the long-term memory; (2) one bounded structured report per round, written to .metis/ralph/<run>/round-N.md.

The loop stops when a round's report declares "status: complete" (objective achieved) or "status: blocked" (concrete blocker), or when maxRounds is exhausted.

Use ONLY when the user explicitly asks for Ralph / fresh-agent iteration. Ordinary long-running work in ONE session does not need this.`
}

func (Ralph) ShortDescription() string {
	return `Iterative fresh-agent loop toward one immutable objective (workspace = memory, one bounded report per round). Use only when the user explicitly asks for Ralph-style iteration.`
}

func (Ralph) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"objective"},
		"properties": map[string]any{
			"objective": map[string]any{
				"type":        "string",
				"description": "The immutable completion objective — identical for every round.",
			},
			"maxRounds": map[string]any{
				"type":        "integer",
				"description": "Round cap (default 5, max 20).",
			},
		},
	}
}

func (r Ralph) IsEnabled() bool {
	return r.gate != nil && r.provider != nil && r.registry != nil
}

func (Ralph) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}

func (Ralph) Concurrency(map[string]any) tools.Concurrency {
	// The loop serializes whole child runs — exclusive keeps two Ralphs
	// from thrashing the same workspace simultaneously.
	return tools.ConcurrencyExclusive
}

func (r Ralph) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	objective, _ := in["objective"].(string)
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return &tools.Result{Output: "Ralph: `objective` is required.", IsError: true}, nil
	}
	if !r.IsEnabled() || r.spawn == nil {
		return &tools.Result{Output: "Ralph: not fully wired (missing provider/registry/gate).", IsError: true}, nil
	}
	maxRounds := ralphDefaultRounds
	switch v := in["maxRounds"].(type) {
	case float64:
		if int(v) > 0 {
			maxRounds = int(v)
		}
	case int:
		if v > 0 {
			maxRounds = v
		}
	case int64:
		if v > 0 {
			maxRounds = int(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			maxRounds = int(n)
		}
	}
	if maxRounds > ralphMaxRounds {
		maxRounds = ralphMaxRounds
	}

	// Run dir keyed by objective hash so a resumed objective reuses its
	// reports; the hash keeps directory names filesystem-safe.
	sum := sha256.Sum256([]byte(objective))
	runDir := filepath.Join(".metis", "ralph", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return &tools.Result{Output: "Ralph: cannot create run dir: " + err.Error(), IsError: true}, nil
	}
	agent.TraceInvocationStarted(ctx)
	defer agent.TraceInvocationEnded(ctx)

	type roundLog struct {
		Round  int    `json:"round"`
		Status string `json:"status"`
		At     string `json:"at"`
	}
	var rounds []roundLog
	finalStatus := "max_rounds"
	var lastReport string
	var stopReason string

	for round := 1; round <= maxRounds; round++ {
		prev := ""
		if round > 1 {
			b, err := os.ReadFile(filepath.Join(runDir, fmt.Sprintf("round-%d.md", round-1)))
			if err == nil {
				prev = capLines(string(b), ralphReportCap)
			}
		}
		reportPath := filepath.Join(runDir, fmt.Sprintf("round-%d.md", round))
		prompt := buildRalphPrompt(objective, round, maxRounds, prev, reportPath)

		if _, err := r.spawn(ctx, prompt); err != nil {
			rounds = append(rounds, roundLog{Round: round, Status: "error", At: nowStamp()})
			finalStatus = "error"
			stopReason = err.Error()
			lastReport = ""
			break
		}
		b, err := os.ReadFile(reportPath)
		if err != nil {
			// Child finished but wrote no report — record and stop; a
			// reportless round means the protocol broke, more rounds
			// would just repeat it.
			rounds = append(rounds, roundLog{Round: round, Status: "no_report", At: nowStamp()})
			finalStatus = "no_report"
			stopReason = "round " + strconv.Itoa(round) + " wrote no report file"
			break
		}
		lastReport = string(b)
		status := ralphReportStatus(lastReport)
		rounds = append(rounds, roundLog{Round: round, Status: status, At: nowStamp()})
		if status == "complete" || status == "blocked" {
			finalStatus = status
			break
		}
	}

	// Persist the run ledger next to the reports.
	ledger, _ := json.MarshalIndent(map[string]any{
		"objective": objective,
		"status":    finalStatus,
		"rounds":    rounds,
		"stopped":   nowStamp(),
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(runDir, "state.json"), ledger, 0o644)

	var b strings.Builder
	fmt.Fprintf(&b, "Ralph run %s — status: %s, rounds: %d/%d\n",
		filepath.Base(runDir), finalStatus, len(rounds), maxRounds)
	for _, rl := range rounds {
		fmt.Fprintf(&b, "  round %d: %s\n", rl.Round, rl.Status)
	}
	if stopReason != "" {
		b.WriteString("stop: " + stopReason + "\n")
	}
	b.WriteString("last report (" + filepath.Join(runDir, fmt.Sprintf("round-%d.md", len(rounds))) + "):\n")
	b.WriteString(capLines(lastReport, 80))
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// buildRalphPrompt renders the per-round child instruction. The report
// contract (first line status) is what the loop parses — keep it in
// sync with ralphReportStatus.
func buildRalphPrompt(objective string, round, maxRounds int, prevReport, reportPath string) string {
	var b strings.Builder
	b.WriteString("You are round " + strconv.Itoa(round) + " of " + strconv.Itoa(maxRounds) + " in a fresh-agent loop. You have NO conversation history — a previous agent worked on this before you; everything it left is in the workspace and (below) its final report.\n\n")
	b.WriteString("OBJECTIVE (immutable — do not reinterpret, do not shrink):\n" + objective + "\n\n")
	if prevReport != "" {
		b.WriteString("PREVIOUS ROUND REPORT (verbatim, the ONLY context you inherit):\n---\n" + prevReport + "\n---\n\n")
	}
	b.WriteString("MANDATE:\n")
	b.WriteString("1. Verify what is already done (read the workspace; trust files over the report).\n")
	b.WriteString("2. Make concrete progress toward the objective this round.\n")
	b.WriteString("3. BEFORE finishing, write your report to " + reportPath + " with EXACTLY this first line:\n")
	b.WriteString("   status: complete   (objective fully achieved and verified)\n")
	b.WriteString("   status: blocked    (concrete blocker you cannot clear — explain on the next line)\n")
	b.WriteString("   status: progress   (more rounds needed)\n")
	b.WriteString("   Then: what you did, what you verified (with evidence), what remains, advice for the next round. ≤" + strconv.Itoa(ralphReportCap) + " lines.\n")
	return b.String()
}

// ralphReportStatus parses the report's first-line status contract.
func ralphReportStatus(report string) string {
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(strings.ToLower(line))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "status:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			switch {
			case strings.HasPrefix(v, "complete"):
				return "complete"
			case strings.HasPrefix(v, "blocked"):
				return "blocked"
			default:
				return "progress"
			}
		}
		return "progress" // first non-empty line isn't a status line
	}
	return "progress"
}

func capLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n… (+" + strconv.Itoa(len(lines)-n) + " more lines)"
}

func nowStamp() string { return time.Now().Format(time.RFC3339) }
