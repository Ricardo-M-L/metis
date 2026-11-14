package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type shortPreferenceProvider struct {
	distillMetadataProvider
	calls    int
	factType string
}

func (provider *shortPreferenceProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	provider.calls++
	kind := provider.factType
	if kind == "" {
		kind = TypeUser
	}
	return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf(`[{"type":%q,"content":"以后都用中文"}]`, kind)}}}, nil
}

func TestDistillationRetryPreservesScopeAndSourceIdentity(t *testing.T) {
	root := t.TempDir()
	first, err := NewMemoryManagerForWorkspace(root, filepath.Join(t.TempDir(), "first-workspace"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMemoryManagerForWorkspace(root, filepath.Join(t.TempDir(), "second-workspace"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &shortPreferenceProvider{factType: TypeProject}
	for _, manager := range []*MemoryManager{first, first, second} {
		if err := manager.DistillTurnWithMetadata(context.Background(), provider, "same-source", "same-message", "以后都用中文", "好的"); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.DistillTurnWithMetadata(context.Background(), provider, "another-source", "same-message", "以后都用中文", "好的"); err != nil {
		t.Fatal(err)
	}
	all, err := first.Archival().Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("facts=%d, want distinct scope and source retained", len(all))
	}
	if err := first.DeleteSession("same-source"); err != nil {
		t.Fatal(err)
	}
	remaining, err := first.Archival().Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].SourceSessionID != "another-source" {
		t.Fatalf("source deletion removed independently owned fact: %+v", remaining)
	}
}

func TestDistillationAcceptsShortPreferenceAndDeduplicatesRetry(t *testing.T) {
	mm, err := NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &shortPreferenceProvider{}
	for i := 0; i < 2; i++ {
		if err := mm.DistillTurnWithMetadata(context.Background(), provider, "preference-owner", "preference-message", "以后都用中文", "好的"); err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls=%d, want 2", provider.calls)
	}
	hits, err := mm.Search(SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Content != "以后都用中文" {
		t.Fatalf("incremental facts=%+v, want one preference", hits)
	}
}

func TestDistillationRejectsDeletedSourceBeforeProvider(t *testing.T) {
	mm, err := NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := mm.DeleteSession("already-deleted"); err != nil {
		t.Fatal(err)
	}
	provider := &shortPreferenceProvider{}
	err = mm.DistillTurnWithMetadata(context.Background(), provider, "already-deleted", "message", "以后都用中文", "好的")
	if !errors.Is(err, ErrSessionDeleted) {
		t.Fatalf("error=%v, want ErrSessionDeleted", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls=%d, want 0 for deleted source", provider.calls)
	}
}
