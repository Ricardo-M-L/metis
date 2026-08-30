package builtin

// lsp_client_test.go — deterministic coverage for the LSP response
// parsers and URI helpers. The full spawn→initialize→query exchange
// needs a real language server, so it's exercised manually / in the tmux
// suite; here we pin the pure functions that turn server JSON into the
// tool's output, plus the framing-adjacent helpers.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLSPReadMsgAcceptsBoundedCaseInsensitiveHeaders(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	framed := fmt.Sprintf("content-length: %d\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n%s", len(body), body)
	conn := &lspConn{r: bufio.NewReader(strings.NewReader(framed))}

	msg, err := conn.readMsg()
	if err != nil {
		t.Fatalf("readMsg: %v", err)
	}
	if string(msg.ID) != "1" || !strings.Contains(string(msg.Result), `"ok":true`) {
		t.Fatalf("decoded message = %#v", msg)
	}
}

func TestLSPReadMsgRejectsMalformedContentLength(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "missing", in: "Content-Type: application/json\r\n\r\n", want: "missing Content-Length"},
		{name: "negative", in: "Content-Length: -1\r\n\r\n", want: "invalid Content-Length"},
		{name: "zero", in: "Content-Length: 0\r\n\r\n", want: "must be positive"},
		{name: "duplicate", in: "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", want: "duplicate Content-Length"},
		{name: "bad name", in: "Content-Length : 2\r\n\r\n{}", want: "malformed lsp header"},
		{name: "lf only", in: "Content-Length: 2\n\n{}", want: "must end with CRLF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &lspConn{r: bufio.NewReader(strings.NewReader(tt.in))}
			_, err := conn.readMsg()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("readMsg error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLSPReadMsgBoundsHeaderAndFrameBeforeAllocation(t *testing.T) {
	oversizedHeader := "X-Fill: " + strings.Repeat("a", maxLSPHeaderBytes) + "\r\nContent-Length: 2\r\n\r\n{}"
	conn := &lspConn{r: bufio.NewReaderSize(strings.NewReader(oversizedHeader), 32)}
	if _, err := conn.readMsg(); !errors.Is(err, errLSPHeaderTooLarge) {
		t.Fatalf("oversized header error = %v, want errLSPHeaderTooLarge", err)
	}
	var aggregate strings.Builder
	for aggregate.Len() <= maxLSPHeaderBytes {
		aggregate.WriteString("X-A: b\r\n")
	}
	aggregate.WriteString("Content-Length: 2\r\n\r\n{}")
	conn = &lspConn{r: bufio.NewReader(strings.NewReader(aggregate.String()))}
	if _, err := conn.readMsg(); !errors.Is(err, errLSPHeaderTooLarge) {
		t.Fatalf("oversized aggregate header error = %v, want errLSPHeaderTooLarge", err)
	}

	oversizedFrame := fmt.Sprintf("Content-Length: %d\r\n\r\n", maxLSPFrameBytes+1)
	conn = &lspConn{r: bufio.NewReader(strings.NewReader(oversizedFrame))}
	if _, err := conn.readMsg(); !errors.Is(err, errLSPFrameTooLarge) {
		t.Fatalf("oversized frame error = %v, want errLSPFrameTooLarge", err)
	}
}

func TestLSPConnCloseWaitIsBoundedAndIdempotent(t *testing.T) {
	waitDone := make(chan error)
	conn := &lspConn{waitDone: waitDone, waitLimit: 20 * time.Millisecond}
	started := time.Now()
	err := conn.Close()
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Close error = %v, want bounded-wait timeout", err)
	}
	if elapsed < 15*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("Close returned after %s, want a bounded wait near 20ms", elapsed)
	}

	started = time.Now()
	if err2 := conn.Close(); err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("second Close error = %v, want cached %v", err2, err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("idempotent Close blocked for %s", elapsed)
	}
}

