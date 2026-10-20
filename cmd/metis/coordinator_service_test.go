package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

const coordinatorServiceTestToken = "coordinator-service-test-secret-0123456789"

func TestCoordinatorServiceRejectsUnsupportedLeaseBeforeStarting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(coordinatorServiceTestToken), 0600); err != nil {
		t.Fatal(err)
	}
	for _, lease := range []string{"100ms", "25h"} {
		err := cmdProjectCoordinatorServe(context.Background(), []string{"missing-run", "--cwd", t.TempDir(), "--agent", "explore", "--token-file", path, "--lease", lease})
		if err == nil || !strings.Contains(err.Error(), "lease must be between 1s and 24h") {
			t.Fatalf("lease %s escaped startup validation: %v", lease, err)
		}
	}
}

func TestCoordinatorServicePrivateTokenAndLoopbackValidation(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "127.0.0.2:8080", "[::1]:65535"} {
		if err := validateCoordinatorServiceListen(address); err != nil {
			t.Fatalf("valid loopback %q: %v", address, err)
		}
	}
	for _, address := range []string{"localhost:8080", ":8080", "0.0.0.0:8080", "[::]:8080", "10.0.0.1:8080", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:http"} {
		if err := validateCoordinatorServiceListen(address); err == nil {
			t.Fatalf("accepted listen %q", address)
		}
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(coordinatorServiceTestToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCoordinatorServiceToken(path); err != nil || got != coordinatorServiceTestToken {
		t.Fatalf("private token: %q %v", got, err)
	}
	if goruntime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readCoordinatorServiceToken(path); err == nil {
			t.Fatal("accepted public token file")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(filepath.Dir(path), "link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readCoordinatorServiceToken(link); err == nil {
			t.Fatal("accepted token symlink")
		}
	}
	for _, value := range []string{"short", strings.Repeat("a", 31), strings.Repeat("é", 16), strings.Repeat("a", 40) + " embedded", strings.Repeat("a", 8193)} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCoordinatorServiceToken(path); err == nil {
			t.Fatalf("accepted invalid token length %d", len(value))
		}
	}
}

func TestCoordinatorServiceWorkerEnvSeparatesIngressCredential(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin:/bin", "METIS_HOME=/old", "METIS_DESKTOP_FRAME_TOKEN=frame-private",
		"INGRESS_AUTH=Bearer " + coordinatorServiceTestToken,
		"INGRESS_TOKEN=" + coordinatorServiceTestToken,
		"OPENAI_API_KEY=separate-provider-credential", "TOKEN_HINT=unrelated",
	}
	env := coordinatorServiceWorkerEnv(parent, "/fixed-home", coordinatorServiceTestToken)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, coordinatorServiceTestToken) || strings.Contains(joined, "frame-private") || strings.Contains(joined, "METIS_HOME=/old") {
		t.Fatalf("worker inherited unrelated service authority: %v", env)
	}
	for _, expected := range []string{"PATH=/usr/bin:/bin", "METIS_HOME=/fixed-home", "OPENAI_API_KEY=separate-provider-credential", "TOKEN_HINT=unrelated"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("worker lost required environment %s", expected)
		}
	}
}

func coordinatorServiceFixture(t *testing.T, ready int) (*projectcoord.Store, coordinatorServiceOptions, string) {
	t.Helper()
	home, workspace := t.TempDir(), t.TempDir()
	store := projectcoord.NewStore(filepath.Join(home, "project-coordinator"))
	items := []projectcoord.WorkItemInput{}
	for i := 0; i < ready; i++ {
		items = append(items, projectcoord.WorkItemInput{ID: fmt.Sprintf("seed-%d", i), Phase: projectcoord.PhaseCustom, Subject: "Seed", Prompt: "fixture-private-prompt"})
	}
	if len(items) == 0 {
		items = append(items, projectcoord.WorkItemInput{ID: "seed", Phase: projectcoord.PhaseCustom, Subject: "Seed", Prompt: "fixture-private-prompt"})
	}
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "fixture-private-goal", Items: items, CheckCommands: []string{"exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	if ready == 0 {
		item, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Complete(context.Background(), run.Workspace, run.ID, item.ID, projectcoord.CompletionInput{Worker: "fixture", Token: item.Lease.Token, Output: "fixture-private-output"}); err != nil {
			t.Fatal(err)
		}
	}
	return store, coordinatorServiceOptions{runID: run.ID, workspace: run.Workspace, agentProfile: "writer", verifierProfile: "reviewer", workers: 2, poll: 50 * time.Millisecond, lease: time.Second}, home
}

