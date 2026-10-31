// Package pluginmanifest validates the capabilities Metis can actually load.
// Reading a manifest never launches a subprocess or contacts a remote service.
package pluginmanifest

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

// Read decodes and checks one installed or staged plugin, including all local
// resources, before any consumer publishes its capabilities or starts an MCP
// server. Unknown fields are errors rather than silently accepted extensions.
func Read(rootDir, expectedName string) (*pubplugin.Manifest, error) {
	if !safeName(expectedName) {
		return nil, errors.New("invalid plugin name")
	}
	manifestPath, err := containedPath(rootDir, "plugin.toml", false)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, errors.New("manifest is unreadable")
	}
	var manifest pubplugin.Manifest
	metadata, err := toml.Decode(string(data), &manifest)
	if err != nil {
		// Decoder diagnostics can contain values from env/headers/args. Only
		// report the failure category; never echo configuration values.
		return nil, errors.New("decode manifest: invalid TOML or field type")
	}
	if unknown := metadata.Undecoded(); len(unknown) > 0 {
		fields := make([]string, 0, len(unknown))
		for _, key := range unknown {
			fields = append(fields, key.String())
		}
		sort.Strings(fields)
		return nil, fmt.Errorf("unsupported manifest field(s): %s; this build supports MCP tools and Skills", strings.Join(fields, ", "))
	}
	if err := validateStringMapTypes(data); err != nil {
		return nil, err
	}
	if err := Validate(&manifest, expectedName); err != nil {
		return nil, err
	}
	for _, relative := range manifest.Skills {
		path, err := containedPath(rootDir, relative, false)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", relative, err)
		}
		loaded, err := skills.Load(path)
		if err != nil || loaded == nil || strings.TrimSpace(loaded.Name) == "" {
			return nil, fmt.Errorf("skill %q: unreadable or invalid skill file", relative)
		}
	}
	servers := append([]pubplugin.MCPServerSpec(nil), manifest.MCPServers...)
	if manifest.MCPServer != nil {
		servers = append(servers, *manifest.MCPServer)
	}
	for _, server := range servers {
		if server.Disabled {
			continue
		}
		directory := server.WorkingDir
		if directory == "" {
			directory = "."
		}
		if _, err := containedPath(rootDir, directory, true); err != nil {
			return nil, fmt.Errorf("mcp working_dir: %w", err)
		}
	}
	return &manifest, nil
}

