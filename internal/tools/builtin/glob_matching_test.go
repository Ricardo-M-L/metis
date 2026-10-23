package builtin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestGlobRootRelativeMatching(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"package.json", "apps/web/package.json", "apps/api/package.json",
		"apps/web/nested/package.json", "native/system/direct.c",
		"native/system/packages/entry/src/main.c", "native/system/packages/entry/src/main.h",
		"native/system/packages/entry/src/main.rs", "native/system/packages/entry/src/main.cc",
		"native/system/packages/entry/src/main.txt", "src/a.go", "src/b.go", "src/z.go",
		"root.go", "nested/nested.go", ".config/settings.json", "中文/入口.go",
	}
	for _, file := range files {
		writeGlobFile(t, root, file)
	}
	cases := []struct {
		name, pattern string
		want          []string
	}{
		{"directory segments without doublestar", "apps/*/package.json", []string{"apps/api/package.json", "apps/web/package.json"}},
		{"absolute root with relative pattern", "native/system/**/*.c", []string{"native/system/direct.c", "native/system/packages/entry/src/main.c"}},
		{"brace alternatives", "native/system/**/*.{c,cc,h,rs}", []string{"native/system/direct.c", "native/system/packages/entry/src/main.c", "native/system/packages/entry/src/main.cc", "native/system/packages/entry/src/main.h", "native/system/packages/entry/src/main.rs"}},
		{"doublestar zero and multiple levels", "**/package.json", []string{"package.json", "apps/web/package.json", "apps/api/package.json", "apps/web/nested/package.json"}},
		{"filename keeps recursive compatibility", "*.go", []string{"src/a.go", "src/b.go", "src/z.go", "root.go", "nested/nested.go", "中文/入口.go"}},
		{"explicit dot anchors root", "./*.go", []string{"root.go"}},
		{"character classes", "src/[ab].go", []string{"src/a.go", "src/b.go"}},
		{"question mark and negated class", "src/[!ab].?o", []string{"src/z.go"}},
		{"hidden directories remain walkable", "**/settings.json", []string{".config/settings.json"}},
		{"unicode path", "中文/*.go", []string{"中文/入口.go"}},
		{"absolute pattern under search root", filepath.Join(root, "src", "[ab].go"), []string{"src/a.go", "src/b.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": tc.pattern, "max_depth": 0})
			if err != nil || res.IsError {
				t.Fatalf("Execute: err=%v result=%+v", err, res)
			}
			want := make([]string, len(tc.want))
			for i, file := range tc.want {
				want[i] = filepath.Join(root, filepath.FromSlash(file))
			}
			assertGlobPaths(t, res.Output, want)
		})
	}
}

func TestGlobWorkspaceAndRelativeRoots(t *testing.T) {
	workspace := t.TempDir()
	other := t.TempDir()
	writeGlobFile(t, workspace, "apps/web/package.json")
	writeGlobFile(t, workspace, "apps/web/src/main.go")
	writeGlobFile(t, other, "apps/wrong/package.json")
	t.Chdir(other)
	ctx := agent.WithCwd(context.Background(), workspace)
	for _, tc := range []struct {
		root, pattern, want string
	}{
		{".", "apps/*/package.json", "apps/web/package.json"},
		{"apps", "*/package.json", "apps/web/package.json"},
		{"apps/web", "src/*.go", "apps/web/src/main.go"},
	} {
		res, err := (Glob{}).Execute(ctx, map[string]any{"root": tc.root, "pattern": tc.pattern, "max_depth": 0})
		if err != nil || res.IsError {
			t.Fatalf("root %q: err=%v result=%+v", tc.root, err, res)
		}
		assertGlobPaths(t, res.Output, []string{filepath.FromSlash(tc.want)})
	}
}

func TestGlobRejectsInvalidPatternsBeforeWalking(t *testing.T) {
	for _, pattern := range []string{"[", "[]", "{a,b", "a}", "trailing\\"} {
		if filepath.Separator == '\\' && pattern == "trailing\\" {
			continue // A backslash is a path separator, not an escape, on Windows.
		}
		t.Run(pattern, func(t *testing.T) {
			res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": filepath.Join(t.TempDir(), "absent"), "pattern": pattern})
			if err != nil || res == nil || !res.IsError || !strings.Contains(strings.ToLower(res.Output), "invalid pattern") {
				t.Fatalf("malformed pattern must be an actionable tool error, got err=%v result=%+v", err, res)
			}
		})
	}
}

