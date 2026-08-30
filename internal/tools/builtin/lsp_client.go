package builtin

// lsp_client.go — a minimal LSP client over stdio for language servers
// that have no query CLI (pyright / typescript-language-server /
// rust-analyzer). Speaks just enough of the protocol for one-shot
// hover / definition / references / implementation lookups:
//
//	spawn → initialize → initialized → didOpen → <query> → shutdown → exit
//
// It is deliberately NOT a long-lived client: each query spins a fresh
// server, asks one question, and tears down. That's slower than a warm
// client but dramatically simpler and correct — no document-sync state to
// keep coherent, no crash recovery. Every backend, including gopls, uses this
// path so didOpen receives bytes from the invocation's approved descriptor.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const (
	maxLSPHeaderBytes = 64 << 10
	maxLSPFrameBytes  = 64 << 20
	// Language servers receive the complete source through didOpen. Reject
	// pathological/generated files before ReadAll allocates for them. This is
	// intentionally much smaller than Read's user-facing cap: LSP adds a JSON
	// copy plus the server's own parsed syntax tree on top of the source bytes.
	maxLSPSourceBytes int64 = 16 << 20
)

var (
	errLSPHeaderTooLarge = errors.New("lsp header too large")
	errLSPFrameTooLarge  = errors.New("lsp frame too large")
	errLSPSourceTooLarge = errors.New("lsp source file too large")
	errLSPSourceChanged  = errors.New("lsp source changed while opening")
)

// lspConn is one stdio JSON-RPC connection to a language server.
type lspConn struct {
	cmd       *exec.Cmd
	w         io.WriteCloser
	stdout    io.Closer
	r         *bufio.Reader
	tree      *lspProcessTreeHandle
	waitDone  <-chan error
	waitLimit time.Duration
	nextID    int
	closeOnce sync.Once
	closeErr  error
}

// lspRawMsg is a decoded JSON-RPC envelope. Result/Error are present on
// responses; Method/Params on requests and notifications; ID present on
// requests and responses but not notifications.
type lspRawMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// writeMsg frames a JSON-RPC message with the LSP Content-Length header
// and writes it. Unlike MCP's NDJSON, LSP requires header framing.
func (c *lspConn) writeMsg(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

// readMsg reads one framed message. Both the header block and body are
// strictly bounded before allocation: language servers are external
// processes and malformed framing must not grow Metis memory without limit.
func (c *lspConn) readMsg() (*lspRawMsg, error) {
	contentLen := -1
	headerBytes := 0
	for {
		line, n, err := readLSPHeaderLine(c.r, maxLSPHeaderBytes-headerBytes)
		headerBytes += n
		if err != nil {
			return nil, err
		}
		if len(line) < 2 || line[len(line)-2] != '\r' || line[len(line)-1] != '\n' {
			return nil, errors.New("malformed lsp header: lines must end with CRLF")
		}
		line = line[:len(line)-2]
		if len(line) == 0 {
			break // end of headers
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok || !validLSPHeaderName(name) {
			return nil, fmt.Errorf("malformed lsp header line %q", line)
		}
		if !strings.EqualFold(name, "Content-Length") {
			continue
		}
		if contentLen >= 0 {
			return nil, errors.New("duplicate Content-Length header")
		}
		value = strings.TrimSpace(value)
		parsed, err := strconv.ParseUint(value, 10, 63)
		if err != nil || parsed == 0 {
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", value, err)
			}
			return nil, errors.New("invalid Content-Length 0: must be positive")
		}
		if parsed > maxLSPFrameBytes {
			return nil, fmt.Errorf("%w: %d bytes exceeds %d-byte limit", errLSPFrameTooLarge, parsed, maxLSPFrameBytes)
		}
		contentLen = int(parsed)
	}
	if contentLen < 0 {
		return nil, errors.New("missing Content-Length header")
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, fmt.Errorf("read lsp body (%d bytes): %w", contentLen, err)
	}
	var m lspRawMsg
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, fmt.Errorf("decode lsp message: %w", err)
	}
	return &m, nil
}