func TestStdioLSPExchangeWithOwnedStdoutPipe(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.py")
	if err := os.WriteFile(source, []byte("value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := lspServer{
		cmd:        executable,
		args:       []string{"-test.run=^TestLSPHelperProcess$", "--", "lsp-helper-process"},
		languageID: "python",
	}
	res, err := runStdioLSPQueryWithSandbox(context.Background(), srv, "hover", source, 1, 1, nil)
	if err != nil {
		t.Fatalf("runStdioLSPQueryWithSandbox: %v", err)
	}
	if res == nil || res.IsError || res.Output != dir {
		resolvedDir, resolveErr := filepath.EvalSymlinks(dir)
		if resolveErr != nil || res == nil || res.IsError || res.Output != resolvedDir {
			t.Fatalf("stdio helper result = %#v (lexical dir %q, resolved dir %q, resolveErr %v)", res, dir, resolvedDir, resolveErr)
		}
	}
}

func TestReadLSPSourceFileRejectsOversizedSourceBeforeAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.py")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxLSPSourceBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = readLSPSourceFile(path)
	if !errors.Is(err, errLSPSourceTooLarge) {
		t.Fatalf("readLSPSourceFile error = %v, want errLSPSourceTooLarge", err)
	}
}

func TestStdioLSPRedactsSuccessfulHoverOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.py")
	if err := os.WriteFile(source, []byte("value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := lspServer{
		cmd:        executable,
		args:       []string{"-test.run=^TestLSPHelperProcess$", "--", "lsp-helper-process", "lsp-helper-redaction"},
		languageID: "python",
	}
	res, err := runStdioLSPQueryWithSandbox(context.Background(), srv, "hover", source, 1, 1, nil)
	if err != nil {
		t.Fatalf("runStdioLSPQueryWithSandbox: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("stdio helper result = %#v", res)
	}
	if strings.Contains(res.Output, "super-secret-value") || !strings.Contains(res.Output, "[REDACTED]") {
		t.Fatalf("successful LSP output was not redacted: %q", res.Output)
	}
}

// TestLSPHelperProcess is re-executed as the fake language-server process for
// TestStdioLSPExchangeWithOwnedStdoutPipe. It exits directly so the testing
// framework never writes status text into the JSON-RPC stdout stream.
func TestLSPHelperProcess(t *testing.T) {
	isHelper := false
	redaction := false
	echoOpen := false
	for _, arg := range os.Args {
		if arg == "lsp-helper-process" {
			isHelper = true
		}
		if arg == "lsp-helper-redaction" {
			redaction = true
		}
		if arg == "lsp-helper-echo-open" {
			echoOpen = true
		}
	}
	if !isHelper {
		return
	}
	conn := &lspConn{r: bufio.NewReader(os.Stdin), w: os.Stdout}
	openedText := ""
	for {
		msg, err := conn.readMsg()
		if err != nil {
			os.Exit(2)
		}
		if len(msg.ID) == 0 {
			if echoOpen && msg.Method == "textDocument/didOpen" {
				var params struct {
					TextDocument struct {
						Text string `json:"text"`
					} `json:"textDocument"`
				}
				if json.Unmarshal(msg.Params, &params) == nil {
					openedText = params.TextDocument.Text
				}
			}
			if msg.Method == "exit" {
				os.Exit(0)
			}
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"capabilities": map[string]any{}}
		case "textDocument/hover":
			// Returning PWD pins the Cmd.Dir/non-nil-Env invariant: os/exec
			// otherwise leaves PWD pointing at the Metis launch directory.
			contents := os.Getenv("PWD")
			if redaction {
				contents = "api_key=super-secret-value"
			}
			if echoOpen {
				contents = openedText
			}
			result = map[string]any{"contents": contents}
		}
		if err := conn.writeMsg(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(msg.ID),
			"result":  result,
		}); err != nil {
			os.Exit(3)
		}
	}
}

func TestPathURIRoundTrip(t *testing.T) {
	p := "/Users/x/proj/main.py"
	if got := uriToPath(pathToURI(p)); got != p {
		t.Errorf("round trip: got %q want %q", got, p)
	}
	// Non-file URIs pass through untouched.
	if got := uriToPath("untitled:Untitled-1"); got != "untitled:Untitled-1" {
		t.Errorf("non-file URI should pass through, got %q", got)
	}
}