func TestGlobPatternComplexityIsBounded(t *testing.T) {
	for _, pattern := range []string{
		strings.Repeat("x", maxGlobPatternBytes+1),
		strings.Repeat("{a,b}", 9),
		strings.Repeat("{", maxGlobBraceDepth+1) + "a" + strings.Repeat("}", maxGlobBraceDepth+1),
	} {
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": t.TempDir(), "pattern": pattern})
		if err != nil || res == nil || !res.IsError || !strings.Contains(res.Output, "invalid pattern") {
			t.Fatalf("complex pattern must be rejected before walking: err=%v result=%+v", err, res)
		}
	}
	for _, pattern := range []string{"{a,b}{c,d}", "{a,{b,{c,d}}}", "[{},]", "*.{c,cc,h,rs}"} {
		if _, err := newGlobMatcher(pattern); err != nil {
			t.Fatalf("ordinary pattern %q was rejected: %v", pattern, err)
		}
	}
}

func TestGlobSingleFileRootAndInvalidLimit(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, root, "hit.go")
	file := filepath.Join(root, "hit.go")
	res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": file, "pattern": "*.go"})
	if err != nil || res.IsError {
		t.Fatalf("single file root: err=%v result=%+v", err, res)
	}
	assertGlobPaths(t, res.Output, []string{file})
	for _, limit := range []int{-1, 0} {
		res, err = (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "*.go", "limit": limit})
		if err != nil || res == nil || !res.IsError || !strings.Contains(res.Output, "limit") {
			t.Fatalf("limit=%d: err=%v result=%+v", limit, err, res)
		}
	}
}

func TestGlobMatcherPlatformSeparatorsAndEscapes(t *testing.T) {
	root := t.TempDir()
	if filepath.Separator == '\\' {
		matcher, err := newGlobMatcher(`src\**\*.go`)
		if err != nil || !matcher.matches(filepath.Join("src", "main.go"), filepath.Join(root, "src", "main.go")) {
			t.Fatalf("Windows separators must normalize: matcher=%+v err=%v", matcher, err)
		}
	} else {
		writeGlobFile(t, root, "literal[1].go")
		res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": `literal\[1\].go`})
		if err != nil || res.IsError {
			t.Fatalf("literal escaped brackets: err=%v result=%+v", err, res)
		}
		assertGlobPaths(t, res.Output, []string{filepath.Join(root, "literal[1].go")})
	}
}

func TestGrepGlobFilterRootRelativeMatching(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"package.json", "apps/web/package.json", "apps/api/package.json", "apps/web/other.txt"} {
		writeGlobFile(t, root, file)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file)), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		pattern string
		count   int
	}{
		{"apps/*/package.json", 2},
		{"**/package.json", 3},
		{"apps/**/*.{json,txt}", 3},
		{"*.json", 3},
		{"./*.json", 1},
		{filepath.Join(root, "apps", "*", "package.json"), 2},
	} {
		res, err := (Grep{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "needle", "glob": tc.pattern})
		if err != nil || res.IsError || strings.Count(res.Output, ":1:needle") != tc.count {
			t.Fatalf("glob %q: err=%v result=%+v; want %d matching lines", tc.pattern, err, res, tc.count)
		}
	}
	res, err := (Grep{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "needle", "glob": "["})
	if err != nil || res == nil || !res.IsError || !strings.Contains(res.Output, "invalid pattern in glob filter") {
		t.Fatalf("malformed Grep glob: err=%v result=%+v", err, res)
	}
}

func TestGlobPreservesWalkBoundsAndOrdering(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"old.go", "new.go", "nested/deep.go", "node_modules/hidden.go", ".git/hidden.go", "build/hidden.go"} {
		writeGlobFile(t, root, file)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "old.go"), old, old); err != nil {
		t.Fatal(err)
	}
	newest := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "new.go"), newest, newest); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		depth int
		limit int
		want  []string
	}{
		{"depth and heavy skip", 1, 10, []string{filepath.Join(root, "new.go"), filepath.Join(root, "old.go")}},
		{"result limit retains newest", 0, 1, []string{filepath.Join(root, "new.go")}},
		{"unlimited depth preserves heavy skip", 0, 10, []string{filepath.Join(root, "new.go"), filepath.Join(root, "old.go"), filepath.Join(root, "nested", "deep.go")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := (Glob{}).Execute(context.Background(), map[string]any{"root": root, "pattern": "**/*.go", "max_depth": tc.depth, "limit": tc.limit})
			if err != nil || res.IsError {
				t.Fatalf("Execute: err=%v result=%+v", err, res)
			}
			assertGlobPaths(t, res.Output, tc.want)
			if !strings.HasPrefix(res.Output, filepath.Join(root, "new.go")+"\n") {
				t.Fatalf("newest match must come first: %q", res.Output)
			}
		})
	}
}

func TestGlobHonorsCancelledContext(t *testing.T) {
	root := t.TempDir()
	writeGlobFile(t, root, "hit.go")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Glob{}).Execute(ctx, map[string]any{"root": root, "pattern": "**/*.go"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search returned %v; want context.Canceled", err)
	}
}

func writeGlobFile(t *testing.T, root, file string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertGlobPaths(t *testing.T, output string, want []string) {
	t.Helper()
	got := strings.Split(strings.TrimSpace(output), "\n")
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%q; want %q", got, want)
	}
}
