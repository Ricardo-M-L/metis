package memory

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory/security"
)

const (
	legacyImportMarker     = ".legacy-store-import.json"
	migrationQuarantineDir = ".migration-quarantine"
)

// MigrateLegacyRoot copies a legacy memory hierarchy into canonicalRoot.
// Safe destination data wins migration conflicts; unsafe destination data is
// quarantined before the merge. The source is never changed or deleted, making
// repeated startup migration safe and recoverable.
func MigrateLegacyRoot(legacyRoot, canonicalRoot string) error {
	return migrateLegacyRoot(legacyRoot, canonicalRoot, "")
}

// MigrateLegacyWorkspaceRoot imports a historical project-local memory tree
// into the shared canonical repository without making it visible to other
// workspaces. Project Markdown files receive stable namespaced topic paths;
// JSONL records carry the same workspace scope.
func MigrateLegacyWorkspaceRoot(legacyRoot, canonicalRoot, workspacePath string) error {
	scope, err := WorkspaceScope(workspacePath)
	if err != nil {
		return err
	}
	return migrateLegacyRoot(legacyRoot, canonicalRoot, scope)
}

func migrateLegacyRoot(legacyRoot, canonicalRoot, workspaceScope string) error {
	legacyRoot = filepath.Clean(strings.TrimSpace(legacyRoot))
	canonicalRoot = filepath.Clean(strings.TrimSpace(canonicalRoot))
	if canonicalRoot == "." {
		return errors.New("memory: empty canonical root")
	}
	if legacyRoot == "." || legacyRoot == canonicalRoot {
		return HardenCanonicalRoot(canonicalRoot)
	}
	// Validate the destination before inspecting the optional source. A missing
	// legacy directory must never turn into a bypass for canonical hardening.
	if err := ensureCanonicalMemoryRoot(canonicalRoot); err != nil {
		return err
	}
	info, err := os.Lstat(legacyRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HardenCanonicalRoot(canonicalRoot)
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("memory: legacy root must not be a symlink: %s", legacyRoot)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: legacy root is not a directory: %s", legacyRoot)
	}
	return withRepositoryLock(canonicalRoot, func() error {
		if err := ensureCanonicalMemoryRoot(canonicalRoot); err != nil {
			return err
		}
		// Destination data wins conflicts, but only after it has passed the same
		// safety boundary as imported data. Otherwise one pre-existing malicious
		// line would survive a perfectly filtered migration and load on startup.
		if err := hardenCanonicalRootLocked(canonicalRoot); err != nil {
			return errors.Join(err, ensurePrivateTree(canonicalRoot))
		}
		migrationErr := migrateLegacyRootLocked(legacyRoot, canonicalRoot, workspaceScope)
		hardeningErr := hardenCanonicalRootLocked(canonicalRoot)
		permissionErr := ensurePrivateTree(canonicalRoot)
		return errors.Join(migrationErr, hardeningErr, permissionErr)
	})
}

// HardenCanonicalRoot applies the migration safety boundary to an already
// canonical repository before MemoryManager loads it. Unsafe legacy records
// are moved out of model-visible paths into a private deterministic quarantine;
// safe records remain active, and small credential leaks are redacted.
//
// Callers that construct MemoryManager directly should invoke this first. The
// constructor integration intentionally lives outside this migration-only file
// so startup policy stays with the repository owner.
func HardenCanonicalRoot(canonicalRoot string) error {
	canonicalRoot = filepath.Clean(strings.TrimSpace(canonicalRoot))
	if err := ensureCanonicalMemoryRoot(canonicalRoot); err != nil {
		return err
	}
	return withRepositoryLock(canonicalRoot, func() error {
		if err := ensureCanonicalMemoryRoot(canonicalRoot); err != nil {
			return err
		}
		hardeningErr := hardenCanonicalRootLocked(canonicalRoot)
		permissionErr := ensurePrivateTree(canonicalRoot)
		return errors.Join(hardeningErr, permissionErr)
	})
}

