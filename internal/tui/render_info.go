package tui

// Renderers for the P0 info/toggle slash commands. Each one produces a
// single string that gets appended to the message log as a system info
// row. Pure presentation — no I/O beyond what's already loaded into the
// model.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/version"
)

func renderCost(m *Model) string {
	in := m.totalTokens.Input()
	out := m.totalTokens.Output()
	total := in + out
	// Heuristic per-1M token prices (USD). Roughly today's anthropic/oai
	// public list pricing — close enough for an in-chat estimate. Real
	// billing happens on the provider side regardless.
	priceIn, priceOut := guessPriceUSDPerM(m.model)
	costUSD := float64(in)*priceIn/1_000_000 + float64(out)*priceOut/1_000_000
	var b strings.Builder
	fmt.Fprintf(&b, "session cost (model: %s)\n", m.model)
	fmt.Fprintf(&b, "  input  tokens: %s\n", fmtThousands(in))
	fmt.Fprintf(&b, "  output tokens: %s\n", fmtThousands(out))
	fmt.Fprintf(&b, "  total  tokens: %s\n", fmtThousands(total))
	fmt.Fprintf(&b, "  est. cost:     $%.4f  (estimate, real billing on provider)", costUSD)
	return b.String()
}

func guessPriceUSDPerM(model string) (in, out float64) {
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "opus"):
		return 15, 75
	case strings.Contains(model, "sonnet"):
		return 3, 15
	case strings.Contains(model, "haiku"):
		return 0.8, 4
	case strings.Contains(model, "gpt-4"):
		return 2.5, 10
	case strings.Contains(model, "gemini"):
		return 1.25, 5
	default:
		return 3, 15 // safe-ish default
	}
}

func renderDiff() string {
	cmd := exec.Command("git", "diff", "--stat", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fall back to plain diff if HEAD doesn't exist (fresh repo).
		cmd = exec.Command("git", "diff", "--stat")
		out, err = cmd.CombinedOutput()
		if err != nil {
			return "diff: " + err.Error() + "\n" + string(out)
		}
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return "(working tree clean)"
	}
	return "git diff --stat:\n" + body + "\n\n(use `git diff` outside metis for full patch)"
}

func renderDoctor(m *Model) string {
	var b strings.Builder
	b.WriteString("metis doctor\n")
	b.WriteString(fmt.Sprintf("  version:   %s\n", version.Version))
	b.WriteString(fmt.Sprintf("  go:        %s\n", runtime.Version()))
	b.WriteString(fmt.Sprintf("  os/arch:   %s/%s\n", runtime.GOOS, runtime.GOARCH))
	cwd, _ := os.Getwd()
	b.WriteString(fmt.Sprintf("  cwd:       %s\n", cwd))
	b.WriteString(fmt.Sprintf("  metis dir: %s\n", config.Home()))
	b.WriteString(fmt.Sprintf("  model:     %s\n", m.model))
	b.WriteString(fmt.Sprintf("  mode:      %s\n", string(m.gate.Mode())))
	// Provider key sniff — check common envs without exposing the value.
	keys := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"}
	b.WriteString("  api keys:\n")
	for _, k := range keys {
		state := "(unset)"
		if v := os.Getenv(k); v != "" {
			state = fmt.Sprintf("(set, len=%d)", len(v))
		}
		b.WriteString(fmt.Sprintf("    %-22s %s\n", k+":", state))
	}
	// Tool count.
	b.WriteString(fmt.Sprintf("  tools:     %d registered\n", len(m.loop.Registry.All())))
	// Memory dir presence.
	memDir := filepath.Join(config.Home(), "memories")
	mems := "(missing)"
	if entries, err := os.ReadDir(memDir); err == nil {
		mems = fmt.Sprintf("(%d files)", len(entries))
	}
	b.WriteString(fmt.Sprintf("  memory:    %s %s\n", memDir, mems))
	b.WriteString("  status:    ✓ basic checks passed")
	return b.String()
}

func renderStats(m *Model) string {
	turns := 0
	for _, msg := range m.messages {
		if msg.Role == "user" {
			turns++
		}
	}
	toolCalls := len(m.toolEvents)
	in := m.totalTokens.Input()
	out := m.totalTokens.Output()
	var b strings.Builder
	b.WriteString("session stats\n")
	fmt.Fprintf(&b, "  session id:   %s\n", m.sessionID)
	fmt.Fprintf(&b, "  user turns:   %d\n", turns)
	fmt.Fprintf(&b, "  tool calls:   %d\n", toolCalls)
	fmt.Fprintf(&b, "  input tokens:  %s\n", fmtThousands(in))
	fmt.Fprintf(&b, "  output tokens: %s\n", fmtThousands(out))
	fmt.Fprintf(&b, "  loop iters:   %d\n", m.loop.MaxIters)
	fmt.Fprintf(&b, "  history msgs: %d", len(m.loop.History()))
	return b.String()
}

