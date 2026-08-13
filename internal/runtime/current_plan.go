package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const emptyCurrentPlan = "# Current Plan\n\n"

// CurrentPlanPath returns the user-editable Markdown plan for one chat
// session. Archived tool-call plans remain timestamped JSON files; this file
// is the single live draft that `/plan`, `/plan open`, and ExitPlanMode share.
func CurrentPlanPath(sessionID string) string {
	tag := strings.TrimSpace(filepath.Base(sessionID))
	if tag == "" || tag == "." || tag == string(filepath.Separator) {
		tag = "current"
	}
	tag = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, tag)
	return filepath.Join(PlansDir(), tag+".md")
}

// ReadCurrentPlan reads the live plan draft. A missing draft is not an error;
// callers use an empty body to distinguish "planning, but no plan yet".
func ReadCurrentPlan(sessionID string) (string, error) {
	data, err := os.ReadFile(CurrentPlanPath(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteCurrentPlan atomically replaces the live plan draft. Plans can contain
// repository details and pasted requirements, so both directory and file are
// private to the current user.
func WriteCurrentPlan(sessionID, body string) error {
	path := CurrentPlanPath(sessionID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// PlansDir historically used 0755 for archived JSON. Tighten it when a
	// live user-authored draft is stored; the draft can contain private repo
	// paths and pasted requirements.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set plans directory permissions: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create plan temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set plan temp permissions: %w", err)
	}
	if _, err := tmp.WriteString(strings.TrimSpace(body) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write plan temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync plan temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close plan temp file: %w", err)
	}
	if err := replaceCurrentPlanFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace current plan: %w", err)
	}
	return nil
}

// EnsureCurrentPlan creates an empty Markdown draft when this session has not
// planned yet and returns its stable path for an external editor.
func EnsureCurrentPlan(sessionID string) (string, error) {
	path := CurrentPlanPath(sessionID)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := WriteCurrentPlan(sessionID, emptyCurrentPlan); err != nil {
		return "", err
	}
	return path, nil
}
