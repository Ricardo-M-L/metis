package session

// limits.go — pre-resume defensive check on session file size.
//
// Why: metis sessions live as JSONL — one line per message — so a
// long-running session with bash outputs / file dumps / pasted logs
// can balloon to tens of MB. Loading that into memory at resume time
// (Loop.Messages = msgs) instantiates the whole transcript at once;
// large transcripts can blow context windows or burn provider tokens
// on a "remind me what we said" round-trip.
//
// Inspired by openclaude's resume-time transcript size guard. We
// chose 8 MiB as the default cliff: empirically a session that's
// generated 8 MiB of JSONL is past the point where context-window
// economics make resume sensible — better to /clear and start fresh,
// or /branch from an earlier turn. Override with METIS_RESUME_MAX_MB
// for users who actively want to push the limit (rare, but the
// escape hatch should exist).
//
// Claude Code itself doesn't enforce this (only its Bash output
// cache has an 8 MiB cap, not the resume path) — confirmed from
// claude-code-sourcemap restored-src/. openclaude's path is the
// reference here.

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// DefaultResumeMaxBytes is the cliff for "transcript so large that
// resume will fail or waste budget." 8 MiB matches openclaude.
// Override at runtime with the METIS_RESUME_MAX_MB env var (sets
// the value in MiB). Zero / negative → disable the check.
const DefaultResumeMaxBytes int64 = 8 * 1024 * 1024

// resumeMaxBytes returns the active cap, honouring the env override.
// Cached per-process is overkill — env reads are cheap and the
// override is rarely set.
func resumeMaxBytes() int64 {
	if v := os.Getenv("METIS_RESUME_MAX_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return 0 // explicit disable
			}
			return int64(n) * 1024 * 1024
		}
	}
	return DefaultResumeMaxBytes
}

// ResumeTooLargeError is returned by CheckResumeSize when a session
// file exceeds the cap. Carries enough context for callers to print
// a useful message: actual size, the cap, and a hint to /clear or
// /branch.
type ResumeTooLargeError struct {
	Path     string
	Bytes    int64
	CapBytes int64
}

func (e *ResumeTooLargeError) Error() string {
	return fmt.Sprintf(
		"session transcript is %s — exceeds the %s resume limit. "+
			"Try /clear to start fresh, /branch <id> from an earlier turn, "+
			"or `METIS_RESUME_MAX_MB=NN metis -r %s` to force-load (may exhaust context).",
		fmtBytes(e.Bytes), fmtBytes(e.CapBytes), shortID(e.Path),
	)
}

// CheckResumeSize stats the session file on disk and returns
// ResumeTooLargeError when it's past the cap. Returns nil for files
// at-or-below cap, or for missing files (the caller's normal Load
// path will surface that error with proper context — we don't want
// to double-report).
//
// Disabled (nil-returning) when METIS_RESUME_MAX_MB <= 0.
func (s *Store) CheckResumeSize(id string) error {
	cap := resumeMaxBytes()
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
		return &ResumeTooLargeError{Path: path, Bytes: st.Size(), CapBytes: cap}
	}
	return nil
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
