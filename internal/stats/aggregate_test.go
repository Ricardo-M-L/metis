package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

// writeFakeSession drops a synthetic session jsonl into dir. The
// fixture mimics the real metis format: one header line + N message
// lines. Used by every aggregator test below.
func writeFakeSession(t *testing.T, dir, id, model string, started time.Time, msgs []fakeMsg) {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	hdr := pubsess.Header{
		ID: id, CreatedAt: started, Model: model,
	}
	_ = enc.Encode(map[string]any{"type": "header", "header": &hdr})
	for _, m := range msgs {
		_ = enc.Encode(map[string]any{
			"type": "message",
			"message": map[string]any{
				"role": m.role,
				"content": []map[string]any{
					{"type": "text", "text": m.text},
				},
			},
		})
	}
}

type fakeMsg struct {
	role string
	text string
}

func TestAggregate_EmptyDirReturnsZeroes(t *testing.T) {
	dir := t.TempDir()
	s, err := Aggregate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Sessions != 0 {
		t.Errorf("empty dir → sessions=%d, want 0", s.Total.Sessions)
	}
	if len(s.ByDayOfWeek) != 7 {
		t.Errorf("dow buckets always 7, got %d", len(s.ByDayOfWeek))
	}
	if len(s.ByHour) != 24 {
		t.Errorf("hour buckets always 24, got %d", len(s.ByHour))
	}
}

func TestAggregate_NonExistentDirReturnsZeroes(t *testing.T) {
	s, err := Aggregate("/var/path/that/does/not/exist/at/all")
	if err != nil {
		t.Fatalf("missing dir should not error, got: %v", err)
	}
	if s.Total.Sessions != 0 {
		t.Errorf("missing dir → sessions=%d, want 0", s.Total.Sessions)
	}
	if len(s.RecentDays) != 30 {
		t.Errorf("missing dir → recent-day buckets=%d, want 30", len(s.RecentDays))
	}
}

func TestAggregate_CountsSessionsAndMessages(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFakeSession(t, dir, "s1", "claude-3-5-sonnet", now, []fakeMsg{
		{"user", "hello"},
		{"assistant", "hi"},
		{"user", "more"},
	})
	writeFakeSession(t, dir, "s2", "claude-3-5-sonnet", now, []fakeMsg{
		{"user", "x"},
		{"assistant", "y"},
	})

	s, err := Aggregate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Sessions != 2 {
		t.Errorf("sessions: got %d, want 2", s.Total.Sessions)
	}
	if s.Total.Messages != 5 {
		t.Errorf("messages: got %d, want 5", s.Total.Messages)
	}
}

func TestAggregate_IgnoresTimingSidecarsAndZeroMessageSessions(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	writeFakeSession(t, dir, "real", "real-model", started, []fakeMsg{{"user", "hello"}})
	writeFakeSession(t, dir, "header-only", "empty-model", started, nil)

	timing := `{"ts":"2026-08-01T09:00:01Z","tool":"Read","elapsed_ms":12}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "real.timing.jsonl"), []byte(timing), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := aggregateAt(dir, started.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Sessions != 1 || s.Total.Messages != 1 || s.Total.ActiveDays != 1 {
		t.Fatalf("auxiliary/empty files polluted totals: %+v", s.Total)
	}
	if len(s.ByModel) != 1 || s.ByModel[0].Model != "real-model" || s.ByModel[0].Sessions != 1 {
		t.Fatalf("auxiliary/empty files polluted model counts: %+v", s.ByModel)
	}
	if len(s.RecentSessions) != 1 || s.RecentSessions[0].ID != "real" {
		t.Fatalf("auxiliary/empty files polluted recent sessions: %+v", s.RecentSessions)
	}
}

func TestAggregate_KeepsMessageBearingHeaderlessCrashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orphan.jsonl")
	body := `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"recovered"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, started, started); err != nil {
		t.Fatal(err)
	}

	s, err := aggregateAt(dir, started.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Sessions != 1 || s.Total.Messages != 1 {
		t.Fatalf("message-bearing crash file was discarded: %+v", s.Total)
	}
	if len(s.RecentSessions) != 1 || s.RecentSessions[0].ID != "orphan" {
		t.Fatalf("recovered row = %+v", s.RecentSessions)
	}
	if len(s.ByModel) != 1 || s.ByModel[0].Model != "(unknown)" {
		t.Fatalf("recovered model bucket = %+v", s.ByModel)
	}
}

