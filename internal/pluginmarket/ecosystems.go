package pluginmarket

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	codexLocalOrigin = "codex-local"
	dshOriginPrefix  = "dsh-profile-"
)

type dshProfileManifest struct {
	Dependencies map[string]string `json:"dependencies"`
	DSH          struct {
		Profile struct {
			Bundles []string `json:"bundles"`
		} `json:"profile"`
	} `json:"dsh"`
}

type dshPackageManifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Keywords    []string `json:"keywords"`
	Homepage    string   `json:"homepage"`
	Repository  any      `json:"repository"`
	Author      any      `json:"author"`
	DSH         struct {
		Bundle struct {
			Patch string `json:"patch"`
		} `json:"bundle"`
	} `json:"dsh"`
}

func readDSHProfileCatalog() ([]localCatalogItem, error) {
	profilesRoot := filepath.Join(dshHome(), "profiles")
	profiles, err := os.ReadDir(profilesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("DeepSeek Harness profiles are not available on this device")
		}
		return nil, fmt.Errorf("read DeepSeek Harness profiles: %w", err)
	}
	var items []localCatalogItem
	seen := map[string]bool{}
	for _, profile := range profiles {
		if !profile.IsDir() || !validComponentName(profile.Name()) {
			continue
		}
		profileRoot := filepath.Join(profilesRoot, profile.Name())
		data, readErr := os.ReadFile(filepath.Join(profileRoot, "package.json"))
		if readErr != nil {
			continue
		}
		var manifest dshProfileManifest
		if json.Unmarshal(data, &manifest) != nil {
			continue
		}
		dependencyNames := make([]string, 0, len(manifest.Dependencies))
		for packageName := range manifest.Dependencies {
			dependencyNames = append(dependencyNames, packageName)
		}
		sort.Strings(dependencyNames)
		bundleSet := make(map[string]bool, len(manifest.DSH.Profile.Bundles))
		for _, packageName := range manifest.DSH.Profile.Bundles {
			bundleSet[packageName] = true
		}
		for _, dependencyName := range dependencyNames {
			if !bundleSet[dependencyName] {
				continue
			}
			packageRoot := filepath.Join(profileRoot, "node_modules", filepath.FromSlash(dependencyName))
			packageData, readErr := os.ReadFile(filepath.Join(packageRoot, "package.json"))
			if readErr != nil {
				continue
			}
			var packageManifest dshPackageManifest
			if json.Unmarshal(packageData, &packageManifest) != nil || packageManifest.DSH.Bundle.Patch == "" {
				continue
			}
			if _, pathErr := secureRegularPath(packageRoot, packageManifest.DSH.Bundle.Patch, 2<<20); pathErr != nil {
				continue
			}
			packageName := packageManifest.Name
			if packageName == "" {
				packageName = dependencyName
			}
			id := dshPluginID(packageName)
			origin := dshOriginPrefix + profile.Name()
			unique := origin + "\x00" + id
			if seen[unique] {
				continue
			}
			seen[unique] = true
			info, _ := os.Stat(filepath.Join(packageRoot, "package.json"))
			modified := time.Time{}
			if info != nil {
				modified = info.ModTime()
			}
			entry := marketplacePluginEntry{
				Name: id, DisplayName: packageName, PackageName: packageName,
				Version: packageManifest.Version, Description: packageManifest.Description,
				Keywords: append([]string(nil), packageManifest.Keywords...), Homepage: packageManifest.Homepage,
				Source: json.RawMessage(`"."`), Ecosystem: "deepseek-harness",
				Capabilities: []string{"Cordis bundle"}, Compatibility: "external",
				Components: []PluginComponent{{
					Kind: "cordis", Support: "external",
					Detail: "Runs through the DSH npm profile, cordis.yml composition, and fiber lifecycle",
				}},
			}
			entry.Skills = discoverSkillFiles(packageRoot, ".", []string{"skills", "SKILL.md"})
			if len(entry.Skills) > 0 {
				entry.Capabilities = append(entry.Capabilities, "Skills")
				entry.Compatibility = "partial"
				entry.Components = append(entry.Components, PluginComponent{
					Kind: "skills", Support: "native", Detail: "Portable SKILL.md files can be imported into METIS",
				})
			}
			items = append(items, localCatalogItem{entry: entry, sourcePath: packageRoot, modified: modified, origin: origin})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].origin == items[j].origin {
			return items[i].entry.Name < items[j].entry.Name
		}
		return items[i].origin < items[j].origin
	})
	return items, nil
}

