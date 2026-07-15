package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

type cronListOptions struct {
	jsonOutput bool
}

func parseCronListOptions(args []string) (cronListOptions, error) {
	var opts cronListOptions
	for _, arg := range args {
		switch arg {
		case "--json":
			opts.jsonOutput = true
		default:
			return cronListOptions{}, fmt.Errorf("cron list: unknown option %q", arg)
		}
	}
	return opts, nil
}

// cronListRecord is the stable machine-readable contract used by native
// clients. Keep timestamps as RFC3339 strings so JavaScript callers do not
// depend on Go's time.Time JSON representation details.
type cronListRecord struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Prompt        string             `json:"prompt"`
	Schedule      agent.CronSchedule `json:"schedule"`
	Enabled       bool               `json:"enabled"`
	Paused        bool               `json:"paused"`
	CreatedAt     string             `json:"createdAt,omitempty"`
	LastRun       string             `json:"lastRun,omitempty"`
	NextRun       string             `json:"nextRun,omitempty"`
	RunCount      int                `json:"runCount"`
	Repeat        int                `json:"repeat"`
	Silent        bool               `json:"silent"`
	SessionMode   string             `json:"sessionMode"`
	AllowTools    []string           `json:"allowTools,omitempty"`
	DisabledTools []string           `json:"disabledTools,omitempty"`
}

func writeCronList(w io.Writer, jobs []*agent.CronJob, opts cronListOptions) error {
	jobs = append([]*agent.CronJob(nil), jobs...)
	sort.SliceStable(jobs, func(i, j int) bool {
		left, right := jobs[i], jobs[j]
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		if left.NextRun.Equal(right.NextRun) {
			return left.ID < right.ID
		}
		if left.NextRun.IsZero() {
			return false
		}
		if right.NextRun.IsZero() {
			return true
		}
		return left.NextRun.Before(right.NextRun)
	})

	if opts.jsonOutput {
		records := make([]cronListRecord, 0, len(jobs))
		for _, job := range jobs {
			if job == nil || job.Ephemeral {
				continue
			}
			records = append(records, cronRecord(job))
		}
		return json.NewEncoder(w).Encode(records)
	}

	printed := 0
	for _, job := range jobs {
		if job == nil || job.Ephemeral {
			continue
		}
		next := "—"
		if !job.NextRun.IsZero() {
			next = job.NextRun.Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(w, "%s  enabled=%v paused=%v next=%s name=%q\n",
			job.ID, job.Enabled, job.Paused, next, job.Name); err != nil {
			return err
		}
		printed++
	}
	if printed == 0 {
		_, err := fmt.Fprintln(w, "(no cron jobs)")
		return err
	}
	return nil
}

func cronRecord(job *agent.CronJob) cronListRecord {
	mode := job.SessionMode
	if mode == "" {
		mode = agent.SessionModeIsolated
	}
	record := cronListRecord{
		ID:            job.ID,
		Name:          job.Name,
		Prompt:        job.Prompt,
		Schedule:      job.Schedule,
		Enabled:       job.Enabled,
		Paused:        job.Paused,
		RunCount:      job.RunCount,
		Repeat:        job.Repeat,
		Silent:        job.Silent,
		SessionMode:   mode,
		AllowTools:    append([]string(nil), job.AllowTools...),
		DisabledTools: append([]string(nil), job.DisabledTools...),
	}
	if !job.CreatedAt.IsZero() {
		record.CreatedAt = job.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !job.LastRun.IsZero() {
		record.LastRun = job.LastRun.UTC().Format(time.RFC3339)
	}
	if !job.NextRun.IsZero() {
		record.NextRun = job.NextRun.UTC().Format(time.RFC3339)
	}
	return record
}
