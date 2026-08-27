// Package memory provides Metis's multi-tier memory system.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DailyNote represents a daily session memory entry.
type DailyNote struct {
	Date      string `json:"date"` // YYYY-MM-DD
	Slug      string `json:"slug"` // Descriptive slug
	SessionID string `json:"session_id"`
	Source    string `json:"source"`     // Command source (new/reset)
	Summary   string `json:"summary"`    // Conversation summary
	CreatedAt string `json:"created_at"` // RFC3339
	UpdatedAt string `json:"updated_at"` // RFC3339
}

// DailyStore manages daily session notes.
type DailyStore struct {
	root string
	mu   sync.RWMutex
}

// NewDailyStore creates a new DailyStore.
func NewDailyStore(root string) (*DailyStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(root, 0o700)
	return &DailyStore{root: root}, nil
}

// Save creates a new daily note file.
// Filename format: YYYY-MM-DD-{slug}.md
func (ds *DailyStore) Save(sessionID, source, summary string) error {
	if ds == nil {
		return nil
	}
	sanitizedSummary, err := sanitizePersistedText(summary)
	if err != nil {
		return err
	}
	// Session identity and lifecycle source are control metadata rather than
	// prose. Never mutate them with a redaction marker: reject a credential-like
	// value so deletion/tombstone identity remains exact.
	if err := validatePersistedMetadata(sessionID, source); err != nil {
		return err
	}
	repositoryRoot := repositoryRootForTier(ds.root, "daily")
	return withRepositoryLock(repositoryRoot, func() error {
		if err := rejectDeletedSessionLocked(repositoryRoot, sessionID); err != nil {
			return err
		}
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return ds.saveLocked(sessionID, source, sanitizedSummary)
	})
}

func (ds *DailyStore) saveLocked(sessionID, source, summary string) error {
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")
	timeStr := now.Format("15:04:05")

	// Generate slug from summary or use timestamp fallback
	slug := generateSlugFromSummary(summary, now)

	filename := dateStr + "-" + slug + ".md"
	path := filepath.Join(ds.root, filename)
	createdAt := now.Format(time.RFC3339Nano)
	// Desktop may persist the same live session at switch, shutdown, and
	// process-exit boundaries. Upsert by session ID so those lifecycle hooks
	// update one note instead of manufacturing duplicates.
	if strings.TrimSpace(sessionID) != "" {
		if existingPath, existing, ok := ds.findBySessionID(sessionID); ok {
			path = existingPath
			if existing.CreatedAt != "" {
				createdAt = existing.CreatedAt
			}
		}
	}
	// Uniqueness guard: two saves in the same minute with the same/empty
	// summary produce the same slug → the same filename → the second
	// silently overwrote the first (losing a session's daily note). If the
	// path is taken, disambiguate with the HHMMSS time, then a counter.
	if _, err := os.Stat(path); err == nil && !ds.pathBelongsToSession(path, sessionID) {
		alt := dateStr + "-" + slug + "-" + strings.ReplaceAll(timeStr, ":", "") + ".md"
		path = filepath.Join(ds.root, alt)
		for i := 2; ; i++ {
			if _, err := os.Stat(path); err != nil {
				break
			}
			path = filepath.Join(ds.root, fmt.Sprintf("%s-%s-%d.md", dateStr, slug, i))
		}
	}

	// Build Markdown entry
	var sb strings.Builder
	sb.WriteString("# Session: ")
	sb.WriteString(dateStr)
	sb.WriteString(" ")
	sb.WriteString(timeStr)
	sb.WriteString(" UTC\n\n")
	sb.WriteString("- **Session ID**: ")
	sb.WriteString(sessionID)
	sb.WriteString("\n")
	sb.WriteString("- **Source**: ")
	sb.WriteString(source)
	sb.WriteString("\n")
	sb.WriteString("- **Created At**: ")
	sb.WriteString(createdAt)
	sb.WriteString("\n")
	sb.WriteString("- **Updated At**: ")
	sb.WriteString(now.Format(time.RFC3339Nano))
	sb.WriteString("\n\n")

	if summary != "" {
		sb.WriteString("## Conversation Summary\n\n")
		sb.WriteString(summary)
		sb.WriteString("\n")
	}

	return atomicWriteFile(path, sb.String(), 0o600)
}

