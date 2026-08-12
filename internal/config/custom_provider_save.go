package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// CustomProviderSpec contains the non-secret fields collected by the
// first-run custom-provider wizard. Credentials deliberately do not belong in
// this type: hand-entered API keys are persisted by internal/auth in
// auth.json, never copied into config.toml.
type CustomProviderSpec struct {
	ID        string
	Transport string
	BaseURL   string
	Model     string
}

var bareProviderID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var userConfigWriteMu sync.Mutex

// SaveUserCustomProvider stores a custom provider in the user configuration
// at $METIS_HOME/config.toml and selects it as provider.default.
//
// This function intentionally edits the user file's text rather than encoding
// the Config returned by Load. Load includes defaults and project-local
// overlays; serializing it would both bloat the user file and accidentally
// promote checkout-specific settings (including legacy inline credentials) to
// user scope. Textual edits also preserve comments and future/unknown fields.
//
// Only canonical, ordinary tables can be updated in place. If an existing
// provider uses an inline table, quoted table name, dotted assignment, or
// another representation that cannot be changed without reformatting the
// surrounding document, the operation fails before writing anything.
func SaveUserCustomProvider(spec CustomProviderSpec) error {
	userConfigWriteMu.Lock()
	defer userConfigWriteMu.Unlock()
	if err := validateCustomProviderSpec(spec); err != nil {
		return err
	}

	dir := Home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create metis home: %w", err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := rejectConfigSymlink(path); err != nil {
		return err
	}
	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read user config: %w", err)
	}

	updated, err := renderUserCustomProvider(original, spec)
	if err != nil {
		return err
	}
	if err := atomicWritePrivateFile(path, updated); err != nil {
		return fmt.Errorf("save user config: %w", err)
	}
	return nil
}

// SaveUserProviderDefault selects a provider in the user configuration while
// leaving all unrelated user text untouched. It is used by first-run setup for
// built-in providers; API keys remain in auth.json.
func SaveUserProviderDefault(id string) error {
	userConfigWriteMu.Lock()
	defer userConfigWriteMu.Unlock()
	if err := validateProviderID(id); err != nil {
		return err
	}

	dir := Home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create metis home: %w", err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := rejectConfigSymlink(path); err != nil {
		return err
	}
	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read user config: %w", err)
	}
	updated, err := renderUserProviderDefault(original, id)
	if err != nil {
		return err
	}
	if err := atomicWritePrivateFile(path, updated); err != nil {
		return fmt.Errorf("save user config: %w", err)
	}
	return nil
}

func rejectConfigSymlink(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect user config: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to replace symlinked user config.toml; edit its target directly")
	}
	if !st.Mode().IsRegular() {
		return errors.New("refusing to replace non-regular user config.toml")
	}
	return nil
}

