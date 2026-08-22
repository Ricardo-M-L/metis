package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// ProviderDefaultOverrideSource reports whether the current project's config
// controls provider.default. CustomProviderOverrideSource does the same for a
// named custom profile. Desktop callers use these preflights so they never
// claim a user-level edit took effect when a project layer still wins.
func ProviderDefaultOverrideSource() (string, error) {
	return providerOverrideSource([]string{"provider", "default"})
}

func CustomProviderOverrideSource(id string) (string, error) {
	if err := validateProviderID(id); err != nil {
		return "", err
	}
	return providerOverrideSource([]string{"provider", "custom", id})
}

func providerOverrideSource(path []string) (string, error) {
	source := ""
	for _, candidate := range projectConfigPaths() {
		if _, err := os.Stat(candidate.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("inspect %s: %w", candidate.label, err)
		}
		var raw map[string]any
		md, err := toml.DecodeFile(candidate.path, &raw)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", candidate.label, err)
		}
		if md.IsDefined(path...) {
			source = candidate.label
		}
	}
	return source, nil
}

var bareProviderID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// UserSetting is one non-secret scalar setting that can be changed from the
// interactive /config panel. The allowlist is intentionally small: callers
// cannot use this API to write provider credentials, hooks, shell commands, or
// a merged Config containing project-local overlays.
type UserSetting struct {
	Key   string
	Value string
}

// SaveUserSettings safely patches ordinary scalar settings in the user-level
// config.toml. It preserves comments, unknown fields, and project overlays by
// editing the raw user file instead of encoding Config returned by Load.
func SaveUserSettings(settings []UserSetting) error {
	_, err := SaveUserSettingsAndLoad(settings)
	return err
}

// SaveUserSettingsAndLoad performs the same guarded update as
// SaveUserSettings and returns the already-validated merged configuration.
// The /config panel uses this transactional form so it never has to write the
// user file and then discover during a second Load that a higher-precedence
// project file makes the resulting configuration invalid.
func SaveUserSettingsAndLoad(settings []UserSetting) (*Config, error) {
	if len(settings) == 0 {
		cfg, _, err := Load()
		return cfg, err
	}
	var loaded *Config
	err := withUserConfigWriteLock(func(path string) error {
		if err := rejectConfigSymlink(path); err != nil {
			return err
		}
		original, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read user config: %w", err)
		}
		for _, setting := range settings {
			source, err := userSettingOverrideSource(setting.Key)
			if err != nil {
				return err
			}
			if source != "" {
				return fmt.Errorf("setting %q is controlled by %s; edit that file instead", setting.Key, source)
			}
		}
		updated, err := renderUserSettings(original, settings)
		if err != nil {
			return err
		}
		loaded, err = loadUserSettingsCandidate(updated)
		if err != nil {
			return err
		}
		if err := verifyUserSettingsCandidate(loaded, settings); err != nil {
			return err
		}
		if err := atomicWritePrivateFile(path, updated); err != nil {
			return fmt.Errorf("save user config: %w", err)
		}
		return nil
	})
	return loaded, err
}

// UserSettingOverrideSource reports whether a setting exposed by /config is
// defined by a higher-precedence project layer. The empty string means a user
// config update can become effective in this working directory. Values are
// deliberately stable labels rather than absolute paths so the settings UI
// does not expose checkout paths in screenshots.
func UserSettingOverrideSource(key string) (string, error) {
	if _, ok := editableUserSettings[key]; !ok {
		return "", fmt.Errorf("setting %q is not editable from /config", key)
	}
	return userSettingOverrideSource(key)
}

