package agent

// This file contains the mutation boundary for the dreaming agent's memory
// directory. The normal Write/Edit tools intentionally optimize for general
// workspace editing and may create a 0644 destination before the extractor's
// post-processing pass runs. Memory files need a stronger contract: sanitize
// and validate the complete replacement first, then create only a 0600 temp
// inode and atomically rename it into place.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory"
	memorysecurity "github.com/Ricardo-M-L/metis/internal/memory/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

const maxSecureAutoMemoryBytes = 64 * 1024 * 1024

// secureAutoMemoryTool deliberately embeds the original tool. This preserves
// its public name, description, input schema, enablement, permission decision,
// and concurrency contract while replacing only the unsafe filesystem commit.
// The executor accepts both Metis' path/old/new fields and the historical
// file_path/old_string/new_string aliases used by restored transcripts.
type secureAutoMemoryTool struct {
	tools.Tool
	root   string
	source AutoMemorySource
	memory memory.Repository
}

func (t secureAutoMemoryTool) ShortDescription() string {
	return tools.DescriptionFor(t.Tool, true)
}

func (t secureAutoMemoryTool) IsDestructive(map[string]any) bool { return true }

func (t secureAutoMemoryTool) Execute(ctx context.Context, input map[string]any) (*tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := autoMemoryStringField(input, "path", "file_path")
	if path == "" {
		return nil, fmt.Errorf("%s: path is required", t.Name())
	}
	target, err := secureAutoMemoryTarget(t.root, path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name(), err)
	}

	var content string
	var expectedSHA256 string
	switch t.Name() {
	case "Write":
		content = autoMemoryStringField(input, "content")
		if len(content) > maxSecureAutoMemoryBytes {
			return nil, fmt.Errorf("Write: content too large: %d bytes exceeds %d byte cap", len(content), maxSecureAutoMemoryBytes)
		}
	case "Edit":
		content, expectedSHA256, err = secureAutoMemoryEditContent(target, input)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("auto-memory: unsupported secure mutation tool %q", t.Name())
	}

	if t.memory != nil {
		err = t.memory.CommitTopic(ctx, memory.TopicMutation{
			Path:    target,
			Content: []byte(content),
			Source: memory.TopicSource{
				SessionID:  t.source.SessionID,
				MessageID:  t.source.MessageID,
				Scope:      t.source.Scope,
				Confidence: t.source.Confidence,
			},
			ExpectedSHA256: expectedSHA256,
		})
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", t.Name(), target, err)
		}
	} else {
		// Isolated unit tests and legacy callers without a repository retain a
		// private atomic fallback. Production runtimes always provide the
		// canonical Repository and therefore share its tombstone/ownership lock.
		prepared, prepareErr := prepareSecureAutoMemoryMemo(content, t.source)
		if prepareErr != nil {
			return nil, fmt.Errorf("%s %s: %w", t.Name(), target, prepareErr)
		}
		if err := atomicWritePrivateMemo(target, prepared); err != nil {
			return nil, err
		}
	}
	if t.Name() == "Edit" {
		return &tools.Result{Output: "edited " + target}, nil
	}
	return &tools.Result{Output: "wrote " + target}, nil
}

// secureAutoMemoryRegistry copies the dream registry and replaces its generic
// Write/Edit entries. MultiEdit is intentionally not copied: it has no
// dream-specific validator and leaving it available would bypass this boundary.
// SkillSynth is copied unchanged because its separate skills directory and
// validator are outside the memory-topic store.
func secureAutoMemoryRegistry(reg *tools.Registry, root string, source AutoMemorySource, repo memory.Repository) *tools.Registry {
	if reg == nil {
		return nil
	}
	secured := tools.NewRegistry()
	for _, tool := range reg.All() {
		switch tool.Name() {
		case "Write", "Edit":
			secured.Register(secureAutoMemoryTool{Tool: tool, root: root, source: source, memory: repo})
		case "MultiEdit":
			continue
		default:
			secured.Register(tool)
		}
	}
	return secured
}

