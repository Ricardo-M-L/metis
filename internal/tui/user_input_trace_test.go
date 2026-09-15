package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/slash"
)

type userInputTraceProvider struct{ visionFakeProvider }

func (userInputTraceProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &backgroundTestStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "answer"},
		{Type: "message_stop", StopReason: "end_turn"},
	}}, nil
}

func installUserInputTrace(t *testing.T, sid string) *session.TraceStore {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_AUTO_MEMORY", "0")
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")
	t.Chdir(t.TempDir())
	store, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previous := runtime.CurrentTraceAdapter()
	adapter := runtime.NewTraceAdapter(store)
	adapter.SetSession(sid)
	runtime.SetTraceAdapter(adapter)
	agent.SetTraceHook(adapter.OnEvent)
	t.Cleanup(func() {
		runtime.SetTraceAdapter(previous)
		if previous == nil {
			agent.SetTraceHook(nil)
		} else {
			agent.SetTraceHook(previous.OnEvent)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func newUserInputTraceModel(t *testing.T, sid string) *Model {
	t.Helper()
	m := newSlashTestModel(t)
	m.sessionID = sid
	m.loop.Provider = userInputTraceProvider{}
	m.eventCh = make(chan agent.Event, 64)
	m.doneCh = make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	m.ctx = ctx
	return m
}

func waitUserInputTraceTurn(t *testing.T, m *Model) {
	t.Helper()
	select {
	case err := <-m.doneCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-m.ctx.Done():
		t.Fatal("local fake-provider turn did not finish")
	}
}

func userInputTraceRows(store *session.TraceStore, sid string) []session.TraceEvent {
	var rows []session.TraceEvent
	for _, event := range store.Events(sid) {
		if event.Kind == "user" {
			rows = append(rows, event)
		}
	}
	return rows
}

func cronInputTraceRows(store *session.TraceStore, sid string) []session.TraceEvent {
	var rows []session.TraceEvent
	for _, event := range store.Events(sid) {
		if event.Kind == "context" && event.Source == "cron" {
			rows = append(rows, event)
		}
	}
	return rows
}

func TestTUIUserInputTraceRecordsRawSubmission(t *testing.T) {
	for _, input := range []string{"hello world", "inspect @nested/file.go", "/batch inspect files", "/review main", "inspect [Image #1] @nested/file.go"} {
		t.Run(input, func(t *testing.T) {
			const sid = "tui-raw-input"
			store := installUserInputTrace(t, sid)
			m := newUserInputTraceModel(t, sid)
			if err := os.Mkdir("nested", 0o700); err != nil {
				t.Fatal(err)
			}
			for name, contents := range map[string]string{"AGENTS.md": "GENERATED_DIRECTORY_HINT", "file.go": "package fixture"} {
				if err := os.WriteFile(filepath.Join("nested", name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want := input
			if strings.Contains(input, "[Image #1]") {
				m.imagePaste = map[int]string{1: makeTinyPNG(t, t.TempDir(), "attachment.png", 8, 8)}
				m.imageCounter = 1
				want += "\n[image attachment]"
			}
			typeRunes(t, m, input)
			pressEnter(t, m)
			waitUserInputTraceTurn(t, m)
			rows := userInputTraceRows(store, sid)
			if len(rows) != 1 || rows[0].Text != want || rows[0].Turn != 1 {
				t.Fatalf("USER trace = %+v, want one turn-1 row %q", rows, want)
			}
			if strings.Contains(input, "@nested/") {
				var providerText strings.Builder
				for _, block := range m.loop.History()[0].Content {
					providerText.WriteString(block.Text)
				}
				if !strings.Contains(providerText.String(), "GENERATED_DIRECTORY_HINT") {
					t.Fatal("fixture did not exercise provider-only directory hints")
				}
			}
			for _, event := range store.Events(sid) {
				if event.Kind == "text" && (event.Turn != rows[0].Turn || event.Sequence <= rows[0].Sequence) {
					t.Fatalf("assistant event did not follow its USER anchor: %+v", event)
				}
			}
		})
	}
}

func TestTUIUserInputTraceRecordsAcceptedSteering(t *testing.T) {
	for _, input := range []string{"use the second option", "/custom second"} {
		t.Run(input, func(t *testing.T) {
			const sid = "tui-steer-input"
			store := installUserInputTrace(t, sid)
			m := newUserInputTraceModel(t, sid)
			m.turnActive = true
			m.slash.Register(slash.Cmd{Name: "custom", Custom: true, Trusted: true, Handler: func(args string) (string, slash.Signal) {
				return "GENERATED_COMMAND_FRAME " + args, slash.SignalCustomPrompt
			}})
			m.input.SetValue(input)
			pressEnter(t, m)
			if m.loop.SteerInjectDrainForTest() == "" {
				t.Fatal("fixture did not accept a steer")
			}
			rows := userInputTraceRows(store, sid)
			if len(rows) != 1 || rows[0].Text != input {
				t.Fatalf("USER trace = %+v, want accepted raw steering %q", rows, input)
			}
		})
	}
}

func TestTUIUserInputTraceExcludesUnsubmittedAndAutomaticWork(t *testing.T) {
	for _, mode := range []string{"local command", "queued", "missing image", "automatic continuation"} {
		t.Run(mode, func(t *testing.T) {
			const sid = "tui-non-input"
			store := installUserInputTrace(t, sid)
			m := newUserInputTraceModel(t, sid)
			switch mode {
			case "local command":
				m.input.SetValue("/help")
			case "queued":
				m.turnActive = true
				m.turnCancelledByUser = true
				m.input.SetValue("queued follow-up")
			case "missing image":
				m.imagePaste = map[int]string{1: filepath.Join(t.TempDir(), "missing.png")}
				m.input.SetValue("inspect [Image #1]")
			case "automatic continuation":
				m.loop.AppendUser("AUTOMATIC_NOTIFICATION")
				m.startAgentTurn(m.ctx)
				waitUserInputTraceTurn(t, m)
			}
			if mode != "automatic continuation" {
				pressEnter(t, m)
			}
			if rows := userInputTraceRows(store, sid); len(rows) != 0 {
				t.Fatalf("non-submitted %s created USER trace: %+v", mode, rows)
			}
		})
	}
}

func TestREPLUserInputTraceRecordsRawSubmission(t *testing.T) {
	const sid = "prompt-test"
	store := installUserInputTrace(t, sid)
	registry := slash.NewRegistry()
	slash.RegisterAll(registry, &config.Config{})
	input := "hello world\n/review main\n/batch inspect files\n/quit\n"
	r, _ := newPromptTestREPL(input, registry)
	r.Loop.Provider = userInputTraceProvider{}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := userInputTraceRows(store, sid)
	want := []string{"hello world", "/review main", "/batch inspect files"}
	if len(rows) != len(want) {
		t.Fatalf("USER trace = %+v, want %d explicit inputs", rows, len(want))
	}
	for i, text := range want {
		if rows[i].Text != text || rows[i].Turn != i+1 {
			t.Fatalf("USER trace[%d] = %+v, want raw input %q in turn %d", i, rows[i], text, i+1)
		}
	}
}

func TestTUIUserInputTraceExcludesQueuedCron(t *testing.T) {
	for _, humanQueued := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled only", true: "mixed scheduled and human"}[humanQueued], func(t *testing.T) {
			const sid = "tui-queued-cron"
			store := installUserInputTrace(t, sid)
			m := newUserInputTraceModel(t, sid)
			service, err := agent.NewCronService(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			job := &agent.CronJob{Prompt: "AUTOMATIC_SCHEDULED_TASK", Enabled: true, Ephemeral: true, Schedule: agent.CronSchedule{Kind: "every", EveryMs: 1000}}
			if err := service.Create(job); err != nil {
				t.Fatal(err)
			}
			m.cronSvc = service
			m.turnActive = true
			m.handleCronTick(job.NextRun.Add(time.Second))
			if len(m.queuedPrompts) != 1 {
				t.Fatal("fixture did not enqueue a scheduled task while busy")
			}
			if humanQueued {
				m.enqueueQueuedItem("actual human follow-up", QueuePriorityNext)
			}
			if len(userInputTraceRows(store, sid)) != 0 || len(cronInputTraceRows(store, sid)) != 0 {
				t.Fatal("unconsumed queued work created trace input")
			}
			m.finalizeTurn(nil)
			m.Update(spinnerTick{})
			waitUserInputTraceTurn(t, m)
			rows := userInputTraceRows(store, sid)
			if !humanQueued && len(rows) != 0 {
				t.Fatalf("queued cron created USER trace: %+v", rows)
			}
			if humanQueued && (len(rows) != 1 || rows[0].Text != "actual human follow-up") {
				t.Fatalf("mixed queue USER trace = %+v, want only original human follow-up", rows)
			}
			contexts := cronInputTraceRows(store, sid)
			if len(contexts) != 1 || contexts[0].Text != job.Prompt || contexts[0].Turn != 1 {
				t.Fatalf("queued cron context = %+v, want automatic input in turn 1", contexts)
			}
			if history := m.loop.History(); len(history) == 0 || !strings.Contains(history[0].Content[0].Text, "AUTOMATIC_SCHEDULED_TASK") {
				t.Fatal("scheduled provider input was lost")
			}
		})
	}
}

func TestTUIUserInputTraceDirectCronIsContext(t *testing.T) {
	const sid = "tui-direct-cron"
	store := installUserInputTrace(t, sid)
	m := newUserInputTraceModel(t, sid)
	m.beginTurn("direct scheduled work")
	waitUserInputTraceTurn(t, m)
	if rows := userInputTraceRows(store, sid); len(rows) != 0 {
		t.Fatalf("direct cron created USER trace: %+v", rows)
	}
	contexts := cronInputTraceRows(store, sid)
	if len(contexts) != 1 || contexts[0].Text != "direct scheduled work" || contexts[0].Turn != 1 {
		t.Fatalf("direct cron context = %+v, want automatic input in turn 1", contexts)
	}
}

func TestTUIUserInputTracePreservesQueuedCommandInvocation(t *testing.T) {
	const sid = "tui-queued-command"
	store := installUserInputTrace(t, sid)
	m := newUserInputTraceModel(t, sid)
	m.slash.Register(slash.Cmd{Name: "custom", Custom: true, Trusted: true, Handler: func(args string) (string, slash.Signal) {
		return "GENERATED_COMMAND_FRAME " + args, slash.SignalCustomPrompt
	}})
	m.loop.AppendUser("previous task")
	m.startAgentTurn(m.ctx)
	waitUserInputTraceTurn(t, m) // Loop has closed steering; TUI still says active.
	m.input.SetValue("/custom second")
	pressEnter(t, m)
	if len(m.queuedPrompts) != 1 {
		t.Fatal("fixture did not queue the command after steering closed")
	}
	m.drainAgentEvents(len(m.eventCh))
	m.finalizeTurn(nil)
	m.Update(spinnerTick{})
	waitUserInputTraceTurn(t, m)
	rows := userInputTraceRows(store, sid)
	if len(rows) != 1 || rows[0].Text != "/custom second" {
		t.Fatalf("queued command USER trace = %+v, want original invocation", rows)
	}
}