func userSettingOverrideSource(key string) (string, error) {
	spec, ok := editableUserSettings[key]
	if !ok {
		return "", fmt.Errorf("setting %q is not editable from /config", key)
	}
	path := append(strings.Split(spec.table, "."), spec.field)
	source := ""
	for _, candidate := range projectConfigPaths() {
		if _, err := os.Stat(candidate.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("inspect %s: %w", candidate.label, err)
		}
		var raw map[string]any
		md, err := toml.DecodeFile(candidate.path, &raw)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", candidate.label, err)
		}
		if md.IsDefined(path...) {
			source = candidate.label
		}
	}
	return source, nil
}

type projectConfigPath struct {
	path  string
	label string
}

func projectConfigPaths() []projectConfigPath {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return []projectConfigPath{
		{path: filepath.Join(cwd, ".metis", "config.toml"), label: "project config (.metis/config.toml)"},
		{path: filepath.Join(cwd, ".metis", "config.local.toml"), label: "project-local config (.metis/config.local.toml)"},
	}
}

type userSettingKind uint8

const (
	userSettingString userSettingKind = iota
	userSettingBool
	userSettingInt
	userSettingFloat
)

type userSettingSpec struct {
	table    string
	field    string
	kind     userSettingKind
	validate func(string) error
}

var editableUserSettings = map[string]userSettingSpec{
	"permission.mode":                     {table: "permission", field: "mode", kind: userSettingString, validate: oneOf("default", "acceptEdits", "plan", "dontAsk", "bypassPermissions")},
	"ui.theme":                            {table: "ui", field: "theme", kind: userSettingString, validate: oneOf("auto", "dark", "light", "dark-daltonized", "nord", "solarized-dark")},
	"ui.markdown":                         {table: "ui", field: "markdown", kind: userSettingBool},
	"ui.show_tokens":                      {table: "ui", field: "show_tokens", kind: userSettingBool},
	"ui.show_tool_json":                   {table: "ui", field: "show_tool_json", kind: userSettingBool},
	"ui.thinking_display":                 {table: "ui", field: "thinking_display", kind: userSettingString, validate: oneOf("show", "auto", "hide")},
	"ui.streamlined_output":               {table: "ui", field: "streamlined_output", kind: userSettingBool},
	"ui.permission_timeout_seconds":       {table: "ui", field: "permission_timeout_seconds", kind: userSettingInt, validate: intRange(0, 86400)},
	"session.auto_compact_threshold":      {table: "session", field: "auto_compact_threshold", kind: userSettingFloat, validate: floatRange(0.1, 1)},
	"session.auto_compact_minimum_tokens": {table: "session", field: "auto_compact_minimum_tokens", kind: userSettingInt, validate: intRange(0, 10000000)},
	"session.max_iterations":              {table: "session", field: "max_iterations", kind: userSettingInt, validate: intRange(1, 100000)},
	"loop_detection.disabled":             {table: "loop_detection", field: "disabled", kind: userSettingBool},
	"ui.performance.reduced_motion":       {table: "ui.performance", field: "reduced_motion", kind: userSettingBool},
	"ui.performance.mouse_wheel_lines":    {table: "ui.performance", field: "mouse_wheel_lines", kind: userSettingInt, validate: intRange(1, 50)},
	"ui.performance.max_mounted_items":    {table: "ui.performance", field: "max_mounted_items", kind: userSettingInt, validate: intRange(0, 100000)},
}

func renderUserSettings(original []byte, settings []UserSetting) ([]byte, error) {
	var raw map[string]any
	var md toml.MetaData
	var err error
	if len(strings.TrimSpace(string(original))) != 0 {
		md, err = toml.Decode(string(original), &raw)
		if err != nil {
			return nil, fmt.Errorf("parse user config: %w", err)
		}
	}
	newline := "\n"
	if strings.Contains(string(original), "\r\n") {
		newline = "\r\n"
	}
	lines := splitLines(string(original))
	seen := make(map[string]struct{}, len(settings))
	for _, setting := range settings {
		spec, ok := editableUserSettings[setting.Key]
		if !ok {
			return nil, fmt.Errorf("setting %q is not editable from /config", setting.Key)
		}
		if _, duplicate := seen[setting.Key]; duplicate {
			return nil, fmt.Errorf("duplicate setting %q", setting.Key)
		}
		seen[setting.Key] = struct{}{}
		value, err := canonicalUserSettingValue(spec, setting.Value)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", setting.Key, err)
		}
		lines, err = setUserScalarInLines(lines, spec, value, md, newline)
		if err != nil {
			return nil, err
		}
	}
	result := []byte(strings.Join(lines, ""))
	var verified map[string]any
	if _, err := toml.Decode(string(result), &verified); err != nil {
		return nil, fmt.Errorf("refusing to write invalid updated user config: %w", err)
	}
	return result, nil
}