func (ds *DailyStore) findBySessionID(sessionID string) (string, DailyNote, bool) {
	entries, err := os.ReadDir(ds.root)
	if err != nil {
		return "", DailyNote{}, false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(ds.root, entry.Name())
		note, err := parseDailyNoteFile(path, entry)
		if err == nil && note.SessionID == sessionID {
			return path, note, true
		}
	}
	return "", DailyNote{}, false
}

func (ds *DailyStore) pathBelongsToSession(path, sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	entryInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	entry := fileInfoDirEntry{entryInfo}
	note, err := parseDailyNoteFile(path, entry)
	return err == nil && note.SessionID == sessionID
}

type fileInfoDirEntry struct{ os.FileInfo }

func (entry fileInfoDirEntry) Type() os.FileMode          { return entry.Mode().Type() }
func (entry fileInfoDirEntry) Info() (os.FileInfo, error) { return entry.FileInfo, nil }

// generateSlugFromSummary creates a URL-safe slug from summary text.
// Falls back to HHMM timestamp if summary is empty or slug would be too long.
func generateSlugFromSummary(summary string, t time.Time) string {
	if summary == "" {
		// Fallback to timestamp slug: HHMM
		return t.Format("1504")
	}

	// Take first meaningful words from summary
	words := strings.Fields(summary)
	var slugParts []string
	charCount := 0
	maxChars := 40

	for _, word := range words {
		// Skip very short words and punctuation-only
		clean := cleanSlugWord(word)
		if len(clean) < 2 {
			continue
		}
		if charCount+len(clean)+1 > maxChars {
			break
		}
		slugParts = append(slugParts, clean)
		charCount += len(clean) + 1
	}

	if len(slugParts) == 0 {
		return t.Format("1504")
	}

	slug := strings.Join(slugParts, "-")
	// Ensure slug is not too long and is lowercase
	if len(slug) > 50 {
		slug = slug[:50]
	}
	return strings.ToLower(slug)
}

// cleanSlugWord removes non-alphanumeric characters and converts to lowercase.
func cleanSlugWord(word string) string {
	var result []rune
	for _, r := range strings.ToLower(word) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			result = append(result, r)
		}
	}
	return string(result)
}

// List returns all daily notes sorted by date (newest first).
func (ds *DailyStore) List(limit int) ([]DailyNote, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	entries, err := os.ReadDir(ds.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var notes []DailyNote
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		note, err := parseDailyNoteFile(filepath.Join(ds.root, entry.Name()), entry)
		if err == nil {
			notes = append(notes, note)
		}
	}

	// Sort by date descending (newest first)
	for i := 0; i < len(notes)-1; i++ {
		for j := i + 1; j < len(notes); j++ {
			// Newest first; break same-day ties by CreatedAt (RFC3339, also
			// sortable) so multiple sessions on one day keep a stable order.
			if notes[j].Date > notes[i].Date ||
				(notes[j].Date == notes[i].Date && notes[j].CreatedAt > notes[i].CreatedAt) {
				notes[i], notes[j] = notes[j], notes[i]
			}
		}
	}

	if limit > 0 && len(notes) > limit {
		notes = notes[:limit]
	}
	return notes, nil
}

