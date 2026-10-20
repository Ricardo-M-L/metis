package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

func skillLearningEvaluation(t *testing.T, dir string, v learning.Version, name string) learning.Evaluation {
	t.Helper()
	store := learning.NewStore(dir)
	suite := learning.Suite{Name: "host-fixture", Cases: []learning.Case{{ID: "one", Prompt: "unit", Expected: "GOOD"}}, MaxTokens: 1000, MaxDurationSeconds: 10}
	identity := learning.EvaluatorIdentity{BinarySHA256: strings.Repeat("b", 64), ProfileSHA256: strings.Repeat("p", 64), Provider: "fixture", Model: "fixture"}
	e, err := store.Evaluate(context.Background(), name, v.ID, suite, identity, func(ctx context.Context, ver learning.Version, c learning.Case) (learning.CaseResult, error) {
		args, _ := json.Marshal(map[string]string{"action": "invoke", "name": name})
		return learning.CaseResult{Output: "GOOD", SkillInvoked: true, ModelRequests: 1, UsageReported: true, Usage: learning.Usage{Input: 1, Output: 1}, Evidence: learning.QueryEvidence{Provider: "fixture", Model: "fixture", SessionID: "unit", ManifestSHA256: ver.ID, TraceSHA256: strings.Repeat("t", 64), TokenEventIDs: []string{"tokens1"}, SkillInvocations: []learning.SkillInvocation{{InvocationID: "inv1", CallID: "call1", Arguments: args, Result: ver.Content, ResultSHA256: learning.Digest([]byte(ver.Content))}}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestDreamSkillSynthStagesNewSkillOutsideActiveCatalog(t *testing.T) {
	dir := t.TempDir()
	loader := NewLoader(dir, "", nil)
	tool := NewSkillSynthTool(NewSynth(dir, loader))
	res, err := tool.Execute(context.Background(), map[string]any{"action": "create_skill", "name": "receipt-format", "description": "fixture", "body": "Return GOOD"})
	if err != nil || res.IsError {
		t.Fatalf("stage %v %+v", err, res)
	}
	if !strings.Contains(res.Output, "candidate_staged") {
		t.Fatal(res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "receipt-format.md")); !os.IsNotExist(err) {
		t.Fatal("Dream wrote active flat file")
	}
	if sk, _ := loader.Get("receipt-format"); sk != nil {
		t.Fatal("unvalidated skill visible")
	}
	st, err := learning.NewStore(dir).Read("receipt-format")
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveVersion != "" || len(st.Versions) != 1 {
		t.Fatal("new candidate autoactivated")
	}
}
func TestDreamUpdatePreservesActiveAndResidentLoaderRefreshesPromotionRollback(t *testing.T) {
	dir := t.TempDir()
	flat := filepath.Join(dir, "receipt-format.md")
	before := []byte("---\nname: receipt-format\ndescription: legacy skill\n---\nReturn BASELINE\n")
	if err := os.WriteFile(flat, before, 0600); err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(dir, "", nil)
	if sk, _ := loader.Get("receipt-format"); sk == nil || !strings.Contains(sk.Prompt, "BASELINE") {
		t.Fatal("baseline missing")
	}
	synth := NewSynth(dir, loader)
	v, err := synth.StageUpdateSkill("receipt-format", "Return GOOD")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(flat)
	if string(after) != string(before) {
		t.Fatal("automatic update replaced active flat file")
	}
	if sk, _ := loader.Get("receipt-format"); sk == nil || !strings.Contains(sk.Prompt, "BASELINE") {
		t.Fatal("staged candidate leaked")
	}
	e := skillLearningEvaluation(t, dir, v, "receipt-format")
	if err := learning.NewStore(dir).Promote(context.Background(), "receipt-format", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	// This same Loader sees another Store/process's atomic activation without
	// explicit Invalidate, rather than only a newly restarted CLI.
	sk, err := loader.Get("receipt-format")
	if err != nil || sk == nil || !strings.Contains(sk.Prompt, "GOOD") || sk.Description != "legacy skill" {
		t.Fatalf("promotion cache %v %+v", err, sk)
	}
	st, _ := learning.NewStore(dir).Read("receipt-format")
	if err := learning.NewStore(dir).Rollback(context.Background(), "receipt-format", st.BaselineVersion); err != nil {
		t.Fatal(err)
	}
	if sk, _ := loader.Get("receipt-format"); sk == nil || !strings.Contains(sk.Prompt, "BASELINE") {
		t.Fatal("rollback not loaded")
	}
}
func TestDreamCannotRetireActiveReplacementBeforeValidation(t *testing.T) {
	dir := t.TempDir()
	s := NewSynth(dir, nil)
	if _, err := s.CreateSkill(SynthSkillMeta{Name: "old-skill"}, "OLD"); err != nil {
		t.Fatal(err)
	}
	res, err := NewSkillSynthTool(s).Execute(context.Background(), map[string]any{"action": "archive_skill", "name": "old-skill"})
	if err != nil || !res.IsError {
		t.Fatalf("archive %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(dir, "old-skill.md")); err != nil {
		t.Fatal("active retired")
	}
}
func TestLearningManifestRejectsNameMismatchAndInlineExecution(t *testing.T) {
	for _, body := range []string{"---\nname: different\n---\nbody", "Run !`echo PASS`", "```!\necho PASS\n```"} {
		if err := ValidateLearningManifest("test-skill", []byte(body)); err == nil {
			t.Fatalf("unsafe candidate %q accepted", body)
		}
	}
}
func TestLearningLayerDoesNotOverrideHigherPriorityProjectSkill(t *testing.T) {
	dir, project := t.TempDir(), t.TempDir()
	baseline := []byte("---\nname: test-skill\n---\nBASE\n")
	_ = os.WriteFile(filepath.Join(dir, "test-skill.md"), baseline, 0600)
	_ = os.WriteFile(filepath.Join(project, "test-skill.md"), []byte("PROJECT"), 0600)
	v, err := NewSynth(dir, nil).StageUpdateSkill("test-skill", "GOOD")
	if err != nil {
		t.Fatal(err)
	}
	e := skillLearningEvaluation(t, dir, v, "test-skill")
	if err := learning.NewStore(dir).Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	sk, err := NewLoader(dir, project, nil).Get("test-skill")
	if err != nil || sk == nil || sk.Prompt != "PROJECT" {
		t.Fatalf("project priority %v %+v", err, sk)
	}
}

func TestLearningFirstStageDoesNotHideLegacyNonCanonicalSource(t *testing.T) {
	for _, kind := range []string{"json", "markdown", "tree"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := "test-skill"
			switch kind {
			case "json":
				_ = os.WriteFile(filepath.Join(dir, name+".json"), []byte(`{"name":"test-skill","prompt":"LEGACY"}`), 0600)
			case "markdown":
				_ = os.WriteFile(filepath.Join(dir, name+".markdown"), []byte("LEGACY"), 0600)
			case "tree":
				_ = os.MkdirAll(filepath.Join(dir, name), 0700)
				_ = os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\nname: test-skill\n---\nLEGACY\n"), 0600)
			}
			loader := NewLoader(dir, "", nil)
			old, _ := loader.Get(name)
			if old == nil || old.Prompt != "LEGACY" {
				t.Fatalf("fixture missing %+v", old)
			}
			if _, err := LearningBaseline(dir, name); err == nil {
				t.Fatal("unsupported source silently treated as a new skill")
			}
			if _, err := NewSynth(dir, loader).StageCreateSkill(SynthSkillMeta{Name: name}, "CANDIDATE"); err == nil {
				t.Fatal("Dream hid a legacy source")
			}
			if _, err := learning.NewStore(dir).Read(name); !os.IsNotExist(err) {
				t.Fatalf("failed adoption created managed state: %v", err)
			}
			if after, _ := loader.Get(name); after == nil || after.Prompt != "LEGACY" {
				t.Fatal("failed proposal changed original active")
			}
		})
	}
}
func TestLearningBaselineNameMismatchKeepsAllUserSkills(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "test-skill.md"), []byte("---\nname: real-name\n---\nORIGINAL\n"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "other-skill.md"), []byte("OTHER"), 0600)
	if _, err := LearningBaseline(dir, "test-skill"); err == nil {
		t.Fatal("mismatched baseline accepted")
	}
	loader := NewLoader(dir, "", nil)
	for _, name := range []string{"real-name", "other-skill"} {
		if sk, _ := loader.Get(name); sk == nil {
			t.Fatalf("valid source %s dropped", name)
		}
	}
}
func TestLearningRejectsCandidatesThatCouldFallbackToBundledSkill(t *testing.T) {
	for _, meta := range []string{"disabled: true", "activate_on: [\"/a/different/workspace\"]"} {
		body := []byte("---\nname: debug\n" + meta + "\n---\nCANDIDATE\n")
		if err := ValidateLearningManifest("debug", body); err == nil {
			t.Fatalf("unavailable candidate accepted: %s", meta)
		}
	}
}

func TestLearningQualityPauseHidesManagedSkillAndResidentLoaderResumes(t *testing.T) {
	dir := t.TempDir()
	name := "receipt-format"
	flat := []byte("---\nname: receipt-format\n---\nBASELINE\n")
	if err := os.WriteFile(filepath.Join(dir, name+".md"), flat, 0600); err != nil {
		t.Fatal(err)
	}
	v, err := NewSynth(dir, nil).StageUpdateSkill(name, "GOOD")
	if err != nil {
		t.Fatal(err)
	}
	s := learning.NewStore(dir)
	e := skillLearningEvaluation(t, dir, v, name)
	if err := s.Promote(context.Background(), name, v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConfigureQuality(context.Background(), name, learning.QualityPolicy{Kind: "skill", WindowSize: 1, MinimumSamples: 1, MaxFailureRate: 0, Action: "pause", Verifier: "host"})
	if err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(dir, "", nil)
	sk, err := loader.Get(name)
	st, _ := s.Read(name)
	if err != nil || sk == nil || sk.LearningVersionSHA256 != v.ID || sk.LearningActivationEpoch != st.ActivationEpoch {
		t.Fatalf("activation metadata not host bound: %+v %v", sk, err)
	}
	b, err := s.BindOutcome(context.Background(), name, "job")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RecordOutcome(context.Background(), name, b, learning.HostOutcome{Verifier: "host", InvocationSHA256: v.ID, EvidenceSHA256: v.ID, Passed: false})
	if err != nil {
		t.Fatal(err)
	}
	if sk, _ := loader.Get(name); sk != nil {
		t.Fatal("suspended skill stayed in resident catalog")
	}
	if err := s.ResumeQuality(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if sk, _ := loader.Get(name); sk == nil || !strings.Contains(sk.Prompt, "GOOD") {
		t.Fatal("host resume not visible in resident loader")
	}
}