func TestAggregate_RecentDaysUsesCurrentNaturalWindowAndIgnoresFutureSessions(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 13, 15, 30, 0, 0, time.UTC)
	writeFakeSession(t, dir, "old", "old-model", now.AddDate(0, 0, -60), []fakeMsg{{"user", "old"}})
	writeFakeSession(t, dir, "recent", "recent-model", now.AddDate(0, 0, -10), []fakeMsg{{"user", "recent"}})
	writeFakeSession(t, dir, "today", "today-model", now.Add(-time.Hour), []fakeMsg{{"user", "today"}})
	writeFakeSession(t, dir, "future", "future-model", now.AddDate(0, 0, 1), []fakeMsg{{"user", "future"}})

	s, err := aggregateAt(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Sessions != 3 || s.Total.Messages != 3 {
		t.Fatalf("future session polluted totals: %+v", s.Total)
	}
	if len(s.RecentDays) != 30 {
		t.Fatalf("recent-day bucket count = %d, want 30", len(s.RecentDays))
	}
	wantFirst := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	wantLast := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if !s.RecentDays[0].Date.Equal(wantFirst) || !s.RecentDays[len(s.RecentDays)-1].Date.Equal(wantLast) {
		t.Fatalf("recent window = %s..%s, want %s..%s",
			s.RecentDays[0].Date, s.RecentDays[len(s.RecentDays)-1].Date, wantFirst, wantLast)
	}
	recentSessions := 0
	for _, day := range s.RecentDays {
		recentSessions += day.Sessions
	}
	if recentSessions != 2 {
		t.Fatalf("recent sessions = %d, want 2", recentSessions)
	}
	for _, model := range s.ByModel {
		if model.Model == "future-model" {
			t.Fatalf("future model leaked into model counts: %+v", s.ByModel)
		}
	}
}

func TestAggregate_GroupsByModel(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFakeSession(t, dir, "s1", "claude-3-5-sonnet", now, []fakeMsg{{"user", "a"}})
	writeFakeSession(t, dir, "s2", "claude-3-5-sonnet", now, []fakeMsg{{"user", "b"}})
	writeFakeSession(t, dir, "s3", "deepseek-r1", now, []fakeMsg{{"user", "c"}})

	s, _ := Aggregate(dir)
	if len(s.ByModel) != 2 {
		t.Fatalf("expected 2 distinct models, got %d: %+v", len(s.ByModel), s.ByModel)
	}
	// sorted desc by sessions — sonnet first.
	if s.ByModel[0].Model != "claude-3-5-sonnet" || s.ByModel[0].Sessions != 2 {
		t.Errorf("top model wrong: %+v", s.ByModel[0])
	}
	if s.ByModel[1].Model != "deepseek-r1" || s.ByModel[1].Sessions != 1 {
		t.Errorf("second model wrong: %+v", s.ByModel[1])
	}
}

func TestAggregate_GroupsByDayOfWeek(t *testing.T) {
	dir := t.TempDir()
	// 2026-05-04 was a Monday, 2026-05-06 was a Wednesday — pick
	// dates whose weekday is well-known so the assertion is firm.
	mon := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	wed := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	writeFakeSession(t, dir, "mon1", "m", mon, []fakeMsg{{"user", "a"}})
	writeFakeSession(t, dir, "mon2", "m", mon, []fakeMsg{{"user", "b"}})
	writeFakeSession(t, dir, "wed1", "m", wed, []fakeMsg{{"user", "c"}})

	s, _ := aggregateAt(dir, wed.AddDate(0, 0, 1))
	dowMap := map[string]int{}
	for _, d := range s.ByDayOfWeek {
		dowMap[d.Day] = d.Sessions
	}
	if dowMap["Mon"] != 2 {
		t.Errorf("Mon: got %d, want 2 (full bucket: %+v)", dowMap["Mon"], s.ByDayOfWeek)
	}
	if dowMap["Wed"] != 1 {
		t.Errorf("Wed: got %d, want 1", dowMap["Wed"])
	}
	if dowMap["Tue"] != 0 {
		t.Errorf("Tue: got %d, want 0 (empty days should still bucket)", dowMap["Tue"])
	}
}