func parseDailyNoteFile(path string, entry os.DirEntry) (DailyNote, error) {
	name := strings.TrimSuffix(entry.Name(), ".md")
	parts := strings.SplitN(name, "-", 4)
	if len(parts) < 4 {
		return DailyNote{}, fmt.Errorf("invalid daily filename: %s", entry.Name())
	}
	note := DailyNote{Date: parts[0] + "-" + parts[1] + "-" + parts[2], Slug: parts[3]}
	if info, err := entry.Info(); err == nil {
		note.CreatedAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return DailyNote{}, err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	inSummary := false
	var summary []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inSummary {
			summary = append(summary, line)
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "- **Session ID**:"):
			note.SessionID = strings.TrimSpace(strings.TrimPrefix(trimmed, "- **Session ID**:"))
		case strings.HasPrefix(trimmed, "- **Source**:"):
			note.Source = strings.TrimSpace(strings.TrimPrefix(trimmed, "- **Source**:"))
		case strings.HasPrefix(trimmed, "- **Created At**:"):
			if value := strings.TrimSpace(strings.TrimPrefix(trimmed, "- **Created At**:")); value != "" {
				note.CreatedAt = value
			}
		case strings.HasPrefix(trimmed, "- **Updated At**:"):
			note.UpdatedAt = strings.TrimSpace(strings.TrimPrefix(trimmed, "- **Updated At**:"))
		case trimmed == "## Conversation Summary":
			inSummary = true
		}
	}
	note.Summary = strings.TrimSpace(strings.Join(summary, "\n"))
	return note, nil
}

// RecentSummary returns a single concatenated summary of the last
// `days` days of session notes, intended for injection into the
// system prompt at session start. Reads the actual file body of each
// note (List() doesn't), trims to maxBytes total, and prepends a
// "## Recent sessions (last N days)" header.
//
// Used by MemoryManager.BuildContext to surface cross-session
// continuity without requiring the user to re-prime the agent each
// day. Pre-fix, daily notes were a write-only tomb — saved on /new
// or /reset but never read back.
//
// `days` 0 means "no recent context" (caller can disable). Empty
// daily store returns "" cleanly. maxBytes 0 means no cap.
func (ds *DailyStore) RecentSummary(days, maxBytes int) string {
	if days <= 0 {
		return ""
	}
	notes, err := ds.List(0)
	if err != nil || len(notes) == 0 {
		return ""
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	var b strings.Builder
	b.WriteString("## Recent sessions (last ")
	if days == 1 {
		b.WriteString("day")
	} else {
		b.WriteString(itoaDays(days) + " days")
	}
	b.WriteString(")\n\n")
	added := 0
	for _, n := range notes {
		if n.Date < cutoff {
			continue
		}
		path := filepath.Join(ds.root, n.Date+"-"+n.Slug+".md")
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// Strip the file header so we don't duplicate metadata; keep
		// the summary body. Heuristic: skip until a blank line, then
		// take the rest.
		s := stripHeaderBlock(string(body))
		if s == "" {
			continue
		}
		b.WriteString("### ")
		b.WriteString(n.Date)
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(s))
		b.WriteString("\n\n")
		added++
		if maxBytes > 0 && b.Len() > maxBytes {
			break
		}
	}
	if added == 0 {
		return ""
	}
	return strings.TrimSpace(b.String())
}

// stripHeaderBlock drops the leading "# Session: ... " header section
// (everything up to the first blank line after the metadata bullets)
// so RecentSummary surfaces only the actual conversation summary.
func stripHeaderBlock(body string) string {
	lines := strings.Split(body, "\n")
	skipUntilBlank := false
	if len(lines) > 0 && strings.HasPrefix(lines[0], "# ") {
		skipUntilBlank = true
	}
	if !skipUntilBlank {
		return body
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" && i > 0 {
			rest := strings.Join(lines[i+1:], "\n")
			// One more blank-line skip if metadata section
			// (bullets) follows the header.
			rest = strings.TrimLeft(rest, "\n")
			if strings.HasPrefix(rest, "- **") {
				// Skip this metadata block too.
				ml := strings.SplitN(rest, "\n\n", 2)
				if len(ml) == 2 {
					return ml[1]
				}
			}
			return rest
		}
	}
	return ""
}

func itoaDays(n int) string {
	// Tiny helper to avoid importing strconv just for this one call;
	// daily counts are always small ints (<= 30).
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
