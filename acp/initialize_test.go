package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// helper: make a Loop with empty registry, suitable for tests that
// only need to drive the ACP layer.
func emptyLoop() *agent.Loop {
	return agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "test-system", 1)
}

// driveOneRequest runs a single JSON-RPC request through serveConn
// over io.Pipe and returns the first parsed response. serveConn
// reads in a loop expecting more input — using bytes.Buffer would
// hang on EOF detection inconsistently. Pipes give clean
// "writer-closed → reader EOF" semantics.
func driveOneRequest(t *testing.T, srv *Server, req Request) Response {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()

	go srv.serveConn(c2sR, s2cW)

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = c2sW.Write(append(b, '\n'))
		// Don't close c2sW — serveConn keeps the connection open and
		// closing it triggers an EOF-handling path that may race with
		// the response. We just read the first response, then drop.
	}()

	sc := bufio.NewScanner(s2cR)
	type result struct {
		raw []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if !sc.Scan() {
			ch <- result{nil, sc.Err()}
			return
		}
		ch <- result{append([]byte(nil), sc.Bytes()...), nil}
	}()

	var raw []byte
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read response: %v", r.err)
		}
		raw = r.raw
	case <-time.After(2 * time.Second):
		t.Fatal("driveOneRequest: timeout waiting for response")
	}

	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("response decode: %v\nraw: %s", err, raw)
	}
	return resp
}

// ─── handleInitialize ────────────────────────────────────────────

func TestInitialize_RepliesWithProtocolVersion(t *testing.T) {
	srv := NewServer(emptyLoop(), "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{ProtocolVersion: "1.0"}),
	})
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	res := decodeResult[InitializeResult](t, resp.Result)
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol_version: got %q, want %q", res.ProtocolVersion, ProtocolVersion)
	}
	if res.Server.Name != "metis" {
		t.Errorf("server name: %q", res.Server.Name)
	}
}

func TestInitialize_RepliesWithSlashCommands(t *testing.T) {
	srv := NewServer(emptyLoop(), "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{ProtocolVersion: "1.0"}),
	})
	res := decodeResult[InitializeResult](t, resp.Result)
	if len(res.SlashCommands) == 0 {
		t.Error("expected at least one slash command")
	}
	gotHelp := false
	for _, c := range res.SlashCommands {
		if c.Name == "/help" {
			gotHelp = true
		}
	}
	if !gotHelp {
		t.Error("expected /help in slash commands")
	}
}

func TestInitialize_AcceptsExternalToolWithUniqueName(t *testing.T) {
	srv := NewServer(emptyLoop(), "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{
			ProtocolVersion: "1.0",
			ExternalTools: []ExternalTool{
				{Name: "my-custom-tool", Description: "client-side"},
			},
		}),
	})
	res := decodeResult[InitializeResult](t, resp.Result)
	if len(res.ExternalTools) != 1 {
		t.Fatalf("want 1 decision, got %d: %+v", len(res.ExternalTools), res.ExternalTools)
	}
	d := res.ExternalTools[0]
	if d.Name != "my-custom-tool" {
		t.Errorf("decision name: %q", d.Name)
	}
	if !d.Accepted {
		t.Errorf("unique tool should be accepted, rejected with: %q", d.Reason)
	}
}

func TestInitialize_RejectsExternalToolThatDuplicatesBuiltin(t *testing.T) {
	// Register a stub "Bash" tool to simulate metis's builtin set,
	// then offer "Bash" as an external — expect rejection.
	loop := emptyLoop()
	loop.Registry.Register(stubTool{name: "Bash"})
	srv := NewServer(loop, "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{
			ProtocolVersion: "1.0",
			ExternalTools:   []ExternalTool{{Name: "Bash"}},
		}),
	})
	res := decodeResult[InitializeResult](t, resp.Result)
	if len(res.ExternalTools) != 1 {
		t.Fatal("expected exactly one decision")
	}
	d := res.ExternalTools[0]
	if d.Accepted {
		t.Error("Bash duplicates a builtin — must be rejected")
	}
	if !strings.Contains(d.Reason, "duplicates") {
		t.Errorf("reason should explain duplication, got: %q", d.Reason)
	}
}

func TestInitialize_MajorVersionMismatchIsError(t *testing.T) {
	srv := NewServer(emptyLoop(), "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{ProtocolVersion: "99.0"}),
	})
	if resp.Error == nil {
		t.Fatal("major-version mismatch must error")
	}
	if !strings.Contains(resp.Error.Message, "protocol_version mismatch") {
		t.Errorf("error message should explain: %q", resp.Error.Message)
	}
}

