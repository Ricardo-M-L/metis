package skills

import (
	"embed"
	"io/fs"
	"path/filepath"
	"strings"

	pubskill "github.com/Ricardo-M-L/metis/pkg/skill"
)

// bundledFS holds the SKILL.md files Metis ships built-in. Embedded at
// build time via go:embed; the contents are sourced from the
// `internal/agent/skills/builtin/` directory (NOT the repo-root /skills/
// directory — that's deliberately kept as a historical artifact path,
// while the canonical authored location is sibling to this code).
//
//go:embed builtin/*.md
var bundledFS embed.FS

// bundledLayer is the priority-0 layer of the multi-source loader. Every
// .md under `builtin/` is parsed once and held in memory — bundled skills
// are immutable for the binary's lifetime, so no Invalidate cycle.
func bundledLayer() Layer {
	return Layer{
		Name:     "bundled",
		Priority: 0,
		Trust:    pubskill.TrustBuiltin,
		Scan: func() ([]Skill, error) {
			var out []Skill
			err := fs.WalkDir(bundledFS, "builtin", func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if isJunkFilename(filepath.Base(path)) {
					return nil
				}
				if strings.ToLower(filepath.Ext(path)) != ".md" {
					return nil
				}
				b, rerr := bundledFS.ReadFile(path)
				if rerr != nil {
					return nil // skip unreadable; non-fatal at startup
				}
				name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				sk, perr := parseMarkdown(b, name)
				if perr != nil {
					return nil // skip malformed bundled skill rather than blocking startup
				}
				out = append(out, *sk)
				return nil
			})
			if err != nil {
				return nil, err
			}
			return out, nil
		},
	}
}
