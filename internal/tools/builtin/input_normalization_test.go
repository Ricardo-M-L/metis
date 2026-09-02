package builtin

import (
	"reflect"
	"testing"

	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

func TestFileToolsNormalizeOnlyDocumentedLegacyAliases(t *testing.T) {
	tests := []struct {
		name string
		tool pubtool.Tool
		in   map[string]any
		want map[string]any
	}{
		{
			name: "read file_path",
			tool: Read{},
			in:   map[string]any{"file_path": "/tmp/a", "offset": 2},
			want: map[string]any{"path": "/tmp/a", "offset": 2},
		},
		{
			name: "write file_path",
			tool: Write{},
			in:   map[string]any{"file_path": "/tmp/a", "content": ""},
			want: map[string]any{"path": "/tmp/a", "content": ""},
		},
		{
			name: "edit aliases",
			tool: Edit{},
			in: map[string]any{
				"file_path":   "/tmp/a",
				"old_string":  "old",
				"new_string":  "",
				"replace_all": true,
			},
			want: map[string]any{"path": "/tmp/a", "old": "old", "new": "", "all": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pubtool.NormalizeToolInput(tt.tool, tt.in)
			if err != nil {
				t.Fatalf("NormalizeToolInput: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalized input:\n got %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestAgentPermissionBoundaryPreservesInnerNormalization(t *testing.T) {
	wrapped := agentPermissionBoundTool{inner: Edit{}}
	got, err := pubtool.NormalizeToolInput(wrapped, map[string]any{
		"file_path":  "/tmp/a",
		"old_string": "old",
		"new_string": "new",
	})
	if err != nil {
		t.Fatalf("NormalizeToolInput: %v", err)
	}
	want := map[string]any{"path": "/tmp/a", "old": "old", "new": "new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized input:\n got %#v\nwant %#v", got, want)
	}
}