func hardenCanonicalRootLocked(canonicalRoot string) error {
	if err := filepath.WalkDir(canonicalRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(canonicalRoot, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == migrationQuarantineDir || strings.HasPrefix(rel, migrationQuarantineDir+"/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if entry.Type()&os.ModeSymlink != 0 {
			if isCanonicalTombstonePath(rel) {
				return quarantineCanonicalTombstoneSymlink(canonicalRoot, path, rel)
			}
			if isCanonicalModelVisiblePath(rel) || knownLegacyMemoryDirectory(rel) {
				return quarantineCanonicalSymlink(canonicalRoot, path, rel)
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}

		// These files are derived from the authoritative JSONL tiers. Keeping an
		// old index permits an unsafe preview to be loaded when the authoritative
		// file is absent, so rebuild instead of trusting it.
		if isCanonicalDerivedIndex(rel) {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := quarantineCanonicalBytes(canonicalRoot, rel, raw); err != nil {
				return err
			}
			return os.Remove(path)
		}
		if !isCanonicalModelVisiblePath(rel) {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		clean, keep, err := sanitizeLegacyMigrationFile(path, rel)
		if err != nil {
			return err
		}
		if keep && bytes.Equal(clean, raw) {
			return os.Chmod(path, 0o600)
		}
		// Preserve the exact pre-hardening bytes for manual recovery without
		// leaving them in a path consumed by Core/Recall/Archive/Topic loaders.
		if err := quarantineCanonicalBytes(canonicalRoot, rel, raw); err != nil {
			return err
		}
		if !keep {
			if isCanonicalTombstonePath(rel) {
				return atomicWriteFile(path, invalidTombstoneSentinel, 0o600)
			}
			return os.Remove(path)
		}
		return atomicWriteFile(path, string(clean), 0o600)
	}); err != nil {
		return err
	}
	return purgeDeletedMigrationDataLocked(canonicalRoot)
}

func purgeDeletedMigrationDataLocked(canonicalRoot string) error {
	return filepath.WalkDir(canonicalRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(canonicalRoot, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == migrationQuarantineDir {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || !isLegacyJSONLPath(rel) && !isLegacyMarkdownPath(rel) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		filtered, keep, err := filterDeletedMigrationContentLocked(canonicalRoot, rel, raw)
		if err != nil {
			return err
		}
		if !keep {
			if isLegacyJSONLPath(rel) {
				return atomicWriteFile(path, "", 0o600)
			}
			return os.Remove(path)
		}
		if bytes.Equal(filtered, raw) {
			return nil
		}
		return atomicWriteFile(path, string(filtered), 0o600)
	})
}

func isCanonicalModelVisiblePath(rel string) bool {
	rel = filepath.ToSlash(rel)
	if isLegacyJSONLPath(rel) || isLegacyMarkdownPath(rel) {
		return true
	}
	return isCanonicalTombstonePath(rel)
}

func isCanonicalTombstonePath(rel string) bool {
	rel = filepath.ToSlash(rel)
	return strings.HasPrefix(rel, tombstoneDirName+"/") &&
		strings.HasSuffix(strings.ToLower(rel), ".json") && strings.Count(rel, "/") == 1
}

func isCanonicalDerivedIndex(rel string) bool {
	switch filepath.ToSlash(rel) {
	case "archival/index.json", "recall/sessions.json":
		return true
	}
	return false
}

func quarantineCanonicalBytes(canonicalRoot, rel string, raw []byte) error {
	dir := filepath.Join(canonicalRoot, migrationQuarantineDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	sumInput := append([]byte(filepath.ToSlash(rel)+"\x00"), raw...)
	sum := sha256.Sum256(sumInput)
	base := safeQuarantineBase(filepath.Base(rel))
	dst := filepath.Join(dir, hex.EncodeToString(sum[:16])+"-"+base+".raw")
	return writeFileExclusive(dst, raw)
}

func quarantineCanonicalSymlink(canonicalRoot, path, rel string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	record := []byte("path=" + filepath.ToSlash(rel) + "\ntarget=" + target + "\n")
	if err := quarantineCanonicalBytes(canonicalRoot, rel+".symlink", record); err != nil {
		return err
	}
	return os.Remove(path)
}

func quarantineCanonicalTombstoneSymlink(canonicalRoot, path, rel string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	record := []byte("path=" + filepath.ToSlash(rel) + "\ntarget=" + target + "\n")
	if err := quarantineCanonicalBytes(canonicalRoot, rel+".symlink", record); err != nil {
		return err
	}
	// Rename the private regular sentinel directly over the symlink. Removing
	// first would create a crash window in which a later constructor could see
	// no tombstone and accept a stale writer.
	return atomicWriteFile(path, invalidTombstoneSentinel, 0o600)
}

func safeQuarantineBase(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "memory"
	}
	return b.String()
}

func migrateLegacyRootLocked(legacyRoot, canonicalRoot, workspaceScope string) error {
	return filepath.WalkDir(legacyRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(legacyRoot, path)
		if err != nil || rel == "." {
			return err
		}
		// A malformed configuration can put the destination underneath the
		// source. Do not walk back into the newly-created destination and
		// recursively copy memory/memory/... forever.
		if samePathOrDescendant(path, canonicalRoot) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dst := legacyMigrationDestination(canonicalRoot, filepath.ToSlash(rel), workspaceScope)
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if !knownLegacyMemoryDirectory(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			return os.Chmod(dst, 0o700)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		sanitized, migrate, err := sanitizeLegacyMigrationFile(path, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if !migrate {
			return nil
		}
		if workspaceScope != "" {
			sanitized, migrate, err = scopeLegacyMigrationFile(filepath.ToSlash(rel), sanitized, workspaceScope)
			if err != nil || !migrate {
				return err
			}
		}
		sanitized, migrate, err = filterDeletedMigrationContentLocked(canonicalRoot, filepath.ToSlash(rel), sanitized)
		if err != nil || !migrate {
			return err
		}
		if _, err := os.Stat(dst); err == nil {
			// A namespaced Markdown destination represents one stable source
			// file in one workspace. Once imported, canonical edits win over
			// the immutable legacy copy on every later startup.
			if workspaceScope != "" && strings.EqualFold(filepath.Ext(dst), ".md") {
				return nil
			}
			return mergeLegacyConflict(sanitized, dst, rel)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeFileExclusive(dst, sanitized)
	})
}

func knownLegacyMemoryDirectory(rel string) bool {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	switch rel {
	case "core.d", "recall", "archival", "daily", "topics", tombstoneDirName:
		return true
	}
	return false
}

func legacyMigrationDestination(canonicalRoot, rel, workspaceScope string) string {
	rel = filepath.ToSlash(rel)
	if workspaceScope == "" || !strings.EqualFold(filepath.Ext(rel), ".md") {
		return filepath.Join(canonicalRoot, filepath.FromSlash(rel))
	}
	prefix := "ws-" + workspaceID(workspaceScope)
	if strings.HasPrefix(rel, "daily/") {
		return filepath.Join(canonicalRoot, "daily", prefix+"--"+filepath.Base(rel))
	}
	// Core blocks and all project topic layouts become namespaced topics. This
	// preserves same-named WORKING/MEMORY files from multiple projects without
	// letting one project occupy the global Core slot.
	sum := sha256.Sum256([]byte(rel))
	base := strings.TrimSuffix(safeQuarantineBase(filepath.Base(rel)), filepath.Ext(rel))
	name := fmt.Sprintf("%s--%x--%s.md", prefix, sum[:4], base)
	return filepath.Join(canonicalRoot, "topics", name)
}

func scopeLegacyMigrationFile(rel string, raw []byte, workspaceScope string) ([]byte, bool, error) {
	rel = filepath.ToSlash(rel)
	if isLegacyJSONLPath(rel) {
		var lines []string
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			scoped, ok := scopeLegacyJSONLRecord(rel, []byte(line), workspaceScope)
			if ok {
				lines = append(lines, string(scoped))
			}
		}
		if len(lines) == 0 {
			return nil, false, nil
		}
		return []byte(strings.Join(lines, "\n") + "\n"), true, nil
	}
	if !isLegacyMarkdownPath(rel) || strings.HasPrefix(rel, "daily/") {
		return raw, true, nil
	}
	fm, body, err := memdir.ParseFile(raw)
	if err != nil {
		return nil, false, err
	}
	if fm == nil || fm.Validate() != nil {
		name := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		fm = &memdir.Frontmatter{
			Name:        name,
			Description: "Imported project memory from " + rel,
			Type:        memdir.TypeProject,
		}
		body = raw
	}
	fm.Scope = scopedMemoryType(string(fm.Type), fm.Scope, workspaceScope)
	rendered, err := memdir.RenderFile(fm, string(body))
	if err != nil {
		return nil, false, err
	}
	return rendered, true, nil
}

func scopeLegacyJSONLRecord(rel string, raw []byte, workspaceScope string) ([]byte, bool) {
	switch filepath.ToSlash(rel) {
	case "recall/messages.jsonl":
		var message Message
		if json.Unmarshal(raw, &message) != nil {
			return nil, false
		}
		message.Scope = workspaceScope
		clean, err := json.Marshal(message)
		return clean, err == nil
	case "archival/passages.jsonl":
		var passage Passage
		if json.Unmarshal(raw, &passage) != nil || strings.TrimSpace(passage.ID) == "" {
			return nil, false
		}
		passage.ID = "ws-" + workspaceID(workspaceScope) + "-" + passage.ID
		passage.Scope = scopedMemoryType(passage.Type, passage.Scope, workspaceScope)
		clean, err := json.Marshal(passage)
		return clean, err == nil
	case "fact.jsonl", "preference.jsonl", "context.jsonl":
		var entry legacyStoreEntry
		if json.Unmarshal(raw, &entry) != nil {
			return nil, false
		}
		memoryType := TypeContext
		if filepath.ToSlash(rel) == "preference.jsonl" || entry.Type == "preference" {
			memoryType = TypeUser
		}
		entry.Scope = scopedMemoryType(memoryType, entry.Scope, workspaceScope)
		if entry.SourceSessionID == "" {
			entry.SourceSessionID = entry.Source
		}
		clean, err := json.Marshal(entry)
		return clean, err == nil
	}
	return raw, true
}

func filterDeletedMigrationContentLocked(canonicalRoot, rel string, raw []byte) ([]byte, bool, error) {
	rel = filepath.ToSlash(rel)
	if isLegacyJSONLPath(rel) {
		lines := make([]string, 0)
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			sessionID := migrationRecordSessionID(rel, []byte(line))
			deleted, err := sessionDeletedLocked(canonicalRoot, sessionID)
			if err != nil {
				return nil, false, err
			}
			if !deleted {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return nil, false, nil
		}
		return []byte(strings.Join(lines, "\n") + "\n"), true, nil
	}
	if !isLegacyMarkdownPath(rel) {
		return raw, true, nil
	}
	sessionID := markdownSourceSessionID(rel, raw)
	deleted, err := sessionDeletedLocked(canonicalRoot, sessionID)
	if err != nil {
		return nil, false, err
	}
	return raw, !deleted, nil
}

func migrationRecordSessionID(rel string, raw []byte) string {
	switch filepath.ToSlash(rel) {
	case "recall/messages.jsonl":
		var message Message
		_ = json.Unmarshal(raw, &message)
		return strings.TrimSpace(message.SessionID)
	case "archival/passages.jsonl":
		var passage Passage
		_ = json.Unmarshal(raw, &passage)
		return strings.TrimSpace(passage.SourceSessionID)
	case "fact.jsonl", "preference.jsonl", "context.jsonl":
		var entry legacyStoreEntry
		_ = json.Unmarshal(raw, &entry)
		if entry.SourceSessionID != "" {
			return strings.TrimSpace(entry.SourceSessionID)
		}
		return strings.TrimSpace(entry.Source)
	}
	return ""
}

func markdownSourceSessionID(rel string, raw []byte) string {
	if strings.HasPrefix(filepath.ToSlash(rel), "daily/") {
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "- **Session ID**:") {
				return strings.TrimSpace(strings.TrimPrefix(trimmed, "- **Session ID**:"))
			}
		}
		return ""
	}
	fm, _, err := memdir.ParseFile(raw)
	if err != nil || fm == nil {
		return ""
	}
	return strings.TrimSpace(fm.OriginSessionID)
}

func sanitizeLegacyMigrationFile(path, rel string) ([]byte, bool, error) {
	rel = filepath.ToSlash(rel)
	base := strings.ToLower(filepath.Base(rel))
	if isLegacyJSONLPath(rel) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false, err
		}
		// An empty authoritative JSONL file is a valid initialized tier, not a
		// corrupt record set. Keep it so constructor hardening only tightens its
		// mode instead of unexpectedly deleting a repository file.
		if len(bytes.TrimSpace(raw)) == 0 {
			return raw, true, nil
		}
		clean, err := sanitizeLegacyJSONL(rel, raw)
		return clean, len(clean) > 0, err
	}
	if isLegacyMarkdownPath(rel) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false, err
		}
		clean, ok := sanitizeLegacyMemoryText(raw)
		return clean, ok, nil
	}
	if strings.HasPrefix(rel, tombstoneDirName+"/") && strings.HasSuffix(base, ".json") {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false, err
		}
		if string(raw) == invalidTombstoneSentinel {
			return raw, true, nil
		}
		var tombstone sessionTombstone
		if json.Unmarshal(raw, &tombstone) != nil || strings.TrimSpace(tombstone.SessionID) == "" {
			return nil, false, nil
		}
		if err := validateLegacyMigrationMetadata(tombstone.SessionID, tombstone.DeletedAt); err != nil {
			return nil, false, nil
		}
		if filepath.Base(tombstonePath(filepath.Dir(filepath.Dir(path)), tombstone.SessionID)) != filepath.Base(path) {
			return nil, false, nil
		}
		return raw, true, nil
	}
	// Derived indexes, lock files, snapshots, dumps, and unknown extensions do
	// not become model-visible memory and are rebuilt from authoritative tiers.
	return nil, false, nil
}

