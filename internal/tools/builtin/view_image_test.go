package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

// writeTinyPNG writes a 2x2 RGBA PNG to dir/name and returns the full
// path. Tiny enough that the encoded bytes (≈70 B) stay nowhere near
// any size cap; we only need a valid PNG for http.DetectContentType
// to return "image/png".
func writeTinyPNG(t *testing.T, dir, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 1, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestViewImage_HappyPath — a real PNG on disk produces a Result with
// the textual summary in Output AND a single ImageAttachment whose
// Data is valid base64 of the original bytes.
func TestViewImage_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := writeTinyPNG(t, dir, "tiny.png")

	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	res, err := v.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("happy-path result marked IsError; Output=%q", res.Output)
	}
	if !strings.Contains(res.Output, "image/png") {
		t.Errorf("Output should mention mime type; got %q", res.Output)
	}
	if !strings.Contains(res.Output, path) {
		t.Errorf("Output should echo the absolute path; got %q", res.Output)
	}
	if len(res.Images) != 1 {
		t.Fatalf("expected 1 image attachment; got %d", len(res.Images))
	}
	img := res.Images[0]
	if img.MediaType != "image/png" {
		t.Errorf("MediaType=%q, want image/png", img.MediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	original, _ := os.ReadFile(path)
	if !bytes.Equal(decoded, original) {
		t.Errorf("base64 round-trip lost data: decoded=%d B, original=%d B", len(decoded), len(original))
	}
}

// TestViewImage_RelativePathResolved — relative paths should be
// promoted via filepath.Abs so the model never sees a path it can't
// re-use after a `cd`.
func TestViewImage_RelativePathResolved(t *testing.T) {
	dir := t.TempDir()
	writeTinyPNG(t, dir, "rel.png")
	prevCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevCwd) })

	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	res, err := v.Execute(context.Background(), map[string]any{"path": "rel.png"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Output, "ViewImage: /") {
		t.Errorf("Output should embed absolute path; got %q", res.Output)
	}
}

// TestViewImage_RejectsNonImage — a text file with .png extension is
// still text per http.DetectContentType; the tool must reject it
// without crashing.
func TestViewImage_RejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(path, []byte("this is plain text, not a PNG"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	res, err := v.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute returned err (want soft error in Result): %v", err)
	}
	if !res.IsError {
		t.Errorf("non-image should set IsError=true; got %+v", res)
	}
	if len(res.Images) != 0 {
		t.Errorf("non-image should attach no images; got %d", len(res.Images))
	}
	if !strings.Contains(res.Output, "not a supported image") {
		t.Errorf("Output should explain the rejection; got %q", res.Output)
	}
}

// TestViewImage_MissingPath — empty path arg → hard error so the
// model gets a clear schema-violation message on the next turn.
func TestViewImage_MissingPath(t *testing.T) {
	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	_, err := v.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path; got nil")
	}
}

// TestViewImage_MissingFile — referenced file doesn't exist; tool
// surfaces the os.Stat error verbatim so the model can recover.
func TestViewImage_MissingFile(t *testing.T) {
	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	_, err := v.Execute(context.Background(), map[string]any{
		"path": "/this/path/does/not/exist.png",
	})
	if err == nil {
		t.Fatal("expected error for missing file; got nil")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("error should mention stat; got %q", err.Error())
	}
}

// TestViewImage_DirectoryRejected — pointing at a directory is a
// schema violation, not a vision request.
func TestViewImage_DirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	v := ViewImage{gate: permission.New(permission.ModeBypass)}
	_, err := v.Execute(context.Background(), map[string]any{"path": dir})
	if err == nil {
		t.Fatal("expected error for directory arg; got nil")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention directory; got %q", err.Error())
	}
}

// TestViewImage_RegisteredAndEnabled — the tool must land in the
// shared builtin registry under its canonical name, otherwise the
// model never sees it.
func TestViewImage_RegisteredAndEnabled(t *testing.T) {
	v := ViewImage{}
	if v.Name() != "ViewImage" {
		t.Errorf("Name() = %q, want ViewImage", v.Name())
	}
	if !v.IsEnabled() {
		t.Error("IsEnabled() = false; ViewImage has no env dep and should always be enabled")
	}
	if v.Concurrency(nil) != 0 {
		// ConcurrencySafe == 0; using 0 instead of importing tools to
		// avoid a self-import-only-for-constant.
		t.Errorf("Concurrency = %v, want ConcurrencySafe", v.Concurrency(nil))
	}
}
