package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/session"
	metisstats "github.com/Ricardo-M-L/metis/internal/stats"
)

type recordedUsage struct {
	sessions   int
	input      int
	output     int
	cacheWrite int
	cacheRead  int
}

// usageActivityRows summarizes the local session archive. /stats is a usage
// dashboard; the current-session diagnostics are appended by each UI surface
// as a separate section below these rows.
func usageActivityRows(store *session.Store) []infoRow {
	dir := filepath.Join(config.Home(), "sessions")
	if store != nil && strings.TrimSpace(store.Dir) != "" {
		dir = store.Dir
	} else {
		store = &session.Store{Dir: dir}
	}
	aggregate, err := metisstats.Aggregate(dir)
	if err != nil {
		return []infoRow{
			{Key: "", Value: "All Sessions"},
			{Key: "archive", Value: "unavailable", Hint: err.Error()},
		}
	}

	rows := []infoRow{
		{Key: "", Value: "All Sessions"},
		{Key: "sessions", Value: fmt.Sprintf("%d", aggregate.Total.Sessions)},
		{Key: "messages", Value: fmt.Sprintf("%d", aggregate.Total.Messages)},
		{Key: "active days", Value: fmt.Sprintf("%d", aggregate.Total.ActiveDays)},
	}
	recentSessions, recentMessages := 0, 0
	for _, day := range aggregate.RecentDays {
		recentSessions += day.Sessions
		recentMessages += day.Messages
	}
	rows = append(rows, infoRow{
		Key: "30-day activity", Value: fmt.Sprintf("%d sessions · %d messages", recentSessions, recentMessages),
	})

	usage := collectRecordedUsage(store)
	if usage.sessions > 0 {
		coverage := fmt.Sprintf("recorded for %d/%d sessions", usage.sessions, aggregate.Total.Sessions)
		rows = append(rows,
			infoRow{Key: "input tokens", Value: fmtThousands(usage.input), Hint: coverage},
			infoRow{Key: "output tokens", Value: fmtThousands(usage.output), Hint: coverage},
		)
		if usage.cacheWrite > 0 || usage.cacheRead > 0 {
			rows = append(rows,
				infoRow{Key: "cache writes", Value: fmtThousands(usage.cacheWrite)},
				infoRow{Key: "cache reads", Value: fmtThousands(usage.cacheRead)},
			)
		}
	} else if aggregate.Total.Sessions > 0 {
		rows = append(rows,
			infoRow{Key: "approx. input tokens", Value: fmtThousands(aggregate.Total.ApproxTokensIn), Hint: "transcript estimate; no cost snapshots"},
			infoRow{Key: "approx. output tokens", Value: fmtThousands(aggregate.Total.ApproxTokensOut), Hint: "transcript estimate; no cost snapshots"},
		)
	}

	if len(aggregate.ByModel) > 0 {
		limit := len(aggregate.ByModel)
		if limit > 3 {
			limit = 3
		}
		models := make([]string, 0, limit)
		for _, model := range aggregate.ByModel[:limit] {
			label := safeArchiveLabel(model.Model)
			if label == "" {
				continue
			}
			models = append(models, fmt.Sprintf("%s × %d", label, model.Sessions))
		}
		if len(models) > 0 {
			rows = append(rows, infoRow{Key: "models", Value: strings.Join(models, ", ")})
		}
	}
	return rows
}

func collectRecordedUsage(store *session.Store) recordedUsage {
	var out recordedUsage
	if store == nil {
		return out
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		cost, ok, err := store.ReadCost(id)
		if err != nil || !ok {
			continue
		}
		out.sessions++
		out.input += cost.InputTokens
		out.output += cost.OutputTokens
		out.cacheWrite += cost.CacheCreateTokens
		out.cacheRead += cost.CacheReadTokens
	}
	return out
}
