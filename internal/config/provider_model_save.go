package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

// ProviderModelOverrideSource identifies a project-level model assignment that
// would shadow a model selected in Desktop's user-level provider settings.
func ProviderModelOverrideSource(id string) (string, error) {
	if err := validateProviderID(id); err != nil {
		return "", err
	}
	if builtInModelProvider(id) {
		return providerOverrideSource([]string{"provider", id, "model"})
	}
	return providerOverrideSource([]string{"provider", "custom", id, "model"})
}

func builtInModelProvider(id string) bool {
	switch id {
	case "anthropic", "openai", "openai-codex", "gemini":
		return true
	default:
		return false
	}
}

// SaveUserProviderModel changes only one model field. In particular it does
// not change provider.default, unlike the first-run custom-provider wizard.
// It preserves the surrounding user TOML and never reads or writes secrets.
func SaveUserProviderModel(id, model string) error {
	if err := validateProviderID(id); err != nil {
		return err
	}
	if model == "" || !validSingleLineTOMLString(model) || strings.IndexFunc(model, unicode.IsSpace) >= 0 {
		return errors.New("provider model must be non-empty single-line text without whitespace")
	}
	return withUserConfigWriteLock(func(path string) error {
		if err := rejectConfigSymlink(path); err != nil {
			return err
		}
		original, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read user config: %w", err)
		}
		var probe userConfigProbe
		var md toml.MetaData
		if len(strings.TrimSpace(string(original))) != 0 {
			md, err = toml.Decode(string(original), &probe)
			if err != nil {
				return fmt.Errorf("parse user config: %w", err)
			}
		}
		table := "provider." + id
		semantic := []string{"provider", id, "model"}
		if !builtInModelProvider(id) {
			table = "provider.custom." + id
			semantic = []string{"provider", "custom", id, "model"}
			if _, exists := probe.Provider.Custom[id]; !exists {
				return fmt.Errorf("custom provider %q is not in the user config", id)
			}
		}
		newline := "\n"
		if strings.Contains(string(original), "\r\n") {
			newline = "\r\n"
		}
		lines := splitLines(string(original))
		start, end, found := findCanonicalTable(lines, table)
		if !found {
			if !builtInModelProvider(id) || md.IsDefined(semantic...) {
				return fmt.Errorf("cannot safely update %s.model: use a canonical [%s] table", table, table)
			}
			lines = appendBlock(lines, []string{"[" + table + "]" + newline, "model = " + quoteTOMLString(model) + newline}, newline)
		} else {
			lines, err = setTableString(lines, start, end, "model", model, md.IsDefined(semantic...), table+".model", newline)
			if err != nil {
				return err
			}
		}
		updated := []byte(strings.Join(lines, ""))
		var verified map[string]any
		if _, err := toml.Decode(string(updated), &verified); err != nil {
			return fmt.Errorf("refusing to write invalid updated user config: %w", err)
		}
		current, ok := verified["provider"].(map[string]any)
		if !ok {
			return errors.New("provider model update verification failed")
		}
		keys := semantic[1:]
		var value any = current
		for _, key := range keys {
			fields, ok := value.(map[string]any)
			if !ok {
				return errors.New("provider model update verification failed")
			}
			value = fields[key]
		}
		if value != model {
			return errors.New("provider model update verification failed")
		}
		if err := atomicWritePrivateFile(path, updated); err != nil {
			return fmt.Errorf("save user config: %w", err)
		}
		return nil
	})
}
