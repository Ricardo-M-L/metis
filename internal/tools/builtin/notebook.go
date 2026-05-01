package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// NotebookEdit edits one cell in a Jupyter .ipynb file. Mirrors
// claude-code's NotebookEditTool. Three modes:
//
//   - "replace"  (default) — replace cell.source with NewSource
//   - "insert"             — insert a new cell at CellIndex
//   - "delete"             — drop the cell at CellIndex
//
// CellIndex is 0-based. Out-of-range insert appends at the tail.
//
// We only touch top-level fields the JSON schema requires; everything
// else (metadata, outputs, execution_count) is preserved as-is.
type NotebookEdit struct{ gate *permission.Gate }

func (NotebookEdit) Name() string { return "NotebookEdit" }
func (NotebookEdit) Description() string {
	return "Edit cells in a Jupyter notebook (.ipynb). Modes: replace (default), insert, delete."
}

func (NotebookEdit) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"path", "cell_index"},
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "absolute path to the .ipynb file"},
			"cell_index": map[string]any{"type": "integer", "description": "0-based cell index"},
			"mode":       map[string]any{"type": "string", "enum": []string{"replace", "insert", "delete"}, "description": "edit mode (default: replace)"},
			"cell_type":  map[string]any{"type": "string", "enum": []string{"code", "markdown"}, "description": "for insert mode (default: code)"},
			"new_source": map[string]any{"type": "string", "description": "new cell source for replace/insert"},
		},
	}
}

func (NotebookEdit) Concurrency(map[string]any) tools.Concurrency {
	// Mutates a file — never run concurrently with other Edit/Write on the
	// same path. We don't have a path-keyed lock, so the simple "exclusive"
	// bucket is enough.
	return tools.ConcurrencyExclusive
}

func (n NotebookEdit) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := n.gate.Check(context.Background(), "NotebookEdit", strFromAny(in["path"]))
	return mapDecision(d), src
}

func (n NotebookEdit) Execute(_ context.Context, in map[string]any) (*tools.Result, error) {
	path, _ := in["path"].(string)
	if path == "" {
		return nil, errors.New("path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	if !strings.HasSuffix(strings.ToLower(path), ".ipynb") {
		return nil, fmt.Errorf("not a Jupyter notebook (expected .ipynb): %s", path)
	}
	idx := intArg(in, "cell_index", -1)
	if idx < 0 {
		return nil, errors.New("cell_index is required and must be >= 0")
	}
	mode, _ := in["mode"].(string)
	if mode == "" {
		mode = "replace"
	}
	newSrc, _ := in["new_source"].(string)
	cellType, _ := in["cell_type"].(string)
	if cellType == "" {
		cellType = "code"
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var nb map[string]any
	if err := json.Unmarshal(raw, &nb); err != nil {
		return nil, fmt.Errorf("not a valid notebook JSON: %w", err)
	}
	cellsAny, ok := nb["cells"].([]any)
	if !ok {
		return nil, errors.New("notebook has no `cells` array")
	}

	switch mode {
	case "replace":
		if idx >= len(cellsAny) {
			return nil, fmt.Errorf("cell_index %d out of range (have %d cells)", idx, len(cellsAny))
		}
		cell, ok := cellsAny[idx].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cell %d is not an object", idx)
		}
		cell["source"] = splitSourceLines(newSrc)
		// Outputs become stale after a source replace — drop them so a
		// later run regenerates rather than showing pre-change output.
		if _, has := cell["outputs"]; has {
			cell["outputs"] = []any{}
		}
		if _, has := cell["execution_count"]; has {
			cell["execution_count"] = nil
		}
		cellsAny[idx] = cell
	case "insert":
		newCell := map[string]any{
			"cell_type": cellType,
			"metadata":  map[string]any{},
			"source":    splitSourceLines(newSrc),
		}
		if cellType == "code" {
			newCell["outputs"] = []any{}
			newCell["execution_count"] = nil
		}
		insertAt := idx
		if insertAt > len(cellsAny) {
			insertAt = len(cellsAny)
		}
		out := make([]any, 0, len(cellsAny)+1)
		out = append(out, cellsAny[:insertAt]...)
		out = append(out, newCell)
		out = append(out, cellsAny[insertAt:]...)
		cellsAny = out
	case "delete":
		if idx >= len(cellsAny) {
			return nil, fmt.Errorf("cell_index %d out of range (have %d cells)", idx, len(cellsAny))
		}
		cellsAny = append(cellsAny[:idx], cellsAny[idx+1:]...)
	default:
		return nil, fmt.Errorf("unknown mode %q (allowed: replace, insert, delete)", mode)
	}
	nb["cells"] = cellsAny

	out, err := json.MarshalIndent(nb, "", " ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, err
	}
	return &tools.Result{Output: fmt.Sprintf("notebook updated (%s, cell %d): %s", mode, idx, path)}, nil
}

// splitSourceLines mirrors how Jupyter stores cell source — usually as
// an array of strings each ending with \n (last line has no \n). Plain
// string is also valid, but the array form is what nbformat canonicalizes.
func splitSourceLines(s string) []any {
	if s == "" {
		return []any{}
	}
	lines := strings.SplitAfter(s, "\n")
	out := make([]any, 0, len(lines))
	for _, l := range lines {
		out = append(out, l)
	}
	return out
}
