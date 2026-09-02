package agent

import (
	"reflect"
	"testing"

	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

func TestSecureAutoMemoryToolNormalizesHistoricalEditAliases(t *testing.T) {
	tool := secureAutoMemoryTool{Tool: autoMemoryEditTool{}}
	got, err := pubtool.NormalizeToolInput(tool, map[string]any{
		"file_path":   "/tmp/memory.md",
		"old_string":  "old",
		"new_string":  "",
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("NormalizeToolInput: %v", err)
	}
	want := map[string]any{
		"path": "/tmp/memory.md",
		"old":  "old",
		"new":  "",
		"all":  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized input:\n got %#v\nwant %#v", got, want)
	}
}