func validateCustomProviderSpec(spec CustomProviderSpec) error {
	if err := validateProviderID(spec.ID); err != nil {
		return err
	}
	switch spec.ID {
	case "anthropic", "openai", "gemini", "google", "custom":
		return fmt.Errorf("custom provider id %q conflicts with a built-in provider", spec.ID)
	}
	switch spec.Transport {
	case "openai_chat", "anthropic_messages", "gemini_native":
	default:
		return fmt.Errorf("unsupported custom provider transport %q", spec.Transport)
	}
	for name, value := range map[string]string{
		"transport": spec.Transport,
		"base_url":  spec.BaseURL,
		"model":     spec.Model,
	} {
		if value == "" {
			return fmt.Errorf("custom provider %s is required", name)
		}
		if !validSingleLineTOMLString(value) {
			return fmt.Errorf("custom provider %s must be valid single-line text", name)
		}
	}
	u, err := url.Parse(spec.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("custom provider base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	for _, r := range spec.Model {
		if r == ' ' || r == '\t' {
			return errors.New("custom provider model must not contain whitespace")
		}
	}
	return nil
}

func validateProviderID(id string) error {
	if !bareProviderID.MatchString(id) {
		return errors.New("provider id must start with a lowercase letter or digit and contain only lowercase letters, digits, '-' or '_'")
	}
	return nil
}

func validSingleLineTOMLString(s string) bool {
	if !utf8.ValidString(s) || strings.ContainsAny(s, "\r\n") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type userConfigProbe struct {
	Provider struct {
		Default string                 `toml:"default"`
		Custom  map[string]ProviderRaw `toml:"custom"`
	} `toml:"provider"`
}

func renderUserCustomProvider(original []byte, spec CustomProviderSpec) ([]byte, error) {
	var probe userConfigProbe
	var md toml.MetaData
	var err error
	if len(strings.TrimSpace(string(original))) != 0 {
		md, err = toml.Decode(string(original), &probe)
		if err != nil {
			return nil, fmt.Errorf("parse user config: %w", err)
		}
	}

	newline := "\n"
	if strings.Contains(string(original), "\r\n") {
		newline = "\r\n"
	}
	lines := splitLines(string(original))

	customHeader := "provider.custom." + spec.ID
	customStart, customEnd, customFound := findCanonicalTable(lines, customHeader)
	_, customDefined := probe.Provider.Custom[spec.ID]
	if customDefined && !customFound {
		return nil, fmt.Errorf("cannot safely update existing custom provider %q: use canonical [%s] table syntax", spec.ID, customHeader)
	}
	if customFound && !customDefined {
		// A valid TOML document with this canonical header should always
		// decode into Provider.Custom. Treat a mismatch as unsafe instead of
		// trying to reason about a schema/type edge case.
		return nil, fmt.Errorf("cannot safely update existing custom provider %q: table shape is unsupported", spec.ID)
	}

	if customFound {
		var updateErr error
		lines, updateErr = updateCanonicalCustomTable(lines, customStart, customEnd, md, spec, newline)
		if updateErr != nil {
			return nil, updateErr
		}
	}

	// Re-scan because inserting missing fields into the custom table can move
	// later table offsets. Adding a child table would conflict with an inline
	// [provider].custom value, so reject that representation explicitly.
	providerStart, providerEnd, providerFound := findCanonicalTable(lines, "provider")
	if providerFound {
		if assignmentCount(lines, providerStart+1, providerEnd, "custom") > 0 {
			return nil, fmt.Errorf("cannot safely update custom provider %q: [provider].custom is an inline value", spec.ID)
		}
	}
	lines, err = setProviderDefaultInLines(lines, spec.ID, md, newline)
	if err != nil {
		return nil, err
	}

	if !customFound {
		block := []string{
			"[" + customHeader + "]" + newline,
			"transport = " + quoteTOMLString(spec.Transport) + newline,
			"base_url = " + quoteTOMLString(spec.BaseURL) + newline,
			"model = " + quoteTOMLString(spec.Model) + newline,
		}
		lines = appendBlock(lines, block, newline)
	}

	result := []byte(strings.Join(lines, ""))
	if err := verifyRenderedCustomProvider(result, spec); err != nil {
		return nil, err
	}
	return result, nil
}

func renderUserProviderDefault(original []byte, id string) ([]byte, error) {
	var probe userConfigProbe
	var md toml.MetaData
	var err error
	if len(strings.TrimSpace(string(original))) != 0 {
		md, err = toml.Decode(string(original), &probe)
		if err != nil {
			return nil, fmt.Errorf("parse user config: %w", err)
		}
	}
	newline := "\n"
	if strings.Contains(string(original), "\r\n") {
		newline = "\r\n"
	}
	lines, err := setProviderDefaultInLines(splitLines(string(original)), id, md, newline)
	if err != nil {
		return nil, err
	}
	result := []byte(strings.Join(lines, ""))
	var verified userConfigProbe
	if _, err := toml.Decode(string(result), &verified); err != nil {
		return nil, fmt.Errorf("refusing to write invalid updated user config: %w", err)
	}
	if verified.Provider.Default != id {
		return nil, errors.New("refusing to write user config: provider.default verification failed")
	}
	return result, nil
}

func setProviderDefaultInLines(lines []string, id string, md toml.MetaData, newline string) ([]string, error) {
	providerStart, providerEnd, providerFound := findCanonicalTable(lines, "provider")
	if providerFound {
		return setTableString(lines, providerStart, providerEnd, "default", id, md.IsDefined("provider", "default"), "provider.default", newline)
	}
	if md.IsDefined("provider", "default") {
		return nil, errors.New("cannot safely update provider.default: use canonical [provider] table syntax")
	}
	firstChild := firstProviderChildTable(lines)
	if firstChild < 0 && providerDefinedWithoutTable(lines) {
		return nil, errors.New("cannot safely update provider.default: provider is defined using inline or dotted syntax")
	}
	block := []string{
		"[provider]" + newline,
		"default = " + quoteTOMLString(id) + newline,
		newline,
	}
	if firstChild >= 0 {
		return insertLines(lines, firstChild, block, newline), nil
	}
	return appendBlock(lines, block, newline), nil
}

func updateCanonicalCustomTable(lines []string, start, end int, md toml.MetaData, spec CustomProviderSpec, newline string) ([]string, error) {
	fields := []struct {
		key   string
		value string
	}{
		{key: "transport", value: spec.Transport},
		{key: "base_url", value: spec.BaseURL},
		{key: "model", value: spec.Model},
	}
	for _, field := range fields {
		semanticPath := []string{"provider", "custom", spec.ID, field.key}
		var err error
		lines, err = setTableString(lines, start, end, field.key, field.value, md.IsDefined(semanticPath...), "custom provider "+spec.ID+"."+field.key, newline)
		if err != nil {
			return nil, err
		}
		// A newly inserted line shifts the table end by one.
		_, end, _ = findCanonicalTable(lines, "provider.custom."+spec.ID)
	}
	return lines, nil
}

func setTableString(lines []string, start, end int, key, value string, semanticallyDefined bool, fieldName, newline string) ([]string, error) {
	positions := assignmentPositions(lines, start+1, end, key)
	if len(positions) > 1 {
		return nil, fmt.Errorf("cannot safely update %s: duplicate assignments", fieldName)
	}
	if len(positions) == 0 {
		if semanticallyDefined {
			return nil, fmt.Errorf("cannot safely update %s: use a canonical direct assignment", fieldName)
		}
		line := key + " = " + quoteTOMLString(value) + newline
		return insertLines(lines, start+1, []string{line}, newline), nil
	}
	pos := positions[0]
	replaced, err := replaceStringAssignment(lines[pos], key, value)
	if err != nil {
		return nil, fmt.Errorf("cannot safely update %s: %w", fieldName, err)
	}
	lines[pos] = replaced
	return lines, nil
}

func verifyRenderedCustomProvider(data []byte, spec CustomProviderSpec) error {
	var probe userConfigProbe
	if _, err := toml.Decode(string(data), &probe); err != nil {
		return fmt.Errorf("refusing to write invalid updated user config: %w", err)
	}
	raw, ok := probe.Provider.Custom[spec.ID]
	if !ok || probe.Provider.Default != spec.ID || raw.Transport != spec.Transport || raw.BaseURL != spec.BaseURL || raw.Model != spec.Model {
		return errors.New("refusing to write user config: custom provider verification failed")
	}
	return nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := make([]string, 0, strings.Count(s, "\n")+1)
	for len(s) > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			lines = append(lines, s[:i+1])
			s = s[i+1:]
			continue
		}
		lines = append(lines, s)
		break
	}
	return lines
}

