package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

const MaxEvaluationFixtureBytes = 1024 * 1024
const FixtureSuccessOutput = "{\"tests_passed\":true}\n"

// EvaluationFixtures is a host-authored, opt-in code evaluation suite. No host
// paths or shell commands are accepted: every case starts from inline files in
// a fresh workspace and runs one immutable Python unittest script.
type EvaluationFixtures struct {
	SchemaVersion int                          `json:"schema_version"`
	Cases         map[string]EvaluationFixture `json:"cases"`
}

type EvaluationFixture struct {
	Files             map[string]string          `json:"files"`
	WritableFiles     []string                   `json:"writable_files"`
	TestPython        string                     `json:"test_python"`
	ExpectedArtifacts map[string]json.RawMessage `json:"expected_artifacts"`
}

// EvaluationFixtureContext contains execution capability paths, not expected
// artifacts. Its exact bytes are pinned by the parent outside the workspace.
type EvaluationFixtureContext struct {
	Workspace     string   `json:"workspace"`
	TestsDir      string   `json:"tests_dir"`
	WritableFiles []string `json:"writable_files"`
	FixtureSHA256 string   `json:"fixture_sha256"`
	TestsSHA256   string   `json:"tests_sha256"`
}

type EvaluationFixtureReport struct {
	FixtureSHA256   string            `json:"fixture_sha256"`
	TestsSHA256     string            `json:"tests_sha256"`
	SourceSHA256    map[string]string `json:"source_sha256"`
	ArtifactSHA256  map[string]string `json:"artifact_sha256"`
	TestsPassed     bool              `json:"tests_passed"`
	TestExitCode    int               `json:"test_exit_code"`
	ArtifactsPassed bool              `json:"artifacts_passed"`
	Error           string            `json:"error,omitempty"`
}

// FixtureAssertionFailure is a completed, failing test/assertion, rather than
// an unavailable evaluator. Baseline failures remain comparable to a repaired
// candidate; infrastructure failures still stop promotion.
type FixtureAssertionFailure struct {
	Reason   string
	ExitCode int
}

func (e *FixtureAssertionFailure) Error() string { return e.Reason }

func fixtureDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func DecodeEvaluationFixtures(data []byte) (EvaluationFixtures, error) {
	var fixtures EvaluationFixtures
	if len(data) == 0 || len(data) > MaxEvaluationFixtureBytes {
		return fixtures, errors.New("fixture suite must contain 1..1048576 bytes")
	}
	if err := decodeFixtureJSON(data, &fixtures); err != nil {
		return fixtures, err
	}
	if fixtures.SchemaVersion != 1 || len(fixtures.Cases) == 0 || len(fixtures.Cases) > 32 {
		return fixtures, errors.New("fixture suite requires schema_version 1 and 1..32 cases")
	}
	for id, fixture := range fixtures.Cases {
		if strings.TrimSpace(id) == "" {
			return fixtures, errors.New("fixture case requires an ID")
		}
		if err := fixture.Validate(); err != nil {
			return fixtures, fmt.Errorf("fixture %s: %w", id, err)
		}
	}
	return fixtures, nil
}

func (f EvaluationFixture) Validate() error {
	if len(f.Files) == 0 || len(f.Files) > 16 || len(f.WritableFiles) == 0 || len(f.WritableFiles) > len(f.Files) || len(f.TestPython) == 0 || len(f.TestPython) > 65536 || len(f.ExpectedArtifacts) == 0 || len(f.ExpectedArtifacts) > 16 {
		return errors.New("fixture requires 1..16 files, declared writable sources, bounded Python tests and 1..16 JSON artifact assertions")
	}
	for name, content := range f.Files {
		if !fixtureFilename(name) || len(content) > 65536 {
			return errors.New("fixture files require bounded content and simple filenames")
		}
	}
	seen := map[string]bool{}
	for _, name := range f.WritableFiles {
		if _, ok := f.Files[name]; !ok || seen[name] || !strings.HasSuffix(name, ".py") {
			return errors.New("writable fixture sources must be unique declared .py files")
		}
		seen[name] = true
	}
	for name, expected := range f.ExpectedArtifacts {
		if !fixtureFilename(name) || !strings.HasSuffix(name, ".json") || len(expected) == 0 || len(expected) > 65536 {
			return errors.New("fixture artifact assertions require bounded JSON and simple .json filenames")
		}
		if _, ok := f.Files[name]; ok {
			return errors.New("artifact must be produced by tests, not supplied as a seed file")
		}
		if _, err := fixtureJSONValue(expected); err != nil {
			return fmt.Errorf("fixture expected artifact: %w", err)
		}
	}
	return nil
}

