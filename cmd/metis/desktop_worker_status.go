package main

import (
	"reflect"
	"sort"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/security"
)

const desktopWorkerStatusInterval = 200 * time.Millisecond
const desktopWorkerStatusAgentLimit = 32
const desktopWorkerStatusTextLimit = 16_000

// startStatus captures stable registry handles before runtime cleanup clears
// them. The returned join must run after Cleanup so its last frame contains
// the reaped child status, including agents removed from the live roster.
func (b *desktopWorkerBridge) startStatus(roster *agent.Roster, registry *jobs.Registry) func(func()) {
	stop, done := make(chan struct{}), make(chan struct{})
	flush := make(chan chan struct{})
	go func() {
		defer close(done)
		sampler := desktopWorkerStatusSampler{roster: roster, jobs: registry, known: make(map[string]*agent.Teammate)}
		var previous *desktopipc.Status
		emit := func(final bool) {
			status := sampler.snapshot(final)
			if !final && previous != nil && equivalentDesktopWorkerStatus(*previous, status) {
				return
			}
			if err := b.encoder.Encode(desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeStatus, Status: &status}); err != nil {
				b.fail(err)
				return
			}
			previous = &status
		}
		emit(false)
		ticker := time.NewTicker(desktopWorkerStatusInterval)
		defer ticker.Stop()
		for {
			select {
			case ack := <-flush:
				emit(false)
				close(ack)
			case <-stop:
				emit(true)
				return
			case <-ticker.C:
				emit(false)
			}
		}
	}()
	return func(cleanup func()) {
		// Capture sub-agents that were created and completed between ticks,
		// before runtime cleanup forgets its bounded completed-agent roster.
		ack := make(chan struct{})
		flush <- ack
		<-ack
		defer func() { close(stop); <-done }()
		cleanup()
	}
}

type desktopWorkerStatusSampler struct {
	roster *agent.Roster
	jobs   *jobs.Registry
	known  map[string]*agent.Teammate
}

func (s *desktopWorkerStatusSampler) snapshot(final bool) desktopipc.Status {
	status := desktopipc.Status{Agents: make([]desktopipc.Subagent, 0), Jobs: make([]desktopipc.Job, 0)}
	if s.roster != nil {
		summary := s.roster.Summary()
		status.SubAgents, status.NamedAgents = summary.Total, summary.Named
		for _, teammate := range s.roster.List() {
			if teammate != nil {
				s.known[teammate.Snapshot().AgentID] = teammate
			}
		}
	}
	snapshots := make([]agent.TeammateSnapshot, 0, len(s.known))
	for _, teammate := range s.known {
		snapshots = append(snapshots, teammate.Snapshot())
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Started.Equal(snapshots[j].Started) {
			return snapshots[i].AgentID < snapshots[j].AgentID
		}
		return snapshots[i].Started.After(snapshots[j].Started)
	})
	if len(snapshots) > desktopWorkerStatusAgentLimit {
		for _, snap := range snapshots[desktopWorkerStatusAgentLimit:] {
			delete(s.known, snap.AgentID)
		}
		snapshots = snapshots[:desktopWorkerStatusAgentLimit]
	}
	for _, snap := range snapshots {
		output, outputTruncated := desktopWorkerStatusText(snap.Output)
		result, resultTruncated := desktopWorkerStatusText(snap.Result)
		elapsed := time.Since(snap.Started)
		if !snap.EndTime.IsZero() {
			elapsed = snap.EndTime.Sub(snap.Started)
		}
		if elapsed < 0 {
			elapsed = 0
		}
		exitError := ""
		if snap.ExitErr != nil {
			exitError = security.RedactSubprocessText(snap.ExitErr.Error())
		}
		status.Agents = append(status.Agents, desktopipc.Subagent{
			Name: snap.Name, AgentID: snap.AgentID, Anonymous: snap.Anonymous, Status: snap.Status.String(), Background: snap.Background,
			StartedAt: snap.Started, EndedAt: snap.EndTime, ElapsedMS: elapsed.Milliseconds(), Output: output, OutputTruncated: outputTruncated,
			Result: result, ResultTruncated: resultTruncated, StopHint: snap.StopHint, ExitError: exitError,
		})
	}
	if s.jobs != nil {
		for _, job := range s.jobs.List() {
			if job.Status == jobs.StatusRunning {
				status.BackgroundTasks++
			}
			if len(status.Jobs) < 32 {
				status.Jobs = append(status.Jobs, desktopipc.Job{ID: job.ID, Description: job.Description, Status: job.Status.String(), StartedAt: job.StartTime})
			}
		}
	}
	if final {
		status.SubAgents, status.NamedAgents, status.BackgroundTasks = 0, 0, 0
	}
	return status
}

// Live elapsed time is derived from StartedAt by the UI. A clock tick alone
// must not resend up to MiBs of unchanged child output over the pipe.
func equivalentDesktopWorkerStatus(a, b desktopipc.Status) bool {
	a.Agents = append([]desktopipc.Subagent(nil), a.Agents...)
	b.Agents = append([]desktopipc.Subagent(nil), b.Agents...)
	for i := range a.Agents {
		if a.Agents[i].EndedAt.IsZero() {
			a.Agents[i].ElapsedMS = 0
		}
	}
	for i := range b.Agents {
		if b.Agents[i].EndedAt.IsZero() {
			b.Agents[i].ElapsedMS = 0
		}
	}
	return reflect.DeepEqual(a, b)
}

func desktopWorkerStatusText(text string) (string, bool) {
	runes := []rune(text)
	if len(runes) <= desktopWorkerStatusTextLimit {
		return text, false
	}
	const marker = "\n… METIS truncated this sub-agent output …\n"
	half := (desktopWorkerStatusTextLimit - len([]rune(marker))) / 2
	return string(runes[:half]) + marker + string(runes[len(runes)-half:]), true
}