// readLSPHeaderLine reads one complete header line while charging every byte
// against remaining. ReadSlice's fixed buffer prevents a server-controlled
// line from allocating before the aggregate header limit is enforced.
func readLSPHeaderLine(r *bufio.Reader, remaining int) ([]byte, int, error) {
	if remaining <= 0 {
		return nil, 0, fmt.Errorf("%w: exceeds %d-byte limit", errLSPHeaderTooLarge, maxLSPHeaderBytes)
	}
	line := make([]byte, 0, min(remaining, r.Size()))
	read := 0
	for {
		fragment, err := r.ReadSlice('\n')
		read += len(fragment)
		if read > remaining {
			return nil, read, fmt.Errorf("%w: exceeds %d-byte limit", errLSPHeaderTooLarge, maxLSPHeaderBytes)
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, read, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if read == remaining {
				return nil, read, fmt.Errorf("%w: exceeds %d-byte limit", errLSPHeaderTooLarge, maxLSPHeaderBytes)
			}
			continue
		case errors.Is(err, io.EOF):
			return nil, read, errors.New("read lsp header: unexpected EOF")
		default:
			return nil, read, fmt.Errorf("read lsp header: %w", err)
		}
	}
}

func validLSPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
}

// Close terminates the complete language-server process tree, closes both
// stdio directions so an in-flight read is released, and waits only for a
// fixed bound. Cmd.Wait has exactly one owner: the goroutine created after
// Start, represented by waitDone.
func (c *lspConn) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		defer closeLSPProcessTreeHandle(c.tree)
		// Kill first: closing stdin can let the leader exit and be reaped
		// while a helper remains in its process group.
		if c.tree != nil {
			terminateLSPProcessTreeHandle(c.tree)
		} else {
			killLSPProcessTree(c.cmd)
		}
		if c.w != nil {
			_ = c.w.Close()
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.waitDone == nil {
			return
		}
		waitLimit := c.waitLimit
		if waitLimit <= 0 {
			waitLimit = lspProcessWaitLimit
		}
		if !waitForLSPProcess(c.waitDone, waitLimit) {
			c.closeErr = fmt.Errorf("timed out after %s waiting for LSP process exit", waitLimit)
		}
	})
	return c.closeErr
}

// request sends a JSON-RPC request and returns the matching response
// result. While waiting it drains server notifications (ignored) and
// server→client requests (answered with a null result so the server's
// handshake — registerCapability / configuration — doesn't stall).
func (c *lspConn) request(method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	if err := c.writeMsg(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}
	want := strconv.Itoa(id)
	for {
		m, err := c.readMsg()
		if err != nil {
			return nil, err
		}
		// Response to our request: ID matches and there's no Method.
		if m.Method == "" && len(m.ID) > 0 && string(m.ID) == want {
			if m.Error != nil {
				return nil, fmt.Errorf("lsp %s: %s", method, m.Error.Message)
			}
			return m.Result, nil
		}
		// Server→client request (has both ID and Method): reply null so
		// the server doesn't block on registerCapability / configuration.
		if m.Method != "" && len(m.ID) > 0 {
			_ = c.writeMsg(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "result": nil})
			continue
		}
		// Notification (Method, no ID) — ignore.
	}
}

