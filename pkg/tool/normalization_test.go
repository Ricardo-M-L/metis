package tool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeAliasesCopiesAndCanonicalizesLegacyKeys(t *testing.T) {
	original := map[string]any{
		"file_path":   "/tmp/example.txt",
		"old_string":  "before",
		"new_string":  "after",
		"replace_all": true,
		"nested":      map[string]any{"keep": "same object"},
	}
	wantOriginal := map[string]any{
		"file_path":   "/tmp/example.txt",
		"old_string":  "before",
		"new_string":  "after",
		"replace_all": true,
		"nested":      map[string]any{"keep": "same object"},
	}

	got, err := NormalizeAliases(original, map[string]string{
		"file_path":   "path",
		"old_string":  "old",
		"new_string":  "new",
		"replace_all": "all",
	})
	if err != nil {
		t.Fatalf("NormalizeAliases: %v", err)
	}
	if !reflect.DeepEqual(original, wantOriginal) {
		t.Fatalf("NormalizeAliases mutated its input:\n got %#v\nwant %#v", original, wantOriginal)
	}
	want := map[string]any{
		"path":   "/tmp/example.txt",
		"old":    "before",
		"new":    "after",
		"all":    true,
		"nested": map[string]any{"keep": "same object"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized input:\n got %#v\nwant %#v", got, want)
	}
}

func TestNormalizeAliasesSameCanonicalValueRemovesAlias(t *testing.T) {
	got, err := NormalizeAliases(
		map[string]any{"path": "/tmp/a", "file_path": "/tmp/a"},
		map[string]string{"file_path": "path"},
	)
	if err != nil {
		t.Fatalf("NormalizeAliases: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{"path": "/tmp/a"}) {
		t.Fatalf("normalized input = %#v", got)
	}
}

func TestNormalizeAliasesRejectsConflictingCanonicalValue(t *testing.T) {
	original := map[string]any{"path": "/tmp/canonical", "file_path": "/tmp/legacy"}
	got, err := NormalizeAliases(original, map[string]string{"file_path": "path"})
	if err == nil {
		t.Fatalf("NormalizeAliases returned %#v, want conflict error", got)
	}
	if !reflect.DeepEqual(original, map[string]any{"path": "/tmp/canonical", "file_path": "/tmp/legacy"}) {
		t.Fatalf("conflict path mutated input: %#v", original)
	}
}

func TestNormalizeAliasesPreservesExplicitEmptyString(t *testing.T) {
	got, err := NormalizeAliases(
		map[string]any{"new_string": ""},
		map[string]string{"new_string": "new"},
	)
	if err != nil {
		t.Fatalf("NormalizeAliases: %v", err)
	}
	value, present := got["new"]
	if !present || value != "" {
		t.Fatalf("explicit empty replacement lost: %#v", got)
	}
}

type normalizationTestTool struct {
	BaseTool
	normalize bool
}

func (normalizationTestTool) Name() string                           { return "normalization-test" }
func (normalizationTestTool) Description() string                    { return "test" }
func (normalizationTestTool) InputSchema() map[string]any            { return map[string]any{"type": "object"} }
func (normalizationTestTool) Concurrency(map[string]any) Concurrency { return ConcurrencySafe }
func (normalizationTestTool) CanUse(context.Context, map[string]any) (Permission, string) {
	return PermissionAllow, ""
}
func (normalizationTestTool) Execute(context.Context, map[string]any) (*Result, error) {
	return &Result{}, nil
}
func (t normalizationTestTool) NormalizeInput(input map[string]any) (map[string]any, error) {
	if !t.normalize {
		return input, nil
	}
	return NormalizeAliases(input, map[string]string{"legacy": "canonical"})
}

func TestNormalizeToolInputDelegatesWithoutMutatingCaller(t *testing.T) {
	original := map[string]any{"legacy": "value"}
	got, err := NormalizeToolInput(normalizationTestTool{normalize: true}, original)
	if err != nil {
		t.Fatalf("NormalizeToolInput: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{"canonical": "value"}) {
		t.Fatalf("normalized input = %#v", got)
	}
	if !reflect.DeepEqual(original, map[string]any{"legacy": "value"}) {
		t.Fatalf("NormalizeToolInput mutated caller: %#v", original)
	}
}

type nestedMutationNormalizer struct{ normalizationTestTool }

func (nestedMutationNormalizer) NormalizeInput(input map[string]any) (map[string]any, error) {
	input["nested"].(map[string]any)["value"] = "changed"
	input["items"].([]any)[0].(map[string]any)["value"] = "changed"
	return input, nil
}

func TestNormalizeToolInputDetachesNestedJSONGraph(t *testing.T) {
	original := map[string]any{
		"nested": map[string]any{"value": "original"},
		"items":  []any{map[string]any{"value": "original"}},
	}
	got, err := NormalizeToolInput(nestedMutationNormalizer{}, original)
	if err != nil {
		t.Fatalf("NormalizeToolInput: %v", err)
	}
	if original["nested"].(map[string]any)["value"] != "original" ||
		original["items"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatalf("normalizer mutated caller's nested input: %#v", original)
	}
	if got["nested"].(map[string]any)["value"] != "changed" ||
		got["items"].([]any)[0].(map[string]any)["value"] != "changed" {
		t.Fatalf("normalized nested changes missing: %#v", got)
	}
}

func TestNormalizeToolInputRejectsCyclicGraph(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	if _, err := NormalizeToolInput(normalizationTestTool{}, cyclic); err == nil {
		t.Fatal("cyclic input should fail normalization")
	}
}

type secretRejectingNormalizer struct{ normalizationTestTool }

func (secretRejectingNormalizer) NormalizeInput(map[string]any) (map[string]any, error) {
	return nil, errors.New("custom normalizer rejected secret sk-live-do-not-echo")
}

func TestNormalizeToolInputSanitizesCustomNormalizerErrors(t *testing.T) {
	const secret = "sk-live-do-not-echo"
	_, err := NormalizeToolInput(secretRejectingNormalizer{}, map[string]any{"token": "ordinary input"})
	if err == nil {
		t.Fatal("custom normalizer error was ignored")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("custom normalizer error leaked rejected input: %q", err)
	}
	if got, want := err.Error(), "tool input normalization failed"; got != want {
		t.Fatalf("error = %q, want fixed safe error %q", got, want)
	}
}

type panickingNormalizer struct{ normalizationTestTool }

func (panickingNormalizer) NormalizeInput(map[string]any) (map[string]any, error) {
	panic("custom normalizer panic with secret sk-live-do-not-echo")
}

func TestNormalizeToolInputContainsCustomNormalizerPanics(t *testing.T) {
	const secret = "sk-live-do-not-echo"
	_, err := NormalizeToolInput(panickingNormalizer{}, map[string]any{"token": "ordinary input"})
	if !errors.Is(err, ErrInputNormalization) {
		t.Fatalf("panic error = %v, want ErrInputNormalization", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("normalizer panic leaked sensitive text: %q", err)
	}
}
