package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	"github.com/Ricardo-M-L/metis/pkg/tool"
)

func TestCoordinatorOperationFlagUsageHandlesZeroValueAndShowsDefaults(t *testing.T) {
	var nilFlag *coordinatorOperationOnceFlag
	if nilFlag.String() != "" || (&coordinatorOperationOnceFlag{}).String() != "" {
		t.Fatal("zero flag wrapper must have a safe empty string representation")
	}
	fs := flag.NewFlagSet("coordinator operation test", flag.ContinueOnError)
	var output bytes.Buffer
	fs.SetOutput(&output)
	fs.String("cwd", "/canonical-workspace", "project working directory")
	fs.Bool("json", true, "emit metadata")
	if err := parseCoordinatorOperationFlags(fs, []string{"--unknown"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unknown flag was not rejected: %v", err)
	}
	for _, expected := range []string{"flag provided but not defined", "Usage of coordinator operation test:", "-cwd value", "(default /canonical-workspace)", "-json", "(default true)"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("flag usage lost %q: %s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), "panic") {
		t.Fatalf("flag usage reported a String panic: %s", output.String())
	}
	output.Reset()
	fs.Usage()
	if strings.Contains(output.String(), "panic") || !strings.Contains(output.String(), "(default /canonical-workspace)") || !strings.Contains(output.String(), "(default true)") {
		t.Fatalf("explicit Usage did not safely display defaults: %s", output.String())
	}
}

func TestCoordinatorOperationCLIRejectsAmbiguousOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no_action", nil, "usage:"},
		{"unknown_action", []string{"replay"}, "usage:"},
		{"missing_run", []string{"list"}, "usage:"},
		{"flag_run", []string{"list", "--json"}, "usage:"},
		{"extra_list_position", []string{"list", "run", "extra"}, "unexpected positional"},
		{"extra_resolve_position", []string{"resolve", "run", "op", "--outcome", "not_applied", "--note", "checked", "extra"}, "unexpected positional"},
		{"unknown_option", []string{"list", "run", "--result-file", "file"}, "flag provided but not defined"},
		{"duplicate_option", []string{"list", "run", "--cwd", "a", "--cwd=b"}, "cannot be repeated"},
		{"duplicate_bool", []string{"list", "run", "--json", "--json=false"}, "cannot be repeated"},
		{"missing_operation", []string{"resolve", "run"}, "usage:"},
		{"missing_outcome", []string{"resolve", "run", "op", "--note", "checked"}, "--outcome"},
		{"invalid_outcome", []string{"resolve", "run", "op", "--outcome", "success", "--note", "checked"}, "--outcome"},
		{"missing_note", []string{"resolve", "run", "op", "--outcome", "not_applied"}, "--note"},
		{"blank_note", []string{"resolve", "run", "op", "--outcome", "not_applied", "--note", " \t"}, "--note"},
		{"oversized_note", []string{"resolve", "run", "op", "--outcome", "not_applied", "--note", strings.Repeat("x", 8193)}, "--note"},
		{"nul_note", []string{"resolve", "run", "op", "--outcome", "not_applied", "--note", "audit\x00receipt"}, "--note"},
		{"missing_result", []string{"resolve", "run", "op", "--outcome", "applied", "--note", "checked"}, "--result-file is required"},
		{"not_applied_result", []string{"resolve", "run", "op", "--outcome", "not_applied", "--note", "checked", "--result-file", "file"}, "must not be supplied"},
		{"not_applied_empty_result", []string{"resolve", "run", "op", "--outcome", "not_applied", "--note", "checked", "--result-file="}, "must not be supplied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdProjectCoordinatorOperations(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCoordinatorOperationResultPreservesCompleteSuccessfulResult(t *testing.T) {
	input := `{"Output":"remote receipt","IsError":false,"Display":"short display","Meta":{"id":9007199254740993},"Presentation":{"kind":"card","parts":[{"title":"receipt"}]},"Images":[{"MediaType":"image/png","Data":"aW1hZ2U="}]}`
	data, err := decodeCoordinatorOperationResult([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	var result tool.Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Output != "remote receipt" || result.IsError || result.Display != "short display" || result.Presentation["kind"] != "card" || len(result.Images) != 1 || result.Images[0].Data != "aW1hZ2U=" {
		t.Fatalf("complete result was lost: %+v", result)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Fatal("receipt identifier lost integer precision")
	}
	for _, valid := range []string{`{"Output":"receipt"}`, `{"Output":"receipt","IsError":false,"Meta":null,"Presentation":null,"Images":null}`} {
		if _, err := decodeCoordinatorOperationResult([]byte(valid)); err != nil {
			t.Fatalf("valid Go result rejected: %s: %v", valid, err)
		}
	}
}

func TestCoordinatorOperationResultRejectsMalformedOrFailedReceipts(t *testing.T) {
	for _, input := range []string{
		``, `null`, `[]`, `{`, `{"Output":"ok"}{}`, `{"Output":"ok"} true`,
		`{"Output":"one","Output":"two"}`, `{"Output":"ok","Meta":{"id":1,"id":2}}`,
		`{"Output":"ok","Presentation":{"parts":[{"id":1,"id":2}]}}`,
		`{"Output":"ok","Images":[{"MediaType":"image/png","Data":"one","Data":"two"}]}`,
		`{"Output":"ok","unknown":"field"}`, `{"output":"ok"}`, `{"Output":1}`,
		`{"Output":null}`, `{"Display":null}`, `{"IsError":null}`, `{"IsError":"false"}`, `{"IsError":true}`,
		`{"Meta":[]}`, `{"Presentation":true}`, `{"Images":{}}`, `{"Images":[null]}`,
		`{"Images":[{"MediaType":null}]}`, `{"Images":[{"Data":1}]}`, `{"Images":[{"extra":"field"}]}`,
		`{"Meta":` + strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130) + "}",
	} {
		t.Run(input[:min(45, len(input))], func(t *testing.T) {
			if _, err := decodeCoordinatorOperationResult([]byte(input)); err == nil {
				t.Fatalf("accepted invalid or failed result: %s", input)
			}
		})
	}
	if _, err := decodeCoordinatorOperationResult([]byte(`{"Output":"` + strings.Repeat("x", projectcoord.MaxOperationResultBytes) + `"}`)); err == nil {
		t.Fatal("accepted an oversized result")
	}
}

func coordinatorOperationCanonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoordinatorOperationResultFileRequiresPrivateCanonicalRegularFile(t *testing.T) {
	dir := coordinatorOperationCanonicalTempDir(t)
	path := filepath.Join(dir, "result.json")
	if err := os.WriteFile(path, []byte(`{"Output":"receipt","IsError":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorOperationResult(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorOperationResult(dir); err == nil {
		t.Fatal("accepted a directory as a receipt")
	}
	if _, err := readCoordinatorOperationResult(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("accepted a missing receipt")
	}
	if goruntime.GOOS == "windows" {
		t.Skip("Unix permission and symlink checks require a Unix filesystem")
	}
	for _, mode := range []os.FileMode{0644, 0640, 0610, 0700} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := readCoordinatorOperationResult(path); err == nil {
			t.Fatalf("accepted receipt permissions %o", mode)
		}
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorOperationResult(path); err != nil {
		t.Fatalf("rejected a narrower readable mode: %v", err)
	}
	fileLink := filepath.Join(dir, "result-link.json")
	if err := os.Symlink(path, fileLink); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorOperationResult(fileLink); err == nil {
		t.Fatal("accepted a receipt symlink")
	}
	parentLink := filepath.Join(dir, "parent-link")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorOperationResult(filepath.Join(parentLink, "result.json")); err == nil {
		t.Fatal("accepted a symlink in a receipt ancestor")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", projectcoord.MaxOperationResultBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorOperationResult(path); err == nil {
		t.Fatal("accepted an oversized receipt file")
	}
}

type coordinatorOperationCLIFixture struct {
	store *projectcoord.Store
	run   projectcoord.Run
	item  projectcoord.WorkItem
	op    projectcoord.OperationRecord
}

func newCoordinatorOperationCLIFixture(t *testing.T, attempts int) coordinatorOperationCLIFixture {
	t.Helper()
	t.Setenv("METIS_HOME", coordinatorOperationCanonicalTempDir(t))
	store := projectCoordinatorStore()
	run, err := store.Create(projectcoord.CreateInput{
		Workspace: coordinatorOperationCanonicalTempDir(t), Goal: "Operation CLI test",
		Items: []projectcoord.WorkItemInput{{ID: "write", Phase: projectcoord.PhaseCustom, Subject: "Write", Prompt: "Write a test remote object", MaxAttempts: attempts}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "test-writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("private raw remote arguments"))
	op, err := store.BeginOperation(run.Workspace, run.ID, item.ID, item.Owner, item.Lease.Token, projectcoord.OperationInput{ToolName: "remote.write", InputSHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	return coordinatorOperationCLIFixture{store, run, item, op}
}

func captureCoordinatorOperationOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = original }()
	err = run()
	data, readErr := os.ReadFile(file.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), err
}

func TestCoordinatorOperationListCLIExcludesPrivateResultAndClaim(t *testing.T) {
	f := newCoordinatorOperationCLIFixture(t, 2)
	if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, f.op.ID, json.RawMessage(`{"Output":"private remote receipt","Meta":{"key":"private receipt metadata"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, jsonOut := range []bool{false, true} {
		args := []string{"list", f.run.ID, "--cwd", f.run.Workspace}
		if jsonOut {
			args = append(args, "--json")
		}
		output, err := captureCoordinatorOperationOutput(t, func() error { return cmdProjectCoordinatorOperations(args) })
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"private remote receipt", "private receipt metadata", "private raw remote arguments", f.item.Lease.Token, "result_json", "claim_token"} {
			if strings.Contains(output, private) {
				t.Fatalf("operation list exposed private field %q", private)
			}
		}
		if !strings.Contains(output, f.op.ID) || !strings.Contains(output, "completed") {
			t.Fatalf("list lost operation metadata: %s", output)
		}
		if jsonOut {
			var payload struct {
				RunID      string                         `json:"run_id"`
				Operations []coordinatorOperationMetadata `json:"operations"`
			}
			if json.Unmarshal([]byte(output), &payload) != nil || payload.RunID != f.run.ID || len(payload.Operations) != 1 || !payload.Operations[0].ResultStored || payload.Operations[0].Occurrence != 1 {
				t.Fatalf("invalid metadata JSON: %s", output)
			}
		}
	}
}

func TestCoordinatorOperationMetadataOccurrenceIncludesLegacySlotOne(t *testing.T) {
	for _, occurrence := range []int{0, 1, 2, 1024} {
		op := projectcoord.OperationRecord{
			ID: "test-operation", Occurrence: occurrence, ClaimToken: "private claim token",
			ResultJSON: json.RawMessage(`{"Output":"private result"}`), Note: "private diagnostic",
			ResolutionNote: "private resolution evidence",
		}
		metadata := coordinatorOperationPublicMetadata(op)
		if metadata.Occurrence != max(1, occurrence) {
			t.Fatalf("occurrence=%d projected as %d", occurrence, metadata.Occurrence)
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"occurrence":`) || strings.Contains(string(data), "private") || strings.Contains(string(data), "claim_token") || strings.Contains(string(data), "result_json") {
			t.Fatalf("occurrence metadata omitted the slot or exposed private evidence: %s", data)
		}
	}
}

func TestCoordinatorOperationResolveCLIRequiresFencedLeaseAndExplicitRecovery(t *testing.T) {
	f := newCoordinatorOperationCLIFixture(t, 1)
	if _, err := f.store.MarkOperationUncertain(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, f.op.ID, "private transport diagnostic"); err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(coordinatorOperationCanonicalTempDir(t), "result.json")
	result := `{"Output":"private remote receipt","IsError":false,"Presentation":{"remote_id":"receipt-123"},"Images":[{"MediaType":"image/png","Data":"aW1hZ2U="}]}`
	if err := os.WriteFile(resultFile, []byte(result), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"resolve", f.run.ID, f.op.ID, "--cwd", f.run.Workspace, "--outcome", "applied", "--result-file", resultFile, "--note", "private remote audit receipt", "--json"}
	if err := cmdProjectCoordinatorOperations(args); err == nil || !strings.Contains(err.Error(), "unexpired worker lease") {
		t.Fatalf("live preparing worker was not fenced before resolution: %v", err)
	}
	time.Sleep(max(time.Until(f.item.Lease.ExpiresAt)+20*time.Millisecond, 0))
	output, err := captureCoordinatorOperationOutput(t, func() error { return cmdProjectCoordinatorOperations(args) })
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private remote receipt", "private transport diagnostic", "private remote audit receipt", f.item.Lease.Token, "result_json", "claim_token"} {
		if strings.Contains(output, private) {
			t.Fatalf("resolve exposed private evidence %q", private)
		}
	}
	if !strings.Contains(output, `"work_recovery_required": true`) {
		t.Fatalf("resolution did not preserve explicit recovery boundary: %s", output)
	}
	run, err := f.store.Get(f.run.Workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Operations) != 1 || run.Operations[0].Status != projectcoord.OperationCompleted || run.Operations[0].Resolution != "applied" || len(run.Items) != 1 || run.Items[0].Status != projectcoord.ItemFailed || run.Items[0].Attempts != 1 {
		t.Fatalf("resolution expanded or silently recovered work: %+v", run)
	}
	var fullResult tool.Result
	if json.Unmarshal(run.Operations[0].ResultJSON, &fullResult) != nil || fullResult.Presentation["remote_id"] != "receipt-123" || len(fullResult.Images) != 1 {
		t.Fatal("applied resolution did not persist the complete tool result")
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "Audited outcome, try again", false); err == nil {
		t.Fatal("operator resolution expanded the original exhausted attempt budget")
	}
	if err := cmdProjectCoordinatorOperations(args); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("completed operation was resolved a second time: %v", err)
	}
}

func TestCoordinatorOperationNotAppliedCLIRecordsResolutionWithoutRequeue(t *testing.T) {
	f := newCoordinatorOperationCLIFixture(t, 2)
	time.Sleep(max(time.Until(f.item.Lease.ExpiresAt)+20*time.Millisecond, 0))
	output, err := captureCoordinatorOperationOutput(t, func() error {
		return cmdProjectCoordinatorOperations([]string{"resolve", f.run.ID, f.op.ID, "--cwd", f.run.Workspace, "--outcome", "not_applied", "--note", "Remote audit confirms no request was applied"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "still requires explicit coordinator recover") {
		t.Fatalf("missing explicit recovery instruction: %s", output)
	}
	run, err := f.store.Get(f.run.Workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Operations[0].Status != projectcoord.OperationNotApplied || len(run.Operations[0].ResultJSON) != 0 || run.Items[0].Status != projectcoord.ItemFailed || run.Items[0].Attempts != 1 {
		t.Fatalf("not-applied resolution silently reran or invented a result: %+v", run)
	}
	if _, err := f.store.Recover(f.run.Workspace, f.run.ID, f.item.ID, "No effect applied, safe to retry within original budget", false); err != nil {
		t.Fatal(err)
	}
	item, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, projectcoord.ClaimOptions{Worker: "next-writer", Lease: time.Second})
	if err != nil || item.Attempts != 2 {
		t.Fatalf("explicit bounded retry failed: %+v %v", item, err)
	}
}
