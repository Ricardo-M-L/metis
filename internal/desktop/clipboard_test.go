package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClipboardFilesFromPathsValidatesAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "芯片 设计")
	file := filepath.Join(root, "notes.txt")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, err := clipboardFilesFromPaths([]string{folder, file, folder, filepath.Join(root, "missing")})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want two existing unique paths", items)
	}
	if items[0].Path != folder || items[0].Name != "芯片 设计" || !items[0].IsDir {
		t.Fatalf("folder item = %+v", items[0])
	}
	if items[1].Path != file || items[1].Name != "notes.txt" || items[1].IsDir {
		t.Fatalf("file item = %+v", items[1])
	}
}

func TestClipboardFilesFromPathsRejectsRelativeInput(t *testing.T) {
	if _, err := clipboardFilesFromPaths([]string{"relative/path"}); err == nil {
		t.Fatal("relative clipboard path unexpectedly accepted")
	}
}