func TestAggregate_GroupsByHour(t *testing.T) {
	dir := t.TempDir()
	at9 := time.Date(2026, 5, 4, 9, 30, 0, 0, time.UTC)
	at14 := time.Date(2026, 5, 4, 14, 0, 0, 0, time.UTC)
	writeFakeSession(t, dir, "h9_1", "m", at9, []fakeMsg{{"user", "a"}})
	writeFakeSession(t, dir, "h9_2", "m", at9, []fakeMsg{{"user", "b"}})
	writeFakeSession(t, dir, "h14", "m", at14, []fakeMsg{{"user", "c"}})

	s, _ := aggregateAt(dir, at14.AddDate(0, 0, 1))
	hourMap := map[int]int{}
	for _, h := range s.ByHour {
		hourMap[h.Hour] = h.Sessions
	}
	if hourMap[9] != 2 {
		t.Errorf("9:00 bucket: got %d, want 2", hourMap[9])
	}
	if hourMap[14] != 1 {
		t.Errorf("14:00 bucket: got %d, want 1", hourMap[14])
	}
}

func TestAggregate_HourDowMatrix(t *testing.T) {
	dir := t.TempDir()
	// Mon=0, Wed=2 in our matrix indexing (Sunday→6, Monday→0, …).
	mon10 := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	wed14 := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	writeFakeSession(t, dir, "mon10", "m", mon10, []fakeMsg{{"user", "a"}})
	writeFakeSession(t, dir, "wed14", "m", wed14, []fakeMsg{{"user", "b"}})
	writeFakeSession(t, dir, "wed14b", "m", wed14, []fakeMsg{{"user", "c"}})

	s, _ := aggregateAt(dir, wed14.AddDate(0, 0, 1))
	if s.HourDowMatrix[0][10] != 1 {
		t.Errorf("[Mon][10]: got %d, want 1", s.HourDowMatrix[0][10])
	}
	if s.HourDowMatrix[2][14] != 2 {
		t.Errorf("[Wed][14]: got %d, want 2", s.HourDowMatrix[2][14])
	}
	if s.HourDowMatrix[3][14] != 0 {
		t.Errorf("[Thu][14]: got %d, want 0 (no fixture for it)", s.HourDowMatrix[3][14])
	}
}

func TestAggregate_RecentSessionsNewestFirstCappedAt20(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		writeFakeSession(t, dir, fmt.Sprintf("s%02d", i), "m",
			base.Add(time.Duration(i)*time.Hour), []fakeMsg{{"user", "message"}})
	}
	s, _ := aggregateAt(dir, base.AddDate(0, 0, 3))
	if len(s.RecentSessions) != 20 {
		t.Errorf("recent cap: got %d, want 20", len(s.RecentSessions))
	}
	// First entry should be the latest start.
	for i := 1; i < len(s.RecentSessions); i++ {
		if s.RecentSessions[i].Started.After(s.RecentSessions[i-1].Started) {
			t.Errorf("recent[%d] %v > recent[%d] %v — order wrong",
				i, s.RecentSessions[i].Started, i-1, s.RecentSessions[i-1].Started)
		}
	}
}

func TestAggregate_TokenApproximation(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 100-char user message + 200-char assistant message → 25 in / 50 out.
	writeFakeSession(t, dir, "tok", "m", now, []fakeMsg{
		{"user", strings.Repeat("a", 100)},
		{"assistant", strings.Repeat("b", 200)},
	})
	s, _ := Aggregate(dir)
	if s.Total.ApproxTokensIn != 25 {
		t.Errorf("approx in: got %d, want 25 (100/4)", s.Total.ApproxTokensIn)
	}
	if s.Total.ApproxTokensOut != 50 {
		t.Errorf("approx out: got %d, want 50 (200/4)", s.Total.ApproxTokensOut)
	}
}

