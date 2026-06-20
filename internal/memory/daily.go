// Package memory provides Metis's multi-tier memory system.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
}

// DailyStore manages daily session notes.
type DailyStore struct {
	root string
}

// NewDailyStore creates a new DailyStore.
func NewDailyStore(root string) (*DailyStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &DailyStore{root: root}, nil
}

// Save creates a new daily note file.
// Filename format: YYYY-MM-DD-{slug}.md
func (ds *DailyStore) Save(sessionID, source, summary string) error {
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")
	timeStr := now.Format("15:04:05")

	// Generate slug from summary or use timestamp fallback
	slug := generateSlugFromSummary(summary, now)

	filename := dateStr + "-" + slug + ".md"
	path := filepath.Join(ds.root, filename)
	// Uniqueness guard: two saves in the same minute with the same/empty
	// summary produce the same slug → the same filename → the second
	// silently overwrote the first (losing a session's daily note). If the
	// path is taken, disambiguate with the HHMMSS time, then a counter.
	if _, err := os.Stat(path); err == nil {
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
	sb.WriteString("\n\n")

	if summary != "" {
		sb.WriteString("## Conversation Summary\n\n")
		sb.WriteString(summary)
		sb.WriteString("\n")
	}

	return atomicWriteFile(path, sb.String(), 0o644)
}

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
		// Parse filename: YYYY-MM-DD-slug.md
		name := strings.TrimSuffix(entry.Name(), ".md")
		parts := strings.SplitN(name, "-", 4)
		if len(parts) < 4 {
			continue
		}
		// First 3 parts are date (YYYY-MM-DD), rest is slug
		date := parts[0] + "-" + parts[1] + "-" + parts[2]
		slug := parts[3]

		info, _ := entry.Info()
		var createdAt string
		if info != nil {
			createdAt = info.ModTime().Format(time.RFC3339)
		}

		notes = append(notes, DailyNote{
			Date:      date,
			Slug:      slug,
			SessionID: "",
			Source:    "",
			Summary:   "",
			CreatedAt: createdAt,
		})
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