func isLegacyJSONLPath(rel string) bool {
	switch filepath.ToSlash(rel) {
	case "recall/messages.jsonl", "archival/passages.jsonl", "fact.jsonl", "preference.jsonl", "context.jsonl":
		return true
	}
	return false
}

func isLegacyMarkdownPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	if !strings.EqualFold(filepath.Ext(rel), ".md") {
		return false
	}
	if !strings.Contains(rel, "/") {
		return true
	}
	if strings.HasPrefix(rel, "daily/") || strings.HasPrefix(rel, "topics/") {
		return strings.Count(rel, "/") == 1
	}
	if strings.HasPrefix(rel, "core.d/") && strings.Count(rel, "/") == 1 {
		base := filepath.Base(rel)
		for _, known := range []string{FileMemMemory, FileMemUser, FileMemSystem, FileMemWorking, FileMemSummary} {
			if strings.EqualFold(base, known) {
				return true
			}
		}
	}
	return false
}

func sanitizeLegacyJSONL(rel string, raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lines := make([]string, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		clean, ok := sanitizeLegacyJSONLRecord(rel, []byte(line))
		if !ok {
			continue
		}
		lines = append(lines, string(clean))
	}
	if err := scanner.Err(); err != nil {
		// A record larger than the bounded scanner limit is not suitable for
		// prompt memory. Treat the file as unsafe so canonical hardening can
		// quarantine it instead of returning early and leaving it loadable.
		return nil, nil
	}
	if len(lines) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func sanitizeLegacyJSONLRecord(rel string, raw []byte) ([]byte, bool) {
	switch filepath.ToSlash(rel) {
	case "recall/messages.jsonl":
		var message Message
		if json.Unmarshal(raw, &message) != nil {
			return nil, false
		}
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "user" && role != "assistant" {
			return nil, false
		}
		content, ok := sanitizeLegacyMemoryText([]byte(message.Content))
		if !ok || len(strings.TrimSpace(string(content))) == 0 {
			return nil, false
		}
		if validateLegacyMigrationMetadata(role, message.Timestamp, message.SessionID, message.SourceMessageID, message.Scope) != nil {
			return nil, false
		}
		if string(content) == message.Content && role == message.Role {
			return raw, true
		}
		message.Role = role
		message.Content = string(content)
		clean, err := json.Marshal(message)
		return clean, err == nil

	case "archival/passages.jsonl":
		var passage Passage
		if json.Unmarshal(raw, &passage) != nil || strings.TrimSpace(passage.ID) == "" {
			return nil, false
		}
		content, ok := sanitizeLegacyMemoryText([]byte(passage.Content))
		if !ok || len(strings.TrimSpace(string(content))) == 0 {
			return nil, false
		}
		metadata := []string{passage.ID, passage.Type, passage.CreatedAt, passage.UpdatedAt, passage.LastUsedAt,
			passage.Source, passage.SourceSessionID, passage.SourceMessageID, passage.Scope}
		metadata = append(metadata, passage.Tags...)
		if validateLegacyMigrationMetadata(metadata...) != nil {
			return nil, false
		}
		if string(content) == passage.Content {
			return raw, true
		}
		passage.Content = string(content)
		clean, err := json.Marshal(passage)
		return clean, err == nil

	case "fact.jsonl", "preference.jsonl", "context.jsonl":
		var entry legacyStoreEntry
		if json.Unmarshal(raw, &entry) != nil {
			return nil, false
		}
		key, keyOK := sanitizeLegacyMemoryText([]byte(entry.Key))
		value, valueOK := sanitizeLegacyMemoryText([]byte(entry.Value))
		metadata := []string{entry.Type, entry.Source, entry.SourceSessionID, entry.Scope, entry.CreatedAt}
		metadata = append(metadata, entry.Tags...)
		if !keyOK || !valueOK || validateLegacyMigrationMetadata(metadata...) != nil {
			return nil, false
		}
		if strings.TrimSpace(string(key)) == "" && strings.TrimSpace(string(value)) == "" {
			return nil, false
		}
		if string(key) == entry.Key && string(value) == entry.Value {
			return raw, true
		}
		entry.Key, entry.Value = string(key), string(value)
		clean, err := json.Marshal(entry)
		return clean, err == nil
	}
	return nil, false
}

func sanitizeLegacyMemoryText(raw []byte) ([]byte, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	redacted, err := sanitizePersistedText(string(raw))
	if err != nil {
		return nil, false
	}
	if hasBlockingThreat(security.ScanAll(redacted)) {
		return nil, false
	}
	return []byte(redacted), true
}

func validateLegacyMigrationMetadata(values ...string) error {
	for _, value := range values {
		clean, ok := sanitizeLegacyMemoryText([]byte(value))
		if !ok || string(clean) != value {
			return ErrUnsafeMemory
		}
	}
	return nil
}

// mergeLegacyConflict preserves both sides for the tier files that contain
// independent records. Core files are intentionally canonical-wins; derived
// indexes are rebuilt by MemoryManager and are also canonical-wins.
func mergeLegacyConflict(srcRaw []byte, dst, rel string) error {
	dstRaw, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	if string(srcRaw) == string(dstRaw) {
		return nil
	}
	rel = filepath.ToSlash(rel)
	switch rel {
	case "recall/messages.jsonl", "archival/passages.jsonl", "fact.jsonl", "preference.jsonl", "context.jsonl":
		return mergeUniqueJSONLLines(dst, dstRaw, srcRaw, rel)
	case "recall/sessions.json", "archival/index.json", legacyImportMarker:
		return nil
	}
	if strings.HasPrefix(rel, "core.d/") {
		return nil
	}
	if strings.HasSuffix(strings.ToLower(rel), ".md") &&
		(strings.HasPrefix(rel, "daily/") || strings.HasPrefix(rel, "topics/") || !strings.Contains(rel, "/")) {
		sum := sha256.Sum256(srcRaw)
		ext := filepath.Ext(dst)
		base := strings.TrimSuffix(dst, ext)
		conflictPath := base + "-legacy-" + hex.EncodeToString(sum[:4]) + ext
		return writeFileExclusive(conflictPath, srcRaw)
	}
	return nil
}

func mergeUniqueJSONLLines(path string, destination, source []byte, rel string) error {
	seen := make(map[string]struct{})
	merged := make([]string, 0)
	for _, raw := range [][]byte{destination, source} {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			identity := jsonlMigrationIdentity(rel, line)
			if _, ok := seen[identity]; ok {
				continue
			}
			seen[identity] = struct{}{}
			merged = append(merged, line)
		}
	}
	content := ""
	if len(merged) > 0 {
		content = strings.Join(merged, "\n") + "\n"
	}
	return atomicWriteFile(path, content, 0o600)
}

