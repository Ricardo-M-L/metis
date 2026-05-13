package tui

// Renderers for the P0 info/toggle slash commands. Each one produces a
// single string that gets appended to the message log as a system info
// row. Pure presentation — no I/O beyond what's already loaded into the
// model.

import (
	"fmt"
	"io"
	"io/fs"
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

func renderDiff(m *Model) string {
	var b strings.Builder

	// Section 1: per-turn diff report — mirrors claude-code's
	// useTurnDiffs.ts. Each user-prompt opens a new "turn"; every
	// Edit / Write / NotebookEdit inside the turn aggregates into
	// per-file +N/-M counts derived from the same go-udiff
	// generator the per-call preview uses. Newest turn first.
	if m != nil && m.loop != nil {
		if turns := m.loop.TurnDiffs(); len(turns) > 0 {
			totalA, totalR, totalFiles := 0, 0, 0
			fileSet := map[string]struct{}{}
			for _, tn := range turns {
				totalA += tn.TotalLinesAdded
				totalR += tn.TotalLinesRemoved
				for path := range tn.Files {
					fileSet[path] = struct{}{}
				}
			}
			totalFiles = len(fileSet)
			fmt.Fprintf(&b, "Session edits across %d turn(s) · %d file(s) · ",
				len(turns), totalFiles)
			fmt.Fprintf(&b, "+%d -%d lines\n\n", totalA, totalR)

			for _, tn := range turns {
				fmt.Fprintf(&b, "Turn %d", tn.Index)
				if tn.UserPromptPreview != "" {
					fmt.Fprintf(&b, "  · %q", tn.UserPromptPreview)
				}
				fmt.Fprintf(&b, "  (%d file(s), +%d -%d)\n",
					tn.FilesChanged, tn.TotalLinesAdded, tn.TotalLinesRemoved)

				paths := make([]string, 0, len(tn.Files))
				for p := range tn.Files {
					paths = append(paths, p)
				}
				sortStrings(paths)
				for _, p := range paths {
					f := tn.Files[p]
					marker := "  ✏ "
					if f.IsNewFile {
						marker = "  ✨"
					}
					fmt.Fprintf(&b, "  %s %s  +%d -%d", marker, f.Path,
						f.LinesAdded, f.LinesRemoved)
					if f.EditCount > 1 {
						fmt.Fprintf(&b, "  (%d edits)", f.EditCount)
					}
					b.WriteString("\n")
				}
				b.WriteString("\n")
			}
		}
	}

	// Section 2: working-tree git diff. Complementary view: shows
	// uncommitted state regardless of which turn produced it AND
	// any non-agent edits.
	cmd := exec.Command("git", "diff", "--stat", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		cmd = exec.Command("git", "diff", "--stat")
		out, err = cmd.CombinedOutput()
		if err != nil {
			b.WriteString("git diff: ")
			b.WriteString(err.Error())
			b.WriteString("\n")
			b.Write(out)
			return b.String()
		}
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		if b.Len() == 0 {
			return "(no agent edits this session, working tree clean)"
		}
		b.WriteString("git diff --stat HEAD: (working tree clean)\n")
		return b.String()
	}
	b.WriteString("git diff --stat HEAD:\n")
	b.WriteString(body)
	b.WriteString("\n\n(use `git diff` outside metis for full patch)")
	return b.String()
}

// sortStrings is a tiny local sort.Strings to avoid widening the
// import set in render_info.go (most other render_* funcs don't
// need sort).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
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
	// Per-category drill-downs (claude-code parity, 2026-05-11 user
	// request, image #1). The grid above gives the bird's-eye; the
	// blocks below give the "what individual tools / files am I
	// paying for" breakdown. Each block is its own header + tree so
	// long sessions can scroll to the section they care about.
	s.WriteString("\n")
	s.WriteString(renderContextCacheBlock(m))
	s.WriteString(renderContextMCPBlock(m))
	s.WriteString(renderContextMemoryBlock(m))
	s.WriteString(renderContextSkillsBlock(m))
	return s.String()
}

