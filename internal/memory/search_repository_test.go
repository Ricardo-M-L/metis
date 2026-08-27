package memory

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRepositorySearchAndAutoRetrieveShareArchiveAndTopicCorpus(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Archival().Insert(Passage{
		ID: "archive-shared", Content: "Copper Finch deployment uses the blue lane.",
		Type: TypeReference, Tags: []string{"deployment"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{
		Path:    filepath.Join(root, "topic_shared.md"),
		Content: topicTestMemo("Nebula Quartz", "Nebula Quartz workstation preference."),
		Source:  TopicSource{SessionID: "topic-owner"},
	}); err != nil {
		t.Fatal(err)
	}

	archiveHits, err := manager.Search(SearchOptions{Query: "Copper Finch", SortBy: "relevance", Limit: 5})
	if err != nil || len(archiveHits) == 0 || archiveHits[0].ID != "archive-shared" {
		t.Fatalf("unified Search missed archive: hits=%+v err=%v", archiveHits, err)
	}
	topicHits, err := manager.Search(SearchOptions{Query: "Nebula Quartz", SortBy: "relevance", Limit: 5})
	if err != nil || len(topicHits) == 0 || topicHits[0].ID[:6] != "topic-" {
		t.Fatalf("unified Search missed topic: hits=%+v err=%v", topicHits, err)
	}
	auto := manager.SearchCandidates("Nebula Quartz", 5)
	if len(auto) == 0 || auto[0].ID != topicHits[0].ID {
		t.Fatalf("SearchCandidates and Search diverged: candidates=%+v search=%+v", auto, topicHits)
	}
	legacyAuto := manager.AutoRetrieveCandidates("Nebula Quartz", 5)
	if len(legacyAuto) == 0 || legacyAuto[0].ID != topicHits[0].ID {
		t.Fatalf("AutoRetrieve and Search diverged: auto=%+v search=%+v", legacyAuto, topicHits)
	}
}

func TestRepositorySearchFiltersTopicType(t *testing.T) {
	root := t.TempDir()
	manager, err := NewMemoryManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CommitTopic(context.Background(), TopicMutation{
		Path:    filepath.Join(root, "project_filter.md"),
		Content: topicTestMemo("Filter Topic", "shared filter keyword"),
		Source:  TopicSource{SessionID: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	misses, err := manager.Search(SearchOptions{Query: "shared filter keyword", SortBy: "relevance", Types: []string{TypeUser}})
	if err != nil || len(misses) != 0 {
		t.Fatalf("type filter leaked project topic: hits=%+v err=%v", misses, err)
	}
	hits, err := manager.Search(SearchOptions{Query: "shared filter keyword", SortBy: "relevance", Types: []string{TypeProject}})
	if err != nil || len(hits) != 1 {
		t.Fatalf("type filter missed project topic: hits=%+v err=%v", hits, err)
	}
}