func jsonlMigrationIdentity(rel, line string) string {
	if filepath.ToSlash(rel) == "archival/passages.jsonl" {
		var passage Passage
		if json.Unmarshal([]byte(line), &passage) == nil && strings.TrimSpace(passage.ID) != "" {
			return "id:" + passage.ID
		}
	}
	return "line:" + line
}

func samePathOrDescendant(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func writeFileExclusive(dst string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if _, err := out.Write(content); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return os.Chmod(dst, 0o600)
}

func ensureCanonicalMemoryRoot(root string) error {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" {
		return errors.New("memory: empty canonical root")
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("memory: canonical root must not be a symlink: %s", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: canonical root is not a directory: %s", root)
	}
	return os.Chmod(root, 0o700)
}

func ensurePrivateTree(root string) error {
	if err := ensureCanonicalMemoryRoot(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		if entry.Type().IsRegular() {
			return os.Chmod(path, 0o600)
		}
		return nil
	})
}

type legacyStoreEntry struct {
	Type            string   `json:"type"`
	Key             string   `json:"key"`
	Value           string   `json:"value"`
	Source          string   `json:"source,omitempty"`
	SourceSessionID string   `json:"source_session_id,omitempty"`
	Scope           string   `json:"scope,omitempty"`
	CreatedAt       string   `json:"created_at"`
	Tags            []string `json:"tags,omitempty"`
}

