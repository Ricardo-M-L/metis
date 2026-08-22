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
	Name        string `json:"name"`
	Source      string `json:"source"`
	Repo        string `json:"repo,omitempty"`
	Builtin     bool   `json:"builtin"`
	Synced      bool   `json:"synced"`
	PluginCount int    `json:"pluginCount"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Plugin struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Marketplace string   `json:"marketplace"`
	Homepage    string   `json:"homepage,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	SourceKind  string   `json:"sourceKind"`
	Installable bool     `json:"installable"`
	Installed   bool     `json:"installed"`
	Unavailable string   `json:"unavailableReason,omitempty"`
}

type Catalog struct {
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
	Name    string                   `json:"name"`
	Plugins []marketplacePluginEntry `json:"plugins"`
}

type marketplacePluginEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Source      json.RawMessage `json:"source"`
	Skills      []string        `json:"skills,omitempty"`
	Homepage    string          `json:"homepage,omitempty"`
}

type pluginSource struct {
	Path string
	URL  string
	Kind string
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
	}
	if err := json.Unmarshal(e.Source, &object); err == nil {
		return pluginSource{Path: object.Path, URL: object.URL, Kind: object.Source}
	}
	return pluginSource{}
}

func (m *Manager) Catalog() Catalog {
	registry := rtpkg.LoadMarketplaceRegistry()
	installed := installedPluginNames()
	catalog := Catalog{Marketplaces: make([]Marketplace, 0, len(registry.Entries))}
	for _, name := range registry.ListNames() {
		entry := registry.Entries[name]
		view := Marketplace{Name: name, Source: entry.Source.Source, Repo: entry.Source.Repo, Builtin: entry.Builtin}
		root := rtpkg.MarketplaceClonePath(name)
		manifest, err := readMarketplaceManifest(root)
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
		view.PluginCount = len(manifest.Plugins)
		if info, statErr := os.Stat(filepath.Join(root, ".claude-plugin", "marketplace.json")); statErr == nil {
			view.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
		catalog.Marketplaces = append(catalog.Marketplaces, view)
		for _, item := range manifest.Plugins {
			if !validComponentName(item.Name) {
				continue
			}
			source := item.resolveSource()
			installable := false
			reason := ""
			switch source.Kind {
			case "path":
				if _, sourceErr := secureSourcePath(root, source.Path); sourceErr == nil {
					installable = true
				} else {
					reason = "Plugin source path is unavailable or unsafe"
				}
			default:
				reason = "External repository plugins are not supported yet"
			}
			catalog.Plugins = append(catalog.Plugins, Plugin{
				Name: item.Name, Description: item.Description, Marketplace: name,
				Homepage: item.Homepage, Skills: append([]string(nil), item.Skills...),
				SourceKind: source.Kind, Installable: installable,
				Installed: installed[item.Name], Unavailable: reason,
			})
		}
	}
	sort.Slice(catalog.Plugins, func(i, j int) bool {
		if catalog.Plugins[i].Name == catalog.Plugins[j].Name {
			return catalog.Plugins[i].Marketplace < catalog.Plugins[j].Marketplace
		}
		return catalog.Plugins[i].Name < catalog.Plugins[j].Name
	})
	return catalog
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
	if _, err := readMarketplaceManifest(staging); err != nil {
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
	registry := rtpkg.LoadMarketplaceRegistry()
	entry, ok := registry.Entries[marketplace]
	if !ok {
		return InstallResult{}, ErrNotFound
	}
	root := rtpkg.MarketplaceClonePath(marketplace)
	manifest, err := readMarketplaceManifest(root)
	if err != nil {
		m.syncMu.Lock()
		err = m.syncOne(ctx, entry)
		m.syncMu.Unlock()
		if err != nil {
			return InstallResult{}, err
		}
		manifest, err = readMarketplaceManifest(root)
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
	if source.Kind != "path" {
		return InstallResult{}, ErrNotInstallable
	}
	sourcePath, err := secureSourcePath(root, source.Path)
	if err != nil {
		return InstallResult{}, fmt.Errorf("%w: %v", ErrNotInstallable, err)
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
	if err := ensurePluginManifest(staging, *selected, source.Path); err != nil {
		_ = os.RemoveAll(staging)
		return InstallResult{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return InstallResult{}, err
	}
	return InstallResult{Name: name, Path: destination, Restart: true, Source: marketplace}, nil
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

func readMarketplaceManifest(root string) (*marketplaceManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, fmt.Errorf("read marketplace catalog: %w", err)
	}
	var manifest marketplaceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse marketplace catalog: %w", err)
	}
	return &manifest, nil
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

func ensurePluginManifest(root string, entry marketplacePluginEntry, source string) error {
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
	skills := rebaseSkills(root, source, entry.Skills)
	if len(skills) == 0 {
		if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
			skills = []string{"SKILL.md"}
		}
	}
	var builder strings.Builder
	fmt.Fprintln(&builder, "manifest_version = 1")
	fmt.Fprintf(&builder, "name = %q\n", entry.Name)
	fmt.Fprintln(&builder, `version = "0.0.0"`)
	fmt.Fprintf(&builder, "description = %q\n", entry.Description)
	if entry.Homepage != "" {
		fmt.Fprintf(&builder, "homepage = %q\n", entry.Homepage)
	}
	if len(skills) > 0 {
		fmt.Fprintln(&builder, "skills = [")
		for _, skill := range skills {
			fmt.Fprintf(&builder, "  %q,\n", filepath.ToSlash(skill))
		}
		fmt.Fprintln(&builder, "]")
	}
	return os.WriteFile(manifestPath, []byte(builder.String()), 0o600)
}

func rebaseSkills(root, source string, skills []string) []string {
	source = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(source)), "./")
	if source == "." {
		source = ""
	}
	result := make([]string, 0, len(skills))
	for _, skill := range skills {
		relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(skill)), "./")
		if source != "" && strings.HasPrefix(relative, source+"/") {
			relative = strings.TrimPrefix(relative, source+"/")
		}
		if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); err == nil {
			result = append(result, filepath.FromSlash(relative))
		}
	}
	return result
}
