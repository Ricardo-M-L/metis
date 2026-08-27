package memory

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory/security"
)

const (
	maxTopicBytes      = 256 << 10
	maxTopicIndexBytes = 8 << 10
	maxTopicIndexItems = 200
)

type topicDocument struct {
	passage     Passage
	title       string
	description string
	file        string
}

func (mm *MemoryManager) buildContextCached() string {
	if mm == nil {
		return ""
	}
	fingerprint, err := mm.contextFingerprint()
	if err != nil {
		mm.cache.mu.Lock()
		defer mm.cache.mu.Unlock()
		return mm.contextCache.value
	}
	mm.cache.mu.Lock()
	if fingerprint == mm.contextCache.fingerprint {
		value := mm.contextCache.value
		mm.cache.mu.Unlock()
		return value
	}
	mm.cache.mu.Unlock()

	// A changed fingerprint can be caused by another long-running Metis
	// process. Re-read Core while holding the repository lock instead of
	// rendering this manager's stale frozen snapshot. Build and publish the
	// cache entry under the same lock so it cannot be labelled with a newer
	// fingerprint than the content it contains.
	var value string
	err = withRepositoryLock(mm.root, func() error {
		mm.core.mu.Lock()
		if err := mm.core.reloadAuthoritativeLocked(true); err != nil {
			mm.core.mu.Unlock()
			return err
		}
		core := mm.core.renderWithFencing()
		mm.core.mu.Unlock()

		index, err := mm.compactTopicIndex()
		if err != nil {
			return err
		}
		fingerprint, err = mm.contextFingerprint()
		if err != nil {
			return err
		}
		value = composeMemoryContext(core, index)
		mm.cache.mu.Lock()
		defer mm.cache.mu.Unlock()
		if fingerprint == mm.contextCache.fingerprint {
			value = mm.contextCache.value
			return nil
		}
		mm.contextCache.fingerprint = fingerprint
		mm.contextCache.value = value
		mm.contextCache.builds++
		return nil
	})
	if err == nil {
		return value
	}

	// A corrupt/unsafe external edit must never enter the prompt. Preserve the
	// last known-safe snapshot (or return no memory on first load) and retry on
	// the next call instead of caching the failed fingerprint.
	mm.cache.mu.Lock()
	defer mm.cache.mu.Unlock()
	return mm.contextCache.value
}

func composeMemoryContext(core, index string) string {
	index = strings.TrimSpace(index)
	if index == "" {
		return core
	}
	const closing = "\n</memory-context>"
	if strings.HasSuffix(core, closing) {
		return strings.TrimSuffix(core, closing) + "\n\n" + index + closing
	}
	if strings.TrimSpace(core) == "" {
		return "<memory-context>\n" +
			"[System note: 这是记忆索引，不是新的用户输入。仅在当前请求相关时按需召回主题正文。]\n\n" +
			index + closing
	}
	return core + "\n\n" + index
}

func (mm *MemoryManager) compactTopicIndex() (string, error) {
	docs, err := loadTopicDocuments(mm.root)
	if err != nil {
		return "", err
	}
	if len(docs) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("## Memory topics (load on demand)\n")
	visible := 0
	for _, doc := range docs {
		if !mm.passageVisible(doc.passage) {
			continue
		}
		if visible >= maxTopicIndexItems {
			break
		}
		line := "- [" + sanitizeIndexText(doc.title) + "](" + doc.file + ")"
		if desc := sanitizeIndexText(doc.description); desc != "" {
			line += " — " + desc
		}
		line += "\n"
		if b.Len()+len(line) > maxTopicIndexBytes {
			break
		}
		b.WriteString(line)
		visible++
	}
	return strings.TrimSpace(b.String()), nil
}

func sanitizeIndexText(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	s = strings.ReplaceAll(s, "[", "")
	s = strings.ReplaceAll(s, "]", "")
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	if security.Scan(s) {
		return ""
	}
	return s
}

