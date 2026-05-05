package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// TestGrep_OffsetPagination feeds a directory of N matching lines and
// verifies offset+max correctly slice out a window.
func TestGrep_OffsetPagination(t *testing.T) {
	tmp := t.TempDir()
	// 50 matching lines across 5 files.
	for i := 0; i < 5; i++ {
		var b strings.Builder
		for j := 0; j < 10; j++ {
			b.WriteString("FOO line\n")
		}
		os.WriteFile(filepath.Join(tmp, "f"+string(rune('0'+i))+".txt"), []byte(b.String()), 0o644)
	}
	gate := permission.New(permission.ModeBypass)
	g := Grep{gate: gate}

	// Page 1: offset=0, max=10
	res1, err := g.Execute(context.Background(), map[string]any{
		"pattern": "FOO", "root": tmp, "max": 10, "offset": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	count1 := strings.Count(res1.Output, "FOO line")
	if count1 != 10 {
		t.Errorf("page 1: got %d matches, want 10", count1)
	}
	if !strings.Contains(res1.Output, "[truncated") {
		t.Errorf("page 1: should mention truncation, got: %s", res1.Output)
	}
	if !strings.Contains(res1.Output, "offset=10") {
		t.Errorf("page 1: should advise offset=10, got: %s", res1.Output)
	}

	// Page 2: offset=10, max=10
	res2, err := g.Execute(context.Background(), map[string]any{
		"pattern": "FOO", "root": tmp, "max": 10, "offset": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	count2 := strings.Count(res2.Output, "FOO line")
	if count2 != 10 {
		t.Errorf("page 2: got %d matches, want 10", count2)
	}
}

// TestGrep_DefaultLimitWhenAbsent: no `max` arg → uses DefaultGrepLimit (250).
func TestGrep_DefaultLimitWhenAbsent(t *testing.T) {
	tmp := t.TempDir()
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("hit\n")
	}
	os.WriteFile(filepath.Join(tmp, "big.txt"), []byte(b.String()), 0o644)

	gate := permission.New(permission.ModeBypass)
	g := Grep{gate: gate}
	res, err := g.Execute(context.Background(), map[string]any{
		"pattern": "hit", "root": tmp,
	})
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(res.Output, "hit\n")
	// Some of the "hit\n" instances may overlap our pattern in the
	// truncation footer, so allow ±2 slop.
	if count < DefaultGrepLimit-2 || count > DefaultGrepLimit+2 {
		t.Errorf("default limit not honoured: got %d matches, want ≈%d", count, DefaultGrepLimit)
	}
	if !strings.Contains(res.Output, "[truncated") {
		t.Errorf("expected truncation footer, got: %s", res.Output)
	}
}

// TestGrep_UnlimitedWithMaxZero: max=0 disables the cap entirely.
func TestGrep_UnlimitedWithMaxZero(t *testing.T) {
	tmp := t.TempDir()
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("hit\n")
	}
	os.WriteFile(filepath.Join(tmp, "big.txt"), []byte(b.String()), 0o644)

	gate := permission.New(permission.ModeBypass)
	g := Grep{gate: gate}
	res, err := g.Execute(context.Background(), map[string]any{
		"pattern": "hit", "root": tmp, "max": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(res.Output, "hit\n")
	if count < 300 {
		t.Errorf("max=0 should be unlimited; got %d matches", count)
	}
	if strings.Contains(res.Output, "[truncated") {
		t.Errorf("max=0 shouldn't show truncation: %s", res.Output)
	}
}

// TestGrep_NoTruncationFooterWhenAllFit: when results fit under max,
// no [truncated] line is emitted (avoids polluting context).
func TestGrep_NoTruncationFooterWhenAllFit(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("only one\n"), 0o644)
	gate := permission.New(permission.ModeBypass)
	g := Grep{gate: gate}
	res, err := g.Execute(context.Background(), map[string]any{
		"pattern": "only", "root": tmp, "max": 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "[truncated") {
		t.Errorf("should not show truncation when all fit: %s", res.Output)
	}
}
