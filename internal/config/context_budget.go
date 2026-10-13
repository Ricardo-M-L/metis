package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	contextBudgetFilename = "context-budgets.json"
	contextBudgetSchema   = 1
	maxContextBudget      = 100_000_000
	maxContextBudgetBytes = 4 << 20
)

// ContextBudgetRoute identifies the generation route, independently of model
// aliases and catalog issuer names. AccountScope is a stable OAuth account id,
// never an access/refresh token. Canonicalization replaces it with a digest.
type ContextBudgetRoute struct {
	ProviderID   string `json:"providerId"`
	Transport    string `json:"transport"`
	BaseURL      string `json:"baseUrl"`
	AccountScope string `json:"accountScope,omitempty"`
}

// ContextBudgetRecord persists an exact model choice. Window zero is explicit
// automatic selection, and therefore overrides an older provider-wide value.
type ContextBudgetRecord struct {
	Route     ContextBudgetRoute `json:"route"`
	Model     string             `json:"model"`
	Window    int                `json:"window"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

type contextBudgetFile struct {
	Schema  int                            `json:"schema"`
	Records map[string]ContextBudgetRecord `json:"records"`
}

// CanonicalContextBudgetRoute returns a secret-free identity that callers may
// expose to settings. URLs containing credentials, query parameters, encoded
// separators or traversal are rejected instead of being silently rewritten.
func CanonicalContextBudgetRoute(route ContextBudgetRoute) (ContextBudgetRoute, error) {
	if !validContextBudgetLabel(route.ProviderID, 256) || !validContextBudgetLabel(route.Transport, 256) {
		return ContextBudgetRoute{}, errors.New("context budget: invalid provider route")
	}
	route.Transport = strings.ToLower(route.Transport)
	switch route.Transport {
	case "anthropic":
		route.Transport = "anthropic_messages"
	case "openai":
		route.Transport = "openai_chat"
	case "gemini":
		route.Transport = "gemini_native"
	case "vertex":
		route.Transport = "vertex_anthropic"
	case "bedrock":
		route.Transport = "bedrock_anthropic"
	}
	u, err := url.Parse(route.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") || !validContextBudgetLabel(u.Host, 512) {
		return ContextBudgetRoute{}, errors.New("context budget: invalid endpoint")
	}
	if u.RawPath != "" || strings.Contains(u.Path, "//") || strings.ContainsAny(u.Path, "\\") || strings.ContainsFunc(u.Path, unicode.IsControl) {
		return ContextBudgetRoute{}, errors.New("context budget: invalid endpoint path")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return ContextBudgetRoute{}, errors.New("context budget: invalid endpoint path")
		}
	}
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path = strings.TrimRight(path.Clean("/"+strings.TrimPrefix(u.Path, "/")), "/")
	route.BaseURL = u.String()
	if route.AccountScope != "" {
		if !validContextBudgetLabel(route.AccountScope, 4096) {
			return ContextBudgetRoute{}, errors.New("context budget: invalid account scope")
		}
		if !isContextBudgetScopeDigest(route.AccountScope) {
			digest := sha256.Sum256([]byte(route.AccountScope))
			route.AccountScope = "sha256:" + hex.EncodeToString(digest[:])
		}
	}
	return route, nil
}

func validContextBudgetLabel(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func isContextBudgetScopeDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil && value == strings.ToLower(value)
}

// ContextBudgetRouteKey is shared by runtime binding and the settings API.
// The exact model id participates in the digest; aliases are never borrowed.
func ContextBudgetRouteKey(route ContextBudgetRoute, model string) (string, error) {
	route, err := CanonicalContextBudgetRoute(route)
	if err != nil {
		return "", err
	}
	if !validContextBudgetLabel(model, 1024) {
		return "", errors.New("context budget: invalid model id")
	}
	identity, _ := json.Marshal(struct {
		Route ContextBudgetRoute `json:"route"`
		Model string             `json:"model"`
	}{route, model})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:]), nil
}

func SetContextBudget(route ContextBudgetRoute, model string, window int) error {
	if window < 0 || window > maxContextBudget {
		return errors.New("context budget: window is outside the supported range")
	}
	route, err := CanonicalContextBudgetRoute(route)
	if err != nil {
		return err
	}
	key, err := ContextBudgetRouteKey(route, model)
	if err != nil {
		return err
	}
	// Share the config transaction lock: Desktop and CLI can update different
	// models without losing another process's records or config changes.
	return withUserConfigWriteLock(func(configPath string) error {
		filename := filepath.Join(filepath.Dir(configPath), contextBudgetFilename)
		store, err := readContextBudgetFile(filename)
		if err != nil {
			return err
		}
		store.Records[key] = ContextBudgetRecord{Route: route, Model: model, Window: window, UpdatedAt: time.Now().UTC()}
		data, err := json.MarshalIndent(store, "", "  ")
		if err != nil {
			return fmt.Errorf("context budget: encode: %w", err)
		}
		if len(data) > maxContextBudgetBytes {
			return errors.New("context budget: store exceeds size limit")
		}
		return atomicWritePrivateFile(filename, append(data, '\n'))
	})
}

func LookupContextBudget(route ContextBudgetRoute, model string) (int, bool, error) {
	key, err := ContextBudgetRouteKey(route, model)
	if err != nil {
		return 0, false, err
	}
	var record ContextBudgetRecord
	var found bool
	// Readers share the transaction lock so a legitimate atomic rename cannot
	// appear as a replaced-file validation failure at a request boundary.
	err = withUserConfigWriteLock(func(configPath string) error {
		store, err := readContextBudgetFile(filepath.Join(filepath.Dir(configPath), contextBudgetFilename))
		if err == nil {
			record, found = store.Records[key]
		}
		return err
	})
	return record.Window, found, err
}

func ListContextBudgets() ([]ContextBudgetRecord, error) {
	var store contextBudgetFile
	err := withUserConfigWriteLock(func(configPath string) error {
		var err error
		store, err = readContextBudgetFile(filepath.Join(filepath.Dir(configPath), contextBudgetFilename))
		return err
	})
	if err != nil {
		return nil, err
	}
	result := make([]ContextBudgetRecord, 0, len(store.Records))
	for _, record := range store.Records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		left, _ := ContextBudgetRouteKey(result[i].Route, result[i].Model)
		right, _ := ContextBudgetRouteKey(result[j].Route, result[j].Model)
		return left < right
	})
	return result, nil
}

func readContextBudgetFile(filename string) (contextBudgetFile, error) {
	empty := contextBudgetFile{Schema: contextBudgetSchema, Records: make(map[string]ContextBudgetRecord)}
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("context budget: inspect store: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxContextBudgetBytes {
		return empty, errors.New("context budget: refusing non-regular or oversized store")
	}
	file, err := os.Open(filename)
	if err != nil {
		return empty, fmt.Errorf("context budget: open store: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return empty, errors.New("context budget: store changed while reading")
	}
	var store contextBudgetFile
	decoder := json.NewDecoder(io.LimitReader(file, maxContextBudgetBytes+1))
	if err := decoder.Decode(&store); err != nil || store.Schema != contextBudgetSchema || store.Records == nil {
		return empty, errors.New("context budget: invalid store")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return empty, errors.New("context budget: trailing store data")
	}
	for key, record := range store.Records {
		canonical, err := CanonicalContextBudgetRoute(record.Route)
		if err != nil || canonical != record.Route || record.Window < 0 || record.Window > maxContextBudget || record.UpdatedAt.IsZero() {
			return empty, errors.New("context budget: invalid record")
		}
		expected, err := ContextBudgetRouteKey(record.Route, record.Model)
		if err != nil || expected != key {
			return empty, errors.New("context budget: record identity mismatch")
		}
	}
	return store, nil
}
