package stats

// render.go — turn an aggregated *Stats into a self-contained HTML
// page. Embedded template (template.html), inline CSS, no external
// JS dependencies — the file works offline, can be emailed, opens
// from `file://` without any network calls.
//
// Why pure inline + no JS framework: claude-code does the equivalent
// in terminal Ink+asciichart; crush in HTML+SQL+embedded JS. We
// chose HTML to match crush's "share via email / open from disk"
// affordance, and pure-Go template rendering to match metis's
// "single binary, works offline" ethos. Charts are CSS-styled <div>
// bars (no chart lib). Heatmap is a CSS grid with per-cell color.

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strings"
	"time"
)

//go:embed template.html
var rawTemplate string

// renderableStats is *Stats plus the fields the template needs that
// aren't present on the original (max values for bar normalisation,
// etc.). Built once per Render call.
type renderableStats struct {
	*Stats
	HeatmapMax    int
	RecentDaysMax int
}

// Render writes the dashboard HTML for s to w. Returns an error only
// for template parse / exec failures — neither happens at runtime
// once the package compiles, but the signature stays io.Writer to
// keep the call site flexible.
func Render(s *Stats) (string, error) {
	r := &renderableStats{Stats: s}

	// Find the heatmap max so heatColor can normalize.
	for _, row := range s.HourDowMatrix {
		for _, v := range row {
			if v > r.HeatmapMax {
				r.HeatmapMax = v
			}
		}
	}
	for _, d := range s.RecentDays {
		if d.Sessions > r.RecentDaysMax {
			r.RecentDaysMax = d.Sessions
		}
	}

	tpl, err := template.New("stats").Funcs(template.FuncMap{
		"pct":       pct,
		"shorten":   shorten,
		"thousands": thousands,
		"iter":      iter,
		"dowShort":  dowShort,
		"heatColor": heatColor,
	}).Parse(rawTemplate)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, r); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// pct returns 100 * num / max, capped to 100. Used by bar-width
// styling. Zero max → 0 (rather than divide-by-zero NaN).
func pct(num, max int) int {
	if max <= 0 {
		return 0
	}
	v := num * 100 / max
	if v > 100 {
		return 100
	}
	if v < 0 {
		return 0
	}
	return v
}

// shorten clips s to maxLen runes with a trailing ellipsis when
// truncation happens. Used so model names like "claude-3-5-sonnet-
// 20241022" don't blow out the bar's left label column.
func shorten(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	return string(r[:maxLen-1]) + "…"
}

// thousands inserts a space every 3 digits — "1234567" → "1 234 567".
// More readable than commas for European audiences and works in any
// terminal font. Negative numbers are returned as-is (rare here —
// stats are counts).
func thousands(n int) string {
	if n < 0 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
		if len(s) > rem {
			b.WriteByte(' ')
		}
	}
	for i := rem; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// iter returns [0, 1, ..., n-1] for {{range $h := iter 24}} usage in
// the template. Go's html/template has no built-in range-over-N.
func iter(n int) []int {
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = i
	}
	return out
}

// dowShort turns a 0..6 (Mon=0) index into a 3-letter weekday
// label. Mirrors aggregate.go's HourDowMatrix indexing convention.
func dowShort(d int) string {
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	if d < 0 || d >= len(names) {
		return "?"
	}
	return names[d]
}

// heatColor returns a CSS color string for a heatmap cell with
// `count` sessions, normalized against `max`. Linear gradient from
// the page's --heat-low to --heat-high. Empty cells get a muted dim
// instead of pure --heat-low so the grid is still visible.
//
// We build the color in HSL space and pick the lightness based on
// intensity. Hue stays at the metis blue (~213°).
func heatColor(count, max int) template.CSS {
	if count <= 0 {
		return "rgba(255,255,255,0.04)"
	}
	if max <= 0 {
		max = 1
	}
	intensity := float64(count) / float64(max)
	// Lightness 12% (almost dark) → 60% (bright) blue. Saturation
	// fixed at 80%.
	lightness := 12 + intensity*48
	return template.CSS(fmt.Sprintf("hsl(213, 80%%, %.0f%%)", lightness))
}

// renderableTimeFormat — exported so callers / tests can normalise
// rendered timestamps the same way the template does.
const renderableTimeFormat = "2006-01-02 15:04"

// FormatTimestamp matches the dashboard's preferred timestamp shape
// for callers that want to print "stats generated at X" alongside
// the HTML output.
func FormatTimestamp(t time.Time) string {
	return t.Format(renderableTimeFormat)
}