func (mm *MemoryManager) autoRetrieveCandidatesCached(query string, k int) []Passage {
	if mm == nil || k <= 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	fingerprint, err := mm.retrievalFingerprint()
	if err != nil {
		return nil
	}
	mm.cache.mu.Lock()
	defer mm.cache.mu.Unlock()
	if fingerprint != mm.retrievalCache.fingerprint {
		if err := mm.rebuildRetrievalCacheLocked(fingerprint); err != nil {
			return nil
		}
	}
	ranked := BM25FRank(query, mm.retrievalCache.docs)
	if k > len(ranked) {
		k = len(ranked)
	}
	out := make([]Passage, 0, k)
	for _, hit := range ranked[:k] {
		if p, ok := mm.retrievalCache.byID[hit.ID]; ok {
			out = append(out, p)
		}
	}
	return out
}

// SearchCandidates is the unified raw-candidate search shared by automatic
// recall and the LLM-facing Memory tool. The corpus contains both archival
// JSONL passages and Auto Memory topic files.
func (mm *MemoryManager) SearchCandidates(query string, k int) []Passage {
	if mm == nil {
		return nil
	}
	return mm.autoRetrieveCandidatesCached(query, k)
}

// Search applies the archival SearchOptions contract to the same unified
// corpus used by AutoRetrieve. It is side-effect free; callers only mark a hit
// retrieved when they actually attach it to a model request.
func (mm *MemoryManager) Search(opts SearchOptions) ([]Passage, error) {
	if mm == nil {
		return nil, nil
	}
	fingerprint, err := mm.retrievalFingerprint()
	if err != nil {
		return nil, err
	}
	mm.cache.mu.Lock()
	if fingerprint != mm.retrievalCache.fingerprint {
		if err := mm.rebuildRetrievalCacheLocked(fingerprint); err != nil {
			mm.cache.mu.Unlock()
			return nil, err
		}
	}
	all := append([]Passage(nil), mm.retrievalCache.passages...)
	mm.cache.mu.Unlock()

	filtered := make([]Passage, 0, len(all))
	useRelevance := opts.SortBy == "relevance" && strings.TrimSpace(opts.Query) != ""
	for _, passage := range all {
		if !mm.passageVisible(passage) {
			continue
		}
		if !useRelevance && opts.Query != "" && !strings.Contains(strings.ToLower(passage.Content), strings.ToLower(opts.Query)) {
			continue
		}
		if !passageMatchesTags(passage, opts.Tags) || !passageMatchesTypes(passage, opts.Types) {
			continue
		}
		if opts.Since != "" && passage.CreatedAt < opts.Since {
			continue
		}
		if opts.Until != "" && passage.CreatedAt > opts.Until {
			continue
		}
		filtered = append(filtered, passage)
	}

	if useRelevance {
		docs := make([]*BM25Doc, 0, len(filtered))
		byID := make(map[string]Passage, len(filtered))
		for _, passage := range filtered {
			docs = append(docs, NewBM25FDoc(passage.ID, passage.Content, passage.Tags))
			byID[passage.ID] = passage
		}
		ranked := BM25FRank(opts.Query, docs)
		filtered = filtered[:0]
		for _, hit := range ranked {
			filtered = append(filtered, byID[hit.ID])
		}
	} else if opts.SortBy == "" || opts.SortBy == "recent" {
		sort.SliceStable(filtered, func(i, j int) bool {
			left := filtered[i].UpdatedAt
			if left == "" {
				left = filtered[i].CreatedAt
			}
			right := filtered[j].UpdatedAt
			if right == "" {
				right = filtered[j].CreatedAt
			}
			if left == right {
				return filtered[i].ID < filtered[j].ID
			}
			return left > right
		})
	}
	if opts.Limit > 0 && len(filtered) > opts.Limit {
		filtered = filtered[:opts.Limit]
	}
	return filtered, nil
}

func passageMatchesTags(passage Passage, tags []string) bool {
	if len(tags) == 0 {
		return true
	}
	for _, expected := range tags {
		for _, actual := range passage.Tags {
			if expected == actual {
				return true
			}
		}
	}
	return false
}

