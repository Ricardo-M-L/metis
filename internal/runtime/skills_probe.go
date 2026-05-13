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

// HasInstalledSkills returns true when at least one user-level skill
// directory exists (i.e. ~/.metis/skills/<name>/SKILL.md is present
// for any <name>) OR when any project-local .metis/skills/<name>/
// SKILL.md exists in cwd.
//
// Bundled skills (compiled into the binary via the skills/embedded
// go:embed FS) are NOT counted here — they're always available, so
// reporting "true" on every install would defeat the purpose of the
// per-boot conditional. The check is for *user-extended* skills.
//
// Cheap implementation: list the parent dir, look for sub-directories.
// We don't recurse into each one to confirm SKILL.md exists; the
// loader handles malformed entries later. Three filesystem stats max.
func HasInstalledSkills() bool {
	// User-level: ~/.metis/skills/<name>/SKILL.md
	userDir := filepath.Join(config.Home(), "skills")
	if entries, err := os.ReadDir(userDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				return true
			}
		}
	}
	// Project-level: ./.metis/skills/<name>/SKILL.md
	if cwd, err := os.Getwd(); err == nil {
		projDir := filepath.Join(cwd, ".metis", "skills")
		if entries, err := os.ReadDir(projDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					return true
				}
			}
		}
	}
	return false
}
