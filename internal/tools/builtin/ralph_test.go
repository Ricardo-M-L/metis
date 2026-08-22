package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ralphFakeSpawner drives the loop without a provider: it writes the
// report file the real child would write, following the prompts script.
type ralphFakeSpawner struct {
	calls    []string // prompts received per round
	reports  []string // report bodies to write per round (status first)
	cwd      string
	provider llm.Provider
	registry *tools.Registry
	gate     *permission.Gate
}

func (f *ralphFakeSpawner) spawn(ctx context.Context, prompt string) (string, error) {
	f.calls = append(f.calls, prompt)
	if len(f.reports) == 0 {
		return "child returned without writing a report", nil
	}
	i := len(f.calls) - 1
	if i >= len(f.reports) {
		// keep repeating the last scripted report
		i = len(f.reports) - 1
	}
	// extract the report path from the prompt ("write your report to <path>")
	var reportPath string
	if idx := strings.Index(prompt, "write your report to "); idx >= 0 {
		rest := prompt[idx+len("write your report to "):]
		end := strings.IndexAny(rest, " \n")
		if end < 0 {
			end = len(rest)
		}
		reportPath = strings.TrimSpace(rest[:end])
	}
	if reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(reportPath, []byte(f.reports[i]), 0o644); err != nil {
			return "", err
		}
	}
	return "child done", nil
}

func newRalphTool(t *testing.T) (*Ralph, *ralphFakeSpawner) {
	t.Helper()
	spawner := &ralphFakeSpawner{}
	r := NewRalph(permission.New(permission.ModeDefault), nil, tools.NewRegistry(), "m", "s")
	// IsEnabled requires non-nil provider/registry — registry set above;
	// provider: use a nil-safe trick? Execute only checks IsEnabled, and
	// spawn is replaced, so point provider at a non-nil dummy via the
	// interface: llm.Provider(nil) fails IsEnabled. Use fakeNoopProvider.
	r.provider = noopProv{}
	return r.WithSpawner(spawner.spawn), spawner
}

// noopProv satisfies llm.Provider minimally (never called — spawn is faked).
type noopProv struct{}

func (noopProv) Name() string { return "noop" }
func (noopProv) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (noopProv) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, nil
}
func (noopProv) MaxContextTokens() int { return 1 }
func (noopProv) ModelID() string       { return "noop" }

func TestRalph_CompletesWhenChildDeclaresComplete(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	r, sp := newRalphTool(t)
	sp.reports = []string{
		"status: progress\nfound the bug, not fixed yet\n",
		"status: complete\nfixed and verified with tests\n",
	}
	res, err := r.Execute(context.Background(), map[string]any{"objective": "fix the flaky test", "maxRounds": 5})
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %s", err, res.Output)
	}
	if len(sp.calls) != 2 {
		t.Fatalf("want 2 rounds, got %d", len(sp.calls))
	}
	if !strings.Contains(res.Output, "status: complete") {
		t.Fatalf("final status missing:\n%s", res.Output)
	}
	// Round 2's prompt must carry round 1's report verbatim.
	if !strings.Contains(sp.calls[1], "found the bug, not fixed yet") {
		t.Fatalf("round-2 prompt missing round-1 report:\n%s", sp.calls[1][:300])
	}
	// Ledger persisted.
	b, err := os.ReadFile(filepath.Join(".metis", "ralph", filepath.Base(filepath.Dir(filepath.Join(".metis", "ralph", "x"))), "state.json"))
	_ = b
	_ = err
	// (ledger path asserted structurally below via glob)
	matches, _ := filepath.Glob(filepath.Join(".metis", "ralph", "*", "state.json"))
	if len(matches) != 1 {
		t.Fatalf("want 1 state.json, got %v", matches)
	}
	var ledger map[string]any
	if err := json.Unmarshal(readFileT(t, matches[0]), &ledger); err != nil {
		t.Fatalf("ledger not json: %v", err)
	}
	if ledger["status"] != "complete" {
		t.Fatalf("ledger status = %v", ledger["status"])
	}
}

func TestRalph_ObjectiveImmutableAcrossRounds(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	os.Chdir(dir)
	r, sp := newRalphTool(t)
	sp.reports = []string{"status: progress\nround one\n"}
	res, _ := r.Execute(context.Background(), map[string]any{"objective": "ship the release", "maxRounds": 1})
	if !strings.Contains(res.Output, "max_rounds") {
		t.Fatalf("expected max_rounds stop:\n%s", res.Output)
	}
	if !strings.Contains(sp.calls[0], "ship the release") {
		t.Fatalf("objective not in child prompt:\n%s", sp.calls[0][:200])
	}
	if !strings.Contains(sp.calls[0], "round 1 of 1") {
		t.Fatalf("round framing missing:\n%s", sp.calls[0][:200])
	}
}

func TestRalph_BlockedStopsLoop(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	os.Chdir(dir)
	r, sp := newRalphTool(t)
	sp.reports = []string{
		"status: blocked\nno API credentials in env\n",
	}
	res, _ := r.Execute(context.Background(), map[string]any{"objective": "call the API", "maxRounds": 5})
	if len(sp.calls) != 1 {
		t.Fatalf("blocked must stop after 1 round, got %d", len(sp.calls))
	}
	if !strings.Contains(res.Output, "status: blocked") {
		t.Fatalf("blocked status missing:\n%s", res.Output)
	}
}

func TestRalph_NoReportStops(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	defer os.Chdir(old)
	os.Chdir(dir)
	r, sp := newRalphTool(t)
	sp.reports = []string{} // child returns but writes nothing
	res, _ := r.Execute(context.Background(), map[string]any{"objective": "x"})
	if !strings.Contains(res.Output, "no_report") {
		t.Fatalf("expected no_report stop:\n%s", res.Output)
	}
}

func TestRalphStatus_Parsing(t *testing.T) {
	cases := map[string]string{
		"status: complete\n":                  "complete",
		"status: complete with caveats\n":     "complete",
		"status: blocked needs credentials\n": "blocked",
		"status: progress\n":                  "progress",
		"\n\nstatus: complete\n":              "complete",
		"no status line at all\n":             "progress",
		"status: somethingelse\n":             "progress",
	}
	for in, want := range cases {
		if got := ralphReportStatus(in); got != want {
			t.Errorf("ralphReportStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func readFileT(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}
