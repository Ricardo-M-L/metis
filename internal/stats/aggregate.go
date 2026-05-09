package stats

// aggregate.go — read ~/.metis/sessions/*.jsonl, fold into a Stats
// struct ready to be rendered by render.go. The dashboard `metis
// stats` is meant to answer "when / how / with what model do I use
// metis", so we build the dimensions (day-of-week, hour, model,
// recent activity, hour×dow heatmap) that match those questions.
//
// We don't have a SQLite layer like crush — sessions are jsonl. So
// the aggregator reads the session header (model, started_at) plus
// message count from each .jsonl file. Token usage isn't recorded
// in session jsonl (it lives in transient cache_stats.go in-memory
// or in dump-prompts files when METIS_DUMP_PROMPTS=1). For MVP we
// approximate token usage by summing content character length / 4
// — the standard "1 token ≈ 4 chars" English rule. Imprecise, but
// good enough to show "this week vs last week" trends.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

// Stats is the everything-aggregated shape rendered by the
// dashboard. Lays out one slot per visualization on the page so the
// template doesn't have to recompute anything.
type Stats struct {
	GeneratedAt time.Time

	Total Total

	// Per-axis breakdowns.
	ByModel       []ModelUsage
	ByDayOfWeek   []DowUsage
	ByHour        []HourUsage
	HourDowMatrix [7][24]int // [dow][hour] = session count

	// Time series for the latest 30 days.
	RecentDays []DayActivity

	// Recent sessions table — last 20 sessions, newest first.
	RecentSessions []SessionRow
}

// Total — top-of-page summary numbers.
type Total struct {
	Sessions          int
	Messages          int
	ApproxTokensIn    int // sum(user-content-chars) / 4
	ApproxTokensOut   int // sum(assistant-content-chars) / 4
	ActiveDays        int
	OldestSessionDate time.Time
	NewestSessionDate time.Time
}

// ModelUsage — one row in the "model breakdown" section.
type ModelUsage struct {
	Model    string
	Sessions int
	Messages int
}

// DowUsage — one bar in the day-of-week chart. Day is "Mon"…"Sun".
type DowUsage struct {
	Day      string
	Sessions int
}

// HourUsage — one bar in the hours-of-day chart.
type HourUsage struct {
	Hour     int
	Sessions int
}

// DayActivity — one date in the rolling 30-day timeline.
type DayActivity struct {
	Date     time.Time
	Sessions int
	Messages int
}

// SessionRow — one row in the "recent sessions" table.
type SessionRow struct {
	ID       string
	Title    string
	Model    string
	Started  time.Time
	Messages int
}

