package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func durableOpsTestServer(t *testing.T) (*Server, *durableOperationsBridge) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := newDurableOperationsBridge(AutomationOptions{Root: filepath.Join(t.TempDir(), "state"), WorkDir: workspacePathKey(t.TempDir()), Executable: binary})
	b.command = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestDurableOperationsCLIHelper$", "--")
		cmd.Env = append(os.Environ(), "METIS_DURABLE_OPS_HELPER=1")
		return cmd
	}
	s := NewServer("127.0.0.1:0", nil, nil)
	s.durableOps = b
	s.selectWorkspacePath(b.options.WorkDir)
	return s, b
}
func durableOpsRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if method == http.MethodPost && path == "/api/operations/action" {
		var object map[string]any
		if json.Unmarshal([]byte(body), &object) == nil && object != nil {
			if _, exists := object["expected_workspace"]; !exists {
				object["expected_workspace"] = s.selectedWorkspacePath()
			}
			if encoded, err := json.Marshal(object); err == nil {
				body = string(encoded)
			}
		}
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}
func TestDurableOperationsCLIHelper(t *testing.T) {
	if os.Getenv("METIS_DURABLE_OPS_HELPER") != "1" {
		return
	}
	if os.Getenv("METIS_DURABLE_OPS_ERROR") == "1" {
		fmt.Fprint(os.Stderr, "credential-private-secret")
		os.Exit(1)
	}
	if os.Getenv("METIS_DESKTOP_FRAME_TOKEN") != "" {
		os.Exit(9)
	}
	fmt.Print(`{"schema_version":1,"workspace":"/trusted","jobs":[{"id":"job-1","name":"岗位","state":"active","credential_refs":["private"],"definition":{"profile":{"system_prompt":"secret"}},"input_tokens":23}],"runs":[{"id":"run-1","operations":[{"id":"op-1","status":"uncertain","claim_token":"private","result_json":{"output":"secret"}}]}],"handoffs":[],"learning":[],"ok":true}`)
	os.Exit(0)
}
func TestDurableOperationsSnapshotUsesTrustedBindingAndRedactsRawRecords(t *testing.T) {
	t.Setenv("METIS_DESKTOP_FRAME_TOKEN", "private-shell-token")
	s, b := durableOpsTestServer(t)
	original := b.command
	b.command = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		if path != b.options.Executable || strings.Join(args, "|") != "operations|snapshot|--cwd|"+b.options.WorkDir+"|--json" {
			t.Fatalf("untrusted invocation %q %v", path, args)
		}
		return original(ctx, path, args...)
	}
	w := durableOpsRequest(t, s, "GET", "/api/operations", "")
	if w.Code != 200 {
		t.Fatalf("snapshot %d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"private", "secret", "credential_refs", "claim_token", "result_json", "system_prompt"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("leaked %s: %s", secret, w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), `"input_tokens":23`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("lost safe metadata: %s", w.Body.String())
	}
}
func TestDurableOperationsActionStrictValidationAndPrivateRequestCleanup(t *testing.T) {
	s, b := durableOpsTestServer(t)
	original := b.command
	calls := 0
	privatePath := ""
	b.command = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		calls++
		if len(args) != 6 || strings.Join(args[:5], "|") != "operations|action|--cwd|"+b.options.WorkDir+"|--request-file" {
			t.Fatalf("action argv %v", args)
		}
		privatePath = args[5]
		info, err := os.Stat(privatePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("private mode %v", info.Mode())
		}
		data, err := os.ReadFile(privatePath)
		if strings.Contains(string(data), "expected_workspace") {
			t.Fatal("HTTP scope leaked into CLI action contract")
		}
		if err != nil {
			t.Fatal(err)
		}
		var action durableOperationAction
		if err = json.Unmarshal(data, &action); err != nil {
			t.Fatal(err)
		}
		if action.Action != "pause_job" || action.ID != "job-1" {
			t.Fatalf("action %#v", action)
		}
		return original(ctx, path, args...)
	}
	for _, body := range []string{`{"action":"shell","id":"job-1"}`, `{"action":"pause_job","id":"job-1","cwd":"/other"}`, `{"action":"pause_job","id":"../other"}`, `{"action":"pause_job","id":"job-1"} {}`, `{"action":"rollback_learning","kind":"skill","name":"example"}`} {
		w := durableOpsRequest(t, s, "POST", "/api/operations/action", body)
		if w.Code != 400 {
			t.Fatalf("accepted invalid %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("invalid requests executed %d commands", calls)
	}
	w := durableOpsRequest(t, s, "POST", "/api/operations/action", `{"action":"pause_job","id":"job-1"}`)
	if w.Code != 200 || calls != 1 {
		t.Fatalf("action %d %s calls %d", w.Code, w.Body.String(), calls)
	}
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Fatalf("private request left behind: %v", err)
	}
}
func TestDurableOperationsLearningActionPreservesReviewedVersion(t *testing.T) {
	s, b := durableOpsTestServer(t)
	original := b.command
	b.command = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			t.Fatal(err)
		}
		var action durableOperationAction
		if err = json.Unmarshal(data, &action); err != nil {
			t.Fatal(err)
		}
		if action.Action != "rollback_learning" || action.Kind != "workflow" || action.Name != "fixture" || action.ExpectedVersion != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || action.ExpectedEpoch != 17 || action.Note != "quality declined" {
			t.Fatalf("lost review: %#v", action)
		}
		return original(ctx, path, args...)
	}
	w := durableOpsRequest(t, s, "POST", "/api/operations/action", `{"action":"rollback_learning","kind":"workflow","name":"fixture","expected_version":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_epoch":17,"note":"quality declined"}`)
	if w.Code != 200 {
		t.Fatalf("learning action %d %s", w.Code, w.Body.String())
	}
}
func TestDurableOperationsOriginMethodAndWorkspaceBoundaries(t *testing.T) {
	s, b := durableOpsTestServer(t)
	calls := 0
	b.command = func(context.Context, string, ...string) *exec.Cmd {
		calls++
		t.Fatal("boundary invoked CLI")
		return nil
	}
	for _, test := range []struct {
		method, path string
		status       int
	}{{"POST", "/api/operations", 405}, {"GET", "/api/operations/action", 405}, {"GET", "/api/operations?cwd=/other", 400}} {
		w := durableOpsRequest(t, s, test.method, test.path, "")
		if w.Code != test.status {
			t.Fatalf("boundary %d %s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/operations/action", strings.NewReader(`{"action":"pause_job","id":"job-1"}`))
	r.Header.Set("Origin", "https://foreign.invalid")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("origin status %d", w.Code)
	}
	if calls != 0 {
		t.Fatal("boundary executed command")
	}
}
func TestDurableOperationsFailureRedactionAndUnavailableBackend(t *testing.T) {
	s, b := durableOpsTestServer(t)
	original := b.command
	b.command = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		cmd := original(ctx, path, args...)
		cmd.Env = append(cmd.Env, "METIS_DURABLE_OPS_ERROR=1")
		return cmd
	}
	w := durableOpsRequest(t, s, "GET", "/api/operations", "")
	if w.Code != 502 || strings.Contains(w.Body.String(), "credential-private-secret") {
		t.Fatalf("failure %d %s", w.Code, w.Body.String())
	}
	s.durableOps = nil
	w = durableOpsRequest(t, s, "GET", "/api/operations", "")
	if w.Code != 503 {
		t.Fatalf("unavailable %d", w.Code)
	}
}

