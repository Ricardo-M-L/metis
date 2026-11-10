package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestReadVisibilityLongLineIsBoundedAndCannotGrantOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "single-line.txt")
	data := "BEGIN_UNIQUE_" + strings.Repeat("Q", 60000) + "_HIDDEN_TAIL"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	gate := permission.New(permission.ModeFullAccess)
	rd, wr := Read{gate: gate, state: state}, Write{gate: gate, state: state}
	res, err := rd.Execute(context.Background(), map[string]any{"path": path, "limit": 1})
	if err != nil || res.IsError {
		t.Fatalf("Read: %+v, %v", res, err)
	}
	if len(res.Output) > 10000 {
		t.Errorf("Read returned unbounded %d-byte output", len(res.Output))
	}
	if !strings.Contains(res.Output, "byte_offset") {
		t.Errorf("missing source-page recovery instructions")
	}
	entry, _ := state.Get(path)
	if !entry.IsPartialView {
		t.Error("bounded/incomplete source must remain a partial view")
	}
	write, err := wr.Execute(context.Background(), map[string]any{"path": path, "content": "replacement"})
	if err != nil || !write.IsError || !strings.Contains(write.Output, FilePartialViewNotEditable) {
		t.Errorf("whole-file overwrite accepted without a visible full read: %+v, %v", write, err)
	}
	res, err = rd.Execute(context.Background(), map[string]any{"path": path})
	if err != nil || strings.Contains(res.Output, "File unchanged since last read") {
		t.Errorf("partial view incorrectly produced unchanged stub: %+v, %v", res, err)
	}
}

func TestReadVisibilityBytePagesReachTailAndKeepUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unicode.txt")
	data := []byte(strings.Repeat("中🙂x", 1800) + "TAIL_CANARY")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	rd := Read{gate: permission.New(permission.ModeFullAccess), state: state}
	start, pages := 0, 0
	for start < len(data) && pages < 30 {
		res, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": start, "byte_limit": 1024})
		if err != nil || res.IsError {
			t.Fatalf("page: %+v, %v", res, err)
		}
		if !utf8.ValidString(res.Output) {
			t.Fatal("page split a UTF-8 rune")
		}
		page, ok := res.Presentation["read_page"].(map[string]any)
		if !ok {
			t.Fatalf("missing byte page metadata: %+v", res.Presentation)
		}
		pageStart, end := intArg(page, "start", -1), intArg(page, "end", -1)
		if pageStart != start || end <= start || end > len(data) {
			t.Fatalf("non-progressing or overlapping source page: %+v", page)
		}
		if !strings.Contains(res.Output, string(data[start:end])) {
			t.Fatal("source page content was not visible")
		}
		start = end
		pages++
	}
	if start != len(data) || pages < 2 {
		t.Fatalf("could not reach EOF: offset=%d/%d pages=%d", start, len(data), pages)
	}
	entry, _ := state.Get(path)
	if entry.IsPartialView {
		t.Fatal("all sequential, fully visible pages did not restore a whole-file view")
	}
}