func secureAutoMemoryEditContent(path string, input map[string]any) (string, string, error) {
	old := autoMemoryStringField(input, "old", "old_string")
	newValue := autoMemoryStringField(input, "new", "new_string")
	if old == "" {
		return "", "", errors.New("Edit: old is required")
	}
	if old == newValue {
		return "", "", errors.New("Edit: old and new are identical")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("Edit: %s is not a regular file", path)
	}
	if info.Size() > maxSecureAutoMemoryBytes {
		return "", "", fmt.Errorf("Edit: file too large: %d bytes exceeds %d byte cap", info.Size(), maxSecureAutoMemoryBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	expected := memory.TopicContentSHA256(raw)
	body := string(raw)
	count := strings.Count(body, old)
	if count == 0 {
		return "", "", errors.New("Edit: old string not found in file")
	}
	all, _ := input["all"].(bool)
	if alias, ok := input["replace_all"].(bool); ok {
		all = alias
	}
	if !all && count > 1 {
		return "", "", fmt.Errorf("Edit: old string appears %d times; pass all=true or supply more context", count)
	}
	if all {
		return strings.ReplaceAll(body, old, newValue), expected, nil
	}
	return strings.Replace(body, old, newValue, 1), expected, nil
}

// prepareSecureAutoMemoryMemo operates entirely in memory. No filesystem
// object is created until the returned bytes have been redacted, threat
// scanned, parsed, and rendered into canonical frontmatter form.
func prepareSecureAutoMemoryMemo(content string, source AutoMemorySource) ([]byte, error) {
	redacted := memdir.Redact(content)
	if redacted.Reject {
		return nil, memory.ErrSensitiveMemory
	}
	if threats := memorysecurity.ScanAll(redacted.Redacted); len(threats) > 0 {
		kinds := make([]string, 0, len(threats))
		seen := make(map[memorysecurity.ThreatKind]struct{}, len(threats))
		for _, threat := range threats {
			if _, ok := seen[threat.Kind]; ok {
				continue
			}
			seen[threat.Kind] = struct{}{}
			kinds = append(kinds, string(threat.Kind))
		}
		return nil, fmt.Errorf("%w: %s", memory.ErrUnsafeMemory, strings.Join(kinds, ","))
	}
	fm, body, err := memdir.ParseFile([]byte(redacted.Redacted))
	if err != nil {
		return nil, err
	}
	if err := fm.Validate(); err != nil {
		return nil, err
	}
	if !fm.Type.IsValid() {
		return nil, fmt.Errorf("memdir: invalid memory type %q", fm.Type)
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, errors.New("memdir: empty memory body")
	}
	// Stamp provenance before the atomic commit. Besides making the first
	// durable version complete, this lets a concurrent DeleteSession sweep a
	// just-renamed memo instead of waiting for post-processing.
	if fm.OriginSessionID == "" {
		fm.OriginSessionID = strings.TrimSpace(source.SessionID)
	}
	if source.MessageID != "" {
		fm.SourceMessageID = source.MessageID
	}
	if source.Scope != "" {
		fm.Scope = source.Scope
	}
	if source.Confidence > 0 {
		fm.Confidence = source.Confidence
	}
	return memdir.RenderFile(fm, string(body))
}

func atomicWritePrivateMemo(path string, content []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auto-memory-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	// Apply the final permission before writing even the first byte. The rename
	// preserves this inode, so neither temp nor destination is ever 0644.
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// secureAutoMemoryTarget resolves the configured root once, then walks any
// nested parent one component at a time. Symlink components and non-regular
// existing targets are rejected so a memory-only writer cannot be redirected
// into another part of the user's filesystem.
func secureAutoMemoryTarget(root, candidate string) (string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(candidate) == "" {
		return "", errors.New("auto-memory: empty root or target")
	}
	if !filepath.IsAbs(candidate) {
		return "", errors.New("auto-memory: target path must be absolute")
	}
	if err := memdir.EnsureRoot(root); err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, ok := relativeMemoryTarget(rootAbs, candidateAbs)
	if !ok {
		rel, ok = relativeMemoryTarget(resolvedRoot, candidateAbs)
	}
	if !ok || rel == "." {
		return "", fmt.Errorf("auto-memory: target %q is outside memory root", candidate)
	}
	if !strings.EqualFold(filepath.Ext(rel), ".md") {
		return "", errors.New("auto-memory: memory target must be a .md file")
	}
	if filepath.Base(rel) == memdir.ENTRYPOINT_NAME {
		return "", errors.New("auto-memory: MEMORY.md is generated and cannot be written directly")
	}

	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	parent := resolvedRoot
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("auto-memory: invalid target path component")
		}
		parent = filepath.Join(parent, part)
		info, statErr := os.Lstat(parent)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, statErr = os.Lstat(parent)
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("auto-memory: target parent %s is not a private directory", parent)
		}
		if err := os.Chmod(parent, 0o700); err != nil {
			return "", err
		}
	}
	target := filepath.Join(resolvedRoot, rel)
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("auto-memory: target %s is not a regular file", target)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	return target, nil
}

func relativeMemoryTarget(root, candidate string) (string, bool) {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func autoMemoryStringField(input map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := input[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
