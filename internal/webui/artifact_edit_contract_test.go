package webui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/artifact"
)

// The UI request generator and runtime guard share one wire contract. A
// valid server-generated request must always bind the same immutable base.
func TestArtifactEditPromptBindsRuntimeReference(t *testing.T) {
	ref := artifactAnnotationReference{ArtifactID: "sample-artifact", Version: 2,
		Digest: strings.Repeat("a", 64), TargetID: "target-5"}
	selection := artifactAnnotationTarget{ID: ref.TargetID, Tag: "a", Selector: "#button", Text: "Try it"}
	prompt, err := artifactAnnotationPrompt(ref, "A sample", selection, "Make this button green")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := artifact.ParseEditReference(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if parsed == nil || parsed.ArtifactID != ref.ArtifactID || parsed.Version != ref.Version ||
		parsed.Digest != ref.Digest || parsed.TargetID != ref.TargetID {
		t.Fatalf("runtime edit binding disagrees with server reference: %+v", parsed)
	}
}
