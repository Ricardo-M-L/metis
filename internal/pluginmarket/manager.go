// Package pluginmarket provides the shared, side-effect-aware plugin catalog
// used by METIS Desktop. Reading the catalog never starts plugin subprocesses.
// Network sync and installation only happen through explicit methods.
package pluginmarket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

var (
	ErrInvalidName      = errors.New("invalid plugin or marketplace name")
	ErrNotFound         = errors.New("plugin not found")
	ErrAlreadyInstalled = errors.New("plugin already installed")
	ErrNotInstallable   = errors.New("plugin source is not installable")
)

const (
	maxCopiedFiles = 20_000
	maxCopiedBytes = int64(512 << 20)
)

type Marketplace struct {
	Name             string `json:"name"`
	DisplayName      string `json:"displayName"`
	Description      string `json:"description,omitempty"`
	Ecosystem        string `json:"ecosystem,omitempty"`
	Source           string `json:"source"`
	Repo             string `json:"repo,omitempty"`
	Builtin          bool   `json:"builtin"`
	Local            bool   `json:"local"`
	Synced           bool   `json:"synced"`
	PluginCount      int    `json:"pluginCount"`
	InstallableCount int    `json:"installableCount"`
	UpdatedAt        string `json:"updatedAt,omitempty"`
	Error            string `json:"error,omitempty"`
}

// Ecosystem describes a compatibility contract, not a catalog source. Codex
// and DeepSeek Harness are intentionally modeled here instead of being
// presented as ordinary marketplaces: their package formats, runtime
// components, and lifecycle semantics are the feature being integrated.
type Ecosystem struct {
	ID           string               `json:"id"`
	DisplayName  string               `json:"displayName"`
	Description  string               `json:"description"`
	Mode         string               `json:"mode"`
	Status       string               `json:"status"`
	PackageCount int                  `json:"packageCount"`
	Components   []EcosystemComponent `json:"components"`
}

type EcosystemComponent struct {
	Kind    string `json:"kind"`
	Support string `json:"support"`
	Detail  string `json:"detail"`
}

type PluginComponent struct {
	Kind    string `json:"kind"`
	Support string `json:"support"`
	Detail  string `json:"detail,omitempty"`
}

type Plugin struct {
	Name          string            `json:"name"`
	DisplayName   string            `json:"displayName,omitempty"`
	Version       string            `json:"version,omitempty"`
	Description   string            `json:"description,omitempty"`
	Developer     string            `json:"developer,omitempty"`
	Category      string            `json:"category,omitempty"`
	Keywords      []string          `json:"keywords,omitempty"`
	Capabilities  []string          `json:"capabilities,omitempty"`
	Marketplace   string            `json:"marketplace"`
	Homepage      string            `json:"homepage,omitempty"`
	Skills        []string          `json:"skills,omitempty"`
	Icon          string            `json:"icon,omitempty"`
	BrandColor    string            `json:"brandColor,omitempty"`
	Ecosystem     string            `json:"ecosystem,omitempty"`
	PackageName   string            `json:"packageName,omitempty"`
	Components    []PluginComponent `json:"components,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	SourceKind    string            `json:"sourceKind"`
	Installable   bool              `json:"installable"`
	Installed     bool              `json:"installed"`
	Unavailable   string            `json:"unavailableReason,omitempty"`
}

type Catalog struct {
	Ecosystems   []Ecosystem   `json:"ecosystems"`
	Marketplaces []Marketplace `json:"marketplaces"`
	Plugins      []Plugin      `json:"plugins"`
	NeedsSync    bool          `json:"needsSync"`
}

type SyncResult struct {
	Marketplace string `json:"marketplace"`
	Updated     bool   `json:"updated"`
	Error       string `json:"error,omitempty"`
}

type InstallResult struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Restart bool   `json:"restartRequired"`
	Source  string `json:"marketplace"`
}

type RemoveResult struct {
	Name          string `json:"name"`
	RecoverableAt string `json:"recoverableAt"`
	Restart       bool   `json:"restartRequired"`
}

type Manager struct {
	syncMu sync.Mutex
	now    func() time.Time
	runGit func(context.Context, ...string) error
}

func NewManager() *Manager {
	return &Manager{
		now: time.Now,
		runGit: func(ctx context.Context, args ...string) error {
			cmd := exec.CommandContext(ctx, "git", args...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				message := strings.TrimSpace(string(output))
				if message == "" {
					message = err.Error()
				}
				return errors.New(message)
			}
			return nil
		},
	}
}

type marketplaceManifest struct {
	Name      string `json:"name"`
	Interface struct {
		DisplayName string `json:"displayName"`
	} `json:"interface,omitempty"`
	Plugins []marketplacePluginEntry `json:"plugins"`
}

type marketplacePluginEntry struct {
	Name          string                    `json:"name"`
	Description   string                    `json:"description"`
	Source        json.RawMessage           `json:"source"`
	Skills        []string                  `json:"skills,omitempty"`
	Homepage      string                    `json:"homepage,omitempty"`
	Category      string                    `json:"category,omitempty"`
	Keywords      []string                  `json:"keywords,omitempty"`
	Version       string                    `json:"version,omitempty"`
	DisplayName   string                    `json:"-"`
	Developer     string                    `json:"-"`
	Capabilities  []string                  `json:"-"`
	Icon          string                    `json:"-"`
	BrandColor    string                    `json:"-"`
	Compatibility string                    `json:"-"`
	Ecosystem     string                    `json:"-"`
	PackageName   string                    `json:"-"`
	Components    []PluginComponent         `json:"-"`
	MCPServers    []pubplugin.MCPServerSpec `json:"-"`
}

type pluginSource struct {
	Path string
	URL  string
	Kind string
	Ref  string
	SHA  string
}

type codexPluginManifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Homepage    string          `json:"homepage"`
	Keywords    []string        `json:"keywords"`
	Skills      json.RawMessage `json:"skills"`
	Apps        json.RawMessage `json:"apps"`
	MCPServers  json.RawMessage `json:"mcpServers"`
	Hooks       json.RawMessage `json:"hooks"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
	Interface struct {
		DisplayName      string   `json:"displayName"`
		ShortDescription string   `json:"shortDescription"`
		LongDescription  string   `json:"longDescription"`
		DeveloperName    string   `json:"developerName"`
		Category         string   `json:"category"`
		Capabilities     []string `json:"capabilities"`
		Logo             string   `json:"logo"`
		ComposerIcon     string   `json:"composerIcon"`
		BrandColor       string   `json:"brandColor"`
		WebsiteURL       string   `json:"websiteURL"`
	} `json:"interface"`
}