func TestPathToURIUsesRFC8089EscapingAcrossPlatforms(t *testing.T) {
	tests := []struct {
		name string
		goos string
		path string
		want string
	}{
		{name: "posix reserved bytes", goos: "darwin", path: "/Users/a project/x#y?z%20.py", want: "file:///Users/a%20project/x%23y%3Fz%2520.py"},
		{name: "linux unicode", goos: "linux", path: "/home/用户/文件.py", want: "file:///home/%E7%94%A8%E6%88%B7/%E6%96%87%E4%BB%B6.py"},
		{name: "windows drive", goos: "windows", path: `C:\Work Dir\x#y?.ts`, want: "file:///C:/Work%20Dir/x%23y%3F.ts"},
		{name: "windows unc", goos: "windows", path: `\\server\share name\x%y.ts`, want: "file://server/share%20name/x%25y.ts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathToURIForOS(tt.goos, tt.path); got != tt.want {
				t.Fatalf("pathToURIForOS(%q, %q) = %q, want %q", tt.goos, tt.path, got, tt.want)
			}
			if got := uriToPathForOS(tt.goos, tt.want); got != tt.path {
				t.Fatalf("uriToPathForOS(%q, %q) = %q, want %q", tt.goos, tt.want, got, tt.path)
			}
		})
	}
}

func TestURIToPathHandlesLocalhostAndLeavesNonFileURIsUntouched(t *testing.T) {
	if got := uriToPathForOS("linux", "file://localhost/tmp/a%20b.go"); got != "/tmp/a b.go" {
		t.Fatalf("localhost file URI = %q", got)
	}
	if got := uriToPathForOS("linux", "https://example.test/a%20b"); got != "https://example.test/a%20b" {
		t.Fatalf("non-file URI changed to %q", got)
	}
	malformed := "file:///%zz"
	if got := uriToPathForOS("linux", malformed); got != malformed {
		t.Fatalf("malformed URI = %q, want original %q", got, malformed)
	}
}

func TestFormatLSPHover_Shapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"null", `null`, ""},
		{"markup", `{"contents":{"kind":"markdown","value":"func F()"}}`, "func F()"},
		{"markedstring", `{"contents":{"language":"go","value":"type T struct{}"}}`, "type T struct{}"},
		{"plainstring", `{"contents":"hello"}`, "hello"},
		{"array", `{"contents":["a",{"value":"b"}]}`, "a\nb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatLSPHover(json.RawMessage(c.in))
			if got != c.want {
				t.Errorf("formatLSPHover(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFormatLSPLocations_Shapes(t *testing.T) {
	// Single Location.
	one := `{"uri":"file:///a/b.py","range":{"start":{"line":4,"character":2}}}`
	if got := formatLSPLocations(json.RawMessage(one)); got != "/a/b.py:5:3" {
		t.Errorf("single location: got %q want /a/b.py:5:3", got)
	}
	// Array of Locations (0-based → 1-based conversion).
	arr := `[{"uri":"file:///a/b.py","range":{"start":{"line":0,"character":0}}},` +
		`{"uri":"file:///c/d.py","range":{"start":{"line":9,"character":4}}}]`
	want := "/a/b.py:1:1\n/c/d.py:10:5"
	if got := formatLSPLocations(json.RawMessage(arr)); got != want {
		t.Errorf("array locations: got %q want %q", got, want)
	}
	// LocationLink (targetUri + targetSelectionRange).
	link := `[{"targetUri":"file:///e/f.rs","targetSelectionRange":{"start":{"line":1,"character":7}}}]`
	if got := formatLSPLocations(json.RawMessage(link)); got != "/e/f.rs:2:8" {
		t.Errorf("location link: got %q want /e/f.rs:2:8", got)
	}
	// Null/empty.
	if got := formatLSPLocations(json.RawMessage(`null`)); got != "" {
		t.Errorf("null locations should be empty, got %q", got)
	}
}

func TestLSPProjectRoot(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Marker at the top.
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "lib.rs")
	if err := os.WriteFile(file, []byte("fn main(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lspProjectRoot(file, []string{"Cargo.toml", ".git"}); got != dir {
		t.Errorf("project root: got %q want %q", got, dir)
	}
	// No marker anywhere → falls back to the file's own dir.
	file2 := filepath.Join(sub, "orphan.rs")
	if err := os.WriteFile(file2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lspProjectRoot(file2, []string{"NoSuchMarker"}); got != sub {
		t.Errorf("fallback root: got %q want %q", got, sub)
	}
}