// notify sends a fire-and-forget JSON-RPC notification.
func (c *lspConn) notify(method string, params any) error {
	return c.writeMsg(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// runStdioLSPQuery spins up the server for srv, runs one query at
// path:line:col (1-based), and returns a human-readable result. line/col
// are converted to LSP's 0-based positions internally.
func runStdioLSPQuery(ctx context.Context, srv lspServer, action, path string, line, col int) (*tools.Result, error) {
	return runStdioLSPQueryWithSandbox(ctx, srv, action, path, line, col, nil)
}

func runStdioLSPQueryWithSandbox(ctx context.Context, srv lspServer, action, path string, line, col int, manager *sandbox.Manager) (*tools.Result, error) {
	approved, err := prepareExistingPath(path, false)
	if err != nil {
		return &tools.Result{
			Output:  fmt.Sprintf("LSP: cannot read %s: %s", filepath.Base(path), security.RedactSubprocessText(err.Error())),
			IsError: true,
		}, nil
	}
	return runApprovedStdioLSPQueryWithSandbox(ctx, srv, action, approved, line, col, manager, nil)
}

func runApprovedStdioLSPQueryWithSandbox(ctx context.Context, srv lspServer, action string, approved approvedExistingPath, line, col int, manager *sandbox.Manager, afterOpen func()) (*tools.Result, error) {
	f, _, err := openApprovedExisting(approved, os.O_RDONLY, afterOpen)
	if err != nil {
		return &tools.Result{Output: "LSP denied: approved source changed before execution", IsError: true}, nil
	}
	defer f.Close()
	src, _, err := readPinnedFile(f, maxLSPSourceBytes)
	if err != nil {
		return &tools.Result{
			Output:  fmt.Sprintf("LSP: cannot read %s: %s", filepath.Base(approved.rawPath), security.RedactSubprocessText(err.Error())),
			IsError: true,
		}, nil
	}
	path := approved.resolvedPath

	// Bound the whole exchange so a wedged server can't hang the tool.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	root := lspProjectRoot(path, srv.rootMarkers)
	cmd := exec.CommandContext(cctx, srv.cmd, srv.args...)
	cmd.Dir = root
	// A non-nil Cmd.Env disables os/exec's automatic PWD rewrite for Cmd.Dir.
	// Keep language-server and toolchain project discovery consistent.
	cmd.Env = security.RestrictedSubprocessEnv(os.Environ(), "PWD="+root)
	if manager != nil {
		cmd.Env = manager.FilterEnv(cmd.Env, false)
		wrapped, err := manager.Wrap(cmd, sandbox.Request{Cwd: root})
		if err != nil {
			return &tools.Result{
				Output:  fmt.Sprintf("LSP: sandbox wrap failed for %s: %s", srv.cmd, security.RedactSubprocessText(err.Error())),
				IsError: true,
			}, nil
		}
		cmd = wrapped
	}
	configureLSPProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own the stdout pipe explicitly. Cmd.Wait is allowed to run concurrently
	// only because this pipe is not one of os/exec's parentIOPipes; Wait must
	// never close a StdoutPipe while readMsg is still consuming a frame.
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stdout = childStdout
	cmd.Stderr = nil // discard server logs; they're noise for one-shot queries
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = childStdout.Close()
		return &tools.Result{Output: fmt.Sprintf("LSP: failed to start %s: %s", srv.cmd, security.RedactSubprocessText(err.Error())), IsError: true}, nil
	}
	tree := attachLSPProcessTree(cmd)
	// The child inherited its own descriptor during Start. Closing the parent's
	// duplicate is required so the reader observes EOF when the child exits.
	_ = childStdout.Close()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	conn := &lspConn{
		cmd:      cmd,
		w:        stdin,
		stdout:   stdout,
		r:        bufio.NewReader(stdout),
		tree:     tree,
		waitDone: waitDone,
	}
	defer func() { _ = conn.Close() }()
	uri := pathToURI(path)

	// Run the exchange in a goroutine so the context timeout actually
	// unblocks us if the server stops responding mid-read.
	type qres struct {
		out string
		err error
	}
	done := make(chan qres, 1)
	go func() {
		out, err := lspExchange(conn, srv, action, uri, string(src), root, line-1, col-1)
		done <- qres{out, err}
	}()

	select {
	case <-cctx.Done():
		output := fmt.Sprintf("LSP %s on %s: timed out after 20s", action, srv.cmd)
		if ctx.Err() != nil {
			output = fmt.Sprintf("LSP %s on %s: cancelled by caller", action, srv.cmd)
		}
		return &tools.Result{Output: output, IsError: true}, nil
	case r := <-done:
		if r.err != nil {
			return &tools.Result{Output: fmt.Sprintf("LSP %s (%s): %s", action, srv.cmd, security.RedactSubprocessText(r.err.Error())), IsError: true}, nil
		}
		body := security.RedactSubprocessText(strings.TrimSpace(r.out))
		if body == "" {
			return &tools.Result{Output: "(no result)"}, nil
		}
		return &tools.Result{Output: body}, nil
	}
}

// inspectLSPSourcePath resolves the actual file an LSP server would open and
// validates its shape/size without reading its contents. Callers use the
// canonical path for both permission-stability checks and the server URI, so a
// workspace-local symlink cannot silently change targets between CanUse and
// Execute.
func inspectLSPSourcePath(path string) (string, os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve source path: %w", err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("source is not a regular file")
	}
	if info.Size() > maxLSPSourceBytes {
		return "", nil, fmt.Errorf("%w: %d bytes exceeds %d-byte limit", errLSPSourceTooLarge, info.Size(), maxLSPSourceBytes)
	}
	return resolved, info, nil
}