// The TOML decoder currently accepts a scalar assigned to a map field and
// drops it. Inspect the original value shapes as well, so a misshapen env or
// headers declaration cannot lose credentials while still passing validation.
func validateStringMapTypes(data []byte) error {
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return errors.New("decode manifest: invalid TOML or field type")
	}
	check := func(server map[string]any) error {
		for field, value := range server {
			if !strings.EqualFold(field, "env") && !strings.EqualFold(field, "headers") {
				continue
			}
			values, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("mcp %s must be a table of string values", field)
			}
			for _, item := range values {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("mcp %s must be a table of string values", field)
				}
			}
		}
		return nil
	}
	for field, value := range raw {
		switch {
		case strings.EqualFold(field, "mcp_server"):
			if server, ok := value.(map[string]any); ok {
				if err := check(server); err != nil {
					return err
				}
			}
		case strings.EqualFold(field, "mcp_servers"):
			if servers, ok := value.([]map[string]any); ok {
				for _, server := range servers {
					if err := check(server); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// SkillPath resolves a declared skill without allowing symlink or path escape.
// Runtime consumers recheck immediately before reading so a changed plugin
// cannot bypass the same boundary that was checked during installation.
func SkillPath(rootDir, relative string) (string, error) {
	return containedPath(rootDir, relative, false)
}

// WorkingDir resolves an existing directory inside the plugin root.
func WorkingDir(rootDir, declared string) (string, error) {
	if declared == "" {
		declared = "."
	}
	return containedPath(rootDir, declared, true)
}

// Validate checks capability declarations without reading files. Version zero
// retains the v1 compatibility behavior. Hooks remain in the public schema for
// compatibility, but are rejected until the runtime implements their contract.
func Validate(manifest *pubplugin.Manifest, expectedName string) error {
	if manifest == nil {
		return errors.New("manifest required")
	}
	if manifest.ManifestVersion == 0 {
		manifest.ManifestVersion = pubplugin.CurrentManifestVersion
	}
	if manifest.ManifestVersion != pubplugin.CurrentManifestVersion {
		return fmt.Errorf("manifest_version=%d not supported (this build understands v%d)", manifest.ManifestVersion, pubplugin.CurrentManifestVersion)
	}
	if !safeName(manifest.Name) {
		return errors.New("manifest name must use letters, digits, dots, underscores, or hyphens")
	}
	if manifest.Name != expectedName {
		return fmt.Errorf("manifest name %q must match plugin dir %q", manifest.Name, expectedName)
	}
	if len(manifest.Hooks) > 0 {
		return errors.New("hooks are not supported by this build; remove hooks declarations before installing or loading the plugin")
	}
	seen := make(map[string]bool)
	activeServers := 0
	checkServer := func(server pubplugin.MCPServerSpec, name, label string) error {
		if !safeName(name) {
			return fmt.Errorf("%s.name must use letters, digits, dots, underscores, or hyphens", label)
		}
		if seen[name] {
			return fmt.Errorf("duplicate mcp server name %q", name)
		}
		seen[name] = true
		if err := validateServer(server, label); err != nil {
			return err
		}
		if !server.Disabled {
			activeServers++
		}
		return nil
	}
	if manifest.MCPServer != nil {
		name := manifest.MCPServer.Name
		if name == "" {
			name = "default"
		}
		if err := checkServer(*manifest.MCPServer, name, "[mcp_server]"); err != nil {
			return err
		}
	}
	for i, server := range manifest.MCPServers {
		if err := checkServer(server, server.Name, fmt.Sprintf("[[mcp_servers]][%d]", i)); err != nil {
			return err
		}
	}
	for _, relative := range manifest.Skills {
		if _, err := relativePath(relative); err != nil {
			return fmt.Errorf("skill %q: %w", relative, err)
		}
	}
	if activeServers == 0 && len(manifest.Skills) == 0 {
		return errors.New("plugin declares no active supported capability; add an enabled MCP server or Skill file")
	}
	return nil
}

func validateServer(server pubplugin.MCPServerSpec, label string) error {
	hasCommand, hasURL := strings.TrimSpace(server.Command) != "", strings.TrimSpace(server.URL) != ""
	if hasCommand == hasURL {
		return fmt.Errorf("%s requires exactly one command or url", label)
	}
	maskedCommand, _, err := maskEnvironmentTemplates(server.Command)
	if err != nil {
		return fmt.Errorf("%s.command contains an invalid environment template", label)
	}
	if strings.ContainsAny(maskedCommand, ";&|`$<>\n\r\x00") {
		return fmt.Errorf("%s.command contains unsupported shell metacharacters", label)
	}
	if hasURL {
		_, templated, templateErr := maskEnvironmentTemplates(server.URL)
		if templateErr != nil {
			return fmt.Errorf("%s.url contains an invalid environment template", label)
		}
		// A leading template may supply the entire URL, including its
		// scheme. Resolve values only at launch, through the existing MCP
		// expansion boundary, rather than reading secrets during validation.
		deferredURL := templated && strings.HasPrefix(server.URL, "${")
		if !deferredURL {
			// Numeric placeholders are legal in both host and port positions.
			// A hostname placeholder would reject valid ${PORT} templates.
			maskedURL := environmentTemplate.ReplaceAllString(server.URL, "443")
			parsed, err := url.Parse(maskedURL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return fmt.Errorf("%s.url must be an absolute HTTP or HTTPS URL", label)
			}
		}
	}
	if server.WorkingDir != "" {
		if _, err := relativePath(server.WorkingDir); err != nil {
			return fmt.Errorf("%s.working_dir: %w", label, err)
		}
	}
	return nil
}

var environmentTemplate = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*(?::-([^}]*))?\}`)

func maskEnvironmentTemplates(value string) (string, bool, error) {
	masked := environmentTemplate.ReplaceAllString(value, "metis-template.invalid")
	if strings.Contains(masked, "${") {
		return "", false, errors.New("invalid environment template")
	}
	return masked, masked != value, nil
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func relativePath(relative string) (string, error) {
	// Treat both separators consistently, including manifests inspected on a
	// different platform from the one where they were authored.
	portable := strings.ReplaceAll(relative, `\`, "/")
	if strings.TrimSpace(portable) == "" || strings.HasPrefix(portable, "/") || (len(portable) > 1 && portable[1] == ':') {
		return "", errors.New("path must be relative to the plugin root")
	}
	clean := filepath.Clean(filepath.FromSlash(portable))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes plugin root")
	}
	return clean, nil
}

func containedPath(root, relative string, directory bool) (string, error) {
	clean, err := relativePath(relative)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("plugin root is unavailable")
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return "", errors.New("plugin root is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, clean))
	if err != nil {
		return "", errors.New("declared path is unavailable")
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes plugin root")
	}
	info, err := os.Stat(resolved)
	if err != nil || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return "", errors.New("declared path has the wrong file type")
	}
	return resolved, nil
}
