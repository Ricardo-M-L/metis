package webui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/session"
)

// These figures are locally persisted provider-reported tokens, not billing
// data. Input and the two cache buckets are disjoint in the session ledger.
type usageBuckets struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CacheReadTokens   int64 `json:"cacheReadTokens"`
	CacheCreateTokens int64 `json:"cacheCreateTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

type usageDay struct {
	Date string `json:"date"`
	usageBuckets
	Turns      int   `json:"turns"`
	DurationMs int64 `json:"durationMs"`
}

type usageModel struct {
	Model string `json:"model"`
	usageBuckets
}

type usageCoverage struct {
	Sessions                  int   `json:"sessions"`
	SessionsWithUsage         int   `json:"sessionsWithUsage"`
	SessionsWithoutUsage      int   `json:"sessionsWithoutUsage"`
	SessionsWithDuration      int   `json:"sessionsWithDuration"`
	InvalidDurationTurns      int   `json:"invalidDurationTurns"`
	MetricOnlySessions        int   `json:"metricOnlySessions"`
	UnreadableSessions        int   `json:"unreadableSessions"`
	UnattributedTokens        int64 `json:"unattributedTokens"`
	InconsistentSessions      int   `json:"inconsistentSessions"`
	InvalidMetricTurns        int   `json:"invalidMetricTurns"`
	UnattributedModelTokens   int64 `json:"unattributedModelTokens"`
	ModelInconsistentSessions int   `json:"modelInconsistentSessions"`
	ModelUnreadableSessions   int   `json:"modelUnreadableSessions"`
}

type usageStatistics struct {
	GeneratedAt             time.Time     `json:"generatedAt"`
	Scope                   string        `json:"scope"`
	Total                   usageBuckets  `json:"total"`
	Daily                   []usageDay    `json:"daily"`
	Models                  []usageModel  `json:"models"`
	Coverage                usageCoverage `json:"coverage"`
	LongestTurnMs           int64         `json:"longestTurnMs"`
	LongestSessionMs        int64         `json:"longestSessionMs"`
	CurrentStreak           int           `json:"currentStreak"`
	LongestStreak           int           `json:"longestStreak"`
	PeakDayTokens           int64         `json:"peakDayTokens"`
	ModelBreakdownAvailable bool          `json:"modelBreakdownAvailable"`
}

func usageCost(c session.CostSnapshot) usageBuckets {
	return usageBuckets{InputTokens: int64(max(0, c.InputTokens)), OutputTokens: int64(max(0, c.OutputTokens)), CacheReadTokens: int64(max(0, c.CacheReadTokens)), CacheCreateTokens: int64(max(0, c.CacheCreateTokens))}
}
func usageMetric(m session.MessageMetric) usageBuckets {
	return usageBuckets{InputTokens: max(0, m.InputTokens), OutputTokens: max(0, m.OutputTokens), CacheReadTokens: max(0, m.CacheReadTokens), CacheCreateTokens: max(0, m.CacheCreateTokens)}
}
func (b *usageBuckets) add(v usageBuckets) {
	b.InputTokens += v.InputTokens
	b.OutputTokens += v.OutputTokens
	b.CacheReadTokens += v.CacheReadTokens
	b.CacheCreateTokens += v.CacheCreateTokens
	b.TotalTokens = b.InputTokens + b.OutputTokens + b.CacheReadTokens + b.CacheCreateTokens
}

// A malformed trace must never wrap around and bypass ledger bounds.
func (b *usageBuckets) checkedAdd(v usageBuckets) bool {
	if v.InputTokens < 0 || v.OutputTokens < 0 || v.CacheReadTokens < 0 || v.CacheCreateTokens < 0 ||
		b.InputTokens > math.MaxInt64-v.InputTokens || b.OutputTokens > math.MaxInt64-v.OutputTokens ||
		b.CacheReadTokens > math.MaxInt64-v.CacheReadTokens || b.CacheCreateTokens > math.MaxInt64-v.CacheCreateTokens {
		return false
	}
	next := usageBuckets{InputTokens: b.InputTokens + v.InputTokens, OutputTokens: b.OutputTokens + v.OutputTokens,
		CacheReadTokens: b.CacheReadTokens + v.CacheReadTokens, CacheCreateTokens: b.CacheCreateTokens + v.CacheCreateTokens}
	var total int64
	for _, n := range []int64{next.InputTokens, next.OutputTokens, next.CacheReadTokens, next.CacheCreateTokens} {
		if total > math.MaxInt64-n {
			return false
		}
		total += n
	}
	next.TotalTokens = total
	*b = next
	return true
}
func usageExceeds(a, b usageBuckets) bool {
	return a.InputTokens > b.InputTokens || a.OutputTokens > b.OutputTokens || a.CacheReadTokens > b.CacheReadTokens || a.CacheCreateTokens > b.CacheCreateTokens
}
func usageUnattributed(total, timed usageBuckets) int64 {
	return max(0, total.InputTokens-timed.InputTokens) + max(0, total.OutputTokens-timed.OutputTokens) + max(0, total.CacheReadTokens-timed.CacheReadTokens) + max(0, total.CacheCreateTokens-timed.CacheCreateTokens)
}

// Read only the first record, never the transcript or its title. Child-agent
// files are excluded because their usage is already attributed to the parent.
func usageSessionHeader(path, id string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 4096), 8<<20)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return false, err
		}
		return false, nil
	}
	var entry struct {
		Type   string `json:"type"`
		Header *struct {
			ID         string `json:"id"`
			SubAgentOf string `json:"sub_agent_of"`
		} `json:"header"`
	}
	if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
		return false, err
	}
	return entry.Type == "header" && entry.Header != nil && entry.Header.ID == id && entry.Header.SubAgentOf == "", nil
}

func mergeUsageMetric(old, next session.MessageMetric) session.MessageMetric {
	old.Turn = next.Turn
	old.InputTokens = max(old.InputTokens, next.InputTokens)
	old.OutputTokens = max(old.OutputTokens, next.OutputTokens)
	old.CacheReadTokens = max(old.CacheReadTokens, next.CacheReadTokens)
	old.CacheCreateTokens = max(old.CacheCreateTokens, next.CacheCreateTokens)
	if !next.StartedAt.IsZero() && (old.StartedAt.IsZero() || next.StartedAt.Before(old.StartedAt)) {
		old.StartedAt = next.StartedAt
	}
	if !next.CompletedAt.IsZero() && (old.CompletedAt.IsZero() || next.CompletedAt.After(old.CompletedAt)) {
		old.CompletedAt = next.CompletedAt
	}
	old.DurationMS = max(old.DurationMS, next.DurationMS)
	return old
}

func collectUsageStatistics(store *session.Store, now time.Time, loc *time.Location, traceStores ...*session.TraceStore) (usageStatistics, error) {
	out := usageStatistics{GeneratedAt: now, Scope: "local", Daily: []usageDay{}, Models: []usageModel{}}
	if store == nil {
		return out, os.ErrInvalid
	}
	entries, err := os.ReadDir(store.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	days := make(map[string]*usageDay)
	models := make(map[string]*usageModel)
	var traceStore *session.TraceStore
	if len(traceStores) > 0 {
		traceStore = traceStores[0]
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".timing.jsonl") || strings.HasSuffix(name, ".message-metrics.jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !validSessionID(id) {
			continue
		}
		ok, err := usageSessionHeader(filepath.Join(store.Dir, name), id)
		if err != nil {
			out.Coverage.UnreadableSessions++
			continue
		}
		if !ok {
			continue
		}
		out.Coverage.Sessions++
		// The trace adapter appends a token event before its observer commits
		// the ledger and turn metric. Snapshot in the same order: a trace row
		// written after this point waits for the next refresh.
		var events []session.TraceEvent
		if traceStore != nil {
			var traceErr error
			events, traceErr = traceStore.ReadUsageEvents(id)
			if traceErr != nil {
				out.Coverage.ModelUnreadableSessions++
				events = nil
			}
		}
		metrics, metricErr := store.ReadMessageMetrics(id)
		// A turn reconciliation commits its cost ledger before its metric
		// sidecar. Read the ledger last to avoid a transient false mismatch
		// when a concurrent writer is finalizing the same turn.
		cost, hasCost, costErr := store.ReadCost(id)
		if costErr != nil || metricErr != nil {
			out.Coverage.UnreadableSessions++
		}
		if costErr != nil {
			hasCost = false
		}
		if !hasCost {
			out.Coverage.SessionsWithoutUsage++
		} else {
			out.Coverage.SessionsWithUsage++
		}
		byTurn := make(map[int]session.MessageMetric, len(metrics))
		for _, m := range metrics {
			if m.Turn > 0 {
				byTurn[m.Turn] = mergeUsageMetric(byTurn[m.Turn], m)
			}
		}
		var timed, allMetrics usageBuckets
		var sessionDuration int64
		hasSessionDuration := false
		durationOverflow := false
		for _, m := range byTurn {
			b := usageMetric(m)
			allMetrics.add(b)
			if m.StartedAt.IsZero() || m.CompletedAt.IsZero() || m.CompletedAt.Before(m.StartedAt) {
				out.Coverage.InvalidMetricTurns++
				continue
			}
			date := m.CompletedAt.In(loc).Format("2006-01-02")
			d := days[date]
			if d == nil {
				d = &usageDay{Date: date}
				days[date] = d
			}
			d.add(b)
			d.Turns++
			timed.add(b)
			// No wall-clock estimate: duration must have been persisted for a
			// completed turn. Duplicate rows are merged by turn above.
			if m.DurationMS > 0 {
				if sessionDuration > math.MaxInt64-m.DurationMS || d.DurationMs > math.MaxInt64-m.DurationMS {
					out.Coverage.InvalidDurationTurns++
					durationOverflow = true
					continue
				}
				sessionDuration += m.DurationMS
				d.DurationMs += m.DurationMS
				hasSessionDuration = true
				out.LongestTurnMs = max(out.LongestTurnMs, m.DurationMS)
			}
		}
		if hasSessionDuration && !durationOverflow {
			out.Coverage.SessionsWithDuration++
			out.LongestSessionMs = max(out.LongestSessionMs, sessionDuration)
		}
		ledger := usageCost(cost)
		if !hasCost && len(byTurn) > 0 {
			ledger = allMetrics
			out.Coverage.MetricOnlySessions++
		}
		if hasCost && usageExceeds(allMetrics, ledger) {
			out.Coverage.InconsistentSessions++
		}
		out.Total.add(ledger)
		out.Coverage.UnattributedTokens += usageUnattributed(ledger, timed)
		// Trace rows are increments while metrics are per-turn absolutes.
		// A session-wide bound alone is unsafe: an uncommitted current turn
		// could borrow the balance of an earlier, unattributed legacy turn.
		type traceTurnUsage struct {
			total   usageBuckets
			models  map[string]usageBuckets
			invalid bool
		}
		traceTurns := make(map[int]*traceTurnUsage)
		for _, event := range events {
			turn := traceTurns[event.Turn]
			if turn == nil {
				turn = &traceTurnUsage{models: make(map[string]usageBuckets)}
				traceTurns[event.Turn] = turn
			}
			var input, output, cacheWrite, cacheRead int64
			if n, err := fmt.Sscanf(event.Text, "input=%d output=%d cache_write=%d cache_read=%d", &input, &output, &cacheWrite, &cacheRead); err != nil || n != 4 || input < 0 || output < 0 || cacheWrite < 0 || cacheRead < 0 {
				turn.invalid = true
				continue
			}
			b := usageBuckets{InputTokens: input, OutputTokens: output, CacheCreateTokens: cacheWrite, CacheReadTokens: cacheRead}
			if !turn.total.checkedAdd(b) {
				turn.invalid = true
				continue
			}
			model := strings.TrimSpace(event.UsageModel)
			if model == "" || len(model) > 512 {
				continue
			}
			v := turn.models[model]
			if !v.checkedAdd(b) {
				turn.invalid = true
				continue
			}
			turn.models[model] = v
		}
		perModel := make(map[string]usageBuckets)
		var attributed usageBuckets
		inconsistent := hasCost && usageExceeds(allMetrics, ledger)
		turnInconsistent := false
		for number, turn := range traceTurns {
			metric, exists := byTurn[number]
			if !hasCost || metricErr != nil || !exists {
				continue
			}
			if turn.invalid || usageExceeds(turn.total, usageMetric(metric)) {
				turnInconsistent = true
				continue
			}
			for model, value := range turn.models {
				v := perModel[model]
				if !v.checkedAdd(value) || !attributed.checkedAdd(value) {
					inconsistent = true
					break
				}
				perModel[model] = v
			}
		}
		if inconsistent || usageExceeds(attributed, ledger) {
			inconsistent = true
			perModel = nil
			attributed = usageBuckets{}
		}
		if inconsistent || turnInconsistent {
			out.Coverage.ModelInconsistentSessions++
		}
		for name, value := range perModel {
			m := models[name]
			if m == nil {
				m = &usageModel{Model: name}
				models[name] = m
			}
			m.add(value)
			out.ModelBreakdownAvailable = true
		}
		out.Coverage.UnattributedModelTokens += usageUnattributed(ledger, attributed)
	}
	for _, day := range days {
		out.Daily = append(out.Daily, *day)
		out.PeakDayTokens = max(out.PeakDayTokens, day.TotalTokens)
	}
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Date < out.Daily[j].Date })
	for _, model := range models {
		out.Models = append(out.Models, *model)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].TotalTokens != out.Models[j].TotalTokens {
			return out.Models[i].TotalTokens > out.Models[j].TotalTokens
		}
		return out.Models[i].Model < out.Models[j].Model
	})
	streak := 0
	var previous time.Time
	for _, day := range out.Daily {
		if day.TotalTokens <= 0 {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", day.Date, loc)
		if err != nil {
			continue
		}
		if !previous.IsZero() && previous.AddDate(0, 0, 1).Equal(date) {
			streak++
		} else {
			streak = 1
		}
		out.LongestStreak = max(out.LongestStreak, streak)
		previous = date
	}
	today := now.In(loc)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	if !previous.IsZero() && (previous.Equal(today) || previous.Equal(today.AddDate(0, 0, -1))) {
		out.CurrentStreak = streak
	}
	return out, nil
}

func (s *Server) handleUsageStatistics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "session store unavailable")
		return
	}
	// Repeated visits and concurrent tabs share a brief snapshot; the page is
	// explicitly read-only and new usage becomes visible on the next refresh.
	s.usageStatsMu.Lock()
	defer s.usageStatsMu.Unlock()
	now := time.Now()
	if s.usageStatsCached == nil || now.Sub(s.usageStatsCachedAt) > 5*time.Second {
		value, err := collectUsageStatistics(s.store, now, time.Local, s.traceStore)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "usage statistics unavailable")
			return
		}
		s.usageStatsCached = &value
		s.usageStatsCachedAt = now
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.usageStatsCached)
}
