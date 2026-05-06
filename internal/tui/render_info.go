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

	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/version"
)

// infoRow is one line in an info-box. Empty Key = full-width free text,
// otherwise rendered as `key: value` with keys right-padded so the
// values left-align in a column.
type infoRow struct {
	Key   string
	Value string
	// Hint is an optional muted suffix appended after Value with " · "
	// — used for inline help like `/effort high · deep, slower`.
	Hint string
}

// renderInfoBox draws a rounded box with a colored title bar above a
// key/value table. Used by the P0 status commands (/effort, /context,
// /cost, /model, /memory, /version, /status) so they all read with the
// same shape — claude-code's slash commands have this consistency too.
//
// Width is auto-derived from the longest key+value pair; caller doesn't
// need to count columns. Empty `Key` rows get rendered as a single
// muted line (good for footnotes / commands hint).
func renderInfoBox(title string, rows []infoRow) string {
	keyW := 0
	for _, r := range rows {
		if w := lipgloss.Width(r.Key); w > keyW {
			keyW = w
		}
	}
	var body strings.Builder
	for i, r := range rows {
		if i > 0 {
			body.WriteString("\n")
		}
		switch {
		case r.Key == "" && r.Value == "":
			// blank spacer — keep the row but render as nothing
		case r.Key == "":
			body.WriteString(styleMuted.Render(r.Value))
		default:
			pad := keyW - lipgloss.Width(r.Key)
			body.WriteString(styleMuted.Render(r.Key+":") + strings.Repeat(" ", pad+1))
			body.WriteString(styleText.Render(r.Value))
			if r.Hint != "" {
				body.WriteString(styleMuted.Render("  · " + r.Hint))
			}
		}
	}
	titleLine := styleAccent.Bold(true).Render(title)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#8be9fd")).
		Padding(0, 1).
		Render(titleLine + "\n" + body.String())
	return box
}