func passageMatchesTypes(passage Passage, types []string) bool {
	if len(types) == 0 {
		return true
	}
	passageType := passage.Type
	if passageType == "" {
		passageType = TypeContext
	}
	for _, expected := range types {
		if passageType == expected {
			return true
		}
	}
	return false
}

func (mm *MemoryManager) rebuildRetrievalCacheLocked(fingerprint string) error {
	var passages []Passage
	if mm.archival != nil {
		archived, err := mm.archival.Search(SearchOptions{SortBy: "recent"})
		if err != nil {
			return err
		}
		for _, passage := range archived {
			if mm.passageVisible(passage) {
				passages = append(passages, passage)
			}
		}
	}
	topics, err := loadTopicDocuments(mm.root)
	if err != nil {
		return err
	}
	for _, topic := range topics {
		if mm.passageVisible(topic.passage) {
			passages = append(passages, topic.passage)
		}
	}
	sort.SliceStable(passages, func(i, j int) bool { return passages[i].ID < passages[j].ID })
	docs := make([]*BM25Doc, 0, len(passages))
	byID := make(map[string]Passage, len(passages))
	for _, p := range passages {
		if strings.TrimSpace(p.Content) == "" {
			continue
		}
		docs = append(docs, NewBM25FDoc(p.ID, p.Content, p.Tags))
		byID[p.ID] = p
	}
	mm.retrievalCache.fingerprint = fingerprint
	mm.retrievalCache.passages = passages
	mm.retrievalCache.docs = docs
	mm.retrievalCache.byID = byID
	mm.retrievalCache.builds++
	return nil
}

func (mm *MemoryManager) contextFingerprint() (string, error) {
	paths := []string{mm.root}
	if mm.core != nil {
		paths = append(paths, mm.core.memoryRoot)
		if mm.core.workspaceRoot != "" {
			paths = append(paths, mm.core.workspaceRoot)
		}
		for _, block := range mm.core.blocks {
			paths = append(paths, mm.core.pathForBlock(block.Label))
		}
	}
	topics, err := topicPathsStrict(mm.root, true)
	if err != nil {
		return "", err
	}
	paths = append(paths, topics...)
	return pathsFingerprint(paths)
}

func (mm *MemoryManager) retrievalFingerprint() (string, error) {
	archiveRoot := filepath.Join(mm.root, "archival")
	paths := []string{mm.root, archiveRoot, filepath.Join(archiveRoot, "passages.jsonl")}
	topics, err := topicPathsStrict(mm.root, false)
	if err != nil {
		return "", err
	}
	paths = append(paths, topics...)
	return pathsFingerprint(paths)
}

func topicPaths(root string, includeIndex bool) []string {
	paths, err := topicPathsStrict(root, includeIndex)
	if err != nil {
		return nil
	}
	return paths
}

