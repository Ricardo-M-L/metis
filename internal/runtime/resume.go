package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// Defense: `--resume <id>` requires the FULL UUID (claude-code parity
	// — see print.ts:5041). The interactive picker is the way to discover
	// ids the user doesn't already have; prefix-resume was tried 2026-05-13
	// and reverted because (a) it papered over the real bug (picker
	// truncating display) and (b) hashes change as sessions grow, so
	// "this prefix is unique today" is a future trap. We do, however,
	// upgrade ENOENT into a friendlier error.
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("resume: empty session id")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, sessionID+".jsonl")); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("resume: no session matches %q (tip: `metis sessions list` to see ids, or `metis -r` to pick interactively)", sessionID)
		}
		return nil, fmt.Errorf("resume: stat %s: %w", sessionID, err)
	}
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
				Verb: permission.Decision(r.Verb),
				// Sanitize the source through the resume boundary: a
				// session file is user-editable, and the gate ranks
				// authority by source prefix — a forged "policy*" /
				// "cli*" source would otherwise resurrect with
				// top-rank, un-overridable authority (2026-06-11
				// review finding). Legit policy/cli rules are re-built
				// fresh at boot anyway, so resumed copies never need
				// those ranks.
				Source: permission.SanitizeResumedSource(r.Source),
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
