package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func newPromptTestREPL(input string, registry *slash.Registry) (*REPL, *bytes.Buffer) {
	gate := permission.New(permission.ModeDefault)
	loop := agent.NewLoop(fakeProvider{}, tools.NewRegistry(), gate, agent.NewHookRegistry(), "system", 2)
	loop.Model = "current-model"
	var out bytes.Buffer
	return &REPL{
		Loop: loop, Gate: gate, Slash: registry, SessionID: "prompt-test",
		Styles: NewStyles(), model: "current-model", cmds: BuildREPLCommands(),
		stdin: strings.NewReader(input), out: &out,
	}, &out
}

func TestPlainREPL_CustomPromptRunsWithoutEchoingExpandedBody(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{
		Name: "hello", Custom: true, Trusted: true,
		Handler: func(args string) (string, slash.Signal) {
			return "EXPANDED INTERNAL BODY: " + args, slash.SignalCustomPrompt
		},
	})
	registry.Register(slash.Cmd{Name: "quit", Handler: func(string) (string, slash.Signal) { return "", slash.SignalQuit }})
	r, out := newPromptTestREPL("/hello world\n/quit\n", registry)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(stripANSI(out.String()), "EXPANDED INTERNAL BODY") {
		t.Fatalf("expanded custom prompt was echoed to stdout:\n%s", stripANSI(out.String()))
	}
	history := r.Loop.History()
	if len(history) == 0 || !strings.Contains(history[0].Content[0].Text, "EXPANDED INTERNAL BODY: world") {
		t.Fatalf("expanded custom prompt never entered the agent turn: %+v", history)
	}
}

func TestPlainREPL_ReviewKeepsInternalFrameOutOfStdoutAndExport(t *testing.T) {
	registry := slash.NewRegistry()
	slash.RegisterAll(registry, &config.Config{})
	r, out := newPromptTestREPL("/review main\n/quit\n", registry)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	stdout := stripANSI(out.String())
	if strings.Contains(stdout, "# Code Review") || strings.Contains(stdout, internalReviewPromptOpen) {
		t.Fatalf("internal review frame leaked to plain REPL stdout:\n%s", stdout)
	}
	exported := conversationText(r.Loop.History())
	if !strings.Contains(exported, "❯ /review main") || strings.Contains(exported, "# Code Review") || strings.Contains(exported, internalReviewPromptOpen) {
		t.Fatalf("plain REPL review export crossed visibility boundary:\n%s", exported)
	}
	if prefill, ok := r.Loop.UndoLastTurnWithPrefill(); !ok || prefill != "/review main" {
		t.Fatalf("plain REPL /review undo prefill = %q, %v; want visible invocation", prefill, ok)
	}
}