func fixtureFilename(name string) bool {
	return evaluationFile.MatchString(name) && !strings.Contains(name, "..")
}

func PrepareEvaluationFixture(workspace, testsDir string, f EvaluationFixture, digest string) (*EvaluationFixtureContext, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if len(digest) != 64 {
		return nil, errors.New("fixture suite SHA256 required")
	}
	for _, dir := range []string{workspace, testsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	testsDir, err = filepath.EvalSymlinks(testsDir)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(workspace) != filepath.Dir(testsDir) || workspace == testsDir {
		return nil, errors.New("fixture workspace and tests must be separate sibling directories")
	}
	for name, content := range f.Files {
		if err := writeFreshFixtureFile(filepath.Join(workspace, name), []byte(content)); err != nil {
			return nil, err
		}
	}
	if err := writeFreshFixtureFile(filepath.Join(testsDir, "checks.py"), []byte(f.TestPython)); err != nil {
		return nil, err
	}
	return &EvaluationFixtureContext{Workspace: workspace, TestsDir: testsDir, WritableFiles: append([]string(nil), f.WritableFiles...), FixtureSHA256: digest, TestsSHA256: fixtureDigest([]byte(f.TestPython))}, nil
}

func writeFreshFixtureFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}

func LoadEvaluationFixtureContext(path, digest, cwd string) (*EvaluationFixtureContext, error) {
	data, err := readFixtureFile(path, MaxEvaluationFixtureBytes)
	if err != nil {
		return nil, err
	}
	if len(digest) != 64 || fixtureDigest(data) != digest {
		return nil, errors.New("fixture execution context changed")
	}
	var c EvaluationFixtureContext
	if err := decodeFixtureJSON(data, &c); err != nil {
		return nil, err
	}
	if c.Workspace != cwd || len(c.FixtureSHA256) != 64 || len(c.TestsSHA256) != 64 || len(c.WritableFiles) == 0 {
		return nil, errors.New("fixture execution context does not match the pinned workspace")
	}
	for _, name := range c.WritableFiles {
		if !fixtureFilename(name) || !strings.HasSuffix(name, ".py") {
			return nil, errors.New("invalid fixture writable source")
		}
	}
	if err := c.validateTests(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *EvaluationFixtureContext) CanWrite(name string) bool {
	for _, writable := range c.WritableFiles {
		if name == writable {
			return true
		}
	}
	return false
}

func (c *EvaluationFixtureContext) validateTests() error {
	data, err := readFixtureFile(filepath.Join(c.TestsDir, "checks.py"), 65536)
	if err != nil {
		return err
	}
	if fixtureDigest(data) != c.TestsSHA256 {
		return errors.New("immutable fixture tests changed")
	}
	return nil
}

// RunTests executes only the pinned system interpreter and immutable host test
// script. A separate sandbox denies all host reads except runtime, workspace
// and tests, and denies networking and writes outside the workspace.
func (c *EvaluationFixtureContext) RunTests(ctx context.Context) (string, error) {
	if err := c.validateTests(); err != nil {
		return "", err
	}
	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Set hard CPU/file-size limits before exec; candidate Python cannot raise
	// them again. The shell script is fixed host code, never candidate input.
	cmd := exec.CommandContext(testCtx, "/bin/sh", "-c", `ulimit -f 2048 && ulimit -t 20 && exec /usr/bin/python3 -I -B "$1"`, "fixture-tests", filepath.Join(c.TestsDir, "checks.py"))
	cmd.Dir = c.Workspace
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "HOME=" + c.Workspace, "TMPDIR=" + c.Workspace, "PYTHONDONTWRITEBYTECODE=1"}
	wrapped, err := sandbox.WrapEvaluation(cmd, c.Workspace, c.TestsDir)
	if err != nil {
		return "", err
	}
	var output fixtureOutputBuffer
	wrapped.Stdout, wrapped.Stderr = &output, &output
	if err := wrapped.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && testCtx.Err() == nil {
			return "", &FixtureAssertionFailure{Reason: fmt.Sprintf("host fixture tests failed (exit %d): %s", exit.ExitCode(), output.String()), ExitCode: exit.ExitCode()}
		}
		return "", fmt.Errorf("host fixture test process unavailable: %w", err)
	}
	if err := c.validateTests(); err != nil {
		return "", err
	}
	return FixtureSuccessOutput, nil
}

