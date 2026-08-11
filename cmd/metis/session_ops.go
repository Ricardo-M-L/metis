package main

// cmd_session_ops.go — Phase E #42-#45. Top-level session management
// subcommands the user expects to find next to `metis chat`:
//
//   metis ps                — list active sessions (newest first)
//   metis logs <id>         — print the session jsonl as a transcript
//   metis kill <id>         — SIGTERM the metis process holding <id>
//                             (works when a pidfile exists under
//                             ~/.metis/run/<id>.pid)
//   metis attach <id>       — equivalent to `metis chat -r <id>`;
//                             named for parity with `tmux attach`
//
// We deliberately keep these on-disk-store backed for v1 — no Unix
// socket / IPC layer yet. The plan calls out a future daemon that
// exposes a richer attach pipeline; once that lands, ps/logs/kill
// flip to talking to the daemon socket if it's running.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// cmdPs lists every session under ~/.metis/sessions, newest first.
// `--limit N` caps the output (default 20).
func cmdPs(_ context.Context, args []string) error {
	limit := 20
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit", "-n":
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil && v > 0 {
					limit = v
				}
				i++
			}
		case "--help", "-h":
			fmt.Println("usage: metis ps [--limit N]")
			fmt.Println("  list recent sessions, newest first.")
			return nil
		}
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	store, err := session.NewStore(cfg.Session.Dir)
	if err != nil {
		return err
	}
	entries, err := store.List(limit)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("(no sessions yet — start one with `metis chat`)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCREATED\tMODEL\tBYTES\tTITLE\tPID")
	for _, e := range entries {
		short := e.ID
		if len(short) > 12 {
			short = short[:12]
		}
		title := e.Title
		if title == "" {
			title = "(untitled)"
		}
		pid := readPidIfExists(e.ID)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			short, e.CreatedAt.Format("2006-01-02 15:04"),
			e.Model, e.Bytes, title, pid)
	}
	return tw.Flush()
}

// cmdLogs prints the jsonl transcript of a session. Compact format:
// each user/assistant message on its own block with role + first line
// + length. Useful for quick scan; for the full payload pipe stdout to
// `jq` or open the file directly.
func cmdLogs(_ context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: metis logs <session-id>")
	}
	id := args[0]
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.Session.Dir, id+".jsonl")
	if _, err := os.Stat(path); err != nil {
		// Try resolving short ids (first 12 chars) — the convention ps
		// uses to display.
		if matches, _ := filepath.Glob(filepath.Join(cfg.Session.Dir, id+"*.jsonl")); len(matches) == 1 {
			path = matches[0]
		} else if len(matches) > 1 {
			return fmt.Errorf("ambiguous session id %q (matches %d files)", id, len(matches))
		} else {
			return fmt.Errorf("no such session: %s", id)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		// Header line — print compactly.
		if model, ok := rec["model"].(string); ok && model != "" {
			fmt.Printf("== session %s · model=%s ==\n", id, model)
			continue
		}
		role, _ := rec["role"].(string)
		if role == "" {
			continue
		}
		text := flattenContent(rec["content"])
		first := text
		if i := strings.Index(first, "\n"); i >= 0 {
			first = first[:i]
		}
		if len(first) > 120 {
			first = first[:117] + "…"
		}
		fmt.Printf("%-9s · %d chars · %s\n", role, len(text), first)
	}
	return nil
}

// cmdKill reads ~/.metis/run/<id>.pid and SIGTERMs the recorded pid.
// Soft-fails when no pidfile exists ("session not running"). Pidfiles
// are written by chat sessions on startup; today only the long-running
// daemon mode populates them, so a chat session started in the
// foreground has no pidfile. The kill-by-pid path stays useful for
// `metis daemon` and any future `metis attach` socket server.
func cmdKill(_ context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: metis kill <session-id>")
	}
	id := args[0]
	pid := readPidIfExists(id)
	if pid == "" || pid == "-" {
		return fmt.Errorf("no pidfile for session %s — was it started in foreground? "+
			"(only daemon-tracked sessions write a pidfile under ~/.metis/run/)", id)
	}
	pidNum, err := strconv.Atoi(pid)
	if err != nil {
		return fmt.Errorf("malformed pidfile for %s: %v", id, err)
	}
	if err := processutil.Terminate(pidNum); err != nil {
		return fmt.Errorf("kill %d: %v", pidNum, err)
	}
	fmt.Printf("(SIGTERM sent to pid %d for session %s)\n", pidNum, id)
	return nil
}

// cmdAttach is presently equivalent to `metis chat -r <id>`. Named
// separately for `tmux attach` parity (users expect ps + attach as a
// pair). Once a real attach pipeline lands, this routes to the
// daemon's socket; until then we just thread to chat with a resume.
func cmdAttach(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: metis attach <session-id>")
	}
	id := args[0]
	// Forward any extra args after id verbatim — lets the user pass
	// `metis attach <id> --bare` or similar.
	rest := args[1:]
	full := append([]string{"-r", id}, rest...)
	return cmdChat(ctx, full)
}

// readPidIfExists looks under ~/.metis/run/<id>.pid for the recorded
// process id. Returns "-" when no pidfile is present, the raw string
// otherwise (caller decides whether to parse as int).
func readPidIfExists(sessionID string) string {
	dir := filepath.Join(config.Home(), "run")
	path := filepath.Join(dir, sessionID+".pid")
	data, err := os.ReadFile(path)
	if err != nil {
		return "-"
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		return "-"
	}
	// Verify the process is still alive — a stale pidfile shows "(dead)"
	// rather than a misleading number.
	if pidNum, err := strconv.Atoi(pid); err == nil {
		if !processutil.Alive(pidNum) {
			return pid + "(dead)"
		}
	}
	return pid
}

// flattenContent extracts text from an llm.Message-shaped content
// list. Tool calls / tool results are summarized rather than rendered
// in full — we want a quick scan, not a transcript replay.
func flattenContent(raw any) string {
	if raw == nil {
		return ""
	}
	arr, ok := raw.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, c := range arr {
		cm, _ := c.(map[string]any)
		t, _ := cm["type"].(string)
		switch t {
		case "text":
			if s, ok := cm["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(s)
			}
		case "tool_use":
			name, _ := cm["name"].(string)
			fmt.Fprintf(&b, "[→ %s]", name)
		case "tool_result":
			if isErr, _ := cm["is_error"].(bool); isErr {
				b.WriteString("[← error]")
			} else {
				b.WriteString("[← ok]")
			}
		case "thinking":
			b.WriteString("[…thinking…]")
		}
	}
	_ = time.Now() // keep time import warm for future timestamp formatting
	return b.String()
}
