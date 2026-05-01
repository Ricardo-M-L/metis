package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
)

func writeNotebook(t *testing.T, path string, cells []map[string]any) {
	t.Helper()
	nb := map[string]any{
		"cells":          cells,
		"metadata":       map[string]any{},
		"nbformat":       4,
		"nbformat_minor": 5,
	}
	b, err := json.Marshal(nb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadNotebook(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var nb map[string]any
	if err := json.Unmarshal(b, &nb); err != nil {
		t.Fatal(err)
	}
	return nb
}

func TestNotebookEdit_Replace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.ipynb")
	writeNotebook(t, p, []map[string]any{
		{"cell_type": "code", "metadata": map[string]any{}, "source": []any{"print(1)\n"}, "outputs": []any{"old"}, "execution_count": 5},
	})
	tool := NotebookEdit{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"path":       p,
		"cell_index": 0,
		"mode":       "replace",
		"new_source": "print(2)\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	nb := loadNotebook(t, p)
	cells := nb["cells"].([]any)
	cell := cells[0].(map[string]any)
	src := cell["source"].([]any)
	if src[0] != "print(2)\n" {
		t.Errorf("source not replaced: %v", src)
	}
	if outputs, ok := cell["outputs"].([]any); !ok || len(outputs) != 0 {
		t.Errorf("outputs should be cleared after replace, got %v", cell["outputs"])
	}
}

func TestNotebookEdit_Insert(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.ipynb")
	writeNotebook(t, p, []map[string]any{
		{"cell_type": "code", "metadata": map[string]any{}, "source": []any{"a\n"}},
	})
	tool := NotebookEdit{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"path":       p,
		"cell_index": 1,
		"mode":       "insert",
		"cell_type":  "markdown",
		"new_source": "# new",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := loadNotebook(t, p)
	cells := nb["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("expected 2 cells, got %d", len(cells))
	}
	new := cells[1].(map[string]any)
	if new["cell_type"] != "markdown" {
		t.Errorf("cell_type = %q", new["cell_type"])
	}
}

func TestNotebookEdit_Delete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.ipynb")
	writeNotebook(t, p, []map[string]any{
		{"cell_type": "code", "metadata": map[string]any{}, "source": []any{"a"}},
		{"cell_type": "code", "metadata": map[string]any{}, "source": []any{"b"}},
	})
	tool := NotebookEdit{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"path":       p,
		"cell_index": 0,
		"mode":       "delete",
	})
	if err != nil {
		t.Fatal(err)
	}
	nb := loadNotebook(t, p)
	cells := nb["cells"].([]any)
	if len(cells) != 1 {
		t.Fatalf("expected 1 cell, got %d", len(cells))
	}
}

func TestNotebookEdit_RejectsNonNotebook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.txt")
	os.WriteFile(p, []byte("hi"), 0o644)
	tool := NotebookEdit{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": p, "cell_index": 0,
	})
	if err == nil {
		t.Errorf("non-.ipynb path should error")
	}
}
