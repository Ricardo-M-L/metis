package tui

import (
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
)

// skillCatalogLoader returns the exact loader used by the live Skill tool.
// That pointer includes universal, project, and late-loaded plugin layers.
// The fallback keeps isolated unit tests and legacy embedders deterministic.
func skillCatalogLoader(loop *agent.Loop, userDir string) *skills.Loader {
	if loop != nil && loop.Registry != nil {
		if tool, ok := loop.Registry.Get("Skill"); ok {
			if provider, ok := tool.(interface {
				CatalogLoader() *skills.Loader
			}); ok {
				if loader := provider.CatalogLoader(); loader != nil {
					return loader
				}
			}
		}
	}
	return skills.NewLoader(userDir, "", nil)
}

func loadSkillCatalog(loop *agent.Loop, userDir string) ([]skills.Skill, error) {
	loader := skillCatalogLoader(loop, userDir)
	// External package managers can change ~/.agents/skills while Metis is
	// running. Explicit UI reads must therefore refresh rather than show the
	// loader's previous cache until restart.
	loader.Invalidate()
	return loader.List()
}