func coordinatorServiceRequest(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:9091"+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestCoordinatorEventHTTPAdmissionIsStrictAndPrivate(t *testing.T) {
	store, opts, _ := coordinatorServiceFixture(t, 0)
	handler := newCoordinatorEventHandler(store, opts, coordinatorServiceTestToken)
	valid := `{"source":"ci","id":"123","subject":"repair","prompt":"fixture-private-prompt","deliverables":["repair.json"]}`
	for _, tc := range []struct {
		name, method, path, body, token string
		want                            int
	}{
		{"missing_auth", "POST", "/v1/events", valid, "", 401},
		{"wrong_auth", "POST", "/v1/events", valid, "wrong", 401},
		{"status_requires_auth", "GET", "/v1/status", "", "", 401},
		{"health_requires_auth", "GET", "/v1/health", "", "", 401},
		{"wrong_method", "GET", "/v1/events", "", coordinatorServiceTestToken, 405},
		{"unknown_field", "POST", "/v1/events", strings.TrimSuffix(valid, "}") + `,"agent":"fullAccess"}`, coordinatorServiceTestToken, 400},
		{"unknown_workspace", "POST", "/v1/events", strings.TrimSuffix(valid, "}") + `,"cwd":"/"}`, coordinatorServiceTestToken, 400},
		{"trailing_json", "POST", "/v1/events", valid + `{}`, coordinatorServiceTestToken, 400},
		{"malformed", "POST", "/v1/events", `{`, coordinatorServiceTestToken, 400},
		{"empty", "POST", "/v1/events", `{}`, coordinatorServiceTestToken, 400},
		{"wrong_type", "POST", "/v1/events", `{"source":42}`, coordinatorServiceTestToken, 400},
		{"outside_delivery", "POST", "/v1/events", strings.Replace(valid, "repair.json", "../escape", 1), coordinatorServiceTestToken, 400},
		{"oversized", "POST", "/v1/events", `{"source":"ci","id":"123","subject":"repair","prompt":"` + strings.Repeat("x", coordinatorEventBodyLimit) + `"}`, coordinatorServiceTestToken, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := coordinatorServiceRequest(handler, tc.method, tc.path, tc.body, tc.token)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "fixture-private") || strings.Contains(w.Body.String(), coordinatorServiceTestToken) {
				t.Fatal("private values echoed in error")
			}
		})
	}
	run, err := store.Get(opts.workspace, opts.runID)
	if err != nil || len(run.Items) != 1 {
		t.Fatalf("invalid admission changed graph: items=%d err=%v", len(run.Items), err)
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9091/v1/events", strings.NewReader(valid))
		r.Header.Set("Authorization", "Bearer "+coordinatorServiceTestToken)
		if header == "Origin" {
			r.Header.Set(header, "https://unrelated.invalid")
		} else {
			r.Header.Set(header, "cross-site")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("cross-origin %s status=%d", header, w.Code)
		}
	}
}

