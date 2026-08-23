package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
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

// PreparedResume is a validated session loaded before the provider and agent
// loop are constructed. Header is intentionally exposed so the composition
// layer can restore provider/model/system defaults before building those
// runtime objects; messages stay private and are applied only through
// ApplyPreparedResume.
type PreparedResume struct {
	SessionID string
	Header    *session.Header
	messages  []llm.Message
}

// PrepareResume validates and loads a session without mutating a Loop or Gate.
// setupRuntime calls this early because provider/model/system must be known
// before the provider client and final system prompt are constructed.
func PrepareResume(store *session.Store, sessionID string) (*PreparedResume, error) {
	if !validResumeSessionID(sessionID) {
		return nil, fmt.Errorf("resume: invalid session id %q", sessionID)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, sessionID+".jsonl")); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("resume: no session matches %q (tip: `metis sessions list` to see ids, or `metis -r` to pick interactively)", sessionID)
		}
		return nil, fmt.Errorf("resume: stat %s: %w", sessionID, err)
	}
	// Bound the physical append-only audit ledger before parsing. This cap is
	// intentionally much higher than the logical context limit because old raw
	// messages remain on disk behind history_replace checkpoints.
	if err := store.CheckResumeSize(sessionID); err != nil {
		return nil, err
	}
	hdr, msgs, err := store.Load(sessionID)
	if err != nil {
		return nil, fmt.Errorf("resume %s: %w", sessionID, err)
	}
	// Apply the context-economics limit only after Load has replayed every
	// history_replace. A compacted session with a large audit ledger therefore
	// resumes from its small logical checkpoint, while genuinely oversized live
	// context is still rejected. Override via METIS_RESUME_MAX_MB.
	if err := session.CheckResumeHistorySize(sessionID, msgs); err != nil {
		return nil, err
	}
	return &PreparedResume{SessionID: sessionID, Header: hdr, messages: msgs}, nil
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
	prepared, err := PrepareResume(store, sessionID)
	if err != nil {
		return nil, err
	}
	return ApplyPreparedResume(prepared, loop, gate, warnOut)
}

// ApplyPreparedResume restores the transcript and session-scoped permission
// state from a session previously loaded by PrepareResume.
func ApplyPreparedResume(prepared *PreparedResume, loop *agent.Loop,
	gate *permission.Gate, warnOut io.Writer) (*ResumeResult, error) {
	if prepared == nil {
		return nil, fmt.Errorf("resume: nil prepared session")
	}
	if warnOut == nil {
		warnOut = os.Stderr
	}
	loop.Restore(prepared.messages)
	hdr := prepared.Header
	if hdr != nil {
		mode := gate.Mode()
		if hdr.Mode != "" {
			mode = permission.Mode(hdr.Mode)
		}
		resumedRules := make([]permission.Rule, 0, len(hdr.AlwaysAllow))
		for _, r := range hdr.AlwaysAllow {
			resumedRules = append(resumedRules, permission.Rule{
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
				Source: permission.ResumedSessionSource(r.Source),
			})
		}
		gate.ResetSessionState(mode, resumedRules)
		if hdr.WorkDir != "" {
			if cwd, _ := os.Getwd(); cwd != "" && cwd != hdr.WorkDir {
				fmt.Fprintf(warnOut,
					"metis: resumed session was in %q, current cwd is %q (not changing dir)\n",
					hdr.WorkDir, cwd)
			}
		}
	}
	return &ResumeResult{SessionID: prepared.SessionID}, nil
}

// validResumeSessionID keeps the raw Stat path and Store.Load path identical.
// Store.path defensively applies filepath.Base, so accepting a separator here
// would let the preflight inspect one file and then load another. Resume is
// intentionally more permissive than --session-id: imported legacy sessions
// may contain spaces or Unicode, provided the id is still a single safe file
// name with no control characters.
func validResumeSessionID(id string) bool {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// WriteFreshHeader stamps the session file with what we know at startup
// for a non-resume run. Pulled out alongside ApplyResume so the
// "either resume an existing session or write a new one" branch in
// setupRuntime stays one line per case.
func WriteFreshHeader(store *session.Store, sessionID, provider, model, system, mode string) error {
	cwd, _ := os.Getwd()
	return store.WriteHeaderFull(session.Header{
		ID:       sessionID,
		Provider: provider,
		Model:    model,
		System:   system,
		WorkDir:  cwd,
		Mode:     mode,
	})
}