func findCanonicalTable(lines []string, name string) (start, end int, found bool) {
	for i, line := range lines {
		header, ok := canonicalTableHeader(line)
		if !ok || header != name {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if isAnyTableHeader(lines[j]) {
				return i, j, true
			}
		}
		return i, len(lines), true
	}
	return 0, 0, false
}

func canonicalTableHeader(line string) (string, bool) {
	body, _ := trimLineEnding(line)
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "[[") {
		return "", false
	}
	closeAt := strings.IndexByte(trimmed, ']')
	if closeAt < 0 {
		return "", false
	}
	rest := strings.TrimSpace(trimmed[closeAt+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", false
	}
	name := strings.TrimSpace(trimmed[1:closeAt])
	if name == "" || strings.ContainsAny(name, "\"'") || strings.Contains(name, " ") || strings.Contains(name, "\t") {
		return "", false
	}
	return name, true
}

func isAnyTableHeader(line string) bool {
	body, _ := trimLineEnding(line)
	trimmed := strings.TrimSpace(body)
	return strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[#")
}

func firstProviderChildTable(lines []string) int {
	for i, line := range lines {
		name, ok := canonicalTableHeader(line)
		if ok && strings.HasPrefix(name, "provider.") {
			return i
		}
	}
	return -1
}

func providerDefinedWithoutTable(lines []string) bool {
	for _, line := range lines {
		body, _ := trimLineEnding(line)
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "[") {
			continue
		}
		if strings.HasPrefix(trimmed, "provider") {
			rest := strings.TrimLeft(trimmed[len("provider"):], " \t")
			if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ".") {
				return true
			}
		}
	}
	return false
}