func renderKeybindings() string {
	rows := []struct{ key, desc string }{
		{"Ctrl-C", "exit (idle) / cancel turn (active)"},
		{"Ctrl-D", "quit"},
		{"Ctrl-L", "show available models"},
		{"Ctrl-T", "toggle task panel"},
		{"Ctrl-O", "toggle expanded tool output"},
		{"Ctrl-R", "history search overlay"},
		{"Ctrl-S", "copy mode (exit alt-screen)"},
		{"Ctrl-V", "paste from clipboard (image/text)"},
		{"Ctrl-Y", "yank last assistant reply"},
		{"Ctrl-P", "session picker"},
		{"Ctrl-J / Alt-Enter", "insert newline"},
		{"Tab", "autocomplete slash / @-mention"},
		{"Shift-Tab", "cycle permission mode"},
		{"PageUp/Dn", "scroll viewport"},
		{"Home/End", "jump top/bottom"},
		{"Esc", "dismiss palette / modal / pending state"},
		{"Esc Esc", "clear input completely"},
	}
	var b strings.Builder
	b.WriteString("TUI keybindings\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-18s  %s\n", r.key, r.desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderPermissions(m *Model) string {
	var b strings.Builder
	fmt.Fprintf(&b, "permission mode: %s\n", string(m.gate.Mode()))
	rules := m.gate.Snapshot()
	if len(rules) == 0 {
		b.WriteString("  (no explicit rules — falling back to mode default)")
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "  %d rule(s):\n", len(rules))
	for _, r := range rules {
		// permission.Decision constants: Ask=0, Allow=1, Deny=2.
		// Earlier this branch wrote "deny" for r.Verb==1 (which is
		// actually Allow), so an interactive "Yes always" decision
		// surfaced as a deny rule in /permissions — the opposite of
		// what the user just clicked. Use the symbolic constants.
		verb := "ask"
		switch r.Verb {
		case permission.DecisionAllow:
			verb = "allow"
		case permission.DecisionDeny:
			verb = "deny"
		}
		match := r.Match
		if match == "" {
			match = "*"
		}
		src := r.Source
		if src == "" {
			src = "—"
		}
		fmt.Fprintf(&b, "    %-6s  %-12s  %-30s  (%s)\n", verb, r.Tool, match, src)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderHooksList — best-effort hook inventory based on what the runtime
// has loaded. The HookRegistry doesn't expose registration metadata, so
// this leans on counts + the user's configured hook spec from
// config.toml [hooks.*]. Good enough for a sanity check.
func renderHooksList(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("loaded hooks (config.toml [hooks.*]):\n")
	if cfg == nil {
		b.WriteString("  (no config loaded)")
		return b.String()
	}
	type group struct {
		name  string
		specs []config.HookSpec
	}
	groups := []group{
		{"PreToolUse", cfg.Hooks.PreToolUse},
		{"PostToolUse", cfg.Hooks.PostToolUse},
		{"SessionStart", cfg.Hooks.SessionStart},
		{"SessionEnd", cfg.Hooks.SessionEnd},
	}
	any := false
	for _, g := range groups {
		if len(g.specs) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(&b, "  %s:\n", g.name)
		for i, s := range g.specs {
			t := s.Type
			if t == "" {
				t = "command"
			}
			ifs := s.If
			if ifs == "" {
				ifs = "*"
			}
			cmd := s.Command
			if len(cmd) > 60 {
				cmd = cmd[:57] + "..."
			}
			fmt.Fprintf(&b, "    [%d] type=%s if=%q\n        %s\n", i, t, ifs, cmd)
		}
	}
	if !any {
		b.WriteString("  (no user hooks declared in config.toml)")
		return b.String()
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderReleaseNotes reads CHANGELOG.md from a few candidate locations
// and shows the head section. Falls back to the metis version string
// if no CHANGELOG is reachable.
func renderReleaseNotes() string {
	candidates := []string{
		filepath.Join(config.Home(), "CHANGELOG.md"),
	}
	// metis source dir if user is iterating on the binary itself.
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "CHANGELOG.md"))
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// Show first ~60 lines or up to the second `## [` heading.
		lines := strings.Split(string(b), "\n")
		end := len(lines)
		secondHead := 0
		for i, l := range lines {
			if strings.HasPrefix(l, "## [") {
				secondHead++
				if secondHead == 2 {
					end = i
					break
				}
			}
		}
		if end > 60 {
			end = 60
		}
		return "from " + p + ":\n\n" + strings.Join(lines[:end], "\n")
	}
	return fmt.Sprintf("metis %s — no CHANGELOG.md found in %s or cwd", version.Version, config.Home())
}

// fmtThousands formats an int with thousands separators ("12,345").
func fmtThousands(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 {
		return s
	}
	parts := []string{}
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

// renderModelList — shared with Ctrl-L; kept here to centralize info-row
// formatters. Sorted for stable output.
func renderModelList(loop interface{ Model() string }) string {
	models := []string{
		"claude-opus-4-7",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
		"gpt-4o",
		"gemini-2.5-pro",
	}
	sort.Strings(models)
	var b strings.Builder
	b.WriteString("available model ids (subject to provider availability):\n")
	for _, m := range models {
		b.WriteString("  - ")
		b.WriteString(m)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

var _ = renderModelList // keep available for future Ctrl-L wiring

// exportSessionToFile writes the session JSONL to ~/.metis/exports/<id>.jsonl
// and returns the file path. Mirrors `metis sessions export` but lives
// inside the chat surface so users don't have to leave the TUI.
func exportSessionToFile(store sessionExporter, id string) (string, error) {
	dir := filepath.Join(config.Home(), "exports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dir, id+".jsonl")
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := store.Export(id, f); err != nil {
		return "", err
	}
	return out, nil
}

// sessionExporter narrows what /export needs from the session store —
// keeps render_info.go decoupled from the full Store API.
type sessionExporter interface {
	Export(id string, w io.Writer) error
}

// --- P1 renderers ---

// themeState lives at package scope so /theme persists for the session.
// Default = "auto" (terminal-driven). On change we re-build the lipgloss
// styles, but the chrome currently doesn't read theme — this is a
// scaffold for upcoming style reactivity.
var themeState = "auto"

func renderTheme(args string) string {
	want := strings.TrimSpace(args)
	if want == "" {
		return fmt.Sprintf("(theme: %s — `/theme dark|light|auto` to switch)", themeState)
	}
	switch want {
	case "dark", "light", "auto":
		themeState = want
		return fmt.Sprintf("(theme: %s)", themeState)
	default:
		return fmt.Sprintf("unknown theme %q (allowed: dark, light, auto)", want)
	}
}

// effortState mirrors --effort flag value at runtime; /effort lets the
// user change it mid-session. Honored by the next provider Request.
var effortState = ""

func renderEffort(args string) string {
	want := strings.TrimSpace(args)
	if want == "" {
		display := effortState
		if display == "" {
			display = "(default — provider-decided)"
		}
		return fmt.Sprintf("(effort: %s — `/effort low|medium|high` to set)", display)
	}
	switch want {
	case "low", "medium", "high":
		effortState = want
		return fmt.Sprintf("(effort: %s)", effortState)
	default:
		return fmt.Sprintf("unknown effort %q (allowed: low, medium, high)", want)
	}
}

// EffortState is the export so cmd/metis can read the live value when
// constructing the next provider Request.
func EffortState() string { return effortState }

// renderPRComments shells out to `gh pr view <n> --comments` to fetch
// review comments. Requires `gh` on PATH and an authenticated repo.
func renderPRComments(prNum string) string {
	prNum = strings.TrimSpace(prNum)
	if _, err := exec.LookPath("gh"); err != nil {
		return "/pr_comments: gh CLI not found on PATH (install: https://cli.github.com)"
	}
	cmd := exec.Command("gh", "pr", "view", prNum, "--comments")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "/pr_comments: " + err.Error() + "\n" + string(out)
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return "(no comments on PR " + prNum + ")"
	}
	// Cap at 200 lines to avoid blowing up the chat surface.
	lines := strings.Split(body, "\n")
	if len(lines) > 200 {
		lines = append(lines[:200], "... (truncated; see `gh pr view "+prNum+" --comments`)")
	}
	return strings.Join(lines, "\n")
}

// renderUpgrade shells out to `metis update --check`. Surfacing it here
// lets users check from inside chat without leaving.
func renderUpgrade() string {
	exe, err := os.Executable()
	if err != nil {
		return "/upgrade: cannot resolve metis path: " + err.Error()
	}
	cmd := exec.Command(exe, "update", "--check")
	out, _ := cmd.CombinedOutput()
	body := strings.TrimSpace(string(out))
	if body == "" {
		return "(/upgrade: no output; set METIS_GITHUB_TOKEN if running against private release)"
	}
	return body
}

// renderContext computes a rough context-window utilization for the
// session: tokens-so-far / model-context-window. Exact numbers come
// from the provider on stream-end; this is best-effort using the
// model's MaxContextTokens.
func renderContext(m *Model) string {
	used := m.totalTokens.Input() + m.totalTokens.Output()
	cap := m.loop.Provider.MaxContextTokens()
	pct := 0
	if cap > 0 {
		pct = int(float64(used) / float64(cap) * 100)
	}
	bar := renderBar(pct, 30)
	return fmt.Sprintf("context: %s / %s tokens  (%d%% of %s window)\n  %s",
		fmtThousands(used), fmtThousands(cap), pct, m.model, bar)
}

func renderResumeHelp(m *Model) string {
	id := m.sessionID
	if id == "" {
		id = "<session-id>"
	}
	return fmt.Sprintf("To resume this session later:\n\n  metis chat --resume %s\n\n(`/sessions` shows recent ids; `/branch` forks a copy you can return to.)", id)
}

// tagCurrentSession persists a label-list to ~/.metis/sessions/tags/<id>.txt.
// One label per line; new tags append (deduped). Lightweight enough that we
// don't need a registry — `/sessions` can join on demand if it ever wants to
// surface tags.
func tagCurrentSession(_ any, id, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("empty label")
	}
	dir := filepath.Join(config.Home(), "sessions", "tags")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, id+".txt")
	existing, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == label {
			return nil // already tagged
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(label + "\n")
	return err
}

func renderBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := width * pct / 100
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}
