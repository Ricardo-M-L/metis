package workflow

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

func TestWorkflowQualitySuspensionCannotFallBackToLegacyFile(t *testing.T) {
	s := NewStore(t.TempDir())
	name := "quality-flow"
	if err := s.Save(Workflow{Name: name, Steps: []Step{{Name: "host", Command: "echo BASELINE"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Propose(context.Background(), Workflow{Name: name, Steps: []Step{{Name: "candidate", Command: "echo PROPOSAL"}}}); err != nil {
		t.Fatal(err)
	}
	ls := s.LearningStore()
	if _, err := ls.ConfigureQuality(context.Background(), name, learning.QualityPolicy{Kind: "workflow", WindowSize: 1, MinimumSamples: 1, MaxFailureRate: 0, Action: "pause", Verifier: "host"}); err != nil {
		t.Fatal(err)
	}
	b, err := ls.BindOutcome(context.Background(), name, "job")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.RecordOutcome(context.Background(), name, b, learning.HostOutcome{Verifier: "host", InvocationSHA256: b.VersionSHA256, EvidenceSHA256: b.VersionSHA256}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(name); err == nil {
		t.Fatal("suspended recipe ran legacy source")
	}
	if err := ls.ResumeQuality(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(name); err != nil {
		t.Fatal(err)
	}
}