func findDSHProfilePlugin(name, origin string) (localCatalogItem, error) {
	items, err := readDSHProfileCatalog()
	if err != nil {
		return localCatalogItem{}, err
	}
	for _, item := range items {
		if item.entry.Name == name && item.origin == origin {
			return item, nil
		}
	}
	return localCatalogItem{}, ErrNotFound
}

func dshPluginID(packageName string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(packageName), "@")
	var builder strings.Builder
	lastDash := false
	for _, char := range trimmed {
		valid := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '.'
		if valid {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		name = "package"
	}
	return "dsh-" + name
}

func dshHome() string {
	if value := strings.TrimSpace(os.Getenv("DSH_HOME")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dsh"
	}
	return filepath.Join(home, ".dsh")
}

func ecosystemViews(claudeCount int, codexCount int, codexErr error, dshCount int, dshErr error) []Ecosystem {
	status := func(count int, err error) string {
		if err != nil {
			return "unavailable"
		}
		if count == 0 {
			return "ready"
		}
		return "connected"
	}
	return []Ecosystem{
		{
			ID: "metis", DisplayName: "METIS native", Mode: "native", Status: "connected",
			Description: "Cross-platform plugin.toml bundles managed directly by the METIS process.",
			Components: []EcosystemComponent{
				{Kind: "skills", Support: "native", Detail: "Namespaced SKILL.md loading"},
				{Kind: "mcp", Support: "native", Detail: "Multiple stdio or HTTP MCP servers"},
				{Kind: "hooks", Support: "native", Detail: "METIS lifecycle hook declarations"},
			},
		},
		{
			ID: "claude", DisplayName: "Claude ecosystem bridge", Mode: "adapter", Status: "connected", PackageCount: claudeCount,
			Description: "Reads Claude marketplace manifests and imports portable skills or native METIS bundles.",
			Components: []EcosystemComponent{
				{Kind: "skills", Support: "native", Detail: "SKILL.md files are imported with namespace isolation"},
				{Kind: "mcp", Support: "native", Detail: "Native METIS plugin.toml bundles retain MCP servers"},
				{Kind: "hooks", Support: "external", Detail: "Claude-specific hook semantics are not implied by catalog compatibility"},
			},
		},
		{
			ID: "codex", DisplayName: "Codex ecosystem bridge", Mode: "adapter", Status: status(codexCount, codexErr), PackageCount: codexCount,
			Description: "Understands .codex-plugin/plugin.json and translates portable components instead of treating Codex as a plugin.",
			Components: []EcosystemComponent{
				{Kind: "skills", Support: "native", Detail: "Imported with namespace isolation"},
				{Kind: "mcp", Support: "translated", Detail: ".mcp.json servers retain argv, env, filters, and working directory"},
				{Kind: "apps", Support: "external", Detail: "Codex connectors still require the Codex account runtime"},
				{Kind: "hooks", Support: "external", Detail: "Not executed until event semantics match"},
			},
		},
		{
			ID: "deepseek-harness", DisplayName: "DeepSeek Harness ecosystem bridge", Mode: "profile", Status: status(dshCount, dshErr), PackageCount: dshCount,
			Description: "Understands npm packages with dsh.bundle.patch and DSH profile composition; imports portable parts without impersonating Cordis.",
			Components: []EcosystemComponent{
				{Kind: "package", Support: "discovered", Detail: "Reads real ~/.dsh/profiles/* dependencies and bundle layers"},
				{Kind: "skills", Support: "native", Detail: "Portable SKILL.md files can be imported"},
				{Kind: "cordis", Support: "external", Detail: "Services, slots, events, HMR, and fibers continue in the DSH runtime"},
			},
		},
	}
}
