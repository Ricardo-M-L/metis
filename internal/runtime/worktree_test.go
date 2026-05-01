package runtime

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

func TestAutoSlugIsUnique(t *testing.T) {
	a := autoSlug()
	if !strings.HasPrefix(a, "wt-") {
		t.Errorf("autoSlug missing prefix: %q", a)
	}
	if err := validateSlug(a); err != nil {
		t.Errorf("autoSlug %q failed validation: %v", a, err)
	}
}
