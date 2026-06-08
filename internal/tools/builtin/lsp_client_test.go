package builtin

// lsp_client_test.go — deterministic coverage for the LSP response
// parsers and URI helpers. The full spawn→initialize→query exchange
// needs a real language server, so it's exercised manually / in the tmux
// suite; here we pin the pure functions that turn server JSON into the
// tool's output, plus the framing-adjacent helpers.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPathURIRoundTrip(t *testing.T) {
	p := "/Users/x/proj/main.py"
	if got := uriToPath(pathToURI(p)); got != p {
		t.Errorf("round trip: got %q want %q", got, p)
	}
	// Non-file URIs pass through untouched.
	if got := uriToPath("untitled:Untitled-1"); got != "untitled:Untitled-1" {
		t.Errorf("non-file URI should pass through, got %q", got)
	}
}

func TestFormatLSPHover_Shapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"null", `null`, ""},
		{"markup", `{"contents":{"kind":"markdown","value":"func F()"}}`, "func F()"},
		{"markedstring", `{"contents":{"language":"go","value":"type T struct{}"}}`, "type T struct{}"},
		{"plainstring", `{"contents":"hello"}`, "hello"},
		{"array", `{"contents":["a",{"value":"b"}]}`, "a\nb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatLSPHover(json.RawMessage(c.in))
			if got != c.want {
				t.Errorf("formatLSPHover(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestFormatLSPLocations_Shapes(t *testing.T) {
	// Single Location.
	one := `{"uri":"file:///a/b.py","range":{"start":{"line":4,"character":2}}}`
	if got := formatLSPLocations(json.RawMessage(one)); got != "/a/b.py:5:3" {
		t.Errorf("single location: got %q want /a/b.py:5:3", got)
	}
	// Array of Locations (0-based → 1-based conversion).
	arr := `[{"uri":"file:///a/b.py","range":{"start":{"line":0,"character":0}}},` +
		`{"uri":"file:///c/d.py","range":{"start":{"line":9,"character":4}}}]`
	want := "/a/b.py:1:1\n/c/d.py:10:5"
	if got := formatLSPLocations(json.RawMessage(arr)); got != want {
		t.Errorf("array locations: got %q want %q", got, want)
	}
	// LocationLink (targetUri + targetSelectionRange).
	link := `[{"targetUri":"file:///e/f.rs","targetSelectionRange":{"start":{"line":1,"character":7}}}]`
	if got := formatLSPLocations(json.RawMessage(link)); got != "/e/f.rs:2:8" {
		t.Errorf("location link: got %q want /e/f.rs:2:8", got)
	}
	// Null/empty.
	if got := formatLSPLocations(json.RawMessage(`null`)); got != "" {
		t.Errorf("null locations should be empty, got %q", got)
	}
}

func TestLSPProjectRoot(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Marker at the top.
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "lib.rs")
	if err := os.WriteFile(file, []byte("fn main(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lspProjectRoot(file, []string{"Cargo.toml", ".git"}); got != dir {
		t.Errorf("project root: got %q want %q", got, dir)
	}
	// No marker anywhere → falls back to the file's own dir.
	file2 := filepath.Join(sub, "orphan.rs")
	if err := os.WriteFile(file2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lspProjectRoot(file2, []string{"NoSuchMarker"}); got != sub {
		t.Errorf("fallback root: got %q want %q", got, sub)
	}
}
