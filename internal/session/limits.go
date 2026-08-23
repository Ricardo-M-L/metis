package session

// limits.go — bounded physical-ledger loading plus logical-history limits.
//
// Why: metis sessions live as JSONL — one line per message — so a
// long-running session with bash outputs / file dumps / pasted logs can
// balloon to tens of MB. history_replace entries make the *logical* resumed
// conversation small after compaction, but intentionally retain the old raw
// messages in the append-only audit ledger. A single 8 MiB stat check therefore
// rejects healthy compacted sessions based on dead physical bytes.
//
// Resume now has two independent guards:
//   - a generous physical ledger cap before parsing, bounding disk I/O and
//     hostile/corrupt files without penalizing normal compaction;
//   - the original 8 MiB limit applied after replaying history_replace, so it
//     measures only the messages that will actually enter the agent loop.
//
// Claude Code itself doesn't enforce this (only its Bash output
// cache has an 8 MiB cap, not the resume path) — confirmed from
// claude-code-sourcemap restored-src/. openclaude's path is the
// reference here.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// DefaultResumeMaxBytes is the logical-history cliff for "context so large
// that resume will fail or waste budget." Override with METIS_RESUME_MAX_MB.
const DefaultResumeMaxBytes int64 = 8 * 1024 * 1024

// DefaultResumePhysicalMaxBytes bounds the append-only audit ledger read. It
// deliberately leaves room for many generations of raw history followed by
// compact history_replace checkpoints while still preventing an unbounded
// resume-time scan. Override with METIS_RESUME_PHYSICAL_MAX_MB.
const DefaultResumePhysicalMaxBytes int64 = 256 * 1024 * 1024

func resumeMaxBytes() int64 {
	return resumeLimitFromEnv("METIS_RESUME_MAX_MB", DefaultResumeMaxBytes)
}

func resumePhysicalMaxBytes() int64 {
	return resumeLimitFromEnv("METIS_RESUME_PHYSICAL_MAX_MB", DefaultResumePhysicalMaxBytes)
}

// resumeScannerMaxBytes keeps Scanner's per-record ceiling consistent with
// the physical-ledger policy. When the physical check is explicitly disabled,
// retain the bounded production default instead of handing Scanner an
// unbounded allocation target. Callers that raise the trusted physical cap get
// a matching line limit (important for old large raw tool-result entries that
// are later superseded by a small history_replace).
func resumeScannerMaxBytes() int {
	limit := resumePhysicalMaxBytes()
	if limit <= 0 {
		limit = DefaultResumePhysicalMaxBytes
	}
	maxInt := int64(int(^uint(0) >> 1))
	if limit > maxInt {
		return int(maxInt)
	}
	return int(limit)
}

func resumeLimitFromEnv(name string, fallback int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return 0 // explicit disable
			}
			return int64(n) * 1024 * 1024
		}
	}
	return fallback
}

type ResumeSizeScope string

const (
	ResumeSizeLogicalHistory ResumeSizeScope = "logical_history"
	ResumeSizePhysicalLedger ResumeSizeScope = "physical_ledger"
)

// ResumeTooLargeError is returned by CheckResumeSize when a session
// file exceeds the cap. Carries enough context for callers to print
// a useful message: actual size, the cap, and a hint to /clear or
// /branch.
type ResumeTooLargeError struct {
	Path     string
	Bytes    int64
	CapBytes int64
	Scope    ResumeSizeScope
}

func (e *ResumeTooLargeError) Error() string {
	if e.Scope == ResumeSizePhysicalLedger {
		return fmt.Sprintf(
			"session audit ledger is %s — exceeds the %s physical resume limit. "+
				"The ledger includes raw pre-compaction history; raise "+
				"METIS_RESUME_PHYSICAL_MAX_MB to force-load `%s` if the file is trusted.",
			fmtBytes(e.Bytes), fmtBytes(e.CapBytes), shortID(e.Path),
		)
	}
	return fmt.Sprintf(
		"session logical history is %s — exceeds the %s resume limit. "+
			"Try /clear to start fresh, /branch <id> from an earlier turn, "+
			"or `METIS_RESUME_MAX_MB=NN metis -r %s` to force-load (may exhaust context).",
		fmtBytes(e.Bytes), fmtBytes(e.CapBytes), shortID(e.Path),
	)
}

// CheckResumeSize checks only the physical append-only ledger size. The name
// remains for source compatibility; callers must apply CheckResumeHistorySize
// to the replayed logical history after Load.
//
// Disabled when METIS_RESUME_PHYSICAL_MAX_MB <= 0.
func (s *Store) CheckResumeSize(id string) error {
	cap := resumePhysicalMaxBytes()
	if cap <= 0 {
		return nil
	}
	path := s.path(id)
	st, err := os.Stat(path)
	if err != nil {
		// File missing / unreadable — let Load surface the error.
		// We return nil so the caller's existing handler runs.
		if isNotExist(err) {
			return nil
		}
		return nil
	}
	if st.Size() > cap {
		return &ResumeTooLargeError{Path: path, Bytes: st.Size(), CapBytes: cap, Scope: ResumeSizePhysicalLedger}
	}
	return nil
}

// CheckResumeHistorySize applies the context-economics cap to the logical
// messages produced by Load after every history_replace has been replayed.
// Old raw audit records do not contribute. The measurement is the compact JSON
// representation of the message slice, a stable approximation of the data the
// runtime is about to retain and send.
func CheckResumeHistorySize(id string, messages []llm.Message) error {
	cap := resumeMaxBytes()
	if cap <= 0 {
		return nil
	}
	bytes, err := logicalHistoryBytes(messages)
	if err != nil {
		return fmt.Errorf("measure logical resume history: %w", err)
	}
	if bytes > cap {
		return &ResumeTooLargeError{
			Path: id, Bytes: bytes, CapBytes: cap, Scope: ResumeSizeLogicalHistory,
		}
	}
	return nil
}

func logicalHistoryBytes(messages []llm.Message) (int64, error) {
	// Opening/closing brackets, plus one comma between adjacent messages.
	total := int64(2)
	for i := range messages {
		encoded, err := json.Marshal(messages[i])
		if err != nil {
			return 0, err
		}
		total += int64(len(encoded))
		if i > 0 {
			total++
		}
	}
	return total, nil
}

// isNotExist wraps os.IsNotExist so it works with errors wrapped by
// fs.PathError (which is what os.Stat returns).
func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	var pe *fs.PathError
	if asPathErr(err, &pe) {
		return os.IsNotExist(pe.Err)
	}
	return os.IsNotExist(err)
}

// asPathErr is a tiny errors.As wrapper kept inline so this file
// doesn't pull in the errors package for one call.
func asPathErr(err error, target **fs.PathError) bool {
	for err != nil {
		if pe, ok := err.(*fs.PathError); ok {
			*target = pe
			return true
		}
		type unwrap interface{ Unwrap() error }
		u, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// fmtBytes prints byte counts in human-readable form: "1.3 MiB",
// "623 KiB", "82 B". Mirrors the units used in ResumeTooLargeError so
// the message is consistent. Standard units, no rounding-up surprises
// (we floor to one decimal place).
func fmtBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(KiB))
	}
	return fmt.Sprintf("%d B", n)
}

// shortID returns the basename without the .jsonl extension, used in
// the suggested `metis -r <id>` hint inside ResumeTooLargeError.
func shortID(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			path = path[i+1:]
			break
		}
	}
	if l := len(path); l > 6 && path[l-6:] == ".jsonl" {
		return path[:l-6]
	}
	return path
}
