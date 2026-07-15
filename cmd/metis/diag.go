package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	"github.com/Ricardo-M-L/metis/internal/version"
	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// cmdDiag — non-interactive health check pipeline. Designed to run on a
// freshly-deployed server: no user prompts, no LLM calls unless
// explicitly requested.
//
//	metis diag                 # local checks only
//	metis diag --llm           # also fire a 1-token LLM ping
//	metis diag --tool-smoke    # also exercise Bash + Read tools locally
//	metis diag --json          # one JSON object instead of text
//
// Exit codes: 0 ok / 1 mandatory check failed / 2 flag parse error.
func cmdDiag(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diag", flag.ContinueOnError)
	llmPing := fs.Bool("llm", false, "fire a one-token LLM round-trip (requires API key + network)")
	toolSmoke := fs.Bool("tool-smoke", false, "exercise Bash + Read tools to verify execution paths")
	jsonOut := fs.Bool("json", false, "emit a single JSON object instead of human text")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: metis diag [--llm] [--tool-smoke] [--json]

Health-check pipeline. Default = local checks only (no network, no LLM).

Flags:
  --llm         also fire a 1-token LLM round-trip
  --tool-smoke  also run Bash + Read on a temp file
  --json        machine-readable output

Exit codes: 0 ok / 1 hard fail / 2 flag error`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := newDiag()

	// --- environment -------------------------------------------------------
	d.section("environment")
	d.ok("metis version", version.Version)
	d.ok("go runtime", fmt.Sprintf("%s %s/%s", goruntime.Version(), goruntime.GOOS, goruntime.GOARCH))
	if cwd, err := os.Getwd(); err == nil {
		d.ok("cwd", cwd)
	}
	d.ok("metis home", config.Home())

	// --- config + provider key --------------------------------------------
	d.section("config")
	cfg, loaded, err := config.Load()
	if err != nil {
		d.fail("load config", err.Error())
	} else {
		d.ok("config files", strings.Join(loaded, ", "))
		provName := cfg.Provider.Default
		if provName == "" {
			provName = "anthropic"
		}
		d.ok("provider.default", provName)
		keyEnv, model := providerKeyAndModel(cfg, provName)
		if model != "" {
			d.ok("model", model)
		}
		switch {
		case keyEnv == "":
			d.warn("api_key_env", "no api_key_env set in config — provider may use literal key")
		case os.Getenv(keyEnv) == "":
			d.fail(keyEnv, "env var unset — provider will fail to authenticate")
		default:
			v := os.Getenv(keyEnv)
			d.ok(keyEnv, fmt.Sprintf("set, %d chars (%s…)", len(v), safePrefix(v, 6)))
		}
	}

	// --- filesystem --------------------------------------------------------
	d.section("filesystem")
	for _, sub := range []string{"sessions", "skills", "memories", "agents", "cache"} {
		p := filepath.Join(config.Home(), sub)
		st, err := os.Stat(p)
		switch {
		case err != nil:
			d.warn(p, "missing — created on first chat")
		case !st.IsDir():
			d.fail(p, "exists but not a directory")
		default:
			tmp := filepath.Join(p, ".diag-write-probe")
			if e := os.WriteFile(tmp, []byte("ok"), 0o644); e != nil {
				d.fail(p, "not writable: "+e.Error())
			} else {
				_ = os.Remove(tmp)
				d.ok(p, "writable")
			}
		}
	}

	// --- tool registry -----------------------------------------------------
	d.section("tools")
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeAcceptEdits)
	if cfg != nil {
		builtin.Register(reg, cfg, gate)
	} else {
		// cfg load failed earlier; build with a zero config so we can
		// still verify tool registration regressions.
		builtin.Register(reg, &config.Config{}, gate)
	}
	all := reg.All()
	d.ok("registered tools", fmt.Sprintf("%d", len(all)))
	if len(all) < 5 {
		d.fail("tool registry", fmt.Sprintf("only %d tools — likely registration regression", len(all)))
	}

	// --- websearch backends ------------------------------------------------
	// Report which of the 4 ordered backends are currently available
	// so users discover the env-var path to richer results. None of
	// the surveyed open-source agent CLIs (claude-code, hermes,
	// crush, deepseek-tui) currently expose this kind of "which key
	// would help" view; we add it because metis defaults to the
	// zero-config DDG floor and most users never realise they can
	// upgrade for free.
	d.section("websearch backends")
	d.reportWebSearchBackends()

	// --- optional: tool smoke ---------------------------------------------
	if *toolSmoke {
		d.section("tool-smoke")
		if e := smokeBashTool(ctx, reg); e != nil {
			d.fail("Bash echo", e.Error())
		} else {
			d.ok("Bash echo", "stdout matched")
		}
		if e := smokeReadTool(ctx, reg); e != nil {
			d.fail("Read tmp file", e.Error())
		} else {
			d.ok("Read tmp file", "first line matched")
		}
	}

	// --- optional: LLM ping ------------------------------------------------
	if *llmPing {
		d.section("llm")
		switch {
		case cfg == nil:
			d.fail("llm ping", "config not loaded")
		default:
			status, msg := pingLLM(ctx, cfg)
			switch status {
			case "ok":
				d.ok("llm ping", msg)
			case "warn":
				d.warn("llm ping", msg)
			default:
				d.fail("llm ping", msg)
			}
		}
	}

	if *jsonOut {
		fmt.Println(d.json())
	} else {
		fmt.Print(d.text())
	}
	if d.hasFail() {
		return errors.New("diag: one or more mandatory checks failed")
	}
	return nil
}

// providerKeyAndModel pulls (api_key_env, model) for the named provider.
func providerKeyAndModel(cfg *config.Config, name string) (string, string) {
	if cfg == nil {
		return "", ""
	}
	switch name {
	case "anthropic":
		return cfg.Provider.Anthropic.APIKeyEnv, cfg.Provider.Anthropic.Model
	case "openai":
		return cfg.Provider.OpenAI.APIKeyEnv, cfg.Provider.OpenAI.Model
	case "gemini":
		return cfg.Provider.Gemini.APIKeyEnv, cfg.Provider.Gemini.Model
	}
	if raw, ok := cfg.Provider.Custom[name]; ok {
		return raw.APIKeyEnv, raw.Model
	}
	return "", ""
}

func smokeBashTool(ctx context.Context, reg *tools.Registry) error {
	t, ok := reg.Get("Bash")
	if !ok {
		return errors.New("Bash tool not registered")
	}
	want := "metis-diag-bash-smoke"
	res, err := t.Execute(ctx, map[string]any{"command": "echo " + want, "timeout_seconds": 5})
	if err != nil {
		return err
	}
	if res == nil || !strings.Contains(res.Output, want) {
		return fmt.Errorf("expected %q in output, got %q", want, outputOrEmpty(res))
	}
	return nil
}

func smokeReadTool(ctx context.Context, reg *tools.Registry) error {
	t, ok := reg.Get("Read")
	if !ok {
		return errors.New("Read tool not registered")
	}
	tmp, err := os.CreateTemp("", "metis-diag-read-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	want := "metis-diag-read-smoke"
	if _, err := tmp.WriteString(want + "\n"); err != nil {
		return err
	}
	tmp.Close()

	res, err := t.Execute(ctx, map[string]any{"path": tmp.Name()})
	if err != nil {
		return err
	}
	if res == nil || !strings.Contains(res.Output, want) {
		return fmt.Errorf("expected %q in output, got %q", want, outputOrEmpty(res))
	}
	return nil
}

func outputOrEmpty(r *tools.Result) string {
	if r == nil {
		return ""
	}
	if len(r.Output) > 200 {
		return r.Output[:200] + "..."
	}
	return r.Output
}

// pingLLM fires a one-shot Complete() with a tiny prompt. Returns:
//
//	("ok",   detail)   — provider returned non-empty content
//	("warn", reason)   — provider replied (no transport error) but
//	                     content was empty / no text block. Treated
//	                     as warn rather than fail because some
//	                     OpenAI-/Anthropic-compatible relays (e.g.
//	                     minimaxi) routinely give back empty bodies
//	                     for very short max_tokens — auth + network
//	                     + parsing all worked, just no payload.
//	("fail", reason)   — transport error / provider build failure
//	                     / explicit non-2xx — real outage signal.
//
// 30s timeout so a slow API doesn't hang diag forever.
func pingLLM(ctx context.Context, cfg *config.Config) (string, string) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	provName := cfg.Provider.Default
	if provName == "" {
		provName = "anthropic"
	}
	pb, err := rtpkg.BuildProvider(cfg, provName, "")
	if err != nil {
		return "fail", fmt.Sprintf("build provider: %v", err)
	}
	resp, err := pb.Provider.Complete(cctx, provider.Request{
		Model:     pb.Model,
		System:    "Reply with exactly: OK",
		MaxTokens: 64,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "ping"}}},
		},
	})
	if err != nil {
		return "fail", err.Error()
	}
	if resp == nil {
		return "warn", "provider returned nil response (auth + network ok, no payload)"
	}
	if len(resp.Content) == 0 {
		return "warn", "provider returned empty content (round-trip ok, no text block)"
	}
	return "ok", "got non-empty completion"
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// -----------------------------------------------------------------------------
// accumulator
// -----------------------------------------------------------------------------

type diagRow struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Detail  string `json:"detail,omitempty"`
}

type diagAcc struct {
	rows    []diagRow
	current string
}

func newDiag() *diagAcc                     { return &diagAcc{current: "general"} }
func (d *diagAcc) section(name string)      { d.current = name }
func (d *diagAcc) ok(name, detail string)   { d.add(name, "ok", detail) }
func (d *diagAcc) warn(name, detail string) { d.add(name, "warn", detail) }
func (d *diagAcc) fail(name, detail string) { d.add(name, "fail", detail) }
func (d *diagAcc) add(name, status, detail string) {
	d.rows = append(d.rows, diagRow{Section: d.current, Name: name, Status: status, Detail: detail})
}
func (d *diagAcc) hasFail() bool {
	for _, r := range d.rows {
		if r.Status == "fail" {
			return true
		}
	}
	return false
}

// reportWebSearchBackends prints the live status of each WebSearch
// backend: ✓ if the gating env var is set (or it's the zero-config
// floor), ✗ + free-tier hint otherwise. We deliberately don't make
// it a fail/warn — WebSearch always works via DDG, the upgrades are
// strictly opt-in quality boosts.
//
// Layout mirrors the cli-web-search "Provider:" status pattern but
// adds the free-tier sign-up nudge so a fresh metis install knows
// exactly how to upgrade without grepping the source. Backend list
// stays in sync with internal/tools/builtin/websearch.go::
// webSearchBackends — the duplicated knowledge is fine here
// because diag is a CLI-side reporting concern that shouldn't
// reach back into internal/ for one slice.
func (d *diagAcc) reportWebSearchBackends() {
	type backendInfo struct {
		name   string // matches webSearchBackends.name
		env    string // gating env var ("" for zero-config)
		tier   string // human-readable free/paid tier
		signup string // one-line "how do I get this?"
	}
	backends := []backendInfo{
		{"tavily", "TAVILY_API_KEY", "1k searches/mo free", "register at tavily.com"},
		{"brave", "BRAVE_SEARCH_API_KEY", "2k queries/mo free, no credit card", "register at api.search.brave.com"},
		{"serper", "SERPER_API_KEY", "paid Google SERP", "register at serper.dev"},
		{"ddg", "", "zero-config fallback", "always available"},
	}
	for _, b := range backends {
		if b.env == "" {
			d.ok(b.name, b.tier)
			continue
		}
		// Precedence mirrors resolveSearchKey() in
		// internal/tools/builtin/websearch.go — env wins, auth.json
		// is the persistent fallback. Report which source actually
		// supplied the key so debugging "why did metis use ddg
		// instead of tavily" doesn't require re-deriving the rule.
		if v := os.Getenv(b.env); v != "" {
			d.ok(b.name, fmt.Sprintf("%s set via env, %d chars (%s…)", b.env, len(v), safePrefix(v, 6)))
			continue
		}
		if v, _ := auth.GetSearchKey(b.name); v != "" {
			d.ok(b.name, fmt.Sprintf("set via auth.json (search:%s), %d chars (%s…)", b.name, len(v), safePrefix(v, 6)))
			continue
		}
		d.warn(b.name, fmt.Sprintf("%s unset — %s (%s; or `metis auth keys put %s <key>`)", b.env, b.tier, b.signup, b.name))
	}
}

func (d *diagAcc) text() string {
	var b strings.Builder
	curSec := ""
	for _, r := range d.rows {
		if r.Section != curSec {
			fmt.Fprintf(&b, "\n[%s]\n", r.Section)
			curSec = r.Section
		}
		marker := "  ✓"
		switch r.Status {
		case "warn":
			marker = "  ⚠"
		case "fail":
			marker = "  ✗"
		}
		fmt.Fprintf(&b, "%s %-26s %s\n", marker, r.Name, r.Detail)
	}
	if d.hasFail() {
		b.WriteString("\nresult: FAIL\n")
	} else {
		b.WriteString("\nresult: OK\n")
	}
	return b.String()
}

func (d *diagAcc) json() string {
	var b strings.Builder
	b.WriteString(`{"result":"`)
	if d.hasFail() {
		b.WriteString("fail")
	} else {
		b.WriteString("ok")
	}
	b.WriteString(`","rows":[`)
	for i, r := range d.rows {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"section":%q,"name":%q,"status":%q,"detail":%q}`,
			r.Section, r.Name, r.Status, r.Detail)
	}
	b.WriteString("]}")
	return b.String()
}
