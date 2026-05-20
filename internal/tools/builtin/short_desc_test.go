package builtin

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools/builtin/bash"
)

// shortDescriptor is the optional interface — the real definition
// lives in pkg/tool, but this file is in internal/tools/builtin (no
// agent dep) so we restate it locally to avoid pulling tools.Tool
// transitively. Same type signature: this works as a structural
// check.
type shortDescriptor interface {
	ShortDescription() string
}

func TestCoreTools_HaveShortDescription(t *testing.T) {
	cases := []struct {
		name string
		tool shortDescriptor
	}{
		{"Bash", bash.Bash{}},
		{"Edit", Edit{}},
		{"Write", Write{}},
		{"Read", Read{}},
		{"Agent", Agent{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			short := c.tool.ShortDescription()
			if len(short) < 50 {
				t.Errorf("%s.ShortDescription too short (%d chars): %q", c.name, len(short), short)
			}
			if len(short) > 600 {
				t.Errorf("%s.ShortDescription too long (%d chars) — defeats the purpose", c.name, len(short))
			}
			if strings.TrimSpace(short) == "" {
				t.Errorf("%s.ShortDescription is blank", c.name)
			}
		})
	}
}

// TestBash_ShortDescription_HasRedirectHint: the short Bash form must
// keep at least one tool-redirect hint so sub-agents that don't see
// the full `# Tool selection` table in base.md still get nudged away
// from cat/find when Bash is the wrong choice.
func TestBash_ShortDescription_HasRedirectHint(t *testing.T) {
	short := bash.Bash{}.ShortDescription()
	if !strings.Contains(strings.ToLower(short), "read") {
		t.Errorf("Bash short desc should mention Read as a redirect; got:\n%s", short)
	}
}
