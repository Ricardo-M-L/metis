package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writeNamedSkill(t *testing.T, dir, name, desc, whenToUse string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\nwhen_to_use: " + whenToUse + "\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mark as agent-created so the DetectOverlaps provenance gate includes it.
	NewUsageStore(dir).RecordCreate(name, SourceAgentSynth)
}

func TestDetectOverlaps_ClustersRedundant(t *testing.T) {
	dir := t.TempDir()
	// Two near-duplicate git-commit skills + one unrelated.
	writeNamedSkill(t, dir, "git-commit-helper", "commit staged changes with a generated git message", "committing code")
	writeNamedSkill(t, dir, "commit-with-message", "generate a git commit message and commit staged changes", "committing code")
	writeNamedSkill(t, dir, "deploy-staging", "rsync the build to the staging server and restart", "deploying to staging")

	clusters := DetectOverlaps(dir)
	if len(clusters) != 1 {
		t.Fatalf("want 1 overlap cluster, got %d: %v", len(clusters), clusters)
	}
	got := clusters[0]
	if len(got) != 2 || got[0] != "commit-with-message" || got[1] != "git-commit-helper" {
		t.Errorf("unexpected cluster membership: %v", got)
	}
}

func TestDetectOverlaps_NoFalsePositive(t *testing.T) {
	dir := t.TempDir()
	writeNamedSkill(t, dir, "deploy-staging", "rsync build to staging and restart", "deploy")
	writeNamedSkill(t, dir, "summarize-pr", "read a pull request and write a short summary", "summarizing PRs")
	if clusters := DetectOverlaps(dir); len(clusters) != 0 {
		t.Errorf("unrelated skills must not cluster, got %v", clusters)
	}
}

func TestDetectOverlaps_SkipsHandWritten(t *testing.T) {
	dir := t.TempDir()
	// Two similar skills but NEITHER is agent-created (no usage record) —
	// e.g. user hand-wrote them. Consolidation must not nominate them.
	body := func(n string) string {
		return "---\nname: " + n + "\ndescription: commit staged changes with a git message\n---\nb\n"
	}
	for _, n := range []string{"git-commit-helper", "commit-with-message"} {
		if err := os.WriteFile(filepath.Join(dir, n+".md"), []byte(body(n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if clusters := DetectOverlaps(dir); len(clusters) != 0 {
		t.Errorf("hand-written (non-agent-created) skills must not cluster, got %v", clusters)
	}
}

func TestDetectOverlaps_CJK(t *testing.T) {
	dir := t.TempDir()
	writeNamedSkill(t, dir, "deploy-zh", "周五部署到预发布服务器并重启服务", "周五部署")
	writeNamedSkill(t, dir, "deploy-zh2", "周五部署预发布服务器重启", "周五部署上线")
	writeNamedSkill(t, dir, "unrelated", "总结一份产品需求文档", "写总结")
	clusters := DetectOverlaps(dir)
	if len(clusters) != 1 || len(clusters[0]) != 2 {
		t.Fatalf("CJK duplicates should cluster, got %v", clusters)
	}
}

func TestCurator_ArchiveRefusesPinned(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "protected", 1)
	c := NewCurator(dir)
	if err := c.Pin("protected"); err != nil {
		t.Fatal(err)
	}
	if err := c.Archive("protected"); err == nil {
		t.Error("Archive must refuse a pinned skill")
	}
	if _, err := os.Stat(filepath.Join(dir, "protected.md")); err != nil {
		t.Error("pinned skill must stay in the live dir")
	}
}

func TestSynth_ArchiveSkill_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	if _, err := s.CreateSkill(SynthSkillMeta{Name: "redundant", Description: "x"}, "# body"); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveSkill("redundant"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// Gone from the live dir, recoverable from .archive via the curator.
	if _, err := os.Stat(filepath.Join(dir, "redundant.md")); !os.IsNotExist(err) {
		t.Error("archived skill should be gone from the live dir")
	}
	archived, _ := NewCurator(dir).ListArchived()
	if len(archived) != 1 || archived[0] != "redundant" {
		t.Errorf("want [redundant] archived, got %v", archived)
	}
	// Path-traversal name is rejected.
	if err := s.ArchiveSkill("../../etc/passwd"); err == nil {
		t.Error("archive_skill should reject a traversal name")
	}
}
