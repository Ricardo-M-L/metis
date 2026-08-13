package tui

// tui_spinner.go isolates the spinner machinery (frames, verbs, ticking,
// shimmer effect) from the main Update/View flow. The spinner is a
// reusable visual element with no Model dependencies — only the tickCmd
// returns a tea.Msg the parent Update consumes.

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// spinnerTick fires every 120ms while the spinner is active. Update()
// uses it to advance frame index and animate the shimmer.
type spinnerTick struct{}

// tick is a generic tea.Msg for non-spinner timed redraws (currently
// unused by external callers but kept for symmetry).
type tick struct{}

// tickCmd schedules the next spinner frame. 40ms = 25 fps — sits right
// at the human "feels continuous" threshold (24fps cinema = 42ms) and
// trades a hair of animation smoothness for noticeably lower idle CPU
// vs 30ms. The user picked this number explicitly after we walked
// through the 16/30/50/100 tradeoffs.
//
// Going lower (16ms / 60fps) doesn't make text appear faster — the
// bottleneck is the LLM's chunk rate (50-200ms between text_deltas),
// not our drain cadence. But power users who want maximum smoothness
// can override via METIS_TICK_MS=16. Sane bounds are enforced (1-1000)
// so a typo doesn't peg a CPU core.
//
// Reduced-motion users still get 500ms tick — animations progress
// (glyph rotates, timer ticks) at a much calmer pace.
func tickCmd() tea.Msg {
	d := tickInterval()
	time.Sleep(d)
	return spinnerTick{}
}

// tickInterval picks the per-frame sleep, honoring (in order):
//
//  1. Reduced-motion mode → 500ms (calm, accessibility win).
//     Triggered by either `[ui.performance].reduced_motion = true` in
//     config.toml OR the package-level reducedMotionEnabled flag.
//  2. METIS_TICK_MS env var → 1..1000ms (power-user override).
//  3. `[ui.performance].tick_ms` from config.toml (1..1000).
//  4. Default 40ms.
//
// Read once per tick rather than cached so toggling reduced-motion or
// editing config + reloading takes effect without a TUI restart.
func tickInterval() time.Duration {
	if reducedMotionEnabled || perfConfig().ReducedMotion {
		return 500 * time.Millisecond
	}
	if v := os.Getenv("METIS_TICK_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1000 {
			return time.Duration(n) * time.Millisecond
		}
	}
	if cfg := perfConfig(); cfg.TickMs >= 1 && cfg.TickMs <= 1000 {
		return time.Duration(cfg.TickMs) * time.Millisecond
	}
	return 40 * time.Millisecond
}

// spinnerFrames mirrors claude-code's macOS spinner — six glyphs that grow
// from a quiet dot through ✢ ✳ ✶ ✻ to a full ✽, then mirror back so the
// star pulses in and out. Source:
// claude-code/components/Spinner/utils.ts:getDefaultCharacters() →
// SpinnerGlyph.tsx SPINNER_FRAMES = [...A, ...A.reverse()].
var spinnerFrames = []string{
	"·", "✢", "✳", "✶", "✻", "✽",
	"✽", "✻", "✶", "✳", "✢", "·",
}

// toolVerbs maps each tool name to its present-progressive verb shown
// next to the spinner during execution. Adding a new tool? Add an entry
// here so the spinner reads "[Read] reading" instead of generic "using".
var toolVerbs = map[string]string{
	"Read": "reading", "Write": "writing", "Glob": "finding",
	"Grep": "searching", "LS": "listing", "Bash": "executing",
	"WebFetch": "fetching", "MCP": "calling", "Edit": "editing",
	"Git": "git", "WebSearch": "searching", "TodoWrite": "tracking",
}

func toolVerb(name string) string {
	if v, ok := toolVerbs[name]; ok {
		return v
	}
	return "using"
}

