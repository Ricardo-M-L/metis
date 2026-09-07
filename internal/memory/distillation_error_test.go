package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

type failingDistillationProvider struct {
	distillMetadataProvider
	err error
}

func (p failingDistillationProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.err
}

func TestDistillationProviderErrorSeparatesExtractionFromStorage(t *testing.T) {
	const user = "Please remember that I consistently prefer concise Chinese answers in every future session."
	const assistant = "Understood. I will keep future answers concise and write them in Chinese unless you ask otherwise."
	t.Run("provider marker preserves cause and raw recall", func(t *testing.T) {
		root := t.TempDir()
		manager, err := NewMemoryManager(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.RecordTurn(context.Background(), "completed-session", "message-1", user, assistant); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("server_is_overloaded")
		err = manager.DistillTurnWithMetadata(context.Background(), failingDistillationProvider{err: cause}, "completed-session", "message-1", user, assistant)
		var providerErr *DistillationProviderError
		if !errors.As(err, &providerErr) || !errors.Is(err, cause) {
			t.Fatalf("error = %v, want typed provider cause", err)
		}
		reloaded, err := NewMemoryManager(root)
		if err != nil {
			t.Fatal(err)
		}
		messages := reloaded.recall.GetMessages()
		if len(messages) != 2 || messages[0].SessionID != "completed-session" || messages[1].Content != assistant {
			t.Fatalf("raw recall did not survive enrichment failure: %#v", messages)
		}
	})
	t.Run("archival filesystem error is not optional", func(t *testing.T) {
		manager, err := NewMemoryManager(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		// An exact test-owned directory makes the authoritative archival file
		// unwritable, without depending on chmod behavior under privileged CI.
		if err := os.Mkdir(filepath.Join(manager.archival.root, "passages.jsonl"), 0o700); err != nil {
			t.Fatal(err)
		}
		err = manager.DistillTurnWithMetadata(context.Background(), distillMetadataProvider{}, "completed-session", "message-1", user, assistant)
		var providerErr *DistillationProviderError
		if err == nil || errors.As(err, &providerErr) || !strings.Contains(err.Error(), "authoritative file is not regular") {
			t.Fatalf("error = %v, want unmodified archival filesystem failure", err)
		}
	})
}