func assignmentCount(lines []string, start, end int, key string) int {
	return len(assignmentPositions(lines, start, end, key))
}

func assignmentPositions(lines []string, start, end int, key string) []int {
	pattern := regexp.MustCompile(`^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=`)
	var positions []int
	for i := start; i < end; i++ {
		body, _ := trimLineEnding(lines[i])
		if pattern.MatchString(body) {
			positions = append(positions, i)
		}
	}
	return positions
}

func replaceStringAssignment(line, key, value string) (string, error) {
	body, ending := trimLineEnding(line)
	pattern := regexp.MustCompile(`^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=[ \t]*`)
	loc := pattern.FindStringIndex(body)
	if loc == nil {
		return "", errors.New("assignment is not canonical")
	}
	oldValue := body[loc[1]:]
	trimmedValue := strings.TrimSpace(oldValue)
	if strings.HasPrefix(trimmedValue, `"""`) || strings.HasPrefix(trimmedValue, `'''`) {
		return "", errors.New("multiline strings are unsupported")
	}

	comment := unquotedCommentIndex(oldValue)
	suffix := ""
	if comment >= 0 {
		start := comment
		for start > 0 && (oldValue[start-1] == ' ' || oldValue[start-1] == '\t') {
			start--
		}
		suffix = oldValue[start:]
	} else {
		end := len(oldValue)
		for end > 0 && (oldValue[end-1] == ' ' || oldValue[end-1] == '\t') {
			end--
		}
		suffix = oldValue[end:]
	}
	return body[:loc[1]] + quoteTOMLString(value) + suffix + ending, nil
}

func unquotedCommentIndex(s string) int {
	quote := byte(0)
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '#':
			return i
		}
	}
	return -1
}

func quoteTOMLString(s string) string {
	// Validation excludes controls for which Go emits TOML-incompatible
	// \xNN escapes. strconv.Quote then produces a valid TOML basic string.
	return strconv.Quote(s)
}

func trimLineEnding(line string) (body, ending string) {
	if strings.HasSuffix(line, "\r\n") {
		return strings.TrimSuffix(line, "\r\n"), "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return strings.TrimSuffix(line, "\n"), "\n"
	}
	return line, ""
}

func insertLines(lines []string, at int, additions []string, newline string) []string {
	if at > 0 {
		_, ending := trimLineEnding(lines[at-1])
		if ending == "" {
			lines[at-1] += newline
		}
	}
	result := make([]string, 0, len(lines)+len(additions))
	result = append(result, lines[:at]...)
	result = append(result, additions...)
	result = append(result, lines[at:]...)
	return result
}

func appendBlock(lines, block []string, newline string) []string {
	if len(lines) > 0 {
		body, ending := trimLineEnding(lines[len(lines)-1])
		if ending == "" {
			lines[len(lines)-1] = body + newline
		}
		if strings.TrimSpace(body) != "" {
			lines = append(lines, newline)
		}
	}
	return append(lines, block...)
}

func atomicWritePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config.toml.*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := replaceUserConfigFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace config.toml: %w", err)
	}
	return nil
}
