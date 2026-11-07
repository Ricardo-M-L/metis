package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type wakeupCreationProvider struct {
	activationTestProvider
	registry *tools.Registry
	t        *testing.T
	calls    int
}

func (p *wakeupCreationProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.calls++
	if p.calls == 1 {
		tool, ok := p.registry.Get("ScheduleWakeup")
		if !ok {
			p.t.Fatal("visible tool disappeared")
		}
		result, err := tool.Execute(ctx, map[string]any{"delaySeconds": 30, "prompt": "continue original", "reason": "creation entry fixture"})
		if err != nil || result.IsError {
			p.t.Fatalf("actual turn tool creation: %+v %v", result, err)
		}
		wrong := tasks.WithSessionID(ctx, "wrong")
		result, err = tool.Execute(wrong, map[string]any{"delaySeconds": 30, "prompt": "wrong", "reason": "wrong"})
		if err != nil || !result.IsError {
			p.t.Fatal("turn context did not reject cross-session creation")
		}
	}
	return &composerSummaryStream{events: []llm.StreamEvent{{Type: "text_delta", TextDelta: "done"}, {Type: "message_delta", StopReason: "end_turn"}, {Type: "message_stop"}}}, nil
}
func TestScheduledWakeupActualDesktopTurnCreatesThenConsumes(t *testing.T) {
	root := t.TempDir()
	svc, _ := agent.NewCronService(root)
	store, _ := session.NewStore(t.TempDir())
	dir := t.TempDir()
	_ = store.WriteHeaderFull(session.Header{ID: "original", Provider: "fake", Model: "model", WorkDir: dir, Mode: string(permission.ModeAsk)})
	gate := permission.New(permission.ModeAsk)
	reg := tools.NewRegistry()
	reg.Register(builtin.NewScheduleWakeup(gate, svc))
	p := &wakeupCreationProvider{activationTestProvider: activationTestProvider{name: "fake", model: "model"}, registry: reg, t: t}
	loop := agent.NewLoop(p, reg, gate, nil, "system", 3)
	loop.Model = "model"
	s := NewServer("", loop, store, RuntimeBindings{InitialSessionID: "original", ProviderName: "fake", CronService: svc})
	response := httptest.NewRecorder()
	s.handleTurn(response, httptest.NewRequest(http.MethodPost, "/api/turn", strings.NewReader(`{"sessionId":"original","input":"schedule"}`)))
	if response.Code != 200 {
		t.Fatalf("formal turn %d %s", response.Code, response.Body.String())
	}
	jobs := svc.List()
	if len(jobs) != 1 {
		t.Fatalf("tool did not create exactly one job: %v", jobs)
	}
	job := jobs[0]
	if job.OriginSessionID != "original" || job.WorkDir != dir || job.Provider != "fake" || job.Model != "model" {
		t.Fatalf("wrong original identity: %+v", job)
	}
	s.consumeScheduledWakeups(context.Background(), time.Now())
	if p.calls != 1 {
		t.Fatal("created job executed before due")
	}
	s.consumeScheduledWakeups(context.Background(), job.NextRun.Add(time.Second))
	if p.calls != 2 {
		t.Fatalf("due original turn calls=%d", p.calls)
	}
	records, err := agent.ListCronRuns(root, job.ID, 10)
	if err != nil || len(records) != 1 || records[0].Status != "succeeded" || records[0].SessionID != "original" {
		t.Fatalf("result receipt %+v %v", records, err)
	}
}
