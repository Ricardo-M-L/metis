// Package sessionfiles exposes read-only previews of files explicitly attached
// to a saved conversation by successful file tools or assistant Markdown links.
// A file ID is session-scoped metadata, never permission to read an arbitrary
// path: every read rediscovers membership and checks the current filesystem.
package sessionfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const MaxPreviewBytes = 512 << 10

var (
	ErrInvalidSession = errors.New("files: a valid session ID is required")
	ErrNotFound       = errors.New("files: file not found in this session")
	ErrNotText        = errors.New("files: preview requires a UTF-8 text file")
	ErrUnavailable    = errors.New("files: session unavailable")
	lineSuffixRE      = regexp.MustCompile(`:\d+(?::\d+)?$`)
)

type File struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	ToolUseID string `json:"toolUseId,omitempty"`
}

type Preview struct {
	File      File   `json:"file"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Loader interface {
	Load(string) (*session.Header, []llm.Message, error)
}

type Service struct {
	loader    Loader
	tempRoots []string
}

func New(loader Loader) *Service {
	roots := []string{os.TempDir()}
	if runtime.GOOS != "windows" {
		roots = append(roots, "/tmp")
	}
	return &Service{loader: loader, tempRoots: roots}
}

type candidate struct {
	File
	canonical string
	root      string
	info      os.FileInfo
}

// List examines transcript membership and file metadata only; it does not read
// generated content. Missing files and paths that cannot be safely previewed
// are omitted. Large or binary files are handled explicitly by Read.
func (s *Service) List(sessionID string) ([]File, error) {
	candidates, err := s.discover(sessionID)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(candidates))
	for _, item := range candidates {
		files = append(files, item.File)
	}
	return files, nil
}

func validSessionID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 200 || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func (s *Service) discover(sessionID string) ([]candidate, error) {
	if !validSessionID(sessionID) {
		return nil, ErrInvalidSession
	}
	if s == nil || s.loader == nil {
		return nil, ErrUnavailable
	}
	header, messages, err := s.loader.Load(sessionID)
	if err != nil || header == nil || header.ID != sessionID {
		return nil, ErrUnavailable
	}
	byPath := make(map[string]candidate)
	add := func(raw, source, toolID string) {
		item, ok := s.resolve(sessionID, header.WorkDir, raw, source, toolID)
		if !ok {
			return
		}
		key := pathKey(item.canonical)
		old, exists := byPath[key]
		if !exists || old.Source != "tool" && source == "tool" {
			byPath[key] = item
		}
	}
	pending := make(map[string]llm.ContentBlock)
	for _, message := range messages {
		// A new user turn cannot complete a stale/orphaned previous tool call.
		if message.Role == llm.RoleUser {
			hasResult := false
			for _, block := range message.Content {
				if block.Type == "tool_result" {
					hasResult = true
				}
			}
			for _, block := range message.Content {
				if !hasResult && !block.Synthetic && block.Type == "text" && transcript.VisibleUserText(block.Text) != "" {
					clear(pending)
					break
				}
			}
		}
		for _, block := range message.Content {
			switch {
			case message.Role == llm.RoleAssistant && block.Type == "text":
				for _, path := range markdownLinks(block.Text) {
					add(path, "assistant", "")
				}
			case message.Role == llm.RoleAssistant && block.Type == "tool_use" && block.ToolUseID != "":
				pending[block.ToolUseID] = block
			case (message.Role == llm.RoleUser || message.Role == llm.RoleTool) && block.Type == "tool_result":
				call, ok := pending[block.ToolUseID]
				delete(pending, block.ToolUseID)
				if !ok || block.IsError {
					continue
				}
				for _, path := range writtenPaths(call) {
					add(path, "tool", call.ToolUseID)
				}
			}
		}
	}
	result := make([]candidate, 0, len(byPath))
	for _, item := range byPath {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func writtenPaths(call llm.ContentBlock) []string {
	stringArg := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := call.ToolInput[key].(string); ok && value != "" {
				return value
			}
		}
		return ""
	}
	switch strings.ToLower(call.ToolName) {
	case "write", "edit":
		if path := stringArg("path", "file_path"); path != "" {
			return []string{path}
		}
	case "apply_patch", "applypatch":
		patch := stringArg("patch", "input", "patch_text")
		var paths []string
		for _, line := range strings.Split(patch, "\n") {
			line = strings.TrimSuffix(line, "\r")
			for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Move to: "} {
				if strings.HasPrefix(line, prefix) {
					paths = append(paths, strings.TrimPrefix(line, prefix))
					break
				}
			}
		}
		return paths
	}
	return nil
}

func markdownLinks(source string) []string {
	var paths []string
	doc := goldmark.DefaultParser().Parse(text.NewReader([]byte(source)))
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := node.(*ast.Link); ok && entering {
			if path := localLink(string(link.Destination)); path != "" && fileKind(path) != "" {
				paths = append(paths, path)
			}
		}
		return ast.WalkContinue, nil
	})
	return paths
}

func localLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "" && u.Scheme != "file" || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" {
		return ""
	}
	path := u.Path // Parse already decodes percent-encoded UTF-8 and spaces.
	if path == "" {
		return ""
	}
	return lineSuffixRE.ReplaceAllString(path, "")
}

func fileKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown":
		return "markdown"
	case ".txt", ".text", ".json", ".yaml", ".yml", ".csv", ".tsv", ".log", ".toml", ".html", ".xml", ".css", ".js", ".jsx", ".ts", ".tsx", ".go", ".py", ".rs", ".sh", ".sql":
		return "text"
	}
	return ""
}

func pathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func cleanAbsolute(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	path = filepath.Clean(path)
	for _, r := range path {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	return path, true
}

func credentialPath(path string) bool {
	if permission.IsSecretReadPath(path) {
		return true
	}
	// The tool permission classifier covers METIS and common system stores.
	// Preview also excludes generic credential filenames/directories belonging
	// to other local agents, even after a successfully recorded write.
	for _, part := range strings.Split(strings.ToLower(filepath.ToSlash(path)), "/") {
		if part == ".credentials" {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "auth.json", "auth.toml", "credentials", "credentials.json", "credentials.toml", ".credentials.json", "tokens.json", "secrets.json":
		return true
	}
	return false
}

func (s *Service) resolve(sessionID, workDir, raw, source, toolID string) (candidate, bool) {
	if raw == "" {
		return candidate{}, false
	}
	if !filepath.IsAbs(raw) {
		if !filepath.IsAbs(workDir) {
			return candidate{}, false
		}
		raw = filepath.Join(workDir, raw)
	}
	path, ok := cleanAbsolute(raw)
	if !ok || fileKind(path) == "" || credentialPath(path) {
		return candidate{}, false
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || credentialPath(canonical) {
		return candidate{}, false
	}
	// Select a lexical scope before resolving symlinks: a workspace symlink
	// cannot borrow the broader temp allowance to escape its workspace.
	roots := append([]string{workDir}, s.tempRoots...)
	var root string
	for _, lexicalRoot := range roots {
		lexicalRoot, ok = cleanAbsolute(lexicalRoot)
		if !ok {
			continue
		}
		resolvedRoot, e := filepath.EvalSymlinks(lexicalRoot)
		if e != nil {
			continue
		}
		// macOS callers commonly use either /tmp or /private/tmp, and
		// either spelling of a symlinked workspace denotes the same scope.
		if !within(lexicalRoot, path) && !within(resolvedRoot, canonical) {
			continue
		}
		if !within(resolvedRoot, canonical) {
			return candidate{}, false
		}
		root = resolvedRoot
		break
	}
	if root == "" {
		if source != "tool" {
			return candidate{}, false
		}
		// A successful recorded write authorizes that file, not its siblings.
		root, err = filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil || !within(root, canonical) {
			return candidate{}, false
		}
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return candidate{}, false
	}
	digest := sha256.Sum256([]byte(sessionID + "\x00" + pathKey(canonical)))
	file := File{ID: "sf_" + hex.EncodeToString(digest[:16]), Path: safeText(path), Name: safeText(filepath.Base(path)), Kind: fileKind(path), Source: source, ToolUseID: safeText(toolID)}
	return candidate{File: file, canonical: canonical, root: root, info: info}, true
}

func (s *Service) Read(sessionID, id string) (*Preview, error) {
	items, err := s.discover(sessionID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		// OpenRoot pins the directory and prevents symlink traversal out of the
		// validated scope even if a path component changes during the read.
		root, err := os.OpenRoot(item.root)
		if err != nil {
			return nil, ErrNotFound
		}
		defer root.Close()
		rel, err := filepath.Rel(item.root, item.canonical)
		if err != nil {
			return nil, ErrNotFound
		}
		file, err := root.Open(rel)
		if err != nil {
			return nil, ErrNotFound
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || !os.SameFile(item.info, info) {
			return nil, ErrNotFound
		}
		data, err := io.ReadAll(io.LimitReader(file, MaxPreviewBytes+1))
		if err != nil {
			return nil, ErrNotFound
		}
		truncated := len(data) > MaxPreviewBytes
		if truncated {
			data = data[:MaxPreviewBytes]
			// The bounded prefix may end partway through one UTF-8 code point.
			start := len(data) - 1
			for start >= 0 && !utf8.RuneStart(data[start]) {
				start--
			}
			if start >= 0 && !utf8.FullRune(data[start:]) {
				data = data[:start]
			}
		}
		if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
			return nil, ErrNotText
		}
		content := safePreview(data)
		if len(content) > MaxPreviewBytes {
			content = content[:MaxPreviewBytes]
			for !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
			truncated = true
		}
		return &Preview{File: item.File, Content: content, Truncated: truncated}, nil
	}
	return nil, ErrNotFound
}

func safeText(value string) string {
	value = ansi.Strip(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
	return security.RedactSubprocessText(value)
}

func safePreview(data []byte) string {
	// Remove terminal escape sequences before finding credential spans, so an
	// inserted color/OSC sequence cannot split a credential field or value.
	source := []byte(safeText(string(data)))
	redactor := security.NewFileCredentialRedactor(source)
	var out strings.Builder
	offset := 0
	for _, line := range strings.SplitAfter(string(source), "\n") {
		out.WriteString(redactor.RedactLineAt(offset, line))
		offset += len(line)
	}
	return security.RedactSubprocessText(out.String())
}