func TestAggregate_HistoryReplaceCountsOnlyLiveSnapshotAndTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replaced.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	started := time.Now()
	if err := enc.Encode(map[string]any{
		"type":   "header",
		"header": pubsess.Header{ID: "replaced", CreatedAt: started, Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	encodeMessage := func(role, text string) {
		t.Helper()
		if err := enc.Encode(map[string]any{
			"type": "message",
			"message": map[string]any{
				"role":    role,
				"content": []map[string]any{{"type": "text", "text": text}},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	encodeMessage("user", strings.Repeat("x", 400))
	encodeMessage("assistant", strings.Repeat("y", 800))
	if err := enc.Encode(map[string]any{
		"type": "history_replace",
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": strings.Repeat("n", 40)}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	encodeMessage("assistant", strings.Repeat("z", 80))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Aggregate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total.Messages != 2 {
		t.Fatalf("messages = %d, want snapshot(1) + tail(1)", s.Total.Messages)
	}
	if s.Total.ApproxTokensIn != 10 || s.Total.ApproxTokensOut != 20 {
		t.Fatalf("tokens include replaced history: in=%d out=%d", s.Total.ApproxTokensIn, s.Total.ApproxTokensOut)
	}
}

func TestAggregate_ActiveDaysCount(t *testing.T) {
	dir := t.TempDir()
	d1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) // skip d2 = active days = 2
	writeFakeSession(t, dir, "d1a", "m", d1, []fakeMsg{{"user", "a"}})
	writeFakeSession(t, dir, "d1b", "m", d1, []fakeMsg{{"user", "b"}})
	writeFakeSession(t, dir, "d2", "m", d2, []fakeMsg{{"user", "c"}})

	s, _ := aggregateAt(dir, d2.AddDate(0, 0, 1))
	if s.Total.ActiveDays != 2 {
		t.Errorf("active days: got %d, want 2", s.Total.ActiveDays)
	}
}

func TestAggregate_MalformedJSONLineSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.jsonl")
	now := time.Now()
	hdr, _ := json.Marshal(map[string]any{"type": "header", "header": map[string]any{"id": "broken", "created_at": now.Format(time.RFC3339), "model": "m"}})
	body := string(hdr) + "\nthis is not json\n"
	body += `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ok"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Aggregate(dir)
	if err != nil {
		t.Fatalf("malformed lines must not break aggregation: %v", err)
	}
	if s.Total.Sessions != 1 {
		t.Errorf("sessions: got %d, want 1 (file must still count)", s.Total.Sessions)
	}
	if s.Total.Messages != 1 {
		t.Errorf("messages: got %d, want 1 (only the valid one counted)", s.Total.Messages)
	}
}

// ─── render integration ─────────────────────────────────────────

func TestRender_ProducesValidHTML(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeFakeSession(t, dir, "demo", "claude-3-5-sonnet", now, []fakeMsg{
		{"user", "hello"},
	})
	s, _ := Aggregate(dir)
	html, err := Render(s)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: should be a complete HTML page.
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("missing doctype")
	}
	if !strings.Contains(html, "metis · usage stats") {
		t.Error("missing page title")
	}
	if !strings.Contains(html, "claude-3-5-sonnet") {
		t.Error("model name not rendered")
	}
	// Heatmap grid must have all 7 day rows.
	for _, dow := range []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"} {
		if !strings.Contains(html, ">"+dow+"<") {
			t.Errorf("dow row %q missing", dow)
		}
	}
}

func TestRender_EmptyStatsStillRenders(t *testing.T) {
	s, _ := Aggregate(t.TempDir())
	html, err := Render(s)
	if err != nil {
		t.Fatalf("empty stats should render cleanly: %v", err)
	}
	// Page should still exist with summary section showing zeros.
	if !strings.Contains(html, "Summary") {
		t.Error("summary section missing on empty input")
	}
}

// ─── helper-fn unit tests ───────────────────────────────────────

func TestPct(t *testing.T) {
	cases := []struct{ num, max, want int }{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{200, 100, 100}, // capped
		{5, 0, 0},       // div-by-zero guard
		{-1, 100, 0},
	}
	for _, c := range cases {
		if got := pct(c.num, c.max); got != c.want {
			t.Errorf("pct(%d,%d)=%d, want %d", c.num, c.max, got, c.want)
		}
	}
}

func TestShorten(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 5, "abcd…"},
		{"αβγδε", 4, "αβγ…"}, // multi-byte must clip on rune boundary
		{"x", 1, "x"},
		{"toolong", 1, "…"},
	}
	for _, c := range cases {
		if got := shorten(c.in, c.max); got != c.want {
			t.Errorf("shorten(%q,%d)=%q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestThousands(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1 000"},
		{1234567, "1 234 567"},
	}
	for _, c := range cases {
		if got := thousands(c.n); got != c.want {
			t.Errorf("thousands(%d)=%q, want %q", c.n, got, c.want)
		}
	}
}

func TestHeatColor_EmptyCellGetsTransparent(t *testing.T) {
	c := heatColor(0, 10)
	if !strings.Contains(string(c), "rgba") {
		t.Errorf("empty cell should be rgba, got %q", c)
	}
}

func TestHeatColor_FullCellGetsHigh(t *testing.T) {
	c := heatColor(10, 10)
	if !strings.Contains(string(c), "hsl") {
		t.Errorf("full cell should be hsl, got %q", c)
	}
}
