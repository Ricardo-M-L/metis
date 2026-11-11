package agent

import (
	"context"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"testing"
	"time"
)

func reviewerTerminal(out chan Event) string {
	close(out)
	stop := ""
	for e := range out {
		if e.Kind == EventLoopDone {
			stop = e.StopReason
		}
	}
	return stop
}

func TestReviewerOutputRecoveryRetainsContract(t *testing.T) {
	t.Setenv(contractDisableEnvVar, "0")
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		toolUseStream("write-fixture", "Bash", `{"command":"sed -i 's/old/new/' fixture.go"}`),
		outputLimitStream("partial implementation report", "max_tokens"),
		textStream(" completed implementation"),
	}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	loop.Registry.Register(&contractBashProbeTool{})
	loop.Registry.Register(contractAvailableAgentTool{})
	out := make(chan Event, 4096)
	if err := RunSubAgentWithContinuations(context.Background(), out, loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	stop := reviewerTerminal(out)
	if stop == "end_turn" {
		t.Fatalf("lost pre-truncation contract: terminal=%s requests=%d risk=%d", stop, len(provider.capturedRequests()), loop.contract.riskScore())
	}
}

func TestReviewerOutputRecoveryRetainsRequiredJob(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.ResetAndWait(0) })
	tool := &requiredJobTestTool{pool: pool, command: "sleep 60", required: true, registerBeforeResult: true}
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		toolUseStream("required-check", "RequiredJob", `{}`),
		outputLimitStream("partial check report", "max_tokens"),
		textStream(" all checks complete"),
	}}
	_, transcript, loop, teammate := continuationFixture(t, provider)
	loop.Registry.Register(tool)
	loop.Jobs = pool
	loop.JobNotify = pool.Notify()
	out := make(chan Event, 4096)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := RunSubAgentWithContinuations(ctx, out, loop, teammate, transcript); err != nil {
		t.Fatal(err)
	}
	stop := reviewerTerminal(out)
	job, ok := pool.Get(tool.id)
	if !ok {
		t.Fatal("required job missing")
	}
	if stop == "end_turn" {
		t.Fatalf("lost required job acceptance: terminal=%s requests=%d job=%s exit=%d", stop, len(provider.capturedRequests()), job.Status, job.ExitCode)
	}
}
