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
// keep coherent, no crash recovery. gopls keeps using its CLI path in
// lsp.go; this client backs every other language.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// lspConn is one stdio JSON-RPC connection to a language server.
type lspConn struct {
	cmd    *exec.Cmd
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int
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

// readMsg reads one framed message: parse headers until the blank line,
// then read exactly Content-Length bytes.
func (c *lspConn) readMsg() (*lspRawMsg, error) {
	contentLen := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if cl, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			contentLen, err = strconv.Atoi(strings.TrimSpace(cl))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
		}
	}
	if contentLen < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	// Cap the frame size so a buggy/garbled server emitting a huge or
	// corrupt Content-Length can't trigger a multi-GB allocation that
	// OOM-kills metis. 64 MiB is far beyond any real hover/definition
	// payload (mirrors the MCP client's MaxStderrBytes ceiling).
	const maxLSPFrame = 64 << 20
	if contentLen > maxLSPFrame {
		return nil, fmt.Errorf("lsp frame too large: %d bytes", contentLen)
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	var m lspRawMsg
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, fmt.Errorf("decode lsp message: %w", err)
	}
	return &m, nil
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
	src, err := os.ReadFile(path)
	if err != nil {
		return &tools.Result{Output: fmt.Sprintf("LSP: cannot read %s: %v", filepath.Base(path), err), IsError: true}, nil
	}

	// Bound the whole exchange so a wedged server can't hang the tool.
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, srv.cmd, srv.args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // discard server logs; they're noise for one-shot queries
	if err := cmd.Start(); err != nil {
		return &tools.Result{Output: fmt.Sprintf("LSP: failed to start %s: %v", srv.cmd, err), IsError: true}, nil
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	conn := &lspConn{cmd: cmd, w: stdin, r: bufio.NewReader(stdout)}
	root := lspProjectRoot(path, srv.rootMarkers)
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
		return &tools.Result{Output: fmt.Sprintf("LSP %s on %s: timed out after 20s", action, srv.cmd), IsError: true}, nil
	case r := <-done:
		if r.err != nil {
			return &tools.Result{Output: fmt.Sprintf("LSP %s (%s): %v", action, srv.cmd, r.err), IsError: true}, nil
		}
		if strings.TrimSpace(r.out) == "" {
			return &tools.Result{Output: "(no result)"}, nil
		}
		return &tools.Result{Output: r.out}, nil
	}
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
	return "file://" + p
}

// uriToPath converts a file:// URI back to a path, leaving non-file URIs
// untouched.
func uriToPath(u string) string {
	if rest, ok := strings.CutPrefix(u, "file://"); ok {
		return rest
	}
	return u
}