func TestCoordinatorEventAckPersistsDeduplicatesAndLocksProfiles(t *testing.T) {
	store, opts, _ := coordinatorServiceFixture(t, 0)
	handler := newCoordinatorEventHandler(store, opts, coordinatorServiceTestToken)
	var wakeCount int
	handler.wake = func() {
		wakeCount++
		// A wake callback is only called after the acknowledged item exists.
		run, err := store.Get(opts.workspace, opts.runID)
		if err != nil || len(run.Items) != 3 {
			t.Errorf("wake preceded durable admission: items=%d err=%v", len(run.Items), err)
		}
	}
	body := `{"source":" ci ","id":" 123 ","subject":" repair ","prompt":" fixture-private-prompt "}`
	var first string
	for i := 0; i < 2; i++ {
		w := coordinatorServiceRequest(handler, "POST", "/v1/events", body, coordinatorServiceTestToken)
		if w.Code != 202 {
			t.Fatalf("admission=%d %s", w.Code, w.Body.String())
		}
		if i == 0 {
			first = w.Body.String()
		} else if first != w.Body.String() {
			t.Fatal("identical event acknowledgement changed")
		}
	}
	run, err := store.Get(opts.workspace, opts.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Items) != 3 || run.Items[1].AgentProfile != "writer" || run.Items[2].AgentProfile != "reviewer" || wakeCount != 2 {
		t.Fatalf("routing/dedup changed: %+v wake=%d", run.Items, wakeCount)
	}
	for _, changed := range []string{strings.Replace(body, "fixture-private-prompt", "changed", 1), strings.Replace(body, "}", `,"deliverables":["other.json"]}`, 1)} {
		if w := coordinatorServiceRequest(handler, "POST", "/v1/events", changed, coordinatorServiceTestToken); w.Code != 409 {
			t.Fatalf("conflict=%d %s", w.Code, w.Body.String())
		}
	}
	restarted := newCoordinatorEventHandler(store, opts, coordinatorServiceTestToken)
	restarted.opts.verifierProfile = "different-verifier"
	if w := coordinatorServiceRequest(restarted, "POST", "/v1/events", body, coordinatorServiceTestToken); w.Code != 409 {
		t.Fatalf("changed verifier accepted: %d", w.Code)
	}
	handler.active = func() int { return 1 }
	w := coordinatorServiceRequest(handler, "GET", "/v1/status", "", coordinatorServiceTestToken)
	var status map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil {
		t.Fatalf("status response=%d %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"run_id", "status", "workers", "active_children", "counts", "spent_usd", "max_budget_usd"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("missing status field %s", key)
		}
		delete(status, key)
	}
	if len(status) != 0 || strings.Contains(w.Body.String(), "fixture-private") || strings.Contains(w.Body.String(), coordinatorServiceTestToken) {
		t.Fatal("status leaked graph/private data")
	}
	unconfigured, err := store.Create(projectcoord.CreateInput{Workspace: opts.workspace, Goal: "missing checks", Items: []projectcoord.WorkItemInput{{ID: "unchecked", Phase: projectcoord.PhaseCustom, Subject: "Unchecked", Prompt: "fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	unconfiguredOptions := opts
	unconfiguredOptions.runID = unconfigured.ID
	unconfiguredHandler := newCoordinatorEventHandler(store, unconfiguredOptions, coordinatorServiceTestToken)
	if w := coordinatorServiceRequest(unconfiguredHandler, "POST", "/v1/events", body, coordinatorServiceTestToken); w.Code != 503 {
		t.Fatalf("admitted event without checks: %d", w.Code)
	}
	handler.closing.Store(true)
	if w := coordinatorServiceRequest(handler, "GET", "/v1/status", "", coordinatorServiceTestToken); w.Code != 503 {
		t.Fatalf("shutdown admission=%d", w.Code)
	}
}

func TestCoordinatorEventConcurrentRedeliveryCreatesOnePair(t *testing.T) {
	store, opts, _ := coordinatorServiceFixture(t, 0)
	handler := newCoordinatorEventHandler(store, opts, coordinatorServiceTestToken)
	body := `{"source":"ci","id":"same","subject":"repair","prompt":"fixture"}`
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if w := coordinatorServiceRequest(handler, "POST", "/v1/events", body, coordinatorServiceTestToken); w.Code != 202 {
				t.Errorf("redelivery=%d", w.Code)
			}
		}()
	}
	group.Wait()
	run, err := store.Get(opts.workspace, opts.runID)
	if err != nil || len(run.Items) != 3 {
		t.Fatalf("duplicates: items=%d err=%v", len(run.Items), err)
	}
}

func newCoordinatorHelperService(t *testing.T, store *projectcoord.Store, opts coordinatorServiceOptions, home, mode string) (*coordinatorService, *coordinatorServiceBuffer, string) {
	t.Helper()
	output := &coordinatorServiceBuffer{limit: 1 << 20}
	manifests := t.TempDir()
	s := newCoordinatorService(store, opts, coordinatorServiceTestToken, os.Args[0], home, output)
	s.command = func(name string, args ...string) *exec.Cmd {
		return exec.Command(name, append([]string{"-test.run=^TestCoordinatorServiceHelperProcess$", "--"}, args...)...)
	}
	// Race helpers otherwise wait one second after os.Exit. Suppress only that
	// instrumentation delay; retain race detection and any caller GORACE options.
	s.env = append(os.Environ(), "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"), "METIS_SERVICE_HELPER_MODE="+mode, "METIS_SERVICE_HELPER_MANIFESTS="+manifests, "METIS_DESKTOP_FRAME_TOKEN=private-native-token", "INGRESS_TOKEN="+coordinatorServiceTestToken)
	return s, output, manifests
}

func waitCoordinatorServiceCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for service condition")
}