// readLSPSourceFile performs a stat-before-allocation bounded read. The
// SameFile check closes the remaining stat/open race: if an attacker swaps the
// path after canonicalisation, Metis refuses the content instead of sending a
// different file to the model/provider.
func readLSPSourceFile(path string) ([]byte, string, error) {
	resolved, before, err := inspectLSPSourcePath(path)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("open source: %w", err)
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat opened source: %w", err)
	}
	if !os.SameFile(before, after) {
		return nil, "", errLSPSourceChanged
	}
	if after.Size() > maxLSPSourceBytes {
		return nil, "", fmt.Errorf("%w: %d bytes exceeds %d-byte limit", errLSPSourceTooLarge, after.Size(), maxLSPSourceBytes)
	}
	src, err := io.ReadAll(io.LimitReader(f, maxLSPSourceBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read source: %w", err)
	}
	if int64(len(src)) > maxLSPSourceBytes {
		return nil, "", fmt.Errorf("%w: content exceeded %d-byte limit while reading", errLSPSourceTooLarge, maxLSPSourceBytes)
	}
	return src, resolved, nil
}

// lspExchange performs the full handshake + query + shutdown and returns
// the formatted result body.
func lspExchange(conn *lspConn, srv lspServer, action, uri, src, root string, line, char int) (string, error) {
	if _, err := conn.request("initialize", map[string]any{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"hover":          map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"definition":     map[string]any{},
				"references":     map[string]any{},
				"implementation": map[string]any{},
			},
		},
	}); err != nil {
		return "", err
	}
	if err := conn.notify("initialized", map[string]any{}); err != nil {
		return "", err
	}
	if err := conn.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": srv.languageID, "version": 1, "text": src,
		},
	}); err != nil {
		return "", err
	}

	pos := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var body string
	var err error
	switch action {
	case "hover":
		var res json.RawMessage
		res, err = conn.request("textDocument/hover", pos)
		body = formatLSPHover(res)
	case "definition":
		var res json.RawMessage
		res, err = conn.request("textDocument/definition", pos)
		body = formatLSPLocations(res)
	case "references":
		refParams := map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     map[string]any{"line": line, "character": char},
			"context":      map[string]any{"includeDeclaration": true},
		}
		var res json.RawMessage
		res, err = conn.request("textDocument/references", refParams)
		body = formatLSPLocations(res)
	case "implementations":
		var res json.RawMessage
		res, err = conn.request("textDocument/implementation", pos)
		body = formatLSPLocations(res)
	default:
		return "", fmt.Errorf("unknown LSP action %q", action)
	}
	if err != nil {
		return "", err
	}

	// Best-effort graceful shutdown; ignore errors (we kill the process
	// in the caller's defer regardless).
	_, _ = conn.request("shutdown", nil)
	_ = conn.notify("exit", nil)
	return body, nil
}

// formatLSPHover extracts readable text from a hover result, which the
// spec allows to be a string, a {kind,value} MarkupContent, a
// {language,value} MarkedString, or an array of those.
func formatLSPHover(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var hov struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hov); err != nil {
		return ""
	}
	return strings.TrimSpace(extractMarkup(hov.Contents))
}

