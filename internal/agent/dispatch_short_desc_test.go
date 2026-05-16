package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

// fakeToolNoShort doesn't implement ShortDescriptor — used to verify
// the fallback to legacy first-paragraph truncation.
type fakeToolNoShort struct {
	name     string
	fullDesc string
}

func (f fakeToolNoShort) Name() string                { return f.name }
func (f fakeToolNoShort) Description() string         { return f.fullDesc }
func (f fakeToolNoShort) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (f fakeToolNoShort) Concurrency(_ map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (f fakeToolNoShort) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (f fakeToolNoShort) Execute(_ context.Context, _ map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "ok"}, nil
}

// fakeToolWithShort implements ShortDescriptor — verify the short
// path is taken when requested.
type fakeToolWithShort struct {
	fakeToolNoShort
	shortDesc string
}

func (f fakeToolWithShort) ShortDescription() string { return f.shortDesc }

func TestDescriptionForTool_FallbackWhenNoShortDescriptor(t *testing.T) {
	full := "First paragraph here that says enough.\n\nSecond paragraph with extra detail.\n\nThird."
	fake := fakeToolNoShort{name: "Fake", fullDesc: full}
	got := descriptionForTool(fake, true)
	if got != "First paragraph here that says enough." {
		t.Errorf("expected first-paragraph truncation as fallback; got:\n%s", got)
	}
}

func TestDescriptionForTool_ShortHitsImplementer(t *testing.T) {
	fake := fakeToolWithShort{
		fakeToolNoShort: fakeToolNoShort{name: "Fake", fullDesc: "Long description with multiple paragraphs.\n\nSecond para has detail."},
		shortDesc:       "Short curated form.",
	}
	if got := descriptionForTool(fake, true); got != "Short curated form." {
		t.Errorf("with ShortDescriptor + short=true, expected curated short; got:\n%s", got)
	}
	// short=false should NOT call ShortDescription, falls back to truncation
	if got := descriptionForTool(fake, false); got == "Short curated form." {
		t.Errorf("with short=false, should not use ShortDescription; got:\n%s", got)
	}
}

func (fakeToolNoShort) IsEnabled() bool { return true }

func (fakeToolWithShort) IsEnabled() bool { return true }
