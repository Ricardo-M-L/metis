package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

func TestLearningFeedbackHostCLIConfigurePauseResumeAndReceiptReplay(t *testing.T) {
	cfg, _ := learningControllerHostFixture(t)
	store := learning.NewStore(cfg.Session.SkillDir)
	if _, err := store.Stage(context.Background(), "test-skill", []byte("BASELINE"), []byte("PROPOSED"), "host"); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		output, err := captureCoordinatorOperationOutput(t, func() error { return cmdLearningFeedback(context.Background(), args) })
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	run("quality-configure", "--name", "test-skill", "--window", "1", "--minimum-samples", "1", "--max-failure-rate", "0", "--action", "pause", "--verifier", "fixed-host-check")
	output := run("feedback-bind", "--name", "test-skill", "--outcome", "business-1")
	var b learning.OutcomeBinding
	if err := json.Unmarshal([]byte(output), &b); err != nil {
		t.Fatal(err)
	}
	receipt := struct {
		Binding learning.OutcomeBinding `json:"binding"`
		Outcome learning.HostOutcome    `json:"outcome"`
	}{b, learning.HostOutcome{Passed: false, Verifier: "fixed-host-check", InvocationSHA256: learning.Digest([]byte("real invocation")), EvidenceSHA256: learning.Digest([]byte("host assertion failed"))}}
	data, _ := json.Marshal(receipt)
	file := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	first := run("feedback", "--name", "test-skill", "--file", file)
	if replay := run("feedback", "--name", "test-skill", "--file", file); first != replay {
		t.Fatal("host CLI replay changed decision")
	}
	st, _ := store.Read("test-skill")
	if !st.Suspended {
		t.Fatal("CLI failed to suspend")
	}
	if err := cmdLearningFeedback(context.Background(), []string{"quality-resume", "--name", "test-skill", "--version", b.VersionSHA256, "--epoch", strconv.FormatUint(b.ActivationEpoch, 10), "--note", "stale"}); err == nil {
		t.Fatal("CLI accepted stale review")
	}
	run("quality-resume", "--name", "test-skill", "--version", st.ActiveVersion, "--epoch", strconv.FormatUint(st.ActivationEpoch, 10), "--note", "checked expected repair")
	run("quality", "--name", "test-skill")
	run("archive", "--name", "test-skill")
	run("controller-archive")
	st, _ = store.Read("test-skill")
	if st.Suspended || st.Quality.Paused {
		t.Fatal("host resume not saved")
	}
}
func TestLearningFeedbackHostCLIRejectsModelOnlyReceipt(t *testing.T) {
	cfg, _ := learningControllerHostFixture(t)
	store := learning.NewStore(cfg.Session.SkillDir)
	_, _ = store.Stage(context.Background(), "test-skill", []byte("BASELINE"), []byte("PROPOSED"), "host")
	_, _ = store.ConfigureQuality(context.Background(), "test-skill", learning.QualityPolicy{Kind: "skill", WindowSize: 1, MinimumSamples: 1, MaxFailureRate: 0, Action: "pause", Verifier: "host"})
	file := filepath.Join(t.TempDir(), "model-claim.json")
	if err := os.WriteFile(file, []byte(`{"binding":{},"outcome":{"passed":false,"verifier":"model"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmdLearningFeedback(context.Background(), []string{"feedback", "--name", "test-skill", "--file", file}); err == nil {
		t.Fatal("unbound model claim accepted")
	}
}