func setUserScalarInLines(lines []string, spec userSettingSpec, value string, md toml.MetaData, newline string) ([]string, error) {
	start, end, found := findCanonicalTable(lines, spec.table)
	path := append(strings.Split(spec.table, "."), spec.field)
	if found {
		positions := assignmentPositions(lines, start+1, end, spec.field)
		if len(positions) > 1 {
			return nil, fmt.Errorf("cannot safely update %s: duplicate assignments", strings.Join(path, "."))
		}
		if len(positions) == 0 {
			if md.IsDefined(path...) {
				return nil, fmt.Errorf("cannot safely update %s: use a canonical direct assignment", strings.Join(path, "."))
			}
			return insertLines(lines, start+1, []string{spec.field + " = " + value + newline}, newline), nil
		}
		updated, err := replaceScalarAssignment(lines[positions[0]], spec.field, value)
		if err != nil {
			return nil, fmt.Errorf("cannot safely update %s: %w", strings.Join(path, "."), err)
		}
		lines[positions[0]] = updated
		return lines, nil
	}
	if md.IsDefined(strings.Split(spec.table, ".")...) || md.IsDefined(path...) {
		return nil, fmt.Errorf("cannot safely update %s: use canonical [%s] table syntax", strings.Join(path, "."), spec.table)
	}
	block := []string{"[" + spec.table + "]" + newline, spec.field + " = " + value + newline, newline}
	return appendBlock(lines, block, newline), nil
}

func replaceScalarAssignment(line, key, value string) (string, error) {
	body, ending := trimLineEnding(line)
	pattern := regexp.MustCompile(`^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=[ \t]*`)
	loc := pattern.FindStringIndex(body)
	if loc == nil {
		return "", errors.New("assignment is not canonical")
	}
	oldValue := body[loc[1]:]
	if strings.HasPrefix(strings.TrimSpace(oldValue), `"""`) || strings.HasPrefix(strings.TrimSpace(oldValue), `'''`) {
		return "", errors.New("multiline values are unsupported")
	}
	suffix := ""
	if comment := unquotedCommentIndex(oldValue); comment >= 0 {
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
	return body[:loc[1]] + value + suffix + ending, nil
}

func canonicalUserSettingValue(spec userSettingSpec, value string) (string, error) {
	value = strings.TrimSpace(value)
	if spec.validate != nil {
		if err := spec.validate(value); err != nil {
			return "", err
		}
	}
	switch spec.kind {
	case userSettingString:
		if !validSingleLineTOMLString(value) {
			return "", errors.New("must be valid single-line text")
		}
		return quoteTOMLString(value), nil
	case userSettingBool:
		if value != "true" && value != "false" {
			return "", errors.New("must be true or false")
		}
		return value, nil
	case userSettingInt:
		if _, err := strconv.Atoi(value); err != nil {
			return "", errors.New("must be an integer")
		}
		return value, nil
	case userSettingFloat:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return "", errors.New("must be a number")
		}
		return value, nil
	default:
		return "", errors.New("unsupported setting type")
	}
}

