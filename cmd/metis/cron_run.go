package main

import (
	"fmt"
	"io"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// advanceManualCronRun records the manual fire before the prompt is executed,
// then reloads the updated job so RunCount/LastRun/Repeat/NextRun shown by the
// desktop and CLI reflect the same fire.
func advanceManualCronRun(svc *agent.CronService, id string) (*agent.CronJob, error) {
	if err := svc.Run(id); err != nil {
		return nil, err
	}
	job, ok := svc.Get(id)
	if !ok {
		return nil, fmt.Errorf("cron job disappeared after run bookkeeping: %s", id)
	}
	return job, nil
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
