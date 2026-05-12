package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// ResumeResult is what ApplyResume hands back: the restored session id
// and any whitelisted state already applied to the gate / loop.
//
// Today the only return is the id; struct shape future-proofs against
// callers wanting to know e.g. "did we change the cwd warning?" without
// inspecting stderr.
type ResumeResult struct {
	SessionID string
}

// ApplyResume restores a previous session into a freshly built Loop +
// Gate. Returns ResumeResult with the resumed id, or an error if the
// session can't be loaded.
//
// State restored from the header (whitelist):
//   - Loop.Messages (full transcript)
//   - Gate mode (only if non-empty in header)
//   - Gate always-allow rules (verbatim, with original Source preserved)
//
// State explicitly NOT restored (own stores / would bleed across sessions):
//   - cron jobs (live in ~/.metis/cron, scoped per-process)
//   - loop detector counters (transient runtime state)
//   - memory tier contents (own store under ~/.metis/memory)
//
// A working-dir mismatch warning lands on stderr but doesn't error —
// the user might intentionally resume a session from a different repo.
//
// Output writer is parameterized so tests don't pollute stderr; pass
// os.Stderr in production.
func ApplyResume(store *session.Store, sessionID string, loop *agent.Loop,
	gate *permission.Gate, warnOut io.Writer) (*ResumeResult, error) {
	if warnOut == nil {
		warnOut = os.Stderr
	}
	// Resolve user-supplied id. The picker prints truncated 12-char
	// prefixes (`0f8244a9-167`) for readability, but the on-disk file
	// is keyed by the full UUID (`0f8244a9-1671-437d-…`). Without this
	// step the user copying the picker's display into `--resume <id>`
	// hits ENOENT — the literal complaint that drove this commit.
	//
	// Resolution: if `sessionID.jsonl` exists, use it verbatim; else
	// scan ~/.metis/sessions for files whose name starts with the
	// input — unique match wins, ambiguous match errors out and lists
	// the candidates so the user knows what to type instead.
	resolved, err := ResolveSessionID(store, sessionID)
	if err != nil {
		return nil, err
	}
	sessionID = resolved
	// Pre-flight size check (#43 / openclaude path): refuse to load a
	// transcript larger than session.DefaultResumeMaxBytes (8 MiB).
	// Past that point, even a successful resume usually starves the
	// model's context window and burns tokens; better to /clear or
	// /branch from an earlier turn. Override via METIS_RESUME_MAX_MB.
	if err := store.CheckResumeSize(sessionID); err != nil {
		return nil, err
	}
	hdr, msgs, err := store.Load(sessionID)
	if err != nil {
		return nil, fmt.Errorf("resume %s: %w", sessionID, err)
	}
	loop.Messages = msgs
	if hdr != nil {
		if hdr.Mode != "" {
			gate.SetMode(permission.Mode(hdr.Mode))
		}
		for _, r := range hdr.AlwaysAllow {
			gate.AppendRules(permission.Rule{
				Tool: r.Tool, Match: r.Match,
				Verb: permission.Decision(r.Verb), Source: r.Source,
			})
		}
		if hdr.WorkDir != "" {
			if cwd, _ := os.Getwd(); cwd != "" && cwd != hdr.WorkDir {
				fmt.Fprintf(warnOut,
					"metis: resumed session was in %q, current cwd is %q (not changing dir)\n",
					hdr.WorkDir, cwd)
			}
		}
	}
	return &ResumeResult{SessionID: sessionID}, nil
}

// ResolveSessionID maps a user-supplied identifier to a full session id.
//
// Accepts (in order):
//
//  1. A full UUID whose `<id>.jsonl` already exists — returned verbatim.
//  2. A prefix that uniquely matches one on-disk session file — the full
//     id is returned. Mirrors `gh issue view 12` / `git log <prefix>` —
//     short ids are the ergonomic default, full ids stay valid.
//  3. An ambiguous prefix — returns an error listing every match so the
//     user knows what to type to disambiguate.
//
// Empty input is an error (callers should already have filtered that out;
// we double-check defensively so a malformed call doesn't return "" success).
//
// Cost: one ReadDir of ~/.metis/sessions. Sessions list typically stays
// in the low hundreds; if it ever grows enough to matter, swap List() for
// a streaming filename scan.
func ResolveSessionID(store *session.Store, input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		return "", fmt.Errorf("resume: empty session id")
	}
	// 1) Fast path — verbatim file exists.
	if _, err := os.Stat(filepath.Join(store.Dir, input+".jsonl")); err == nil {
		return input, nil
	}
	// 2) Prefix scan.
	entries, err := store.List(0)
	if err != nil {
		return "", fmt.Errorf("resume: list sessions: %w", err)
	}
	var matches []string
	for _, e := range entries {
		if strings.HasPrefix(e.ID, input) {
			matches = append(matches, e.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("resume: no session matches %q (tip: `metis sessions list` to see ids)", input)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		// Cap the displayed list — a 20-way ambiguity is unusual but
		// shouldn't dump 400 lines onto the user's terminal.
		const showMax = 8
		shown := matches
		more := ""
		if len(shown) > showMax {
			shown = shown[:showMax]
			more = fmt.Sprintf(" (+%d more)", len(matches)-showMax)
		}
		return "", fmt.Errorf("resume: prefix %q is ambiguous, matches %d sessions: %s%s",
			input, len(matches), strings.Join(shown, ", "), more)
	}
}

// WriteFreshHeader stamps the session file with what we know at startup
// for a non-resume run. Pulled out alongside ApplyResume so the
// "either resume an existing session or write a new one" branch in
// setupRuntime stays one line per case.
func WriteFreshHeader(store *session.Store, sessionID, model, system, mode string) error {
	cwd, _ := os.Getwd()
	return store.WriteHeaderFull(session.Header{
		ID:      sessionID,
		Model:   model,
		System:  system,
		WorkDir: cwd,
		Mode:    mode,
	})
}