func oneOf(values ...string) func(string) error {
	return func(value string) error {
		for _, candidate := range values {
			if value == candidate {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(values, ", "))
	}
}

func intRange(minimum, maximum int) func(string) error {
	return func(value string) error {
		n, err := strconv.Atoi(value)
		if err != nil || n < minimum || n > maximum {
			return fmt.Errorf("must be an integer from %d to %d", minimum, maximum)
		}
		return nil
	}
}

func floatRange(minimum, maximum float64) func(string) error {
	return func(value string) error {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < minimum || n > maximum {
			return fmt.Errorf("must be a number from %g to %g", minimum, maximum)
		}
		return nil
	}
}

// loadUserSettingsCandidate applies the would-be user file in memory, then
// decodes the same project layers Load would apply after it. No filesystem
// replacement happens until this succeeds, so a malformed project overlay or
// a security-sensitive validation error cannot leave disk and runtime split.
func loadUserSettingsCandidate(userData []byte) (*Config, error) {
	cfg := defaults()
	if len(strings.TrimSpace(string(userData))) != 0 {
		md, err := toml.Decode(string(userData), cfg)
		if err != nil {
			return nil, fmt.Errorf("validate updated user config: %w", err)
		}
		if err := validateUserSettingsMetadata(md, "updated user config"); err != nil {
			return nil, err
		}
	}
	for _, candidate := range projectConfigPaths() {
		if _, err := os.Stat(candidate.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", candidate.label, err)
		}
		md, err := toml.DecodeFile(candidate.path, cfg)
		if err != nil {
			return nil, fmt.Errorf("validate %s: %w", candidate.label, err)
		}
		if err := validateUserSettingsMetadata(md, candidate.label); err != nil {
			return nil, err
		}
	}
	if !validSandboxMode(cfg.Tools.Bash.Sandbox.Mode) {
		return nil, fmt.Errorf("invalid tools.bash.sandbox.mode %q (want off, permissions, or auto-allow)", cfg.Tools.Bash.Sandbox.Mode)
	}
	if !validSandboxNetwork(cfg.Tools.Bash.Sandbox.Network) {
		return nil, fmt.Errorf("invalid tools.bash.sandbox.network %q (want allow or block)", cfg.Tools.Bash.Sandbox.Network)
	}
	return cfg, nil
}

func validateUserSettingsMetadata(md toml.MetaData, source string) error {
	if md.IsDefined("bash") || md.IsDefined("bash", "sandbox") {
		return fmt.Errorf("validate %s: [bash.sandbox] is not a valid table; use [tools.bash.sandbox]", source)
	}
	return nil
}

func verifyUserSettingsCandidate(cfg *Config, settings []UserSetting) error {
	for _, setting := range settings {
		spec := editableUserSettings[setting.Key]
		expected, err := normalizedUserSettingValue(spec, setting.Value)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", setting.Key, err)
		}
		actual, ok := effectiveUserSettingValue(cfg, setting.Key)
		if !ok || actual != expected {
			return fmt.Errorf("refusing to write user config: %s verification failed (wanted %q, effective value is %q)", setting.Key, expected, actual)
		}
	}
	return nil
}

func normalizedUserSettingValue(spec userSettingSpec, value string) (string, error) {
	value = strings.TrimSpace(value)
	if spec.validate != nil {
		if err := spec.validate(value); err != nil {
			return "", err
		}
	}
	switch spec.kind {
	case userSettingString:
		if !validSingleLineTOMLString(value) {
			return "", errors.New("must be valid single-line text")
		}
		return value, nil
	case userSettingBool:
		if value != "true" && value != "false" {
			return "", errors.New("must be true or false")
		}
		return value, nil
	case userSettingInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return "", errors.New("must be an integer")
		}
		return strconv.Itoa(n), nil
	case userSettingFloat:
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return "", errors.New("must be a finite number")
		}
		return strconv.FormatFloat(n, 'g', -1, 64), nil
	default:
		return "", errors.New("unsupported setting type")
	}
}