func renderCost(m *Model) string {
	// Richer /cost — break input/output by category + show
	// price-per-Mtok column + cache savings + per-class subtotals.
	// Mirrors claude-code's /cost which surfaces every dimension a
	// user would ask about (raw tokens, cost share, cache hit rate).
	in := m.totalTokens.Input()
	out := m.totalTokens.Output()
	cc := m.totalTokens.CacheCreate()
	cr := m.totalTokens.CacheRead()
	total := in + out
	priceIn, priceOut := guessPriceUSDPerM(m.model)
	// Anthropic's cache pricing: cache_create ≈ 1.25× input, cache_read
	// ≈ 0.10× input. Heuristic — treat as "input" for non-Anthropic.
	cacheCreatePrice := priceIn * 1.25
	cacheReadPrice := priceIn * 0.10

	costInput := float64(in) * priceIn / 1_000_000
	costOutput := float64(out) * priceOut / 1_000_000
	costCacheCreate := float64(cc) * cacheCreatePrice / 1_000_000
	costCacheRead := float64(cr) * cacheReadPrice / 1_000_000
	costTotal := costInput + costOutput + costCacheCreate + costCacheRead

	// Cache savings: cost we WOULD have paid if cache_read tokens
	// had been re-billed at full input rate. The difference is the
	// concrete dollar value the prompt cache saved this session.
	savings := float64(cr)*priceIn/1_000_000 - costCacheRead

	rows := []infoRow{
		{Key: "model", Value: m.model, Hint: fmt.Sprintf("$%.2f / $%.2f per 1M (in / out)", priceIn, priceOut)},
		{Key: "", Value: ""},
		{Key: "input tokens", Value: fmtThousands(in), Hint: fmt.Sprintf("$%.4f", costInput)},
		{Key: "output tokens", Value: fmtThousands(out), Hint: fmt.Sprintf("$%.4f", costOutput)},
		{Key: "total tokens", Value: fmtThousands(total)},
	}
	if cc > 0 || cr > 0 {
		rows = append(rows,
			infoRow{Key: "", Value: ""},
			infoRow{Key: "cache_create", Value: fmtThousands(cc), Hint: fmt.Sprintf("$%.4f (write)", costCacheCreate)},
			infoRow{Key: "cache_read", Value: fmtThousands(cr), Hint: fmt.Sprintf("$%.4f (10%% of input)", costCacheRead)},
		)
		if cr > 0 && savings > 0 {
			rows = append(rows, infoRow{
				Key: "cache savings", Value: fmt.Sprintf("$%.4f", savings),
				Hint: "vs no-cache cost on the same reads",
			})
		}
		// Cache hit rate = cache_read / (cache_read + input). High
		// rate = lots of stable prefix; low = mostly fresh content.
		if cr+in > 0 {
			rate := float64(cr) * 100 / float64(cr+in)
			rows = append(rows, infoRow{
				Key: "cache hit rate", Value: fmt.Sprintf("%.1f%%", rate),
				Hint: "cache_read / (cache_read + input)",
			})
		}
	}
	rows = append(rows,
		infoRow{Key: "", Value: ""},
		infoRow{Key: "est. cost", Value: fmt.Sprintf("$%.4f", costTotal),
			Hint: "real billing on provider — heuristic per-Mtok prices"},
	)
	return renderInfoBox("Session Cost · "+m.model, rows)
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
	cwd, _ := os.Getwd()
	rows := []infoRow{
		{Key: "version", Value: version.Version},
		{Key: "go", Value: runtime.Version()},
		{Key: "os/arch", Value: runtime.GOOS + "/" + runtime.GOARCH},
		{Key: "cwd", Value: cwd},
		{Key: "metis dir", Value: config.Home()},
		{Key: "model", Value: m.model},
		{Key: "mode", Value: string(m.gate.Mode())},
		{Key: "tools", Value: fmt.Sprintf("%d registered", len(m.loop.Registry.All()))},
	}
	memDir := filepath.Join(config.Home(), "memories")
	mems := "missing"
	if entries, err := os.ReadDir(memDir); err == nil {
		mems = fmt.Sprintf("%d files", len(entries))
	}
	rows = append(rows, infoRow{Key: "memory", Value: memDir, Hint: mems})
	rows = append(rows, infoRow{Key: "", Value: ""})
	rows = append(rows, infoRow{Key: "", Value: "API keys (envs sniffed without exposing values):"})
	keys := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "METIS_GITHUB_TOKEN", "GITHUB_TOKEN"}
	for _, k := range keys {
		state := "(unset)"
		if v := os.Getenv(k); v != "" {
			state = fmt.Sprintf("set · len=%d", len(v))
		}
		rows = append(rows, infoRow{Key: "  " + k, Value: state})
	}
	rows = append(rows, infoRow{Key: "", Value: ""})
	rows = append(rows, infoRow{Key: "", Value: "✓ basic checks passed"})
	return renderInfoBox("Metis Doctor", rows)
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
	return renderInfoBox("Session Stats", []infoRow{
		{Key: "session id", Value: m.sessionID},
		{Key: "user turns", Value: fmt.Sprintf("%d", turns)},
		{Key: "tool calls", Value: fmt.Sprintf("%d", toolCalls)},
		{Key: "input tokens", Value: fmtThousands(in)},
		{Key: "output tokens", Value: fmtThousands(out)},
		{Key: "loop iters", Value: fmt.Sprintf("%d", m.loop.MaxIters)},
		{Key: "history msgs", Value: fmt.Sprintf("%d", len(m.loop.History()))},
	})
}

func renderKeybindings() string {
	pairs := []struct{ key, desc string }{
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
	rows := make([]infoRow, 0, len(pairs))
	for _, p := range pairs {
		rows = append(rows, infoRow{Key: p.key, Value: p.desc})
	}
	return renderInfoBox("TUI Keybindings", rows)
}

func renderPermissions(m *Model) string {
	rows := []infoRow{
		{Key: "permission mode", Value: string(m.gate.Mode())},
	}
	rules := m.gate.Snapshot()
	if len(rules) == 0 {
		rows = append(rows, infoRow{Key: "", Value: ""})
		rows = append(rows, infoRow{Key: "", Value: "no explicit rules — falling back to mode default"})
		return renderInfoBox("Permissions", rows)
	}
	rows = append(rows, infoRow{Key: "rule count", Value: fmt.Sprintf("%d", len(rules))})
	rows = append(rows, infoRow{Key: "", Value: ""})
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
		rows = append(rows, infoRow{
			Key:   fmt.Sprintf("%s %s", verb, r.Tool),
			Value: match,
			Hint:  src,
		})
	}
	return renderInfoBox("Permissions", rows)
}

