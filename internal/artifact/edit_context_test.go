package artifact

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func editContextTestPrompt(id string, version int) string {
	return ArtifactEditPromptPrefix + "\n\n```json\n{\"metis_artifact_annotation\":{\"artifactId\":\"" + id + "\",\"version\":" + strconv.Itoa(version) + ",\"digest\":\"" + strings.Repeat("a", 64) + "\",\"targetId\":\"target-1\",\"instruction\":\"Make the heading smaller\",\"title\":\"Demo\",\"selection\":{\"id\":\"target-1\",\"tag\":\"h1\",\"selector\":\"html > body > h1\",\"text\":\"Demo\"}}}\n```\nUpdate the selected saved version."
}

func TestParseEditReferenceRecognizesOnlyCanonicalLeadingPayload(t *testing.T) {
	valid := editContextTestPrompt("demo-id", 1)
	ref, err := ParseEditReference(valid)
	if err != nil || ref == nil || ref.ArtifactID != "demo-id" || ref.Version != 1 || ref.TargetID != "target-1" {
		t.Fatalf("canonical reference: ref=%+v err=%v", ref, err)
	}
	for _, plain := range []string{"Please update the demo", "> " + valid, "The following is quoted:\n" + valid, "```text\n" + valid + "\n```"} {
		ref, err := ParseEditReference(plain)
		if err != nil || ref != nil {
			t.Fatalf("ordinary or quoted message bound an edit: ref=%+v err=%v", ref, err)
		}
	}
	for name, malformed := range map[string]string{
		"missing fence":      ArtifactEditPromptPrefix,
		"wrong first fence":  ArtifactEditPromptPrefix + "\n```text\nignored\n```\n" + valid,
		"wrong id":           strings.Replace(valid, "demo-id", "../demo-id", 1),
		"fractional version": strings.Replace(valid, `"version":1`, `"version":1.5`, 1),
		"zero version":       strings.Replace(valid, `"version":1`, `"version":0`, 1),
		"bad digest":         strings.Replace(valid, strings.Repeat("a", 64), "not-a-digest", 1),
		"bad target":         strings.Replace(valid, `"targetId":"target-1"`, `"targetId":"target-0"`, 1),
		"selection mismatch": strings.Replace(valid, `"id":"target-1"`, `"id":"target-2"`, 1),
		"duplicate version":  strings.Replace(valid, `"version":1`, `"version":1,"version":2`, 1),
		"unknown owner":      strings.Replace(valid, `"artifactId":`, `"sessionId":"foreign","artifactId":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if ref, err := ParseEditReference(malformed); err == nil || ref != nil {
				t.Fatalf("malformed canonical input accepted: ref=%+v err=%v", ref, err)
			}
		})
	}
}

func TestEditReferenceContextIsOwnerScopedAndClearedForNewPlainRequest(t *testing.T) {
	ctx, err := BindEditReference(context.Background(), "owner-a", editContextTestPrompt("demo-id", 1))
	if err != nil {
		t.Fatal(err)
	}
	if version, matched, err := SelectedEditVersion(ctx, "owner-a", "demo-id"); err != nil || !matched || version != 1 {
		t.Fatalf("selected edit = %d, %v, %v", version, matched, err)
	}
	if _, matched, err := SelectedEditVersion(ctx, "owner-b", "demo-id"); !matched || !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("another session inherited the edit: matched=%v err=%v", matched, err)
	}
	if _, matched, err := SelectedEditVersion(ctx, "owner-a", "other-id"); matched || err != nil {
		t.Fatalf("unrelated artifact constrained: matched=%v err=%v", matched, err)
	}
	plain, err := BindEditReference(ctx, "owner-a", "Now write a new ordinary answer")
	if err != nil {
		t.Fatal(err)
	}
	if _, matched, err := SelectedEditVersion(plain, "owner-a", "demo-id"); matched || err != nil {
		t.Fatalf("a later request inherited the prior edit: matched=%v err=%v", matched, err)
	}
}