func effectiveUserSettingValue(cfg *Config, key string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	switch key {
	case "permission.mode":
		return cfg.Permission.Mode, true
	case "ui.theme":
		return cfg.UI.Theme, true
	case "ui.markdown":
		return strconv.FormatBool(cfg.UI.Markdown), true
	case "ui.show_tokens":
		return strconv.FormatBool(cfg.UI.ShowTokens), true
	case "ui.show_tool_json":
		return strconv.FormatBool(cfg.UI.ShowToolJSON), true
	case "ui.thinking_display":
		return cfg.UI.ThinkingDisplay, true
	case "ui.streamlined_output":
		return strconv.FormatBool(cfg.UI.StreamlinedOutput), true
	case "ui.permission_timeout_seconds":
		return strconv.Itoa(cfg.UI.PermissionTimeoutSeconds), true
	case "session.auto_compact_threshold":
		return strconv.FormatFloat(cfg.Session.AutoCompactThreshold, 'g', -1, 64), true
	case "session.auto_compact_minimum_tokens":
		return strconv.Itoa(cfg.Session.AutoCompactMinimumTokens), true
	case "session.max_iterations":
		return strconv.Itoa(cfg.Session.MaxIterations), true
	case "loop_detection.disabled":
		return strconv.FormatBool(cfg.LoopDetection.Disabled), true
	case "ui.performance.reduced_motion":
		return strconv.FormatBool(cfg.UI.Performance.ReducedMotion), true
	case "ui.performance.mouse_wheel_lines":
		return strconv.Itoa(cfg.UI.Performance.MouseWheelLines), true
	case "ui.performance.max_mounted_items":
		return strconv.Itoa(cfg.UI.Performance.MaxMountedItems), true
	default:
		return "", false
	}
}

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
	if err := validateCustomProviderSpec(spec); err != nil {
		return err
	}
	return withUserConfigWriteLock(func(path string) error {
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
	})
}

// SaveUserProviderDefault selects a provider in the user configuration while
// leaving all unrelated user text untouched. It is used by first-run setup for
// built-in providers; API keys remain in auth.json.
func SaveUserProviderDefault(id string) error {
	if err := validateProviderID(id); err != nil {
		return err
	}
	return withUserConfigWriteLock(func(path string) error {
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
	})
}

