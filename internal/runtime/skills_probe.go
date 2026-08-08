package runtime

// skills_probe.go — cheap "does the user have any skills installed?"
// detector consumed by the SkillsSection getter. Returns false on a
// fresh metis install so the skills guidance doesn't waste tokens
// telling the model to consult skills that don't exist.
//
// We deliberately do NOT load skill metadata here — that's the
// agent/skills loader's job. We just answer the boolean: any
// directory entry that could be a skill?

import (
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// HasInstalledSkills returns true when at least one user-level skill exists
// under Metis's own root, the cross-agent ~/.agents/skills root, or a
// project-local .metis/skills directory.
//
// Bundled skills (compiled into the binary via the skills/embedded
// go:embed FS) are NOT counted here — they're always available, so
// reporting "true" on every install would defeat the purpose of the
// per-boot conditional. The check is for *user-extended* skills.
//
// Cheap implementation: list each parent and look for a directory/symlink or
// a supported flat manifest. We don't recurse to validate SKILL.md; the loader
// handles malformed entries later.
func HasInstalledSkills() bool {
	if dirHasSkillCandidate(filepath.Join(config.Home(), "skills")) {
		return true
	}
	// Universal Agent Skills root. `npx skills` installs here and symlinks
	// client-specific roots back to it; scan the canonical root only.
	if dirHasSkillCandidate(universalSkillsDir()) {
		return true
	}
	// Project-level: ./.metis/skills/<name>/SKILL.md
	if cwd, err := os.Getwd(); err == nil {
		if dirHasSkillCandidate(filepath.Join(cwd, ".metis", "skills")) {
			return true
		}
	}
	return false
}

// universalSkillsDir resolves the cross-agent Agent Skills root. Normal runs
// use ~/.agents/skills. When METIS_HOME is set (tests and sandboxed installs),
// keep the universal root under that override so a supposedly isolated runtime
// never scans the developer machine's real ~/.agents catalog.
func universalSkillsDir() string {
	if metisHome := os.Getenv("METIS_HOME"); metisHome != "" {
		return filepath.Join(metisHome, ".agents", "skills")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "skills")
}

func dirHasSkillCandidate(dir string) bool {
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
		name := e.Name()
		if len(name) == 0 || name[0] == '.' {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			if info, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil && info.IsDir() {
				return true
			}
		}
		switch filepath.Ext(name) {
		case ".md", ".markdown", ".json":
			return true
		}
	}
	return false
}