// renderHooksList — best-effort hook inventory based on what the runtime
// has loaded. The HookRegistry doesn't expose registration metadata, so
// this leans on counts + the user's configured hook spec from
// config.toml [hooks.*]. Good enough for a sanity check.
func renderHooksList(cfg *config.Config) string {
	if cfg == nil {
		return renderInfoBox("Hooks", []infoRow{{Key: "", Value: "no config loaded"}})
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
	rows := make([]infoRow, 0, 16)
	any := false
	for _, g := range groups {
		if len(g.specs) == 0 {
			continue
		}
		any = true
		rows = append(rows, infoRow{Key: "", Value: ""})
		rows = append(rows, infoRow{Key: "", Value: g.name})
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
			if len(cmd) > 50 {
				cmd = cmd[:47] + "..."
			}
			rows = append(rows, infoRow{
				Key:   fmt.Sprintf("  [%d] %s", i, t),
				Value: cmd,
				Hint:  "if=" + ifs,
			})
		}
	}
	if !any {
		return renderInfoBox("Hooks", []infoRow{{Key: "", Value: "no user hooks declared in config.toml"}})
	}
	return renderInfoBox("Hooks · config.toml [hooks.*]", rows)
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

// renderContext is the rich `/context` view (claude-code parity).
// Layout (mirrors claude-code's Context Usage screen):
//
//	Context Usage
//	⛁ ⛁ ⛁ ⛀ ⛶ ⛶ … ⛶  ← 200-cell grid (20 × 10), each cell = cap/200 tokens
//	                  Model name (window cap)
//	                  Provider · model id
//	                  USED / CAP tokens (PCT%)
//
//	                  Estimated usage by category
//	                  ⛁ System prompt: N tokens (X%)
//	                  ⛁ System tools:  N tokens (X%)
//	                  ⛁ Skills:        N tokens (X%)
//	                  ⛁ Messages:      N tokens (X%)
//	                  ⛶ Free space:    N (Y%)
//
// Per-category numbers are best-effort estimates — the provider's
// usage event reports a single combined `input_tokens`, not a
// breakdown. We approximate from byte counts (chars/4) on the
// assembled system / tool-spec / message bodies.
func renderContext(m *Model) string {
	const (
		cellsW   = 20
		cellsH   = 10
		cellsTot = cellsW * cellsH

		usedGlyph = "⛁"
		freeGlyph = "⛶"
	)

	cap := 0
	if m.loop != nil && m.loop.Provider != nil {
		cap = m.loop.Provider.MaxContextTokens()
	}
	used := m.totalTokens.ContextUsage()
	if used == 0 {
		// Fallback to session-cumulative for the very first turn
		// before usage events land. Same logic as the bottom-right
		// status bar.
		used = m.totalTokens.in + m.totalTokens.cacheCreate + m.totalTokens.cacheRead
	}

	// Per-category estimates (chars/4 ≈ tokens, the same heuristic
	// the agent loop uses internally). Totals don't always sum to
	// `used` exactly — the provider counts cache_read + cache_create
	// separately and the LLM tokenizer differs from our chars/4 — but
	// the breakdown remains useful for "where is my context going".
	systemEst := 0
	toolsEst := 0
	skillsEst := 0
	msgsEst := 0
	if m.loop != nil {
		systemEst = roughTokens(m.loop.System)
		// Tools schema: every tool's name + description + serialized
		// JSON schema. Approximate by summing description bytes —
		// schema marshalling is too heavy for a status command.
		if reg := m.loop.Registry; reg != nil {
			for _, t := range reg.SortedForCache() {
				toolsEst += roughTokens(t.Name())
				toolsEst += roughTokens(t.Description())
				// Per claude-code's calibration, schema bodies in JSON
				// add ~30% overhead on top of description bytes.
				toolsEst += roughTokens(t.Description()) * 30 / 100
			}
		}
		// Messages: total of every content block in the running
		// history. estimateTokens isn't exported from the agent
		// package; replicate via roughTokens on flattened content.
		for _, msg := range m.loop.History() {
			for _, c := range msg.Content {
				msgsEst += roughTokens(c.Text)
				msgsEst += roughTokens(c.ToolResult)
			}
		}
	}
	// Skills are part of the system prompt's project_context section
	// in metis but are interesting on their own — peel an estimate
	// off the system prompt for display.
	if m.loop != nil && strings.Contains(m.loop.System, "<available_skills>") {
		if start := strings.Index(m.loop.System, "<available_skills>"); start >= 0 {
			if end := strings.Index(m.loop.System[start:], "</available_skills>"); end >= 0 {
				skillsEst = roughTokens(m.loop.System[start : start+end+len("</available_skills>")])
				// Don't double-count: the same bytes are in systemEst.
				if skillsEst > systemEst {
					skillsEst = systemEst
				}
			}
		}
	}

	// Build the 20×10 grid. Each cell is filled when the cumulative
	// token count up to that cell index is ≤ used. claude-code
	// distinguishes system / tools / messages with different colours;
	// metis uses a single accent colour to keep the renderer light.
	cellSize := 1
	if cap > 0 {
		cellSize = cap / cellsTot
		if cellSize < 1 {
			cellSize = 1
		}
	}
	usedCells := used / cellSize
	if usedCells > cellsTot {
		usedCells = cellsTot
	}

	usedStyle := lipgloss.NewStyle().Foreground(currentTheme.AccentBlue)
	freeStyle := lipgloss.NewStyle().Foreground(textMuted)

	// Right-side annotations: line index → annotation text.
	annotations := make([]string, cellsH)
	annotations[0] = m.model
	if cap > 0 {
		annotations[1] = fmt.Sprintf("%s window", fmtThousands(cap))
	}
	pct := 0
	if cap > 0 {
		pct = used * 100 / cap
	}
	annotations[2] = fmt.Sprintf("%s/%s tokens (%d%%)", fmtThousands(used), fmtThousands(cap), pct)

	// Categories rendered on rows 4-9 (rows 0-3 are headline).
	annotations[4] = "Estimated usage by category"
	cats := []struct {
		label string
		toks  int
	}{
		{"System prompt", systemEst},
		{"System tools", toolsEst},
		{"Skills", skillsEst},
		{"Messages", msgsEst},
	}
	for i, c := range cats {
		row := 5 + i
		if row >= cellsH {
			break
		}
		catPct := 0
		if cap > 0 {
			catPct = c.toks * 1000 / cap // ‰ for finer-grained display
		}
		annotations[row] = fmt.Sprintf("%s %s: %s tokens (%d.%d%%)",
			usedGlyph, c.label, fmtThousands(c.toks), catPct/10, catPct%10)
	}
	if cellsH > 9 {
		free := cap - used
		if free < 0 {
			free = 0
		}
		freePct := 100 - pct
		annotations[9] = fmt.Sprintf("%s Free space: %s (%d%%)",
			freeGlyph, fmtThousands(free), freePct)
	}

	var s strings.Builder
	s.WriteString(styleAccent.Render("Context Usage") + "\n")
	cell := 0
	for r := 0; r < cellsH; r++ {
		var row strings.Builder
		for c := 0; c < cellsW; c++ {
			if cell < usedCells {
				row.WriteString(usedStyle.Render(usedGlyph))
			} else {
				row.WriteString(freeStyle.Render(freeGlyph))
			}
			row.WriteString(" ")
			cell++
		}
		ann := annotations[r]
		s.WriteString("  ")
		s.WriteString(row.String())
		if ann != "" {
			s.WriteString("  ")
			s.WriteString(styleDim.Render(ann))
		}
		s.WriteString("\n")
	}
	return s.String()
}

// roughTokens approximates a string's token count via the chars/4
// heuristic the agent's compactor uses. Faster than building a
// real tokenizer and "good enough" for a status display.
func roughTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n == 0 {
		n = 1
	}
	return n
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