// Aggregate walks sessionsDir, reads every .jsonl file, and builds
// a Stats. Returns (empty-but-valid, nil) on a missing or empty
// directory so the caller doesn't have to special-case "fresh
// install" — the dashboard just shows zero everywhere.
func Aggregate(sessionsDir string) (*Stats, error) {
	s := &Stats{GeneratedAt: time.Now()}
	dowCount := map[time.Weekday]int{}
	hourCount := map[int]int{}
	modelCount := map[string]*ModelUsage{}
	dayCount := map[string]*DayActivity{}
	activeDays := map[string]struct{}{}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		row, err := readSessionFile(filepath.Join(sessionsDir, e.Name()))
		if err != nil || row == nil {
			continue
		}
		s.Total.Sessions++
		s.Total.Messages += row.Messages
		s.Total.ApproxTokensIn += row.tokensIn
		s.Total.ApproxTokensOut += row.tokensOut

		dow := row.Started.Weekday()
		hour := row.Started.Hour()
		dowCount[dow]++
		hourCount[hour]++
		s.HourDowMatrix[(int(dow)+6)%7][hour]++ // shift Sunday→6, Monday→0

		dayKey := row.Started.Format("2006-01-02")
		activeDays[dayKey] = struct{}{}
		if d, ok := dayCount[dayKey]; ok {
			d.Sessions++
			d.Messages += row.Messages
		} else {
			dayCount[dayKey] = &DayActivity{
				Date: time.Date(row.Started.Year(), row.Started.Month(), row.Started.Day(), 0, 0, 0, 0, row.Started.Location()),
				Sessions: 1, Messages: row.Messages,
			}
		}

		modelKey := row.Model
		if modelKey == "" {
			modelKey = "(unknown)"
		}
		if m, ok := modelCount[modelKey]; ok {
			m.Sessions++
			m.Messages += row.Messages
		} else {
			modelCount[modelKey] = &ModelUsage{
				Model: modelKey, Sessions: 1, Messages: row.Messages,
			}
		}

		s.RecentSessions = append(s.RecentSessions, SessionRow{
			ID:       row.ID,
			Title:    row.Title,
			Model:    row.Model,
			Started:  row.Started,
			Messages: row.Messages,
		})

		if s.Total.OldestSessionDate.IsZero() || row.Started.Before(s.Total.OldestSessionDate) {
			s.Total.OldestSessionDate = row.Started
		}
		if row.Started.After(s.Total.NewestSessionDate) {
			s.Total.NewestSessionDate = row.Started
		}
	}

	s.Total.ActiveDays = len(activeDays)

	// Day-of-week — fixed Mon..Sun ordering so the chart label order
	// is stable regardless of which days fired.
	dowOrder := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	dowName := map[time.Weekday]string{
		time.Monday: "Mon", time.Tuesday: "Tue", time.Wednesday: "Wed",
		time.Thursday: "Thu", time.Friday: "Fri", time.Saturday: "Sat",
		time.Sunday: "Sun",
	}
	for _, d := range dowOrder {
		s.ByDayOfWeek = append(s.ByDayOfWeek, DowUsage{Day: dowName[d], Sessions: dowCount[d]})
	}

	// Hour — 0..23 sequential.
	for h := 0; h < 24; h++ {
		s.ByHour = append(s.ByHour, HourUsage{Hour: h, Sessions: hourCount[h]})
	}

	// Models — sort by sessions desc.
	for _, m := range modelCount {
		s.ByModel = append(s.ByModel, *m)
	}
	sort.Slice(s.ByModel, func(i, j int) bool {
		return s.ByModel[i].Sessions > s.ByModel[j].Sessions
	})

	// Recent sessions — newest first, cap 20.
	sort.Slice(s.RecentSessions, func(i, j int) bool {
		return s.RecentSessions[i].Started.After(s.RecentSessions[j].Started)
	})
	if len(s.RecentSessions) > 20 {
		s.RecentSessions = s.RecentSessions[:20]
	}

	// Recent 30 days — fill from newest day backwards. Days with no
	// activity get explicit zeros so the timeline isn't gappy.
	if !s.Total.NewestSessionDate.IsZero() {
		newest := time.Date(s.Total.NewestSessionDate.Year(), s.Total.NewestSessionDate.Month(),
			s.Total.NewestSessionDate.Day(), 0, 0, 0, 0, s.Total.NewestSessionDate.Location())
		for i := 0; i < 30; i++ {
			d := newest.AddDate(0, 0, -i)
			key := d.Format("2006-01-02")
			if v, ok := dayCount[key]; ok {
				s.RecentDays = append(s.RecentDays, *v)
			} else {
				s.RecentDays = append(s.RecentDays, DayActivity{Date: d})
			}
		}
		// Reverse so the chart reads left-to-right oldest→newest.
		for i, j := 0, len(s.RecentDays)-1; i < j; i, j = i+1, j-1 {
			s.RecentDays[i], s.RecentDays[j] = s.RecentDays[j], s.RecentDays[i]
		}
	}

	return s, nil
}

// sessionRowRaw is the in-flight aggregate of one session file
// before it's split into the various Stats slots.
type sessionRowRaw struct {
	ID         string
	Title      string
	Model      string
	Started    time.Time
	Messages   int
	tokensIn   int // approximate, see content-char/4 rule
	tokensOut  int
}

// readSessionFile parses one session jsonl. Returns nil + nil on a
// header-only file (not yet user-engaged) so the aggregator can
// skip without erroring. Errors are returned only for actual I/O
// failures.
func readSessionFile(path string) (*sessionRowRaw, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	row := &sessionRowRaw{
		ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	type entry struct {
		Type    string         `json:"type"`
		Header  *pubsess.Header `json:"header,omitempty"`
		Message *llm.Message   `json:"message,omitempty"`
	}
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // skip malformed line, don't fail whole file
		}
		switch e.Type {
		case "header":
			if e.Header != nil {
				row.Started = e.Header.CreatedAt
				row.Model = e.Header.Model
				row.Title = e.Header.Title
				row.ID = e.Header.ID
			}
		case "message":
			if e.Message != nil {
				row.Messages++
				chars := 0
				for _, b := range e.Message.Content {
					chars += len(b.Text) + len(b.ToolResult)
				}
				if e.Message.Role == "user" || e.Message.Role == "tool" {
					row.tokensIn += chars / 4
				} else {
					row.tokensOut += chars / 4
				}
			}
		}
	}
	if row.Started.IsZero() {
		// Files with no header (early-write crash) — use file mtime
		// as a fallback so they still show up in the timeline rather
		// than getting silently skipped.
		if st, err := os.Stat(path); err == nil {
			row.Started = st.ModTime()
		} else {
			return nil, nil
		}
	}
	return row, nil
}
