package main

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// cronProgressRecorder exposes only assistant text and tool names/status. Raw
// tool arguments and results may contain secrets and never enter run progress.
type cronProgressRecorder struct {
	run      *agent.CronRun
	text     strings.Builder
	activity []agent.CronRunActivity
	last     time.Time
	warned   bool
}

func (p *cronProgressRecorder) observe(ev agent.Event) {
	if p == nil || p.run == nil {
		return
	}
	force := false
	switch ev.Kind {
	case agent.EventTextDelta:
		if ev.TextDelta == "" {
			return
		}
		p.text.WriteString(ev.TextDelta)
		// Keep a little more than the persisted limit so redaction can see a
		// secret that crosses a token boundary before the snapshot is clipped.
		if p.text.Len() > 32*1024 {
			content := p.text.String()
			tail := content[len(content)-24*1024:]
			for !utf8.ValidString(tail) {
				tail = tail[1:]
			}
			p.text.Reset()
			p.text.WriteString(tail)
		}
	case agent.EventToolStart:
		p.activity = append(p.activity, agent.CronRunActivity{Kind: "tool_start", Tool: ev.ToolName})
		force = true
	case agent.EventToolResult:
		kind := "tool_done"
		if ev.ToolResult != nil && ev.ToolResult.IsError {
			kind = "tool_failed"
		}
		p.activity = append(p.activity, agent.CronRunActivity{Kind: kind, Tool: ev.ToolName})
		force = true
	case agent.EventLoopDone:
		force = true
	case agent.EventError:
		force = true
	default:
		return
	}
	if len(p.activity) > 64 {
		p.activity = append([]agent.CronRunActivity(nil), p.activity[len(p.activity)-64:]...)
	}
	if !force && time.Since(p.last) < 500*time.Millisecond {
		return
	}
	if err := p.run.SetProgress(p.text.String(), p.activity); err != nil && !p.warned {
		fmt.Fprintf(os.Stderr, "[cron] live progress unavailable: %v\n", err)
		p.warned = true
	}
	p.last = time.Now()
}
