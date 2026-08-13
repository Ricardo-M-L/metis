package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestUsageActivityRowsSanitizesArchiveModelLabels(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantLabel string
	}{
		{
			name:      "terminal control sequence",
			model:     "model\x1b[2Jspoof",
			wantLabel: "[private]",
		},
		{
			name:      "credential-shaped model",
			model:     "ghp_" + strings.Repeat("a", 36),
			wantLabel: "[private]",
		},
		{
			name:      "bounded unicode label",
			model:     strings.Repeat("模", 80),
			wantLabel: strings.Repeat("模", 63) + "…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			header := session.Header{
				ID:        "session",
				CreatedAt: time.Now().Add(-time.Minute),
				Model:     tc.model,
			}
			if err := store.WriteHeaderFull(header); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendMessage(header.ID, llm.Message{
				Role:    llm.RoleUser,
				Content: []llm.ContentBlock{{Type: "text", Text: "hello"}},
			}); err != nil {
				t.Fatal(err)
			}

			rows := usageActivityRows(store)
			values := make([]string, 0, len(rows))
			for _, row := range rows {
				values = append(values, row.Value)
			}
			output := strings.Join(values, "\n")
			if !strings.Contains(output, tc.wantLabel+" × 1") {
				t.Fatalf("sanitized label %q missing from rows:\n%s", tc.wantLabel, output)
			}
			if tc.model != tc.wantLabel && strings.Contains(output, tc.model) {
				t.Fatalf("raw archive label leaked into rows:\n%s", output)
			}
			if strings.ContainsRune(output, '\x1b') {
				t.Fatalf("terminal escape leaked into rows: %q", output)
			}
		})
	}
}