// thinkingVerbs are the rotating gerunds shown next to the spinner. Pool
// is intentionally bigger than claude-code's so the agent doesn't repeat
// the same verb three turns in a row in normal conversation length.
//
// Curated to be tonally consistent: gentle, slightly playful, no aggression
// or anthropomorphizing. Anything that sounds urgent ("racing"), violent
// ("crushing"), or self-serious ("pontificating") is kept out. Pool size
// is large enough that the pseudo-random selection (UnixNano % len) never
// repeats consecutively in a normal session.
var thinkingVerbs = []string{
	// classic "thinking" set
	"thinking", "pondering", "contemplating", "musing", "deliberating",
	"reasoning", "analyzing", "cogitating", "ruminating", "puzzling",
	"reflecting", "considering", "wondering", "speculating", "weighing",
	// "working on it" set
	"processing", "computing", "evaluating", "calculating", "estimating",
	"interpreting", "decoding", "parsing", "inspecting", "examining",
	"surveying", "probing", "scanning", "sifting", "sleuthing",
	"investigating", "tracing",
	// craft / build set
	"brewing", "hatching", "distilling", "composing", "forging",
	"weaving", "synthesizing", "ideating", "drafting", "sketching",
	"prototyping", "shaping", "polishing", "refining", "tinkering",
	"assembling", "stitching", "knitting",
	// strategy set
	"scheming", "plotting", "strategizing", "planning", "mapping",
	"orchestrating", "arranging", "outlining", "blueprinting",
	// untangle set
	"untangling", "unraveling", "disentangling", "decluttering",
	"reconciling", "reasoning-through", "working-through",
	// gentle / playful set
	"noodling", "marinating", "percolating", "simmering", "stewing",
	"meandering", "wandering", "noodle-dancing", "daydreaming",
	"woolgathering", "mulling",
	// search set
	"searching", "exploring", "browsing", "hunting", "spelunking",
	"prospecting", "rummaging", "foraging",
	// transform set
	"transforming", "translating", "transcribing", "transmuting",
	"reformulating", "rephrasing", "rewording",
	// build-toward-answer set
	"converging", "narrowing", "focusing", "homing-in", "zeroing-in",
	"crystallizing", "coalescing", "consolidating",
	// connect set
	"connecting", "linking", "bridging", "binding", "stitching-up",
	// gentle action set
	"reading", "skimming", "studying", "absorbing", "ingesting",
	"digesting", "soaking-in",
	// final / wrap set
	"polishing-up", "wrapping-up", "finalizing", "tidying", "cleaning-up",
	"buttoning-up", "tightening", "smoothing",
	// imaginative set
	"imagining", "envisioning", "visualizing", "picturing", "daydreaming-up",
	// claude-code-flavored
	"hatching-up", "tinkering-with", "tinkering-away", "puttering",
	"finagling", "rejiggering", "wrangling", "shimmying",
	// inquiry set
	"asking-around", "checking-in", "verifying", "double-checking",
	"cross-checking", "confirming", "fact-checking", "second-guessing",
	// creative-language set
	"wordsmithing", "editing", "proofreading", "punching-up",
	"polishing-prose", "phrasing", "framing", "captioning",
	// tinker / fiddle set
	"fiddling", "twiddling", "tweaking", "adjusting", "calibrating",
	"fine-tuning", "dialing-in", "balancing", "trimming",
	// gather set
	"gathering", "collecting", "rounding-up", "pulling-together",
	"assembling-bits", "harvesting",
	// architect set
	"architecting", "structuring", "scaffolding", "underpinning",
	"foundation-laying", "frameworking",
	// study set
	"poring-over", "reviewing", "perusing", "scrutinizing", "auditing",
	"appraising", "assessing", "vetting",
	// craft / forge set v2
	"smithing", "carving", "chiseling", "molding", "kneading",
	"sculpting", "etching", "embossing",
	// idle-but-alive set
	"hovering", "lingering", "circling", "loitering", "lurking-pleasantly",
	// final clinch set
	"locking-in", "sealing-up", "stamping", "signing-off", "wrapping",
	"closing-out", "checking-off",
}

func pickSpinnerVerb() string {
	return thinkingVerbs[time.Now().UnixNano()%int64(len(thinkingVerbs))]
}