func TestReadVisibilityProjectionObserverInvalidatesOnlyItsOwnSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.txt")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	rd := Read{gate: permission.New(permission.ModeFullAccess), state: state}
	observer, ok := any(rd).(interface {
		ObserveResultProjection(context.Context, map[string]any, bool)
	})
	if !ok {
		t.Fatal("Read has no model-result projection observer")
	}
	first, err := rd.Execute(context.Background(), map[string]any{"path": path, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	observer.ObserveResultProjection(context.Background(), first.Presentation, false)
	entry, _ := state.Get(path)
	if !entry.IsPartialView {
		t.Fatal("spilled full result retained overwrite authority")
	}
	second, err := rd.Execute(context.Background(), map[string]any{"path": path, "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	observer.ObserveResultProjection(context.Background(), second.Presentation, true)
	observer.ObserveResultProjection(context.Background(), first.Presentation, false)
	entry, _ = state.Get(path)
	if entry.IsPartialView {
		t.Fatal("older spilled result downgraded a newer fully visible snapshot")
	}
}

func TestReadVisibilityWaitsForModelReceiptBeforeGrantingWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.txt")
	if err := os.WriteFile(path, []byte("complete\nsource\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	gate := permission.New(permission.ModeFullAccess)
	rd, wr := Read{gate: gate, state: state}, Write{gate: gate, state: state}
	ctx := tools.WithResultProjection(context.Background())
	res, err := rd.Execute(ctx, map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	before, err := wr.Execute(context.Background(), map[string]any{"path": path, "content": "too soon"})
	if err != nil || !before.IsError {
		t.Fatalf("unprojected Read granted Write: %+v %v", before, err)
	}
	rd.ObserveResultProjection(ctx, res.Presentation, true)
	after, err := wr.Execute(context.Background(), map[string]any{"path": path, "content": "allowed after receipt"})
	if err != nil || after.IsError {
		t.Fatalf("complete model-visible Read did not grant Write: %+v %v", after, err)
	}
}

func TestReadVisibilityManyShortLinesBoundRenderedOverhead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many-lines.txt")
	data := strings.Repeat("a\n", 2000)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	res, err := (Read{gate: permission.New(permission.ModeFullAccess), state: state}).Execute(context.Background(), map[string]any{"path": path})
	if err != nil || res.IsError {
		t.Fatalf("Read: %+v %v", res, err)
	}
	if len(res.Output) > 10000 || !strings.Contains(res.Output, data) {
		t.Fatalf("line prefixes manufactured an oversized/lossy result: %d", len(res.Output))
	}
	entry, _ := state.Get(path)
	if entry.IsPartialView {
		t.Fatal("complete compact byte view of short source became partial")
	}
}

func TestReadVisibilityByteRedactionAcrossPageBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redacted.txt")
	secret := strings.Repeat("K", 128)
	data := []byte("-----BEGIN PRIVATE KEY-----\n" + secret + "\n-----END PRIVATE KEY-----\n" + strings.Repeat("中", 4000) + "TAIL_VISIBLE")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	rd := Read{gate: permission.New(permission.ModeFullAccess), state: NewReadFileState()}
	start := 0
	for pages := 0; start < len(data) && pages < 30; pages++ {
		res, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": start, "byte_limit": 1024})
		if err != nil || res.IsError {
			t.Fatalf("page: %+v %v", res, err)
		}
		if strings.Contains(res.Output, strings.Repeat("K", 16)) || !utf8.ValidString(res.Output) {
			t.Fatal("page exposed credential bytes or split UTF8")
		}
		page := res.Presentation["read_page"].(map[string]any)
		end := intArg(page, "end", -1)
		if end <= start {
			t.Fatalf("redacted page made no progress: %+v", page)
		}
		if end == len(data) && !strings.Contains(res.Output, "TAIL_VISIBLE") {
			t.Fatal("redaction lost unrelated tail")
		}
		start = end
	}
	if start != len(data) {
		t.Fatal("redacted source could not reach EOF")
	}
	entry, _ := rd.state.Get(path)
	if !entry.IsPartialView {
		t.Fatal("redacted pages granted whole-file authority")
	}
}

func TestReadVisibilityRedactionExpansionKeepsPublicTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expanded-redactions.txt")
	data := []byte(strings.Repeat("password:\n  x\n", 500) + "PUBLIC_UNRELATED_TAIL_CANARY")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	rd := Read{gate: permission.New(permission.ModeFullAccess), state: NewReadFileState()}
	start, tailVisible := 0, false
	for pages := 0; start < len(data) && pages < 30; pages++ {
		res, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": start, "byte_limit": 8192})
		if err != nil || res.IsError {
			t.Fatalf("Read page: %+v %v", res, err)
		}
		page := res.Presentation["read_page"].(map[string]any)
		end := intArg(page, "end", -1)
		if end <= start || end > len(data) || len(res.Output) > 10000 {
			t.Fatalf("invalid fitted page: range=%d-%d output=%d", start, end, len(res.Output))
		}
		if strings.Contains(res.Output, "  x\n") {
			t.Fatal("page exposed the credential value")
		}
		tailVisible = tailVisible || strings.Contains(res.Output, "PUBLIC_UNRELATED_TAIL_CANARY")
		if end == len(data) && !tailVisible {
			t.Fatalf("reported EOF while silently dropping public tail; source=%d output=%d", len(data), len(res.Output))
		}
		start = end
	}
	if start != len(data) || !tailVisible {
		t.Fatal("redaction-expanded source did not make progress to public tail")
	}
}

func TestReadVisibilityCompleteConfirmedPagesRestoreWholeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all-pages.txt")
	data := []byte(strings.Repeat("page-body-", 2400))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	gate := permission.New(permission.ModeFullAccess)
	rd, wr := Read{gate: gate, state: state}, Write{gate: gate, state: state}
	ctx := tools.WithResultProjection(context.Background())
	for start := 0; start < len(data); {
		res, err := rd.Execute(ctx, map[string]any{"path": path, "byte_offset": start, "byte_limit": 4096})
		if err != nil || res.IsError {
			t.Fatalf("Read: %+v %v", res, err)
		}
		end := intArg(res.Presentation["read_page"].(map[string]any), "end", -1)
		if end <= start {
			t.Fatal("Read made no progress")
		}
		if end == len(data) {
			before, err := wr.Execute(context.Background(), map[string]any{"path": path, "content": "NOT_YET"})
			if err != nil || !before.IsError {
				t.Fatalf("unconfirmed last page granted Write: %+v %v", before, err)
			}
		}
		rd.ObserveResultProjection(ctx, res.Presentation, true)
		start = end
	}
	result, err := wr.Execute(context.Background(), map[string]any{"path": path, "content": "FULLY_READ_REPLACEMENT"})
	if err != nil || result.IsError {
		t.Fatalf("complete visible coverage did not restore Write: %+v %v", result, err)
	}
}

func TestReadVisibilityMissingMiddleAndChangedHashDoNotGrantWrite(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_middle", true: "changed_hash"}[changed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "incomplete.txt")
			data := []byte(strings.Repeat("abcdef", 5000))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			state := NewReadFileState()
			gate := permission.New(permission.ModeFullAccess)
			rd, wr := Read{gate: gate, state: state}, Write{gate: gate, state: state}
			first, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": 0, "byte_limit": 4096})
			if err != nil || first.IsError {
				t.Fatal(err)
			}
			if changed {
				data[0] = 'Z'
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				for start := 4096; start < len(data); {
					res, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": start, "byte_limit": 4096})
					if err != nil || res.IsError {
						t.Fatalf("Read: %+v %v", res, err)
					}
					start = intArg(res.Presentation["read_page"].(map[string]any), "end", -1)
				}
			} else {
				if _, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": len(data) - 4096, "byte_limit": 4096}); err != nil {
					t.Fatal(err)
				}
			}
			res, err := wr.Execute(context.Background(), map[string]any{"path": path, "content": "MUST_NOT_OVERWRITE"})
			if err != nil || !res.IsError {
				t.Fatalf("incomplete current-version coverage granted Write: %+v %v", res, err)
			}
		})
	}
}