func TestDurableOperationsFollowsTrustedSelectionAndRejectsOldReview(t *testing.T) {
	s, b := durableOpsTestServer(t)
	oldWorkspace := b.options.WorkDir
	newWorkspace := workspacePathKey(t.TempDir())
	s.selectWorkspacePath(newWorkspace)
	original := b.command
	calls := 0
	b.command = func(ctx context.Context, path string, args ...string) *exec.Cmd {
		calls++
		if args[3] != newWorkspace {
			t.Fatalf("old workspace used: %v", args)
		}
		return original(ctx, path, args...)
	}
	w := durableOpsRequest(t, s, "GET", "/api/operations", "")
	if w.Code != 200 {
		t.Fatalf("snapshot %d %s", w.Code, w.Body.String())
	}
	data, _ := json.Marshal(map[string]any{"action": "pause_job", "id": "job-1", "expected_workspace": oldWorkspace})
	w = durableOpsRequest(t, s, "POST", "/api/operations/action", string(data))
	if w.Code != 409 || calls != 1 {
		t.Fatalf("old review accepted: %d %s calls=%d", w.Code, w.Body.String(), calls)
	}
	w = durableOpsRequest(t, s, "POST", "/api/operations/action", `{"action":"pause_job","id":"job-1"}`)
	if w.Code != 200 || calls != 2 {
		t.Fatalf("current review %d %s", w.Code, w.Body.String())
	}
}

func TestDurableOperationsNoteLimitUsesUTF8Bytes(t *testing.T) {
	a := durableOperationAction{Action: "rollback_learning", Kind: "workflow", Name: "fixture", ExpectedVersion: strings.Repeat("a", 64), ExpectedEpoch: 17}
	for _, note := range []string{strings.Repeat("x", 512), strings.Repeat("中", 170) + "ab"} {
		a.Note = note
		if err := a.validate(); err != nil {
			t.Fatalf("rejected exact 512-byte reason: %v", err)
		}
	}
	for _, note := range []string{strings.Repeat("x", 513), strings.Repeat("中", 171)} {
		a.Note = note
		if err := a.validate(); err == nil {
			t.Fatal("accepted more than 512 bytes")
		}
	}
}
