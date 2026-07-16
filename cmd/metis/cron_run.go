package main

import (
	"fmt"
	"io"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// advanceManualCronRun records the manual fire before the prompt is executed
// and returns the immutable snapshot saved by that same storage transaction.
func advanceManualCronRun(svc *agent.CronService, id string) (*agent.CronJob, error) {
	return svc.RunNow(id)
}

func reportCronFireError(w io.Writer, job *agent.CronJob, err error) error {
	if err == nil {
		return nil
	}
	id := "unknown"
	if job != nil && job.ID != "" {
		id = job.ID
	}
	_, _ = fmt.Fprintf(w, "[cron] job %s failed: %v\n", id, err)
	return err
}