// turnDoneVerbs is the past-tense pool used by the end-of-turn summary
// (`✻ Cogitated for 1m 32s`). claude-code rotates through these so the
// transcript doesn't read like a stuck loop. Tone-matched to the
// gerund pool above: gentle, slightly playful, never aggressive.
var turnDoneVerbs = []string{
	"Cogitated", "Pondered", "Brewed", "Baked", "Mused",
	"Deliberated", "Reasoned", "Reflected", "Considered",
	"Composed", "Drafted", "Worked", "Computed", "Stewed",
	"Marinated", "Percolated", "Synthesized", "Distilled",
	"Tinkered", "Wrangled", "Assembled", "Polished",
}

// pickTurnDoneVerb chooses a past-tense verb for the turn-end summary.
// Independent from pickSpinnerVerb's gerund pool so a turn doesn't read
// "Pondering ... Pondered for 30s" — that's a stutter, not a summary.
func pickTurnDoneVerb() string {
	return turnDoneVerbs[time.Now().UnixNano()%int64(len(turnDoneVerbs))]
}

// formatTurnDuration prints a turn duration in claude-code's compact
// form: under a minute → `Xs`, under an hour → `Mm Ss`. Anything past
// an hour is rare in practice and just gets shown as `Hh Mm`.
func formatTurnDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// toolUseFlashStyle pulses the spinner verb between the standard
// shimmer band and a brighter orange tint while a tool is running.
// claude-code's tool-use mode does the same — visual reinforcement
// that "the model is now using a tool, not just thinking". Without
// this, the tool-running phase looks identical to the thinking phase
// and the user can't tell whether they're waiting on the model or on
// a side-effecting call (which matters for slow Bash / WebFetch).
//
// Reduced-motion: skip the sine-wave pulse and return a static orange.
// The user still gets the "this is a tool, not thinking" signal via
// color but no flicker.
func toolUseFlashStyle(elapsed time.Duration) lipgloss.Style {
	if reducedMotionEnabled || perfConfig().ReducedMotion {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#ffb74d"))
	}
	t := elapsed.Seconds()
	// 1.0s period (faster than thinking shimmer's 2s), so it reads as
	// "active work" not "drifting thought".
	sin := math.Sin(t * math.Pi * 2 / 1.0)
	f := (sin + 1) / 2 // → [0,1]
	// Interpolate between shimmer-grey #909090 and accent-orange #ffb74d
	// so the verb glows orange-ish during tool exec.
	r := uint8(0x90 + f*(0xff-0x90))
	g := uint8(0x90 + f*(0xb7-0x90))
	b := uint8(0x90 + f*(0x4d-0x90))
	return lipgloss.NewStyle().Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b)))
}

// shimmerStyle renders a sine-wave-pulsing grey on the spinner verb.
//
// Color range tuned for low contrast — user feedback was the previous
// 153↔185 band felt too aggressive on dark terminals. Now: 110↔150,
// which sits just above the chat surface's textMuted (#606060) so the
// verb reads as "alive but not shouting." Period stays 2s; shimmer
// activates 3s into the turn so a quick reply doesn't flicker.
//
// Reduced-motion: skip pulsing, return a static mid-grey. Same
// semantics ("the model is thinking") but without the flicker.
func shimmerStyle(elapsed time.Duration) lipgloss.Style {
	if reducedMotionEnabled || perfConfig().ReducedMotion {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#909090"))
	}
	const start = 3 * time.Second
	if elapsed < start {
		// Pre-shimmer state: solid mid-grey, NOT bold (bold + shimmer
		// later was the loudest combo; drop bold here for a softer
		// "thinking" appearance during the first 3 seconds).
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#909090"))
	}
	t := elapsed - start
	// sin oscillates [-1, 1] over a 2-second period.
	sin := math.Sin(t.Seconds() * math.Pi)
	f := (sin + 1) / 2 // → [0, 1]
	// Lower band: dimmer floor (110 = near-textMuted), peak below the
	// previous bottom (150 < old 153). Net effect: contrast against
	// the bg drops by ~25%.
	g := uint8(110 + f*(150-110))
	return lipgloss.NewStyle().Foreground(lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", g, g, g)))
}

// formatElapsed prints a wall-clock-friendly duration that ticks visibly
// even sub-second. "0s" looks frozen; "120ms"/"0.5s"/"3s" looks alive.
// Past one minute the spinner switches to claude-code's "Mm Ss" form so
// long-running turns don't read as a runaway 3-digit second count.
func formatElapsed(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