type localCatalogItem struct {
	entry      marketplacePluginEntry
	sourcePath string
	modified   time.Time
	origin     string
}

func (e marketplacePluginEntry) resolveSource() pluginSource {
	if len(e.Source) == 0 {
		return pluginSource{Path: ".", Kind: "path"}
	}
	var path string
	if err := json.Unmarshal(e.Source, &path); err == nil {
		if strings.TrimSpace(path) == "" {
			path = "."
		}
		return pluginSource{Path: path, Kind: "path"}
	}
	var object struct {
		Source string `json:"source"`
		URL    string `json:"url"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	}
	if err := json.Unmarshal(e.Source, &object); err == nil {
		kind := object.Source
		if kind == "local" {
			kind = "path"
		}
		return pluginSource{Path: object.Path, URL: object.URL, Kind: kind, Ref: object.Ref, SHA: object.SHA}
	}
	return pluginSource{}
}

func catalogPlugin(marketplace string, item marketplacePluginEntry, sourcePath, sourceKind string, installed map[string]bool) Plugin {
	item = enrichPluginMetadata(sourcePath, item)
	item.Skills = discoverSkillFiles(sourcePath, item.resolveSource().Path, item.Skills)
	hasNativeManifest := regularFile(filepath.Join(sourcePath, "plugin.toml"))
	installable := hasNativeManifest || len(item.Skills) > 0 || len(item.MCPServers) > 0
	compatibility := "catalog"
	reason := "This plugin requires a runtime that METIS does not provide yet"
	if item.Compatibility == "external" {
		compatibility = "external"
		reason = "This package uses Cordis services or client slots and stays in the DeepSeek Harness runtime"
	}
	if hasNativeManifest {
		compatibility = "native"
		reason = ""
	} else if len(item.Skills) > 0 || len(item.MCPServers) > 0 {
		compatibility = "skills"
		if len(item.MCPServers) > 0 {
			compatibility = "translated"
		}
		reason = ""
		if item.Compatibility == "partial" || skillsRequireCodexRuntime(sourcePath, item.Skills) {
			compatibility = "partial"
		}
	}
	icon := ""
	if item.Icon != "" {
		icon = "/api/plugins/icon?marketplace=" + neturl.QueryEscape(marketplace) + "&plugin=" + neturl.QueryEscape(item.Name)
	}
	displayName := item.DisplayName
	if displayName == "" {
		displayName = item.Name
	}
	return Plugin{
		Name: item.Name, DisplayName: displayName, Version: item.Version,
		Description: item.Description, Developer: item.Developer, Category: item.Category,
		Keywords: append([]string(nil), item.Keywords...), Capabilities: append([]string(nil), item.Capabilities...),
		Marketplace: marketplace, Homepage: item.Homepage, Skills: append([]string(nil), item.Skills...),
		Icon: icon, BrandColor: item.BrandColor, Ecosystem: item.Ecosystem,
		PackageName: item.PackageName, Components: append([]PluginComponent(nil), item.Components...), Compatibility: compatibility,
		SourceKind: sourceKind, Installable: installable, Installed: installed[item.Name], Unavailable: reason,
	}
}

func enrichPluginMetadata(sourcePath string, item marketplacePluginEntry) marketplacePluginEntry {
	manifestPath := filepath.Join(sourcePath, ".codex-plugin", "plugin.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		// Claude plugins use a smaller manifest. Parse the overlapping fields so
		// version and author still make it into the shared card model.
		manifestPath = filepath.Join(sourcePath, ".claude-plugin", "plugin.json")
		data, err = os.ReadFile(manifestPath)
	}
	if err != nil {
		return item
	}
	var manifest codexPluginManifest
	if json.Unmarshal(data, &manifest) != nil {
		return item
	}
	if manifest.Version != "" {
		item.Version = manifest.Version
	}
	if manifest.Description != "" {
		item.Description = manifest.Description
	}
	if manifest.Interface.ShortDescription != "" {
		item.Description = manifest.Interface.ShortDescription
	}
	if manifest.Homepage != "" {
		item.Homepage = manifest.Homepage
	}
	if manifest.Interface.WebsiteURL != "" {
		item.Homepage = manifest.Interface.WebsiteURL
	}
	item.Keywords = mergeStrings(item.Keywords, manifest.Keywords)
	declaredSkills := rawStringList(manifest.Skills)
	item.Skills = mergeStrings(item.Skills, declaredSkills)
	item.DisplayName = manifest.Interface.DisplayName
	item.Developer = manifest.Interface.DeveloperName
	if item.Developer == "" {
		item.Developer = manifest.Author.Name
	}
	if manifest.Interface.Category != "" {
		item.Category = manifest.Interface.Category
	}
	item.Capabilities = append([]string(nil), manifest.Interface.Capabilities...)
	if len(declaredSkills) > 0 {
		item.Capabilities = mergeStrings(item.Capabilities, []string{"Skills"})
		item.Components = append(item.Components, PluginComponent{Kind: "skills", Support: "native", Detail: "Loaded through the METIS skill runtime"})
	}
	if rawPresent(manifest.Apps) {
		item.Capabilities = mergeStrings(item.Capabilities, []string{"Codex app"})
		item.Components = append(item.Components, PluginComponent{Kind: "apps", Support: "external", Detail: "Requires the Codex connector runtime and account authorization"})
		item.Compatibility = "partial"
	}
	if rawPresent(manifest.MCPServers) {
		item.Capabilities = mergeStrings(item.Capabilities, []string{"MCP"})
		servers, mcpErr := readCodexMCPServers(sourcePath, manifest.MCPServers)
		if mcpErr == nil && len(servers) > 0 {
			item.MCPServers = servers
			item.Components = append(item.Components, PluginComponent{Kind: "mcp", Support: "translated", Detail: fmt.Sprintf("%d MCP server(s) mapped to the METIS lifecycle", len(servers))})
		} else {
			item.Components = append(item.Components, PluginComponent{Kind: "mcp", Support: "unavailable", Detail: "The MCP declaration could not be translated safely"})
			item.Compatibility = "partial"
		}
	}
	if rawPresent(manifest.Hooks) {
		item.Capabilities = mergeStrings(item.Capabilities, []string{"Hooks"})
		item.Components = append(item.Components, PluginComponent{Kind: "hooks", Support: "external", Detail: "Codex hook semantics are not executed by the METIS runtime"})
		item.Compatibility = "partial"
	}
	item.Icon = manifest.Interface.Logo
	if item.Icon == "" {
		item.Icon = manifest.Interface.ComposerIcon
	}
	item.BrandColor = manifest.Interface.BrandColor
	return item
}

type codexMCPDocument struct {
	MCPServers map[string]codexMCPServer `json:"mcpServers"`
}

type codexMCPServer struct {
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Env           map[string]string `json:"env"`
	Cwd           string            `json:"cwd"`
	WorkingDir    string            `json:"working_dir"`
	Enabled       *bool             `json:"enabled"`
	Disabled      bool              `json:"disabled"`
	EnabledTools  []string          `json:"enabled_tools"`
	DisabledTools []string          `json:"disabled_tools"`
}

func readCodexMCPServers(root string, declaration json.RawMessage) ([]pubplugin.MCPServerSpec, error) {
	data := declaration
	var relative string
	if json.Unmarshal(declaration, &relative) == nil {
		path, err := secureRegularPath(root, relative, 1<<20)
		if err != nil {
			return nil, err
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	var document codexMCPDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.MCPServers) == 0 {
		if err := json.Unmarshal(data, &document.MCPServers); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(document.MCPServers))
	for name := range document.MCPServers {
		if validComponentName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	servers := make([]pubplugin.MCPServerSpec, 0, len(names))
	for _, name := range names {
		server := document.MCPServers[name]
		workingDir := server.Cwd
		if workingDir == "" {
			workingDir = server.WorkingDir
		}
		if workingDir == "" {
			workingDir = "."
		}
		if filepath.IsAbs(workingDir) || filepath.Clean(workingDir) == ".." || strings.HasPrefix(filepath.Clean(workingDir), ".."+string(filepath.Separator)) {
			continue
		}
		disabled := server.Disabled || (server.Enabled != nil && !*server.Enabled)
		if server.Command == "" && server.URL == "" {
			continue
		}
		servers = append(servers, pubplugin.MCPServerSpec{
			Name: name, Command: server.Command, Args: append([]string(nil), server.Args...), URL: server.URL,
			Headers: cloneMap(server.Headers), Env: cloneMap(server.Env), WorkingDir: filepath.ToSlash(filepath.Clean(workingDir)),
			EnabledTools: append([]string(nil), server.EnabledTools...), DisabledTools: append([]string(nil), server.DisabledTools...),
			Disabled: disabled,
		})
	}
	return servers, nil
}

func secureRegularPath(root, relative string, maxBytes int64) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("absolute component path rejected")
	}
	clean := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(relative, "./")))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("component path escapes plugin")
	}
	path := filepath.Join(root, clean)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxBytes {
		return "", ErrNotFound
	}
	return path, nil
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func rawPresent(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return trimmed != "" && trimmed != "null" && trimmed != "[]" && trimmed != "{}" && trimmed != `""`
}

func rawStringList(value json.RawMessage) []string {
	if len(value) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(value, &one) == nil && strings.TrimSpace(one) != "" {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(value, &many) == nil {
		return many
	}
	return nil
}

func mergeStrings(groups ...[]string) []string {
	seen := map[string]bool{}
	var merged []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			merged = append(merged, value)
		}
	}
	return merged
}

func discoverSkillFiles(sourcePath, source string, declared []string) []string {
	if len(declared) == 0 {
		if info, err := os.Stat(filepath.Join(sourcePath, "skills")); err == nil && info.IsDir() {
			declared = []string{"skills"}
		} else if regularFile(filepath.Join(sourcePath, "SKILL.md")) {
			declared = []string{"SKILL.md"}
		}
	}
	source = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(source)), "./")
	if source == "." {
		source = ""
	}
	seen := map[string]bool{}
	var result []string
	for _, raw := range declared {
		relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(raw)), "./")
		if source != "" && strings.HasPrefix(relative, source+"/") {
			relative = strings.TrimPrefix(relative, source+"/")
		}
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") {
			continue
		}
		candidate := filepath.Join(sourcePath, filepath.FromSlash(relative))
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			appendSkillPath(sourcePath, candidate, seen, &result)
			continue
		}
		_ = filepath.WalkDir(candidate, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.Type()&os.ModeSymlink != 0 {
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.IsDir() && strings.EqualFold(entry.Name(), "SKILL.md") {
				appendSkillPath(sourcePath, path, seen, &result)
			}
			return nil
		})
	}
	sort.Strings(result)
	return result
}

func appendSkillPath(root, path string, seen map[string]bool, result *[]string) {
	if !strings.EqualFold(filepath.Base(path), "SKILL.md") || !regularFile(path) {
		return
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || seen[relative] {
		return
	}
	seen[relative] = true
	*result = append(*result, relative)
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func skillsRequireCodexRuntime(root string, skills []string) bool {
	markers := []string{
		"load_workspace_dependencies", "mcp__codex_apps__", "plugin://",
		"[@presentations]", "[@documents]", "[@spreadsheets]", "[@pdf]",
	}
	for _, relative := range skills {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			continue
		}
		if len(data) > 512<<10 {
			data = data[:512<<10]
		}
		body := string(data)
		for _, marker := range markers {
			if strings.Contains(body, marker) {
				return true
			}
		}
	}
	return false
}

func supportedRemoteSource(source pluginSource) bool {
	if source.Kind != "url" && source.Kind != "git-subdir" && source.Kind != "git" {
		return false
	}
	_, ok := normalizedRemoteURL(source.URL)
	if !ok {
		return false
	}
	if source.SHA != "" && !validGitSHA(source.SHA) {
		return false
	}
	return source.Ref == "" || validGitRef(source.Ref)
}

func normalizedRemoteURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		parts := strings.Split(raw, "/")
		if len(parts) != 2 || !validComponentName(parts[0]) || !validComponentName(strings.TrimSuffix(parts[1], ".git")) {
			return "", false
		}
		return "https://github.com/" + raw, true
	}
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Path == "" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "gitlab.com" && host != "codeberg.org" {
		return "", false
	}
	return parsed.String(), true
}

func validGitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func validGitRef(value string) bool {
	if value == "" || len(value) > 200 || strings.HasPrefix(value, "-") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune(`~^:?*[\\`, r) {
			return false
		}
	}
	return true
}

func (m *Manager) Catalog() Catalog {
	registry := rtpkg.LoadMarketplaceRegistry()
	installed := installedPluginNames()
	catalog := Catalog{Marketplaces: make([]Marketplace, 0, len(registry.Entries))}
	for _, name := range registry.ListNames() {
		entry := registry.Entries[name]
		view := marketplaceView(entry)
		if entry.IsLocalCatalog() {
			items, updatedAt, err := readCodexCacheCatalog()
			if err != nil {
				view.Error = err.Error()
				catalog.Marketplaces = append(catalog.Marketplaces, view)
				continue
			}
			view.Synced = true
			view.UpdatedAt = updatedAt
			for _, item := range items {
				plugin := catalogPlugin(name, item.entry, item.sourcePath, "local", installed)
				catalog.Plugins = append(catalog.Plugins, plugin)
				view.PluginCount++
				if plugin.Installable && !plugin.Installed {
					view.InstallableCount++
				}
			}
			catalog.Marketplaces = append(catalog.Marketplaces, view)
			continue
		}
		root := rtpkg.MarketplaceClonePath(name)
		manifest, err := readMarketplaceManifest(entry, root)
		if err != nil {
			if os.IsNotExist(errors.Unwrap(err)) || os.IsNotExist(err) {
				catalog.NeedsSync = true
			} else if _, statErr := os.Stat(root); statErr == nil {
				view.Error = "Marketplace catalog is unreadable"
			} else {
				catalog.NeedsSync = true
			}
			catalog.Marketplaces = append(catalog.Marketplaces, view)
			continue
		}
		view.Synced = true
		if view.DisplayName == name && manifest.Interface.DisplayName != "" {
			view.DisplayName = manifest.Interface.DisplayName
		}
		manifestPath, _ := marketplaceManifestPath(entry, root)
		if info, statErr := os.Stat(manifestPath); statErr == nil {
			view.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
		for _, item := range manifest.Plugins {
			if !validComponentName(item.Name) {
				continue
			}
			item.Ecosystem = entry.Ecosystem
			source := item.resolveSource()
			sourcePath, sourceErr := secureSourcePath(root, source.Path)
			if source.Kind != "path" || sourceErr != nil {
				plugin := Plugin{
					Name: item.Name, DisplayName: item.Name, Description: item.Description,
					Marketplace: name, Homepage: item.Homepage, Category: item.Category,
					Keywords: append([]string(nil), item.Keywords...), SourceKind: source.Kind,
					Installed: installed[item.Name], Ecosystem: item.Ecosystem, Compatibility: "unavailable",
					Unavailable: "External repository plugins are not supported yet",
				}
				if supportedRemoteSource(source) {
					plugin.Installable = true
					plugin.Compatibility = "remote"
					plugin.Unavailable = ""
				}
				if source.Kind == "path" {
					plugin.Unavailable = "Plugin source path is unavailable or unsafe"
				}
				catalog.Plugins = append(catalog.Plugins, plugin)
				view.PluginCount++
				if plugin.Installable && !plugin.Installed {
					view.InstallableCount++
				}
				continue
			}
			plugin := catalogPlugin(name, item, sourcePath, source.Kind, installed)
			catalog.Plugins = append(catalog.Plugins, plugin)
			view.PluginCount++
			if plugin.Installable && !plugin.Installed {
				view.InstallableCount++
			}
		}
		catalog.Marketplaces = append(catalog.Marketplaces, view)
	}

	// Ecosystem adapters are discovered independently from marketplaces. This
	// is the core distinction the UI needs: a local Codex cache or a DSH npm
	// profile is a runtime/package ecosystem, not a marketplace pretending to
	// publish METIS-native bundles.
	codexItems, _, codexErr := readCodexCacheCatalog()
	for _, item := range codexItems {
		item.entry.Ecosystem = "codex"
		catalog.Plugins = append(catalog.Plugins, catalogPlugin(codexLocalOrigin, item.entry, item.sourcePath, "ecosystem", installed))
	}
	dshItems, dshErr := readDSHProfileCatalog()
	for _, item := range dshItems {
		catalog.Plugins = append(catalog.Plugins, catalogPlugin(item.origin, item.entry, item.sourcePath, "ecosystem", installed))
	}
	counts := map[string]int{}
	for _, plugin := range catalog.Plugins {
		counts[plugin.Ecosystem]++
	}
	catalog.Ecosystems = ecosystemViews(counts["claude"], counts["codex"], codexErr, counts["deepseek-harness"], dshErr)
	sort.Slice(catalog.Plugins, func(i, j int) bool {
		if catalog.Plugins[i].Name == catalog.Plugins[j].Name {
			return catalog.Plugins[i].Marketplace < catalog.Plugins[j].Marketplace
		}
		return catalog.Plugins[i].Name < catalog.Plugins[j].Name
	})
	return catalog
}

func marketplaceView(entry rtpkg.MarketplaceEntry) Marketplace {
	displayName := strings.TrimSpace(entry.DisplayName)
	if displayName == "" {
		displayName = entry.Name
	}
	return Marketplace{
		Name: entry.Name, DisplayName: displayName, Description: entry.Description,
		Ecosystem: entry.Ecosystem, Source: entry.Source.Source, Repo: entry.Source.Repo,
		Builtin: entry.Builtin, Local: entry.IsLocalCatalog(),
	}
}

func (m *Manager) Sync(ctx context.Context, requested []string) ([]SyncResult, Catalog) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	registry := rtpkg.LoadMarketplaceRegistry()
	names := requested
	if len(names) == 0 {
		names = registry.ListNames()
	}
	seen := make(map[string]bool, len(names))
	results := make([]SyncResult, 0, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		result := SyncResult{Marketplace: name}
		entry, ok := registry.Entries[name]
		if !ok || !validComponentName(name) {
			result.Error = "Unknown marketplace"
			results = append(results, result)
			continue
		}
		if err := m.syncOne(ctx, entry); err != nil {
			result.Error = err.Error()
		} else {
			result.Updated = true
		}
		results = append(results, result)
	}
	return results, m.Catalog()
}

func (m *Manager) syncOne(ctx context.Context, entry rtpkg.MarketplaceEntry) error {
	if entry.IsLocalCatalog() {
		// A local cache refresh is just a re-scan performed by Catalog. Its
		// absence must not make a multi-market network sync fail.
		return nil
	}
	destination := rtpkg.MarketplaceClonePath(entry.Name)
	if _, err := os.Stat(filepath.Join(destination, ".git")); err == nil {
		return m.runGit(ctx, "-C", destination, "pull", "--ff-only", "--quiet")
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("marketplace cache exists but is not a git checkout")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect marketplace cache: %w", err)
	}
	cloneURL := entry.CloneURL()
	if cloneURL == "" {
		return fmt.Errorf("unsupported marketplace source")
	}
	root := filepath.Dir(destination)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	staging := destination + ".sync-" + fmt.Sprint(m.now().UnixNano())
	if err := m.runGit(ctx, "clone", "--depth", "1", "--quiet", cloneURL, staging); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("sync failed: %w", err)
	}
	if _, err := readMarketplaceManifest(entry, staging); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("catalog missing: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	return nil
}

func (m *Manager) Install(ctx context.Context, name, marketplace string) (InstallResult, error) {
	if !validComponentName(name) || !validComponentName(marketplace) {
		return InstallResult{}, ErrInvalidName
	}
	if marketplace == codexLocalOrigin {
		item, err := findCodexCachePlugin(name)
		if err != nil {
			return InstallResult{}, err
		}
		item.entry.Ecosystem = "codex"
		item.entry = enrichPluginMetadata(item.sourcePath, item.entry)
		item.entry.Skills = discoverSkillFiles(item.sourcePath, ".", item.entry.Skills)
		return m.installFromPath(name, marketplace, item.sourcePath, item.entry)
	}
	if strings.HasPrefix(marketplace, dshOriginPrefix) {
		item, err := findDSHProfilePlugin(name, marketplace)
		if err != nil {
			return InstallResult{}, err
		}
		return m.installFromPath(name, marketplace, item.sourcePath, item.entry)
	}
	registry := rtpkg.LoadMarketplaceRegistry()
	entry, ok := registry.Entries[marketplace]
	if !ok {
		return InstallResult{}, ErrNotFound
	}
	root := rtpkg.MarketplaceClonePath(marketplace)
	if entry.IsLocalCatalog() {
		item, err := findCodexCachePlugin(name)
		if err != nil {
			return InstallResult{}, err
		}
		item.entry = enrichPluginMetadata(item.sourcePath, item.entry)
		item.entry.Skills = discoverSkillFiles(item.sourcePath, ".", item.entry.Skills)
		return m.installFromPath(name, marketplace, item.sourcePath, item.entry)
	}
	manifest, err := readMarketplaceManifest(entry, root)
	if err != nil {
		m.syncMu.Lock()
		err = m.syncOne(ctx, entry)
		m.syncMu.Unlock()
		if err != nil {
			return InstallResult{}, err
		}
		manifest, err = readMarketplaceManifest(entry, root)
		if err != nil {
			return InstallResult{}, err
		}
	}
	var selected *marketplacePluginEntry
	for i := range manifest.Plugins {
		if manifest.Plugins[i].Name == name {
			selected = &manifest.Plugins[i]
			break
		}
	}
	if selected == nil {
		return InstallResult{}, ErrNotFound
	}
	source := selected.resolveSource()
	if source.Kind != "path" && !supportedRemoteSource(source) {
		return InstallResult{}, ErrNotInstallable
	}
	sourcePath := ""
	cleanup := func() {}
	if source.Kind == "path" {
		sourcePath, err = secureSourcePath(root, source.Path)
		if err != nil {
			return InstallResult{}, fmt.Errorf("%w: %v", ErrNotInstallable, err)
		}
	} else {
		sourcePath, cleanup, err = m.checkoutRemoteSource(ctx, marketplace, name, source)
		if err != nil {
			return InstallResult{}, err
		}
		defer cleanup()
	}
	selectedValue := enrichPluginMetadata(sourcePath, *selected)
	selectedValue.Skills = discoverSkillFiles(sourcePath, source.Path, selectedValue.Skills)
	return m.installFromPath(name, marketplace, sourcePath, selectedValue)
}

func (m *Manager) checkoutRemoteSource(ctx context.Context, marketplace, name string, source pluginSource) (string, func(), error) {
	if !supportedRemoteSource(source) {
		return "", func() {}, ErrNotInstallable
	}
	cloneURL, ok := normalizedRemoteURL(source.URL)
	if !ok {
		return "", func() {}, ErrNotInstallable
	}
	tempRoot := filepath.Join(rtpkg.PluginsDir(), ".marketplace-downloads")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		return "", func() {}, err
	}
	staging, err := os.MkdirTemp(tempRoot, marketplace+"-"+name+"-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	cloneArgs := []string{"clone", "--depth", "1", "--quiet"}
	if source.Ref != "" && source.SHA == "" {
		cloneArgs = append(cloneArgs, "--branch", source.Ref)
	}
	cloneArgs = append(cloneArgs, cloneURL, staging)
	if err := m.runGit(ctx, cloneArgs...); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("download plugin source: %w", err)
	}
	if source.SHA != "" {
		if err := m.runGit(ctx, "-C", staging, "fetch", "--depth", "1", "origin", source.SHA); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("fetch pinned plugin revision: %w", err)
		}
		if err := m.runGit(ctx, "-C", staging, "checkout", "--detach", "--quiet", source.SHA); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("checkout pinned plugin revision: %w", err)
		}
	}
	subdir := source.Path
	if subdir == "" {
		subdir = "."
	}
	sourcePath, err := secureSourcePath(staging, subdir)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("%w: remote plugin path is unavailable or unsafe", ErrNotInstallable)
	}
	return sourcePath, cleanup, nil
}

func (m *Manager) installFromPath(name, marketplace, sourcePath string, selected marketplacePluginEntry) (InstallResult, error) {
	if !regularFile(filepath.Join(sourcePath, "plugin.toml")) && len(selected.Skills) == 0 && len(selected.MCPServers) == 0 {
		return InstallResult{}, ErrNotInstallable
	}
	pluginsRoot := rtpkg.PluginsDir()
	if err := os.MkdirAll(pluginsRoot, 0o755); err != nil {
		return InstallResult{}, err
	}
	destination := filepath.Join(pluginsRoot, name)
	if _, err := os.Stat(destination); err == nil {
		return InstallResult{}, ErrAlreadyInstalled
	} else if !os.IsNotExist(err) {
		return InstallResult{}, err
	}
	staging := filepath.Join(pluginsRoot, ".install-"+name+"-"+fmt.Sprint(m.now().UnixNano()))
	if err := copyTree(sourcePath, staging); err != nil {
		_ = os.RemoveAll(staging)
		return InstallResult{}, err
	}
	if err := ensurePluginManifest(staging, selected); err != nil {
		_ = os.RemoveAll(staging)
		return InstallResult{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return InstallResult{}, err
	}
	return InstallResult{Name: name, Path: destination, Restart: true, Source: marketplace}, nil
}

// PluginIconPath resolves only the icon declared by a validated marketplace
// plugin manifest. The web layer can serve this file without exposing a
// general-purpose filesystem endpoint.
func (m *Manager) PluginIconPath(marketplace, name string) (string, error) {
	if !validComponentName(name) || !validComponentName(marketplace) {
		return "", ErrInvalidName
	}
	if marketplace == codexLocalOrigin {
		item, err := findCodexCachePlugin(name)
		if err != nil {
			return "", err
		}
		selected := enrichPluginMetadata(item.sourcePath, item.entry)
		if selected.Icon == "" {
			return "", ErrNotFound
		}
		return secureAssetPath(item.sourcePath, selected.Icon)
	}
	if strings.HasPrefix(marketplace, dshOriginPrefix) {
		return "", ErrNotFound
	}
	registry := rtpkg.LoadMarketplaceRegistry()
	entry, ok := registry.Entries[marketplace]
	if !ok {
		return "", ErrNotFound
	}
	var selected marketplacePluginEntry
	var sourcePath string
	if entry.IsLocalCatalog() {
		item, err := findCodexCachePlugin(name)
		if err != nil {
			return "", err
		}
		selected = item.entry
		sourcePath = item.sourcePath
	} else {
		root := rtpkg.MarketplaceClonePath(marketplace)
		manifest, err := readMarketplaceManifest(entry, root)
		if err != nil {
			return "", ErrNotFound
		}
		found := false
		for _, item := range manifest.Plugins {
			if item.Name != name {
				continue
			}
			selected = item
			resolved := item.resolveSource()
			if resolved.Kind != "path" {
				return "", ErrNotFound
			}
			sourcePath, err = secureSourcePath(root, resolved.Path)
			if err != nil {
				return "", ErrNotFound
			}
			found = true
			break
		}
		if !found {
			return "", ErrNotFound
		}
	}
	selected = enrichPluginMetadata(sourcePath, selected)
	if selected.Icon == "" {
		return "", ErrNotFound
	}
	return secureAssetPath(sourcePath, selected.Icon)
}

func secureAssetPath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("absolute asset path rejected")
	}
	clean := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(relative, "./")))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("asset path escapes plugin")
	}
	path := filepath.Join(root, clean)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<20 {
		return "", ErrNotFound
	}
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return path, nil
	default:
		return "", ErrNotFound
	}
}

func (m *Manager) Remove(name string) (RemoveResult, error) {
	if !validComponentName(name) {
		return RemoveResult{}, ErrInvalidName
	}
	pluginsRoot := rtpkg.PluginsDir()
	target := filepath.Join(pluginsRoot, name)
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return RemoveResult{}, ErrNotFound
		}
		return RemoveResult{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return RemoveResult{}, ErrNotFound
	}
	trashRoot := filepath.Join(filepath.Dir(pluginsRoot), "trash", "plugins")
	if err := os.MkdirAll(trashRoot, 0o700); err != nil {
		return RemoveResult{}, err
	}
	stamp := m.now().UTC().Format("20060102T150405.000000000Z")
	destination := filepath.Join(trashRoot, name+"-"+stamp)
	for suffix := 1; ; suffix++ {
		if _, err := os.Stat(destination); os.IsNotExist(err) {
			break
		}
		destination = filepath.Join(trashRoot, fmt.Sprintf("%s-%s-%d", name, stamp, suffix))
	}
	if err := os.Rename(target, destination); err != nil {
		return RemoveResult{}, err
	}
	return RemoveResult{Name: name, RecoverableAt: destination, Restart: true}, nil
}

func readMarketplaceManifest(entry rtpkg.MarketplaceEntry, root string) (*marketplaceManifest, error) {
	manifestPath, err := marketplaceManifestPath(entry, root)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read marketplace catalog: %w", err)
	}
	var manifest marketplaceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse marketplace catalog: %w", err)
	}
	return &manifest, nil
}

func marketplaceManifestPath(entry rtpkg.MarketplaceEntry, root string) (string, error) {
	relative := strings.TrimSpace(entry.Manifest)
	if relative == "" {
		if strings.EqualFold(entry.Format, "codex") {
			relative = ".agents/plugins/marketplace.json"
		} else {
			relative = ".claude-plugin/marketplace.json"
		}
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("absolute marketplace manifest path rejected")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("marketplace manifest path escapes checkout")
	}
	return filepath.Join(root, clean), nil
}

func readCodexCacheCatalog() ([]localCatalogItem, string, error) {
	root := filepath.Join(codexHome(), "plugins", "cache")
	marketplaces, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", errors.New("Codex plugin cache is not available on this device")
		}
		return nil, "", fmt.Errorf("read Codex plugin cache: %w", err)
	}
	byName := map[string]localCatalogItem{}
	var newest time.Time
	for _, marketplace := range marketplaces {
		if !marketplace.IsDir() {
			continue
		}
		marketRoot := filepath.Join(root, marketplace.Name())
		plugins, readErr := os.ReadDir(marketRoot)
		if readErr != nil {
			continue
		}
		for _, pluginDir := range plugins {
			if !pluginDir.IsDir() || !validComponentName(pluginDir.Name()) {
				continue
			}
			versions, readErr := os.ReadDir(filepath.Join(marketRoot, pluginDir.Name()))
			if readErr != nil {
				continue
			}
			for _, version := range versions {
				if !version.IsDir() {
					continue
				}
				sourcePath := filepath.Join(marketRoot, pluginDir.Name(), version.Name())
				manifestPath := filepath.Join(sourcePath, ".codex-plugin", "plugin.json")
				info, statErr := os.Stat(manifestPath)
				if statErr != nil || !info.Mode().IsRegular() {
					continue
				}
				item := localCatalogItem{
					entry:      marketplacePluginEntry{Name: pluginDir.Name(), Source: json.RawMessage(`"."`)},
					sourcePath: sourcePath, modified: info.ModTime(),
				}
				current, exists := byName[pluginDir.Name()]
				if !exists || item.modified.After(current.modified) {
					byName[pluginDir.Name()] = item
				}
				if item.modified.After(newest) {
					newest = item.modified
				}
			}
		}
	}
	items := make([]localCatalogItem, 0, len(byName))
	for _, item := range byName {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].entry.Name < items[j].entry.Name })
	updatedAt := ""
	if !newest.IsZero() {
		updatedAt = newest.UTC().Format(time.RFC3339)
	}
	return items, updatedAt, nil
}

func findCodexCachePlugin(name string) (localCatalogItem, error) {
	items, _, err := readCodexCacheCatalog()
	if err != nil {
		return localCatalogItem{}, err
	}
	for _, item := range items {
		if item.entry.Name == name {
			return item, nil
		}
	}
	return localCatalogItem{}, ErrNotFound
}

func codexHome() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

func installedPluginNames() map[string]bool {
	installed := map[string]bool{}
	entries, err := os.ReadDir(rtpkg.PluginsDir())
	if err != nil {
		return installed
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "marketplaces" || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(rtpkg.PluginsDir(), entry.Name(), "plugin.toml")); err == nil {
			installed[entry.Name()] = true
		}
	}
	return installed
}

func validComponentName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func secureSourcePath(root, relative string) (string, error) {
	if relative == "" {
		relative = "."
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("absolute source path rejected")
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Clean(relative)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootResolved, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("source path escapes marketplace")
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("source is not a directory")
	}
	return candidate, nil
}

func copyTree(source, destination string) error {
	files := 0
	var bytesCopied int64
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("plugin contains unsupported symlink: %s", relative)
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytesCopied += info.Size()
		if files > maxCopiedFiles || bytesCopied > maxCopiedBytes {
			return errors.New("plugin exceeds safe installation size")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, maxCopiedBytes+1))
		closeErr := output.Close()
		inputErr := input.Close()
		return errors.Join(copyErr, closeErr, inputErr)
	})
}

func ensurePluginManifest(root string, entry marketplacePluginEntry) error {
	manifestPath := filepath.Join(root, "plugin.toml")
	if _, err := os.Stat(manifestPath); err == nil {
		var manifest pubplugin.Manifest
		if _, err := toml.DecodeFile(manifestPath, &manifest); err != nil {
			return fmt.Errorf("installed manifest unreadable: %w", err)
		}
		if manifest.Name != entry.Name {
			return fmt.Errorf("installed manifest name %q does not match %q", manifest.Name, entry.Name)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	skills := make([]string, 0, len(entry.Skills))
	for _, skill := range entry.Skills {
		relative := filepath.Clean(skill)
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if regularFile(filepath.Join(root, relative)) {
			skills = append(skills, relative)
		}
	}
	if len(skills) == 0 {
		if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
			skills = []string{"SKILL.md"}
		}
	}
	version := entry.Version
	if version == "" {
		version = "0.0.0"
	}
	for i := range skills {
		skills[i] = filepath.ToSlash(skills[i])
	}
	manifest := pubplugin.Manifest{
		ManifestVersion: pubplugin.CurrentManifestVersion,
		Name:            entry.Name, Version: version, Description: entry.Description,
		Homepage: entry.Homepage, Skills: skills,
		MCPServers: append([]pubplugin.MCPServerSpec(nil), entry.MCPServers...),
	}
	var builder strings.Builder
	if err := toml.NewEncoder(&builder).Encode(manifest); err != nil {
		return fmt.Errorf("encode translated plugin manifest: %w", err)
	}
	return os.WriteFile(manifestPath, []byte(builder.String()), 0o600)
}