func TestReadVisibilityBytePageFinalRedactionCannotGrantCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inline-token.txt")
	secret := "ghp_" + strings.Repeat("G", 36)
	data := []byte(strings.Repeat("ordinary ", 1200) + secret)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	rd := Read{gate: permission.New(permission.ModeFullAccess), state: state}
	for start := 0; start < len(data); {
		res, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": start, "byte_limit": 8192})
		if err != nil || res.IsError {
			t.Fatalf("Read: %+v %v", res, err)
		}
		if strings.Contains(res.Output, secret) {
			t.Fatal("byte page exposed inline token")
		}
		start = intArg(res.Presentation["read_page"].(map[string]any), "end", -1)
		if start < 0 {
			t.Fatal("missing source page offset")
		}
	}
	entry, _ := state.Get(path)
	if !entry.IsPartialView {
		t.Fatal("final-redacted page was counted as complete source coverage")
	}
}

func TestReadVisibilityByteFragmentsRetainCredentialContext(t *testing.T) {
	for _, tc := range []struct{ name, source, value string }{
		{"token_prefix", "prefix ghp_" + strings.Repeat("G", 36) + " PUBLIC_TAIL", strings.Repeat("G", 36)},
		{"assignment_key", "password=ordinary-private-value PUBLIC_TAIL", "ordinary-private-value"},
		{"json_key", `{"client_secret":"ordinary-private-value","public":"PUBLIC_TAIL"}`, "ordinary-private-value"},
		{"authorization", "Authorization: Bearer ordinary-private-value\nPUBLIC_TAIL", "ordinary-private-value"},
		{"url_userinfo", "postgres://tester:ordinary-private-value@localhost/PUBLIC_TAIL", "ordinary-private-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fragment.txt")
			if err := os.WriteFile(path, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			start := strings.Index(tc.source, tc.value) + 4
			fragment := tc.source[start : start+8]
			state := NewReadFileState()
			rd := Read{gate: permission.New(permission.ModeFullAccess), state: state}
			res, err := rd.Execute(context.Background(), map[string]any{"path": path, "byte_offset": start, "byte_limit": 8})
			if err != nil || res.IsError {
				t.Fatalf("Read fragment: %+v %v", res, err)
			}
			if strings.Contains(res.Output, fragment) || !strings.Contains(res.Output, "[REDACTED]") {
				t.Fatal("byte fragment lost enclosing credential context")
			}
			entry, _ := state.Get(path)
			if !entry.IsPartialView {
				t.Fatal("credential fragment granted whole-file coverage")
			}
		})
	}
}
