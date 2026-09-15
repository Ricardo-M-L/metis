package webui

import "time"

func traceDisplayTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Format(time.RFC3339)
}