type legacyImportState struct {
	Version  int             `json:"version"`
	Imported map[string]bool `json:"imported"`
}

func (mm *MemoryManager) importLegacyStoreJSONL() error {
	return mm.ImportLegacyStore(mm.root)
}

// ImportLegacyStore imports the deprecated agent Store JSONL files found in
// sourceRoot into this repository's archival tier. A content hash marker in
// the destination makes imports idempotent across copied and original legacy
// roots. Source files are never changed or deleted.
func (mm *MemoryManager) ImportLegacyStore(sourceRoot string) error {
	return mm.importLegacyStore(sourceRoot, "")
}

// ImportLegacyStoreForWorkspace imports deprecated Store JSONL from a
// project-local source. User preferences remain global; fact/context entries
// receive the stable workspace namespace.
func (mm *MemoryManager) ImportLegacyStoreForWorkspace(sourceRoot, workspacePath string) error {
	scope, err := WorkspaceScope(workspacePath)
	if err != nil {
		return err
	}
	return mm.importLegacyStore(sourceRoot, scope)
}

func (mm *MemoryManager) importLegacyStore(sourceRoot, workspaceScope string) error {
	if mm == nil || mm.archival == nil {
		return nil
	}
	sourceRoot = filepath.Clean(strings.TrimSpace(sourceRoot))
	if sourceRoot == "." {
		return nil
	}
	info, err := os.Lstat(sourceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("memory: legacy root must not be a symlink: %s", sourceRoot)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: legacy root is not a directory: %s", sourceRoot)
	}
	state := legacyImportState{Version: 1, Imported: map[string]bool{}}
	markerPath := filepath.Join(mm.root, legacyImportMarker)
	if raw, err := os.ReadFile(markerPath); err == nil {
		_ = json.Unmarshal(raw, &state)
		if state.Imported == nil {
			state.Imported = map[string]bool{}
		}
	}
	changed := false
	for _, typ := range []string{"fact", "preference", "context"} {
		path := filepath.Join(sourceRoot, typ+".jsonl")
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			raw := append([]byte(nil), scanner.Bytes()...)
			var entry legacyStoreEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				sum := sha256.Sum256(append([]byte(typ+"\x00"), raw...))
				hash := hex.EncodeToString(sum[:])
				state.Imported[hash] = true
				changed = true
				continue
			}
			pType := TypeContext
			if typ == "preference" {
				pType = TypeUser
			}
			effectiveScope := scopedMemoryType(pType, entry.Scope, workspaceScope)
			if workspaceScope == "" && strings.TrimSpace(entry.Scope) == "" {
				effectiveScope = "legacy-store"
			}
			hashPrefix := typ + "\x00"
			if workspaceScope != "" || strings.TrimSpace(entry.Scope) != "" {
				hashPrefix += effectiveScope + "\x00"
			}
			sum := sha256.Sum256(append([]byte(hashPrefix), raw...))
			hash := hex.EncodeToString(sum[:])
			if state.Imported[hash] {
				continue
			}
			content := strings.TrimSpace(entry.Value)
			if key := strings.TrimSpace(entry.Key); key != "" {
				if content == "" {
					content = key
				} else {
					content = key + ": " + content
				}
			}
			if content == "" {
				state.Imported[hash] = true
				changed = true
				continue
			}
			tags := append([]string(nil), entry.Tags...)
			tags = append(tags, "legacy:"+typ)
			p := Passage{
				ID:              "legacy-" + hash[:24],
				Content:         content,
				Type:            pType,
				Tags:            tags,
				CreatedAt:       entry.CreatedAt,
				UpdatedAt:       entry.CreatedAt,
				Source:          entry.Source,
				SourceSessionID: firstNonEmpty(entry.SourceSessionID, entry.Source),
				Scope:           effectiveScope,
				Confidence:      1,
			}
			if err := mm.archival.Insert(p); err != nil {
				// Unsafe/sensitive legacy records are intentionally quarantined:
				// remember the hash so every startup does not retry them, while
				// leaving the original JSONL untouched for manual recovery.
				if errors.Is(err, ErrUnsafeMemory) || errors.Is(err, ErrSensitiveMemory) || errors.Is(err, ErrSessionDeleted) {
					state.Imported[hash] = true
					changed = true
					continue
				}
				_ = f.Close()
				return fmt.Errorf("import legacy %s: %w", typ, err)
			}
			state.Imported[hash] = true
			changed = true
		}
		err = scanner.Err()
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	// encoding/json sorts string map keys, making the marker byte-stable.
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicWriteFile(markerPath, string(raw), 0o600)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