// extractMarkup flattens the several shapes hover `contents` can take.
func extractMarkup(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// {kind,value} or {language,value}
	var obj struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Value != "" {
		return obj.Value
	}
	// array of any of the above
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, el := range arr {
			if p := extractMarkup(el); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// formatLSPLocations renders Location | Location[] | LocationLink[] as
// "file:line:col" lines (1-based, to match the tool's input convention).
func formatLSPLocations(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	type lspPos struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	}
	type lspRange struct {
		Start lspPos `json:"start"`
	}
	// Try array first (definition/references usually return arrays).
	var arr []struct {
		URI            string    `json:"uri"`
		Range          lspRange  `json:"range"`
		TargetURI      string    `json:"targetUri"`
		TargetRange    *lspRange `json:"targetRange"`
		TargetSelRange *lspRange `json:"targetSelectionRange"`
	}
	render := func(uri string, rng lspRange) string {
		p := uriToPath(uri)
		return fmt.Sprintf("%s:%d:%d", p, rng.Start.Line+1, rng.Start.Character+1)
	}
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		var lines []string
		for _, l := range arr {
			if l.URI != "" {
				lines = append(lines, render(l.URI, l.Range))
			} else if l.TargetURI != "" {
				rng := lspRange{}
				if l.TargetSelRange != nil {
					rng = *l.TargetSelRange
				} else if l.TargetRange != nil {
					rng = *l.TargetRange
				}
				lines = append(lines, render(l.TargetURI, rng))
			}
		}
		return strings.Join(lines, "\n")
	}
	// Single Location object.
	var one struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	}
	if json.Unmarshal(raw, &one) == nil && one.URI != "" {
		return render(one.URI, one.Range)
	}
	return ""
}

// lspProjectRoot walks up from the file's dir looking for a root marker;
// falls back to the file's own dir.
func lspProjectRoot(path string, markers []string) string {
	dir := filepath.Dir(path)
	cur := dir
	for {
		for _, m := range markers {
			if _, err := os.Stat(filepath.Join(cur, m)); err == nil {
				return cur
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break // reached filesystem root
		}
		cur = parent
	}
	return dir
}

// pathToURI converts an absolute path to a file:// URI.
func pathToURI(p string) string {
	return pathToURIForOS(runtime.GOOS, p)
}

// uriToPath converts a file:// URI back to a path, leaving non-file URIs
// untouched.
func uriToPath(u string) string {
	return uriToPathForOS(runtime.GOOS, u)
}

// pathToURIForOS is split from pathToURI so Windows drive and UNC behaviour is
// covered on Unix CI too. url.URL performs the RFC 3986 escaping that naive
// "file://" concatenation misses for spaces, '#', '?', '%' and Unicode.
func pathToURIForOS(goos, p string) string {
	if goos == "windows" {
		slash := strings.ReplaceAll(p, `\`, "/")
		if strings.HasPrefix(slash, "//") {
			rest := strings.TrimPrefix(slash, "//")
			host, tail, ok := strings.Cut(rest, "/")
			if ok && host != "" {
				return (&url.URL{Scheme: "file", Host: host, Path: "/" + tail}).String()
			}
		}
		if len(slash) >= 2 && slash[1] == ':' && !strings.HasPrefix(slash, "/") {
			slash = "/" + slash
		}
		return (&url.URL{Scheme: "file", Path: slash}).String()
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(p)}).String()
}

func uriToPathForOS(goos, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return raw
	}
	host := u.Host
	if strings.EqualFold(host, "localhost") {
		host = ""
	}
	path := u.Path // url.Parse has already decoded percent escapes safely.
	if goos == "windows" {
		if host != "" {
			return `\\` + host + strings.ReplaceAll(path, "/", `\`)
		}
		if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		return strings.ReplaceAll(path, "/", `\`)
	}
	if host != "" {
		return "//" + host + path
	}
	return filepath.FromSlash(path)
}