// Verify reruns tests independently after the model has finished. It removes
// prior artifact files first and checks fresh, regular JSON artifacts against
// expectations that were never written into the candidate workspace.
func (c *EvaluationFixtureContext) Verify(ctx context.Context, f EvaluationFixture) (EvaluationFixtureReport, error) {
	r := EvaluationFixtureReport{FixtureSHA256: c.FixtureSHA256, TestsSHA256: c.TestsSHA256, SourceSHA256: map[string]string{}, ArtifactSHA256: map[string]string{}, TestExitCode: -1}
	for name := range f.Files {
		data, err := readFixtureFile(filepath.Join(c.Workspace, name), 65536)
		if err != nil {
			return r, err
		}
		r.SourceSHA256[name] = fixtureDigest(data)
		if !c.CanWrite(name) && string(data) != f.Files[name] {
			return r, errors.New("read-only fixture seed changed")
		}
	}
	for name := range f.ExpectedArtifacts {
		if err := os.Remove(filepath.Join(c.Workspace, name)); err != nil && !os.IsNotExist(err) {
			return r, err
		}
	}
	if _, err := c.RunTests(ctx); err != nil {
		var assertion *FixtureAssertionFailure
		if errors.As(err, &assertion) {
			r.TestExitCode = assertion.ExitCode
		}
		return r, err
	}
	r.TestsPassed = true
	r.TestExitCode = 0
	for name, expected := range f.ExpectedArtifacts {
		data, err := readFixtureFile(filepath.Join(c.Workspace, name), 65536)
		if err != nil {
			return r, &FixtureAssertionFailure{Reason: fmt.Sprintf("host fixture artifact %s missing or invalid: %v", name, err)}
		}
		r.ArtifactSHA256[name] = fixtureDigest(data)
		actual, err := fixtureJSONValue(data)
		if err != nil {
			return r, &FixtureAssertionFailure{Reason: fmt.Sprintf("host fixture artifact %s malformed: %v", name, err)}
		}
		want, err := fixtureJSONValue(expected)
		if err != nil {
			return r, err
		}
		if !reflect.DeepEqual(actual, want) {
			return r, &FixtureAssertionFailure{Reason: fmt.Sprintf("host fixture artifact %s failed JSON assertion", name)}
		}
	}
	for name, before := range r.SourceSHA256 {
		data, err := readFixtureFile(filepath.Join(c.Workspace, name), 65536)
		if err != nil {
			return r, err
		}
		if fixtureDigest(data) != before {
			return r, errors.New("fixture source changed during independent host tests")
		}
	}
	r.ArtifactsPassed = true
	return r, nil
}

func readFixtureFile(path string, max int) ([]byte, error) {
	// Resolve through a directory capability so a candidate racing a symlink
	// replacement cannot make the unsandboxed host read outside that directory.
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(max) {
		return nil, errors.New("fixture requires a bounded regular file without symlinks")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("fixture file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, errors.New("fixture file exceeds size limit")
	}
	return data, nil
}

func decodeFixtureJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("fixture requires one JSON object")
	}
	return nil
}
func fixtureJSONValue(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var out, extra any
	if err := d.Decode(&out); err != nil {
		return nil, err
	}
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("fixture artifact requires one JSON value")
	}
	return out, nil
}

// Bound capture while the process is running, not only after it exits.
type fixtureOutputBuffer struct{ bytes.Buffer }

func (b *fixtureOutputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remain := 16384 - b.Len(); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
