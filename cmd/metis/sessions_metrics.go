package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/session"
)

type sessionMetricsOptions struct {
	id         string
	jsonOutput bool
}

func parseSessionMetricsOptions(args []string) (sessionMetricsOptions, error) {
	var opts sessionMetricsOptions
	for _, arg := range args {
		switch {
		case arg == "--json":
			opts.jsonOutput = true
		case strings.HasPrefix(arg, "-"):
			return sessionMetricsOptions{}, fmt.Errorf("sessions metrics: unknown option %q", arg)
		case opts.id != "":
			return sessionMetricsOptions{}, errors.New("usage: metis sessions metrics <id> [--json]")
		default:
			opts.id = arg
		}
	}
	if strings.TrimSpace(opts.id) == "" || opts.id == "." || opts.id == ".." || filepath.Base(opts.id) != opts.id || strings.ContainsAny(opts.id, `/\`) {
		return sessionMetricsOptions{}, errors.New("usage: metis sessions metrics <id> [--json]")
	}
	return opts, nil
}

// printSessionMetrics reads the presentation sidecar only. In particular it
// does not reconcile trace usage or create a missing metrics file: querying a
// past session must not change its accounting or timing data.
func printSessionMetrics(store *session.Store, args []string, out io.Writer) error {
	opts, err := parseSessionMetricsOptions(args)
	if err != nil {
		return err
	}
	metrics, err := store.ReadMessageMetrics(opts.id)
	if err != nil {
		return err
	}
	if opts.jsonOutput {
		if metrics == nil {
			metrics = []session.MessageMetric{}
		}
		return json.NewEncoder(out).Encode(metrics)
	}
	if len(metrics) == 0 {
		_, err = fmt.Fprintf(out, "no message metrics recorded for session %s\n", opts.id)
		return err
	}
	for _, metric := range metrics {
		firstToken := "unknown"
		if metric.TTFTMS > 0 {
			firstToken = fmt.Sprintf("%dms", metric.TTFTMS)
		}
		if _, err = fmt.Fprintf(out, "turn=%d duration=%dms first-token=%s input=%d output=%d tok/s=%.2f\n",
			metric.Turn, metric.DurationMS, firstToken, metric.InputTokens, metric.OutputTokens, metric.TokPerSec); err != nil {
			return err
		}
		if err := printSessionStartupSummary(out, metric.StartupSummary); err != nil {
			return err
		}
	}
	return nil
}

func printSessionStartupSummary(out io.Writer, summary *session.StartupSummary) error {
	if summary == nil {
		summary = &session.StartupSummary{}
	}
	source, requestID := summary.Source, summary.RequestID
	if source == "" {
		source = "unknown"
	}
	if requestID == "" {
		requestID = "unknown"
	}
	ms := func(value *int64) string {
		if value == nil {
			return "unknown"
		}
		return fmt.Sprintf("%dms", *value)
	}
	// These two groups use different meanings: milestones are measured from
	// API receipt; queue values are durations. Never present their sum as a
	// startup duration, and preserve a measured zero separately from unknown.
	_, err := fmt.Fprintf(out, "startup source=%s requestId=%s milestones-from-api-receipt: received=%s worker=%s runtime=%s mcp=%s requestWritten=%s firstByte=%s firstThinking=%s firstText=%s total=%s; durations: queue=%s executionQueue=%s\n",
		source, requestID, ms(summary.ReceivedMS), ms(summary.WorkerMS), ms(summary.RuntimeMS), ms(summary.MCPMS),
		ms(summary.RequestWrittenMS), ms(summary.FirstByteMS), ms(summary.FirstThinkingMS), ms(summary.FirstTextMS), ms(summary.TotalMS),
		ms(summary.QueueMS), ms(summary.ExecutionQueueMS))
	return err
}