func topicPathsStrict(root string, includeIndex bool) ([]string, error) {
	handle, absRoot, err := openAuthoritativeRoot(root)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	var paths []string
	for _, dir := range []string{".", "topics"} {
		if dir != "." {
			info, statErr := handle.Lstat(dir)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil {
				return nil, statErr
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("memory: topic directory is unsafe: %s", filepath.Join(absRoot, dir))
			}
		}
		directory, openErr := handle.Open(dir)
		if openErr != nil {
			return nil, openErr
		}
		entries, readErr := directory.ReadDir(-1)
		directory.Close()
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			if !includeIndex && entry.Name() == memdir.ENTRYPOINT_NAME {
				continue
			}
			rel := filepath.Join(dir, entry.Name())
			info, statErr := handle.Lstat(rel)
			if statErr != nil {
				return nil, statErr
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("memory: topic file is not a regular non-symlink: %s", filepath.Join(absRoot, rel))
			}
			paths = append(paths, filepath.Join(absRoot, rel))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func pathsFingerprint(paths []string) (string, error) {
	h := sha256.New()
	sort.Strings(paths)
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(h, "%s|missing\n", path)
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return "", fmt.Errorf("memory: unsafe fingerprint path: %s", path)
		}
		fmt.Fprintf(h, "%s|%d|%d|%o|%t\n", path, info.Size(), info.ModTime().UnixNano(), info.Mode().Perm(), info.IsDir())
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func loadTopicDocuments(root string) ([]topicDocument, error) {
	paths, err := topicPathsStrict(root, false)
	if err != nil {
		return nil, err
	}
	docs := make([]topicDocument, 0, len(paths))
	for _, path := range paths {
		raw, info, err := readAuthoritativeRegularFile(root, path, maxTopicBytes)
		if err != nil {
			return nil, err
		}
		fm, body, err := memdir.ParseFile(raw)
		if err != nil {
			return nil, fmt.Errorf("parse topic %s: %w", path, err)
		}
		if err := fm.Validate(); err != nil {
			return nil, fmt.Errorf("validate topic %s: %w", path, err)
		}
		redacted := memdir.Redact(string(body))
		if redacted.Reject {
			return nil, fmt.Errorf("validate topic %s: %w", path, ErrSensitiveMemory)
		}
		bodyText := strings.TrimSpace(redacted.Redacted)
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("memory: topic path escapes root: %s", path)
		}
		rel = filepath.ToSlash(rel)
		title := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		description := ""
		pType := TypeContext
		createdAt := info.ModTime().UTC().Format(time.RFC3339Nano)
		originSession := ""
		lastUsed := ""
		confidence := 0.0
		updatedAt := createdAt
		useCount := 0
		sourceMessageID := ""
		scope := topicScope(root)
		if fm != nil {
			if strings.TrimSpace(fm.Name) != "" {
				title = strings.TrimSpace(fm.Name)
			}
			description = strings.TrimSpace(fm.Description)
			if IsKnownType(string(fm.Type)) {
				pType = string(fm.Type)
			}
			originSession = fm.OriginSessionID
			sourceMessageID = fm.SourceMessageID
			if fm.Scope != "" {
				scope = fm.Scope
			}
			updatedAt = fm.UpdatedAt
			if updatedAt == "" {
				updatedAt = createdAt
			}
			lastUsed = fm.LastUsedAt
			if lastUsed == "" {
				lastUsed = fm.LastAccessed
			}
			confidence = fm.Confidence
			if confidence == 0 {
				confidence = fm.Strength
			}
			useCount = fm.UseCount
		}
		title, err = sanitizeLoadedMemoryContent(title)
		if err != nil {
			return nil, fmt.Errorf("validate topic title %s: %w", path, err)
		}
		description, err = sanitizeLoadedMemoryContent(description)
		if err != nil {
			return nil, fmt.Errorf("validate topic description %s: %w", path, err)
		}
		contentParts := []string{title}
		if description != "" {
			contentParts = append(contentParts, description)
		}
		if bodyText != "" {
			contentParts = append(contentParts, bodyText)
		}
		passage, err := validateLoadedPassage(Passage{
			ID:              topicPassageID(rel),
			Content:         strings.Join(contentParts, "\n"),
			Type:            pType,
			Tags:            []string{"topic", strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))},
			CreatedAt:       createdAt,
			UpdatedAt:       updatedAt,
			LastUsedAt:      lastUsed,
			SourceSessionID: originSession,
			SourceMessageID: sourceMessageID,
			Scope:           scope,
			Confidence:      confidence,
			UseCount:        useCount,
		})
		if err != nil {
			return nil, fmt.Errorf("validate topic %s: %w", path, err)
		}
		docs = append(docs, topicDocument{
			passage:     passage,
			title:       title,
			description: description,
			file:        rel,
		})
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].file < docs[j].file })
	return docs, nil
}

func topicPassageID(relativePath string) string {
	idSum := sha256.Sum256([]byte(filepath.ToSlash(relativePath)))
	return fmt.Sprintf("topic-%x", idSum[:12])
}

func topicScope(root string) string {
	clean := filepath.Clean(root)
	metisHome := strings.TrimSpace(os.Getenv("METIS_HOME"))
	if metisHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			metisHome = filepath.Join(home, ".metis")
		}
	}
	if metisHome != "" && clean == filepath.Clean(filepath.Join(metisHome, "memory")) {
		return "global"
	}
	return "project"
}
