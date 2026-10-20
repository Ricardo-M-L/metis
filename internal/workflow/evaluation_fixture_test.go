package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func repairEvaluationFixture() EvaluationFixture {
	return EvaluationFixture{
		Files:         map[string]string{"calculator.py": "def multiply(a, b):\n    return a + b\n"},
		WritableFiles: []string{"calculator.py"},
		TestPython: `import json, os, sys, unittest
sys.path.insert(0, os.getcwd())
from calculator import multiply
class CalculatorTests(unittest.TestCase):
    def test_positive(self): self.assertEqual(multiply(17, 19), 323)
    def test_zero(self): self.assertEqual(multiply(7, 0), 0)
    def test_negative(self): self.assertEqual(multiply(-4, 5), -20)
result = unittest.TextTestRunner().run(unittest.defaultTestLoader.loadTestsFromTestCase(CalculatorTests))
if not result.wasSuccessful(): sys.exit(1)
with open("result.json", "w") as f: json.dump({"answer": multiply(17,19)}, f)
`,
		ExpectedArtifacts: map[string]json.RawMessage{"result.json": json.RawMessage(`{"answer":323}`)},
	}
}

func prepareRepairFixture(t *testing.T) (*EvaluationFixtureContext, EvaluationFixture) {
	t.Helper()
	if d := sandbox.Doctor(); !d.Available {
		t.Skip(d.Err)
	}
	base := t.TempDir()
	f := repairEvaluationFixture()
	c, err := PrepareEvaluationFixture(filepath.Join(base, "workspace"), filepath.Join(base, "tests"), f, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestEvaluationCodeFixtureRunsRealPythonFailureAndRepair(t *testing.T) {
	c, f := prepareRepairFixture(t)
	_, err := c.RunTests(context.Background())
	var assertion *FixtureAssertionFailure
	if !errors.As(err, &assertion) || !strings.Contains(err.Error(), "AssertionError") {
		t.Fatalf("real buggy Python did not fail assertions: %v", err)
	}
	wf := Workflow{Name: "repair", Steps: []Step{
		{Name: "repair-code", Command: `printf '%s\n' 'def multiply(a, b):' '    return a * b' > calculator.py`},
		{Name: "run-tests", Command: "fixture-test"},
	}}
	results := Run(context.Background(), wf, RunOptions{Cwd: c.Workspace, StopOnError: true, Environment: []string{"PATH=/usr/bin:/bin", "HOME=" + c.Workspace}, EvaluationFixture: c})
	if Failed(results) || results[1].Output != FixtureSuccessOutput {
		t.Fatalf("real repair/test workflow failed: %+v", results)
	}
	var combined strings.Builder
	for _, step := range results {
		combined.WriteString(step.Output)
	}
	if combined.String() != FixtureSuccessOutput {
		t.Fatalf("fixture successful step evidence is not exact host JSON: %q", combined.String())
	}
	report, err := c.Verify(context.Background(), f)
	if err != nil || !report.TestsPassed || !report.ArtifactsPassed || len(report.SourceSHA256["calculator.py"]) != 64 || len(report.ArtifactSHA256["result.json"]) != 64 {
		t.Fatalf("independent artifact acceptance failed: %+v %v", report, err)
	}
	// The parent removes any stale artifact and reruns the trusted test script.
	if err := os.WriteFile(filepath.Join(c.Workspace, "result.json"), []byte(`{"answer":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(context.Background(), f); err != nil {
		t.Fatalf("stale artifact was not independently regenerated: %v", err)
	}
	f.ExpectedArtifacts["result.json"] = json.RawMessage(`{"answer":324}`)
	if _, err := c.Verify(context.Background(), f); !errors.As(err, &assertion) {
		t.Fatalf("wrong business artifact accepted: %v", err)
	}
}

func TestEvaluationCodeFixtureRejectsPolicyAndContextDrift(t *testing.T) {
	c, _ := prepareRepairFixture(t)
	for _, commands := range [][]string{
		{`printf '%s' changed > readonly.py`, `fixture-test`},
		{`printf '%s' changed > checks.py`, `fixture-test`},
		{`printf '%s' changed > ../tests/checks.py`, `fixture-test`},
		{`python3 -c 'print(323)'`, `fixture-test`},
		{`fixture-test > result.json`},
		{`fixture-test other`},
		{`true`},
	} {
		wf := Workflow{Name: "unsafe"}
		for i, command := range commands {
			wf.Steps = append(wf.Steps, Step{Name: string(rune('a' + i)), Command: command})
		}
		if err := ValidateEvaluationFixtureRecipe(wf, c); err == nil {
			t.Errorf("unsafe fixture recipe accepted: %v", commands)
		}
	}
	if err := ValidateEvaluationRecipe(Workflow{Steps: []Step{{Name: "test", Command: "fixture-test"}}}); err == nil {
		t.Fatal("fixture command escaped opt-in boundary")
	}
	data, _ := json.Marshal(c)
	path := filepath.Join(filepath.Dir(c.Workspace), "context.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEvaluationFixtureContext(path, fixtureDigest(data), c.Workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEvaluationFixtureContext(path, strings.Repeat("b", 64), c.Workspace); err == nil {
		t.Fatal("context digest drift accepted")
	}
	if err := os.WriteFile(filepath.Join(c.TestsDir, "checks.py"), []byte("print('forged')"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunTests(context.Background()); err == nil {
		t.Fatal("immutable tests changed without detection")
	}
}

func TestEvaluationFixtureSchemaRejectsPathsAndArtifactSeeds(t *testing.T) {
	for _, mutate := range []func(*EvaluationFixture){
		func(f *EvaluationFixture) { f.Files["../oracle.py"] = "x" },
		func(f *EvaluationFixture) { f.WritableFiles = []string{"missing.py"} },
		func(f *EvaluationFixture) { f.Files["result.json"] = `{"answer":323}` },
		func(f *EvaluationFixture) { f.ExpectedArtifacts["../result.json"] = json.RawMessage(`{}`) },
		func(f *EvaluationFixture) { f.ExpectedArtifacts["result.json"] = json.RawMessage(`{} {}`) },
	} {
		f := repairEvaluationFixture()
		mutate(&f)
		if err := f.Validate(); err == nil {
			t.Fatal("unsafe fixture schema accepted")
		}
	}
	data, _ := json.Marshal(EvaluationFixtures{SchemaVersion: 1, Cases: map[string]EvaluationFixture{"repair": repairEvaluationFixture()}})
	if _, err := DecodeEvaluationFixtures(data); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeEvaluationFixtures(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("extra fixture object accepted")
	}
}

func TestEvaluationFixtureRejectsSymlinkArtifact(t *testing.T) {
	c, f := prepareRepairFixture(t)
	if err := os.WriteFile(filepath.Join(c.Workspace, "calculator.py"), []byte("def multiply(a,b): return a*b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.TestPython = `import os
os.symlink("calculator.py", "result.json")
`
	if err := os.WriteFile(filepath.Join(c.TestsDir, "checks.py"), []byte(f.TestPython), 0o600); err != nil {
		t.Fatal(err)
	}
	c.TestsSHA256 = fixtureDigest([]byte(f.TestPython))
	if _, err := c.Verify(context.Background(), f); err == nil || !strings.Contains(err.Error(), "without symlinks") {
		t.Fatalf("symlink artifact accepted: %v", err)
	}
}