// DeleteUserCustomProvider removes one canonical [provider.custom.<id>] table
// from the user config while preserving every unrelated byte range. The
// active default must be changed first so deletion cannot silently select a
// different endpoint. Credentials are intentionally handled by internal/auth.
func DeleteUserCustomProvider(id string) error {
	if err := validateProviderID(id); err != nil {
		return err
	}
	switch id {
	case "anthropic", "openai", "gemini", "google", "custom":
		return fmt.Errorf("provider %q is built in and cannot be deleted", id)
	}
	return withUserConfigWriteLock(func(path string) error {
		if err := rejectConfigSymlink(path); err != nil {
			return err
		}
		original, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read user config: %w", err)
		}
		var probe userConfigProbe
		if _, err := toml.Decode(string(original), &probe); err != nil {
			return fmt.Errorf("parse user config: %w", err)
		}
		if _, ok := probe.Provider.Custom[id]; !ok {
			return nil
		}
		if probe.Provider.Default == id {
			return fmt.Errorf("custom provider %q is the default; select another default before deleting it", id)
		}
		lines := splitLines(string(original))
		start, end, found := findCanonicalTable(lines, "provider.custom."+id)
		if !found {
			return fmt.Errorf("cannot safely delete custom provider %q: use canonical [provider.custom.%s] table syntax", id, id)
		}
		updatedLines := append(append([]string(nil), lines[:start]...), lines[end:]...)
		updated := []byte(strings.Join(updatedLines, ""))
		var verified userConfigProbe
		if _, err := toml.Decode(string(updated), &verified); err != nil {
			return fmt.Errorf("refusing to write invalid updated user config: %w", err)
		}
		if _, stillPresent := verified.Provider.Custom[id]; stillPresent {
			return errors.New("refusing to write user config: custom provider deletion verification failed")
		}
		if err := atomicWritePrivateFile(path, updated); err != nil {
			return fmt.Errorf("save user config: %w", err)
		}
		return nil
	})
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
	structural := tomlStructuralLines(lines)
	for i, line := range lines {
		if !structural[i] {
			continue
		}
		header, ok := canonicalTableHeader(line)
		if !ok || header != name {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if structural[j] && isAnyTableHeader(lines[j]) {
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
	structural := tomlStructuralLines(lines)
	for i, line := range lines {
		if !structural[i] {
			continue
		}
		name, ok := canonicalTableHeader(line)
		if ok && strings.HasPrefix(name, "provider.") {
			return i
		}
	}
	return -1
}

func providerDefinedWithoutTable(lines []string) bool {
	structural := tomlStructuralLines(lines)
	for i, line := range lines {
		if !structural[i] {
			continue
		}
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
	structural := tomlStructuralLines(lines)
	var positions []int
	for i := start; i < end; i++ {
		if !structural[i] {
			continue
		}
		body, _ := trimLineEnding(lines[i])
		if pattern.MatchString(body) {
			positions = append(positions, i)
		}
	}
	return positions
}

type tomlMultilineKind uint8

const (
	tomlMultilineNone tomlMultilineKind = iota
	tomlMultilineBasic
	tomlMultilineLiteral
)

// tomlStructuralLines marks lines whose first byte is outside a TOML
// multiline string. The text-preserving writer intentionally supports only
// canonical table headers and direct scalar assignments; without this small
// lexical guard, a multiline documentation string containing lines such as
// "[ui]" and "markdown = true" could be mistaken for real configuration.
// Lines that begin inside a multiline string stay non-structural even if the
// closing delimiter occurs later on that line (valid TOML permits only
// whitespace/comments after it, so there is no useful assignment to recover).
func tomlStructuralLines(lines []string) []bool {
	mask := make([]bool, len(lines))
	mode := tomlMultilineNone
	for i, line := range lines {
		mask[i] = mode == tomlMultilineNone
		body, _ := trimLineEnding(line)
		mode = scanTOMLMultilineState(body, mode)
	}
	return mask
}

func scanTOMLMultilineState(line string, mode tomlMultilineKind) tomlMultilineKind {
	for i := 0; i < len(line); {
		switch mode {
		case tomlMultilineBasic:
			if strings.HasPrefix(line[i:], `"""`) && !escapedAt(line, i) {
				mode = tomlMultilineNone
				i += 3
				continue
			}
			i++
			continue
		case tomlMultilineLiteral:
			if strings.HasPrefix(line[i:], `'''`) {
				mode = tomlMultilineNone
				i += 3
				continue
			}
			i++
			continue
		}

		switch line[i] {
		case '#':
			return mode
		case '"':
			if strings.HasPrefix(line[i:], `"""`) {
				mode = tomlMultilineBasic
				i += 3
				continue
			}
			i = skipTOMLBasicString(line, i+1)
		case '\'':
			if strings.HasPrefix(line[i:], `'''`) {
				mode = tomlMultilineLiteral
				i += 3
				continue
			}
			if end := strings.IndexByte(line[i+1:], '\''); end >= 0 {
				i += end + 2
			} else {
				return mode
			}
		default:
			i++
		}
	}
	return mode
}

func skipTOMLBasicString(line string, start int) int {
	escaped := false
	for i := start; i < len(line); i++ {
		switch {
		case escaped:
			escaped = false
		case line[i] == '\\':
			escaped = true
		case line[i] == '"':
			return i + 1
		}
	}
	return len(line)
}

func escapedAt(line string, at int) bool {
	backslashes := 0
	for i := at - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
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