func TestInitialize_OmittedProtocolVersionAccepted(t *testing.T) {
	// Lenient default: missing protocol_version is treated as
	// "client doesn't care, give me whatever you've got."
	srv := NewServer(emptyLoop(), "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{}),
	})
	if resp.Error != nil {
		t.Fatalf("blank protocol_version should be accepted: %+v", resp.Error)
	}
	res := decodeResult[InitializeResult](t, resp.Result)
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("server should still advertise its own version: %q", res.ProtocolVersion)
	}
}

// ─── ServerVersion plumbing ──────────────────────────────────────

func TestSetServerVersion_FlowsIntoInitializeResult(t *testing.T) {
	old := serverVersion
	defer func() { serverVersion = old }()
	SetServerVersion("v9.9.9-test")

	srv := NewServer(emptyLoop(), "")
	resp := driveOneRequest(t, srv, Request{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: paramsFrom(t, InitializeParams{ProtocolVersion: "1.0"}),
	})
	res := decodeResult[InitializeResult](t, resp.Result)
	if res.Server.Version != "v9.9.9-test" {
		t.Errorf("server version: %q", res.Server.Version)
	}
}

// ─── DisplayBlock encoding round-trip ────────────────────────────

func TestDisplayBlock_RoundTripJSON(t *testing.T) {
	cases := []DisplayBlock{
		{Type: DisplayBlockBrief, Text: "Read 12 files"},
		{Type: DisplayBlockDiff, Path: "main.go", OldText: "abc", NewText: "abd"},
		{Type: DisplayBlockTodo, Items: []TodoItem{
			{Title: "First", Status: TodoCompleted},
			{Title: "Second", Status: TodoInProgress},
		}},
		{Type: DisplayBlockShell, Path: "/tmp", Command: "ls", Text: "a\nb\n"},
		{Type: DisplayBlockText, Text: "hello", Language: "go"},
	}
	for _, in := range cases {
		t.Run(string(in.Type), func(t *testing.T) {
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			var out DisplayBlock
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatal(err)
			}
			if out.Type != in.Type {
				t.Errorf("type round-trip lost: %q vs %q", out.Type, in.Type)
			}
			if out.Text != in.Text {
				t.Errorf("text mismatch: %q vs %q", out.Text, in.Text)
			}
			if out.Path != in.Path {
				t.Errorf("path mismatch: %q vs %q", out.Path, in.Path)
			}
			if len(out.Items) != len(in.Items) {
				t.Errorf("items count: %d vs %d", len(out.Items), len(in.Items))
			}
		})
	}
}

func TestDisplayBlock_DiffOmitsEmptyDataField(t *testing.T) {
	// A diff block has no Data — ensure the JSON encoding doesn't
	// include `"data":null` (would confuse strict clients).
	b, _ := json.Marshal(DisplayBlock{Type: DisplayBlockDiff, OldText: "a", NewText: "b"})
	if strings.Contains(string(b), `"data"`) {
		t.Errorf("empty data should be omitted, got: %s", b)
	}
	if strings.Contains(string(b), `"items"`) {
		t.Errorf("empty items should be omitted, got: %s", b)
	}
}

// ─── majorOf helper ──────────────────────────────────────────────

func TestMajorOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.0", "1"},
		{"1.7.3", "1"},
		{"99.42", "99"},
		{"nodot", "nodot"},
		{"", ""},
	}
	for _, c := range cases {
		if got := majorOf(c.in); got != c.want {
			t.Errorf("majorOf(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// ─── helpers ─────────────────────────────────────────────────────

func paramsFrom[T any](t *testing.T, v T) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func decodeResult[T any](t *testing.T, raw any) T {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode result: %v\nraw: %s", err, b)
	}
	return v
}

// ─── stub tool for builtin-collision test ────────────────────────

// stubTool is a minimal tools.Tool used to register a name into the
// registry so the initialize handler's collision check fires. Real
// metis builtins implement more than this, but the registry's Get
// only cares about the Name.
type stubTool struct{ name string }

func (s stubTool) Name() string                                 { return s.name }
func (s stubTool) Description() string                          { return "stub" }
func (s stubTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (s stubTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (s stubTool) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (s stubTool) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: ""}, nil
}

func (stubTool) IsEnabled() bool { return true }