func TestCoordinatorServicePoolAndSecondServiceRespectDurableClaims(t *testing.T) {
	store, opts, home := coordinatorServiceFixture(t, 4)
	s1, logs1, manifests1 := newCoordinatorHelperService(t, store, opts, home, "complete")
	s2, logs2, manifests2 := newCoordinatorHelperService(t, projectcoord.NewStore(filepath.Join(home, "project-coordinator")), opts, home, "complete")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done1, done2 := make(chan struct{}), make(chan struct{})
	go func() { s1.runPool(ctx); close(done1) }()
	go func() { s2.runPool(ctx); close(done2) }()
	waitCoordinatorServiceCondition(t, func() bool {
		run, err := store.Get(opts.workspace, opts.runID)
		return err == nil && run.Status == projectcoord.RunCompleted
	})
	cancel()
	<-done1
	<-done2
	run, err := store.Get(opts.workspace, opts.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range run.Items {
		if item.Attempts != 1 || item.Status != projectcoord.ItemCompleted {
			t.Fatalf("duplicate execution: %+v", item)
		}
	}
	if s1.active.Load() != 0 || s2.active.Load() != 0 {
		t.Fatal("service retained owned children")
	}
	for i, log := range []*coordinatorServiceBuffer{logs1, logs2} {
		active, maximum := 0, 0
		decoder := json.NewDecoder(bytes.NewReader(log.data()))
		for {
			var event coordinatorServiceEvent
			if err := decoder.Decode(&event); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if event.Kind == "worker_started" {
				active++
				if active > maximum {
					maximum = active
				}
			} else if event.Kind == "worker_finished" || event.Kind == "worker_idle" {
				active--
			}
		}
		if maximum > opts.workers || active != 0 {
			t.Fatalf("service %d escaped pool: max=%d final=%d", i, maximum, active)
		}
		if strings.Contains(string(log.data()), "fixture-private") || strings.Contains(string(log.data()), coordinatorServiceTestToken) || strings.Contains(string(log.data()), "private-native-token") {
			t.Fatal("private child output reached service log")
		}
	}
	manifestCount := 0
	for _, directory := range []string{manifests1, manifests2} {
		files, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			data, err := os.ReadFile(filepath.Join(directory, file.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var manifest coordinatorServiceHelperManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.CWD != opts.workspace || manifest.Home != home || manifest.FrameToken != "" || manifest.IngressToken != "" || !strings.Contains(strings.Join(manifest.Args, " "), "--max-items 1 --summary-json") {
				t.Fatalf("child launch contract: %+v", manifest)
			}
			manifestCount++
		}
	}
	if manifestCount < 4 {
		t.Fatalf("missing child manifests: %d", manifestCount)
	}
}

func TestCoordinatorServiceHTTPArrivalAutomaticallyDispatchesHelper(t *testing.T) {
	store, opts, home := coordinatorServiceFixture(t, 0)
	s, output, _ := newCoordinatorHelperService(t, store, opts, home, "complete")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx, listener) }()
	req, err := http.NewRequest("POST", "http://"+listener.Addr().String()+"/v1/events", strings.NewReader(`{"source":"fixture","id":"arrival","subject":"arrival","prompt":"fixture-private-prompt","deliverables":["result.json"]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+coordinatorServiceTestToken)
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("HTTP arrival status=%d", response.StatusCode)
	}
	waitCoordinatorServiceCondition(t, func() bool {
		run, err := store.Get(opts.workspace, opts.runID)
		return err == nil && len(run.Items) == 3 && run.Status == projectcoord.RunCompleted
	})
	if _, err := os.Stat(filepath.Join(opts.workspace, "result.json")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.data(), []byte(`"kind":"service_ready"`)) || !bytes.Contains(output.data(), []byte(`"kind":"service_stopped"`)) {
		t.Fatal("missing structured listener lifecycle")
	}
}

func TestCoordinatorServiceIdleRaceAndFailureBackoffAreBounded(t *testing.T) {
	for _, mode := range []string{"idle", "fail"} {
		t.Run(mode, func(t *testing.T) {
			store, opts, home := coordinatorServiceFixture(t, 1)
			opts.workers = 1
			s, logs, _ := newCoordinatorHelperService(t, store, opts, home, mode)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { s.runPool(ctx); close(done) }()
			time.Sleep(350 * time.Millisecond)
			cancel()
			<-done
			started := bytes.Count(logs.data(), []byte(`"kind":"worker_started"`))
			limit := 7
			if mode == "fail" {
				limit = 3
			}
			if started == 0 || started > limit {
				t.Fatalf("unbounded/absent starts: %d, limit=%d", started, limit)
			}
			if mode == "idle" && !bytes.Contains(logs.data(), []byte(`"kind":"worker_idle"`)) {
				t.Fatal("no-ready race was not classified as idle")
			}
			if bytes.Contains(logs.data(), []byte("fixture-private")) {
				t.Fatal("raw failing child output escaped")
			}
		})
	}
}

func TestCoordinatorServiceShutdownJoinsGracefulAndForcedChildren(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("POSIX graceful signal contract")
	}
	for _, mode := range []string{"wait", "ignore-term"} {
		t.Run(mode, func(t *testing.T) {
			store, opts, home := coordinatorServiceFixture(t, 1)
			opts.workers = 1
			s, _, manifests := newCoordinatorHelperService(t, store, opts, home, mode)
			if s.shutdownGrace != 5*time.Second {
				t.Fatal("production shutdown grace changed")
			}
			s.shutdownGrace = 100 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { s.runPool(ctx); close(done) }()
			var pid int
			waitCoordinatorServiceCondition(t, func() bool {
				files, _ := os.ReadDir(manifests)
				if len(files) != 1 {
					return false
				}
				data, _ := os.ReadFile(filepath.Join(manifests, files[0].Name()))
				var manifest coordinatorServiceHelperManifest
				if json.Unmarshal(data, &manifest) != nil {
					return false
				}
				pid = manifest.PID
				return pid > 0
			})
			start := time.Now()
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown did not join child")
			}
			if processutil.Alive(pid) || s.active.Load() != 0 {
				t.Fatalf("owned child survived: pid=%d", pid)
			}
			if mode == "ignore-term" && time.Since(start) < s.shutdownGrace {
				t.Fatal("forced shutdown skipped graceful interval")
			}
		})
	}
}

type coordinatorServiceHelperManifest struct {
	Args                                []string
	CWD, Home, FrameToken, IngressToken string
	PID                                 int
}

// This helper has no provider or model: it proves only HTTP dispatch, child
// ownership, durable claim and shutdown contracts. Real-query acceptance is a
// separate CLI test and must not be inferred from this fixture.
func TestCoordinatorServiceHelperProcess(t *testing.T) {
	mode := os.Getenv("METIS_SERVICE_HELPER_MODE")
	if mode == "" {
		return
	}
	args := []string{}
	for i, value := range os.Args {
		if value == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 3 || args[0] != "coordinator" || args[1] != "run" {
		os.Exit(2)
	}
	var term chan os.Signal
	if mode == "wait" {
		term = make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
	} else if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	cwd, _ := os.Getwd()
	manifest, _ := json.Marshal(coordinatorServiceHelperManifest{Args: args, CWD: cwd, Home: os.Getenv("METIS_HOME"), FrameToken: os.Getenv("METIS_DESKTOP_FRAME_TOKEN"), IngressToken: os.Getenv("INGRESS_TOKEN"), PID: os.Getpid()})
	if err := os.WriteFile(filepath.Join(os.Getenv("METIS_SERVICE_HELPER_MANIFESTS"), strconv.Itoa(os.Getpid())+".json"), manifest, 0600); err != nil {
		os.Exit(2)
	}
	if mode == "wait" {
		<-term
		fmt.Printf("{\"outcomes_count\":0,\"run_id\":%q,\"status\":\"active\",\"private\":\"fixture-private-child-output\"}\n", args[2])
		os.Exit(130)
	}
	if mode == "ignore-term" {
		for {
			time.Sleep(time.Minute)
		}
	}
	if mode == "idle" {
		fmt.Printf("{\"outcomes_count\":0,\"run_id\":%q,\"status\":\"active\",\"private\":\"fixture-private-child-output\"}\n", args[2])
		os.Exit(0)
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "fixture-private-child-error")
		os.Exit(1)
	}
	value := func(flag string) string {
		for i, arg := range args {
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	store := projectcoord.NewStore(filepath.Join(os.Getenv("METIS_HOME"), "project-coordinator"))
	item, err := store.ClaimNext(value("--cwd"), args[2], projectcoord.ClaimOptions{Worker: value("--worker"), Lease: time.Second})
	if errors.Is(err, projectcoord.ErrNoReadyWork) {
		fmt.Printf("{\"outcomes_count\":0,\"run_id\":%q,\"status\":\"active\"}\n", args[2])
		os.Exit(0)
	}
	if err != nil {
		os.Exit(2)
	}
	time.Sleep(100 * time.Millisecond)
	for _, path := range item.Deliverables {
		if err := os.WriteFile(filepath.Join(cwd, path), []byte(`{"fixture":true}`), 0600); err != nil {
			os.Exit(2)
		}
	}
	if _, err := store.Complete(context.Background(), cwd, args[2], item.ID, projectcoord.CompletionInput{Worker: value("--worker"), Token: item.Lease.Token, Output: "fixture-private-child-output"}); err != nil {
		os.Exit(2)
	}
	fmt.Printf("{\"outcomes_count\":1,\"run_id\":%q,\"status\":\"active\",\"private\":\"fixture-private-lease-token\"}\n", args[2])
	os.Exit(0)
}
