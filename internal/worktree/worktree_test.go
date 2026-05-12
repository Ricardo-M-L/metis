package worktree

import (
	"strings"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	good := []string{"feature-x", "fix.123", "release_2026", "stack/page", "a"}
	for _, s := range good {
		if err := validateSlug(s); err != nil {
			t.Errorf("validateSlug(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "..", "a/..", "/leading", "trailing/", "with space", strings.Repeat("a", 65)}
	for _, s := range bad {
		if err := validateSlug(s); err == nil {
			t.Errorf("validateSlug(%q) should error", s)
		}
	}
}

func TestBranchName(t *testing.T) {
	if got := branchName("feature-x"); got != "metis/feature-x" {
		t.Errorf("branchName: %q", got)
	}
}

// TestAutoSlugUnique — the 2026-05-12 collision fix. The previous
// `time.Now().UnixNano() & 0xfffffff` masked slug collided when two
// sub-agents spawned within the same nanosecond from G.1's parallel
// dispatch. Switched to crypto/rand-backed hex so 1000 rapid calls
// produce 1000 distinct slugs.
func TestAutoSlugUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		s := AutoSlug()
		if !strings.HasPrefix(s, "wt-") {
			t.Fatalf("iter %d: AutoSlug missing prefix: %q", i, s)
		}
		if err := validateSlug(s); err != nil {
			t.Fatalf("iter %d: AutoSlug %q failed validation: %v", i, s, err)
		}
		if seen[s] {
			t.Fatalf("iter %d: duplicate slug %q (collision broke parallel-spawn safety)", i, s)
		}
		seen[s] = true
	}
}
