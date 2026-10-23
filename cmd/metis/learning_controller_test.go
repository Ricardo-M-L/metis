package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
)

func learningControllerHostFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg := &config.Config{Provider: config.ProviderSet{Default: "anthropic", Anthropic: config.ProviderAnthropic{BaseURL: "https://initial.test.invalid", Model: "fixture-model", APIKey: "unit-secret-fixture", MaxTokens: 1024}}}
	writeLearningControllerConfig(t, home, cfg)
	loaded, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return loaded, home
}
func writeLearningControllerConfig(t *testing.T, home string, cfg *config.Config) {
	t.Helper()
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
func TestLearningControllerConfigureFreezesSuiteAndExplicitAllowance(t *testing.T) {
	requireCoordinatorPOSIXTest(t, "learning host mutation requires POSIX directory fsync")
	_, home := learningControllerHostFixture(t)
	data, _ := json.Marshal(validLearningSuiteFixture())
	suite := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(suite, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"configure", "--name", "test-skill", "--suite", suite, "--max-evaluations", "2", "--max-total-tokens", "400000", "--auto-promote"}
	if err := cmdLearningController(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	c := learning.NewController(filepath.Join(home, "learning-controller"))
	st, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	p := st.Policies["skill/test-skill"]
	if !p.AutoPromote || p.MaxEvaluations != 2 || p.MaxTotalTokens != 400000 || p.SuiteSHA256 != learning.SuiteDigest(validLearningSuiteFixture()) || p.Identity.Provider != "anthropic" || p.Identity.Model != "fixture-model" {
		t.Fatalf("policy=%+v", p)
	}
	if err := os.Remove(suite); err != nil {
		t.Fatal(err)
	}
	// Polling an empty candidate store needs neither the original suite file
	// nor an API request. Inspection reads the frozen checker and receipts.
	if err := cmdLearningController(context.Background(), []string{"watch", "--once", "--name", "test-skill"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdLearningController(context.Background(), []string{"policy", "--name", "test-skill"}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(c.Dir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("unit-secret-fixture")) {
		t.Fatal("controller policy stored provider API key")
	}
}
func TestLearningControllerRequiresHostConfigurationAndBudget(t *testing.T) {
	_, home := learningControllerHostFixture(t)
	data, _ := json.Marshal(validLearningSuiteFixture())
	suite := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(suite, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdLearningController(context.Background(), []string{"configure", "--name", "test-skill", "--suite", suite}); err == nil {
		t.Fatal("implicit paid-loop budget accepted")
	}
	if err := cmdLearningController(context.Background(), []string{"watch", "--once", "--name", "missing"}); err == nil {
		t.Fatal("unconfigured checker invented")
	}
	if err := cmdLearningController(context.Background(), []string{"watch", "--once"}); err != nil {
		t.Fatal(err)
	}
	st, err := learning.NewController(filepath.Join(home, "learning-controller")).Read()
	if err != nil || len(st.Policies) != 0 || len(st.Receipts) != 0 {
		t.Fatalf("unconfigured watch paid: %+v %v", st, err)
	}
}
func TestLearningControllerIdentityDetectsRouteChangeAndIgnoresKeyRotation(t *testing.T) {
	cfg, _ := learningControllerHostFixture(t)
	i, err := learningControllerIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider.Anthropic.APIKey = "another-secret-fixture"
	rotated, err := learningControllerIdentity(cfg)
	if err != nil || rotated != i {
		t.Fatalf("key rotation changed route identity: %+v %v", rotated, err)
	}
	cfg.Provider.Anthropic.BaseURL = "https://different.test.invalid"
	changed, err := learningControllerIdentity(cfg)
	if err != nil || changed.ProviderConfigSHA256 == i.ProviderConfigSHA256 {
		t.Fatalf("changed route retained identity: %+v %v", changed, err)
	}
	skillRoot, err := learningControllerArtifactRoot(cfg, "skill")
	if err != nil || !filepath.IsAbs(skillRoot) {
		t.Fatalf("artifact root=%q %v", skillRoot, err)
	}
	workflowRoot, err := learningControllerArtifactRoot(cfg, "workflow")
	if err != nil || workflowRoot == skillRoot || !filepath.IsAbs(workflowRoot) {
		t.Fatalf("workflow root=%q %v", workflowRoot, err)
	}
}
func TestLearningControllerBackendRejectsChangedRouteBeforeRealQuery(t *testing.T) {
	cfg, home := learningControllerHostFixture(t)
	i, err := learningControllerIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, err := learningControllerArtifactRoot(cfg, "skill")
	if err != nil {
		t.Fatal(err)
	}
	p := learning.ControllerPolicy{ControllerPolicySpec: learning.ControllerPolicySpec{Kind: "skill", Name: "test-skill", ArtifactRoot: root, Suite: validLearningSuiteFixture(), Identity: i}}
	cfg.Provider.Anthropic.BaseURL = "https://changed.test.invalid"
	writeLearningControllerConfig(t, home, cfg)
	e, err := learningControllerBackend(cfg).Evaluate(context.Background(), p, "fixture-version")
	if err == nil || e.ID != "" {
		t.Fatalf("changed route dispatched evaluation: %+v %v", e, err)
	}
}
