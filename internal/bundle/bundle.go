package bundle

// bundle.go — profile bundles (DSH bundle parity, metis-native shape).
//
// A bundle is a DIRECTORY containing:
//
//	bundle.toml   manifest: name (slug, required), version, description, author
//	agents/*.md   sub-agent profiles      → installed to <home>/agents/
//	skills/*/     skill dirs (SKILL.md)   → installed to <home>/skills/
//
// Installation is copy-based and recorded in <home>/bundles.json (the
// ledger): name → version, source path, installed file list. list reads
// the ledger; remove deletes the recorded files (only paths inside the
// managed dirs, and only ones that still exist) and drops the ledger
// row. Deliberately NO config.toml merging: bundles deliver skills and
// agent profiles, never provider/permission config — a bundle that
// could rewrite the permission posture would be a supply-chain hole.
//
// DSH's npm-package bundles map onto this as: package.json≈bundle.toml,
// skills/→skills/, commands/→agents/ (metis folds command-docs and
// agent-profiles into the same Markdown-with-front-matter artifact).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Manifest is bundle.toml.
type Manifest struct {
	Name        string `toml:"name"`
	Version     string `toml:"version"`
	Description string `toml:"description"`
	Author      string `toml:"author"`
}

// InstallRecord is one row of bundles.json.
type InstallRecord struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Source    string    `json:"source"`
	Files     []string  `json:"files"`
	Installed time.Time `json:"installed"`
}

// Ledger is the bundles.json document.
type Ledger struct {
	Bundles []InstallRecord `json:"bundles"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]{1,40}$`)

// LoadManifest parses <dir>/bundle.toml. name must be a slug.
func LoadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.toml"))
	if err != nil {
		return nil, fmt.Errorf("bundle.toml not found in %s: %w", dir, err)
	}
	m := &Manifest{}
	text := string(raw)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			m.Name = val
		case "version":
			m.Version = val
		case "description":
			m.Description = val
		case "author":
			m.Author = val
		}
	}
	if !namePattern.MatchString(m.Name) {
		return nil, fmt.Errorf("bundle name %q is not a valid slug (lowercase letters, digits, -, _; 2-41 chars)", m.Name)
	}
	return m, nil
}

// Install copies the bundle's agents/ and skills/ into home and records
// the ledger row. Returns the record. Re-installing the same name
// overwrites (removes the old files first) — idempotent upgrades.
func Install(home, dir string) (*InstallRecord, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	led, err := loadLedger(home)
	if err != nil {
		return nil, err
	}
	// Upgrade path: clear any prior install of the same name.
	for i, rec := range led.Bundles {
		if rec.Name == m.Name {
			removeFiles(home, rec.Files)
			led.Bundles = append(led.Bundles[:i], led.Bundles[i+1:]...)
			break
		}
	}

	rec := InstallRecord{Name: m.Name, Version: m.Version, Source: dir, Installed: time.Now()}
	// agents/*.md → home/agents/<file>
	agentsSrc := filepath.Join(dir, "agents")
	if ents, err := os.ReadDir(agentsSrc); err == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			rel, err := copyFile(filepath.Join(agentsSrc, e.Name()), filepath.Join(home, "agents", e.Name()), home)
			if err != nil {
				return nil, err
			}
			rec.Files = append(rec.Files, rel)
		}
	}
	// skills/<name>/ → home/skills/<name>/ (recursive)
	skillsSrc := filepath.Join(dir, "skills")
	if ents, err := os.ReadDir(skillsSrc); err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			rels, err := copyDir(filepath.Join(skillsSrc, e.Name()), filepath.Join(home, "skills", e.Name()), home)
			if err != nil {
				return nil, err
			}
			rec.Files = append(rec.Files, rels...)
		}
	}
	if len(rec.Files) == 0 {
		return nil, errors.New("bundle installs nothing: no agents/*.md and no skills/<name>/ found")
	}
	sort.Strings(rec.Files)
	led.Bundles = append(led.Bundles, rec)
	if err := saveLedger(home, led); err != nil {
		return nil, err
	}
	return &rec, nil
}

// List returns the ledger (verified: files that vanished are noted by
// the caller via Missing count).
func List(home string) ([]InstallRecord, error) {
	led, err := loadLedger(home)
	if err != nil {
		return nil, err
	}
	return led.Bundles, nil
}

// Remove deletes a bundle's recorded files and ledger row.
func Remove(home, name string) error {
	led, err := loadLedger(home)
	if err != nil {
		return err
	}
	for i, rec := range led.Bundles {
		if rec.Name != name {
			continue
		}
		removeFiles(home, rec.Files)
		led.Bundles = append(led.Bundles[:i], led.Bundles[i+1:]...)
		return saveLedger(home, led)
	}
	return fmt.Errorf("bundle %q is not installed", name)
}

// MissingFiles counts recorded files that no longer exist (drift view).
func MissingFiles(home string, rec InstallRecord) int {
	n := 0
	for _, f := range rec.Files {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			n++
		}
	}
	return n
}

func loadLedger(home string) (*Ledger, error) {
	raw, err := os.ReadFile(filepath.Join(home, "bundles.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &Ledger{}, nil
		}
		return nil, err
	}
	var led Ledger
	if err := json.Unmarshal(raw, &led); err != nil {
		return nil, fmt.Errorf("bundles.json corrupted: %w", err)
	}
	return &led, nil
}

func saveLedger(home string, led *Ledger) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(led, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "bundles.json"), b, 0o644)
}

// copyFile copies src→dst (creating dirs) and returns the path RELATIVE
// to home (ledger stores home-relative paths).
func copyFile(src, dst, home string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return "", err
	}
	return filepath.Rel(home, dst)
}

func copyDir(src, dst, home string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		r, err := copyFile(p, filepath.Join(dst, rel), home)
		if err != nil {
			return err
		}
		rels = append(rels, r)
		return nil
	})
	return rels, err
}

// removeFiles deletes home-relative paths and prunes empty parent dirs.
func removeFiles(home string, rels []string) {
	for _, f := range rels {
		p := filepath.Join(home, f)
		if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(home)+string(os.PathSeparator)) {
			continue // path-escape guard: never delete outside home
		}
		os.Remove(p)
		// prune empty skill dirs up to (not including) home
		for d := filepath.Dir(p); d != home && strings.HasPrefix(d, home); d = filepath.Dir(d) {
			if ents, err := os.ReadDir(d); err == nil && len(ents) == 0 {
				os.Remove(d)
			} else {
				break
			}
		}
	}
}