// renderContextCacheBlock surfaces prompt-cache effectiveness for the
// provider's last turn and the session as a whole. The data comes from
// usage events the provider reports (cache_read_input_tokens /
// cache_creation_input_tokens) — already tracked by tokenTracker but
// previously visible only via /cost.
//
// The user's actual ask (2026-05-11): "I can't see whether my cache is
// hitting or not, please surface it in /context." For MiniMax via the
// /anthropic endpoint, real metis sessions hit 80-94% on stable turns
// and drop sharply when a fat tool_result (e.g. screenshot PNG) busts
// the prefix — both states should be visible at a glance.
//
// Block omitted entirely when there's been no cache activity (cache
// totals all zero); avoids a "Cache: 0 tokens" noise row in fresh
// sessions before the first API call.
func renderContextCacheBlock(m *Model) string {
	if m == nil {
		return ""
	}
	lastRead := m.totalTokens.LastCacheRead()
	lastCreate := m.totalTokens.LastCacheCreate()
	lastIn := m.totalTokens.LastIn()
	sessRead := m.totalTokens.CacheRead()
	sessCreate := m.totalTokens.CacheCreate()
	sessIn := m.totalTokens.Input()
	if lastRead == 0 && lastCreate == 0 && sessRead == 0 && sessCreate == 0 {
		return ""
	}
	lastRate := m.totalTokens.LastCacheHitRate()
	sessRate := m.totalTokens.CacheHitRate()

	var b strings.Builder
	b.WriteString(styleAccent.Render("Cache · prompt-prefix reuse") + "\n")

	// This-turn row. Hit-rate emoji-free: a colon-delimited triplet
	// (read / create / fresh) reads cleanly in a transcript that's
	// also being grepped or copy-pasted out.
	lastTotal := lastRead + lastCreate + lastIn
	b.WriteString("  " + styleDim.Render("├ this turn:    ") +
		fmt.Sprintf("%s read · %s create · %s fresh   %s",
			fmtThousands(lastRead),
			fmtThousands(lastCreate),
			fmtThousands(lastIn),
			styleDim.Render(fmt.Sprintf("hit %.0f%% of %s", lastRate*100, fmtThousands(lastTotal)))) + "\n")

	// Session-cumulative row. The number that tells the user "is
	// caching saving me money across this whole conversation?" —
	// distinct from the per-turn signal which fluctuates with each
	// tool result.
	sessTotal := sessRead + sessCreate + sessIn
	b.WriteString("  " + styleDim.Render("└ session avg:  ") +
		fmt.Sprintf("%s read · %s create · %s fresh   %s",
			fmtThousands(sessRead),
			fmtThousands(sessCreate),
			fmtThousands(sessIn),
			styleDim.Render(fmt.Sprintf("hit %.0f%% of %s", sessRate*100, fmtThousands(sessTotal)))) + "\n")

	// Optional cost-savings hint when the provider catalog publishes
	// a cache_read price. cached tokens cost 10% of fresh (Anthropic
	// + MiniMax pricing) so the savings ≈ cache_read × 90% × input
	// price. Skip the line entirely when we don't know the price —
	// better silent than misleading.
	if m.loop != nil && m.loop.Provider != nil {
		savings := cacheSavingsForSession(m, sessRead)
		if savings > 0 {
			b.WriteString("  " + styleDim.Render(fmt.Sprintf("  saved ~$%.4f vs. uncached prefix this session", savings)) + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// cacheSavingsForSession estimates the dollar savings the user got
// from prompt cache this session. Uses the provider catalog price if
// it knows the current model; returns 0 when pricing is unknown so
// the renderer can suppress the line rather than show a misleading $0.
//
// Formula: cached tokens billed at ~10% of input price; savings is the
// 90% delta. Matches the heuristic in render_info.go::renderCost.
func cacheSavingsForSession(m *Model, cacheRead int) float64 {
	if cacheRead == 0 || m == nil || m.model == "" {
		return 0
	}
	priceIn, _ := guessPriceUSDPerM(m.model)
	if priceIn <= 0 {
		return 0
	}
	return float64(cacheRead) * priceIn * 0.9 / 1_000_000
}

// renderContextMCPBlock builds the "MCP tools · /mcp (loaded on-demand)"
// section: per-tool token cost for tools whose full schema is currently
// in the prompt, plus a names-only "Available" list for the rest.
//
// "Loaded" = the schema is being sent on the next turn (P6 discovered
// set OR LazyMode=standard OR the auto threshold didn't fire). We
// derive this by calling Loop.ToolSpecsSnapshot() and comparing each
// mcp__ entry's schema shape against the lazy placeholder. The
// placeholder has additionalProperties=true + a fixed description; a
// real schema doesn't.
func renderContextMCPBlock(m *Model) string {
	if m == nil || m.loop == nil || m.loop.Registry == nil {
		return ""
	}
	specs := m.loop.ToolSpecsSnapshot()
	type entry struct {
		name string
		toks int
	}
	var loaded []entry
	loadedByName := make(map[string]bool, 8)
	for _, sp := range specs {
		if !strings.HasPrefix(sp.Name, "mcp__") {
			continue
		}
		if isLazyPlaceholderSchema(sp.InputSchema) {
			continue
		}
		toks := roughTokens(sp.Name) + roughTokens(sp.Description)
		// Schema bytes add ~30% on top of description per claude-code's
		// calibration — same heuristic the grid uses.
		toks += roughTokens(sp.Description) * 30 / 100
		loaded = append(loaded, entry{name: sp.Name, toks: toks})
		loadedByName[sp.Name] = true
	}
	// Collect "Available" = mcp__ tools registered but NOT loaded.
	var available []string
	for _, t := range m.loop.Registry.SortedForCache() {
		n := t.Name()
		if !strings.HasPrefix(n, "mcp__") {
			continue
		}
		if loadedByName[n] {
			continue
		}
		available = append(available, n)
	}
	if len(loaded) == 0 && len(available) == 0 {
		return ""
	}
	sort.SliceStable(loaded, func(i, j int) bool { return loaded[i].name < loaded[j].name })
	sort.Strings(available)

	var b strings.Builder
	b.WriteString(styleAccent.Render("MCP tools · /mcp (loaded on-demand)") + "\n\n")
	if len(loaded) > 0 {
		b.WriteString(styleDim.Render("Loaded") + "\n")
		for i, e := range loaded {
			prefix := "├ "
			if i == len(loaded)-1 {
				prefix = "└ "
			}
			b.WriteString("  " + styleDim.Render(prefix) +
				e.name + styleDim.Render(fmt.Sprintf(": %s tokens", fmtThousands(e.toks))) + "\n")
		}
		b.WriteString("\n")
	}
	if len(available) > 0 {
		b.WriteString(styleDim.Render("Available") + "\n")
		for i, n := range available {
			prefix := "├ "
			if i == len(available)-1 {
				prefix = "└ "
			}
			b.WriteString("  " + styleDim.Render(prefix) + n + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderContextMemoryBlock surfaces the user's memory files (under
// ~/.metis/memory/) with per-file token cost. Walks the directory each
// time the user runs /context — files are small (a few KB at most)
// and the user only runs /context occasionally, so the IO cost is
// negligible vs cluttering Model with cached state.
func renderContextMemoryBlock(m *Model) string {
	memDir := filepath.Join(config.Home(), "memory")
	entries, err := os.ReadDir(memDir)
	if err != nil {
		return "" // no memory dir → omit the section entirely
	}
	type memEntry struct {
		path string
		toks int
	}
	var items []memEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(memDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		items = append(items, memEntry{
			path: filepath.Join("memory", e.Name()), // relative for display
			toks: roughTokens(string(raw)),
		})
	}
	if len(items) == 0 {
		return ""
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].path < items[j].path })
	var b strings.Builder
	b.WriteString(styleAccent.Render("Memory files · /memory") + "\n")
	for i, e := range items {
		prefix := "├ "
		if i == len(items)-1 {
			prefix = "└ "
		}
		b.WriteString("  " + styleDim.Render(prefix) + e.path +
			styleDim.Render(fmt.Sprintf(": %s tokens", fmtThousands(e.toks))) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// renderContextSkillsBlock surfaces the loaded skills. Walks the
// configured skillDir for per-skill manifests; falls back to listing
// names if manifests can't be read. Skill files are typically tiny
// (just a manifest + a few prompt lines), so the per-skill token
// number isn't dramatic — but the LIST is what most users want to
// see when they ask "what skills are active right now".
func renderContextSkillsBlock(m *Model) string {
	if m == nil || m.skillDir == "" {
		return ""
	}
	entries, err := os.ReadDir(m.skillDir)
	if err != nil {
		return ""
	}
	type skillEntry struct {
		name string
		toks int
	}
	var items []skillEntry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(m.skillDir, name)
		st, err := os.Stat(full)
		if err != nil || !st.IsDir() {
			// .json manifests are also skills — capture as a name + size pair.
			if !st.IsDir() && strings.HasSuffix(name, ".json") {
				raw, _ := os.ReadFile(full)
				items = append(items, skillEntry{
					name: strings.TrimSuffix(name, ".json"),
					toks: roughTokens(string(raw)),
				})
			}
			continue
		}
		// Directory skill — sum sizes of .md / SKILL.md children.
		toks := 0
		_ = filepath.WalkDir(full, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") && !strings.HasSuffix(d.Name(), ".json") {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			toks += roughTokens(string(raw))
			return nil
		})
		items = append(items, skillEntry{name: name, toks: toks})
	}
	if len(items) == 0 {
		return ""
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].toks > items[j].toks })
	var b strings.Builder
	b.WriteString(styleAccent.Render("Skills · /skills") + "\n")
	for i, e := range items {
		prefix := "├ "
		if i == len(items)-1 {
			prefix = "└ "
		}
		b.WriteString("  " + styleDim.Render(prefix) + e.name +
			styleDim.Render(fmt.Sprintf(": %s tokens", fmtThousands(e.toks))) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// isLazyPlaceholderSchema returns true for the shape that
// stripAndAppendToolSearch leaves behind for deferred tools. Two
// signals must BOTH match (lone additionalProperties=true could be a
// real schema with no fixed fields).
func isLazyPlaceholderSchema(s map[string]any) bool {
	if s == nil {
		return false
	}
	ap, _ := s["additionalProperties"].(bool)
	desc, _ := s["description"].(string)
	return ap && strings.Contains(desc, "deferred")
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
