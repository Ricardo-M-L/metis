package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func contextBudgetTestRoute() ContextBudgetRoute {
	return ContextBudgetRoute{ProviderID: "fixture", Transport: "openai_chat", BaseURL: "https://gateway.example/v1"}
}

func TestContextBudgetExactRouteModelAndAutoReset(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	base := contextBudgetTestRoute()
	if err := SetContextBudget(base, "model-a", 900000); err != nil {
		t.Fatal(err)
	}
	otherProvider, otherTransport, otherEndpoint, otherAccount := base, base, base, base
	otherProvider.ProviderID = "other-profile"
	otherTransport.Transport = "openai_responses"
	otherEndpoint.BaseURL = "https://gateway.example/other/v1"
	otherAccount.AccountScope = "other-account"
	for _, route := range []ContextBudgetRoute{otherProvider, otherTransport, otherEndpoint, otherAccount} {
		if _, found, err := LookupContextBudget(route, "model-a"); err != nil || found {
			t.Fatalf("route borrowed another budget: route=%+v found=%v err=%v", route, found, err)
		}
	}
	if _, found, err := LookupContextBudget(base, "model-b"); err != nil || found {
		t.Fatalf("new model inherited a budget: found=%v err=%v", found, err)
	}
	if err := SetContextBudget(base, "model-a", 0); err != nil {
		t.Fatal(err)
	}
	if window, found, err := LookupContextBudget(base, "model-a"); err != nil || !found || window != 0 {
		t.Fatalf("explicit automatic reset was erased: window=%d found=%v err=%v", window, found, err)
	}
}

func TestContextBudgetCanonicalIdentityAndSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	route := contextBudgetTestRoute()
	route.BaseURL = "https://GATEWAY.example:443/v1/"
	route.AccountScope = "fixture-account-must-never-be-written"
	if err := SetContextBudget(route, "exact-model", 500000); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalContextBudgetRoute(route)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.BaseURL != "https://gateway.example/v1" || canonical.AccountScope == route.AccountScope {
		t.Fatalf("canonical route retained raw scope or changed endpoint: %+v", canonical)
	}
	if again, err := CanonicalContextBudgetRoute(canonical); err != nil || again != canonical {
		t.Fatalf("canonical route is not idempotent: %+v %v", again, err)
	}
	if window, found, err := LookupContextBudget(canonical, "exact-model"); err != nil || !found || window != 500000 {
		t.Fatalf("canonical settings/runtime identity split: %d %v %v", window, found, err)
	}
	data, err := os.ReadFile(filepath.Join(home, contextBudgetFilename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(route.AccountScope)) {
		t.Fatal("store leaked raw OAuth account scope")
	}
	info, err := os.Stat(filepath.Join(home, contextBudgetFilename))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("budget store is not private: %v %v", info, err)
	}
	records, err := ListContextBudgets()
	if err != nil || len(records) != 1 || records[0].Route != canonical || records[0].Model != "exact-model" {
		t.Fatalf("list lost canonical identity: %+v %v", records, err)
	}
}

func TestContextBudgetRejectsInvalidRouteBeforeWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	for _, endpoint := range []string{
		"https://user:secret@gateway.example/v1", "https://gateway.example/v1?api_key=secret", "https://gateway.example/v1#secret",
		"https://gateway.example/../v1", "https://gateway.example/a%2fb", "https://gateway.example//v1", "https://gateway.example/v1?", "file:///tmp/secret",
	} {
		route := contextBudgetTestRoute()
		route.BaseURL = endpoint
		if err := SetContextBudget(route, "model", 200000); err == nil {
			t.Fatalf("unsafe route was accepted: %q", endpoint)
		}
	}
	for _, window := range []int{-1, maxContextBudget + 1} {
		if err := SetContextBudget(contextBudgetTestRoute(), "model", window); err == nil {
			t.Fatalf("invalid budget %d was accepted", window)
		}
	}
	if err := SetContextBudget(contextBudgetTestRoute(), "model\nother", 200000); err == nil {
		t.Fatal("invalid model id was accepted")
	}
	if _, err := os.Stat(filepath.Join(home, contextBudgetFilename)); !os.IsNotExist(err) {
		t.Fatalf("invalid route wrote a store: %v", err)
	}
}

func TestContextBudgetConcurrentWritesPreserveEveryModel(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const writers = 32
	errors := make(chan error, writers)
	var workers sync.WaitGroup
	for i := range writers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errors <- SetContextBudget(contextBudgetTestRoute(), fmt.Sprintf("model-%d", i), 200000+i)
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range writers {
		if window, found, err := LookupContextBudget(contextBudgetTestRoute(), fmt.Sprintf("model-%d", i)); err != nil || !found || window != 200000+i {
			t.Fatalf("concurrent write %d lost: %d %v %v", i, window, found, err)
		}
	}
}

func TestContextBudgetWriterProcess(t *testing.T) {
	index := os.Getenv("METIS_CONTEXT_BUDGET_WRITER_INDEX")
	if index == "" {
		return
	}
	home := Home()
	if err := os.WriteFile(filepath.Join(home, "writer-"+index+".ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitForConfigLockTestFile(t, filepath.Join(home, "writers.release"), 5*time.Second)
	for i := range 5 {
		if err := SetContextBudget(contextBudgetTestRoute(), fmt.Sprintf("process-%s-model-%d", index, i), 300000+i); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextBudgetConcurrentProcessesPreserveEveryModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for i := range 4 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestContextBudgetWriterProcess$")
		cmd.Env = append(os.Environ(), fmt.Sprintf("METIS_CONTEXT_BUDGET_WRITER_INDEX=%d", i))
		output := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands, outputs = append(commands, cmd), append(outputs, output)
		t.Cleanup(func() { _ = cmd.Process.Kill() })
	}
	for i := range 4 {
		waitForConfigLockTestFile(t, filepath.Join(home, fmt.Sprintf("writer-%d.ready", i)), 3*time.Second)
	}
	if err := os.WriteFile(filepath.Join(home, "writers.release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("writer %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	for i := range 4 {
		for j := range 5 {
			if window, found, err := LookupContextBudget(contextBudgetTestRoute(), fmt.Sprintf("process-%d-model-%d", i, j)); err != nil || !found || window != 300000+j {
				t.Fatalf("process record %d/%d lost: %d %v %v", i, j, window, found, err)
			}
		}
	}
}

func TestContextBudgetRejectsCorruptOrSymlinkedStore(t *testing.T) {
	for _, mode := range []string{"corrupt", "symlink", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			filename := filepath.Join(home, contextBudgetFilename)
			if mode == "symlink" {
				target := filepath.Join(t.TempDir(), "external-budget.json")
				if err := os.WriteFile(target, []byte(`{"schema":1,"records":{}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filename); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				data := "broken-json"
				if mode == "trailing" {
					data = `{"schema":1,"records":{}} {"unexpected":true}`
				}
				if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := LookupContextBudget(contextBudgetTestRoute(), "model"); err == nil {
				t.Fatal("lookup ignored invalid store")
			}
			if err := SetContextBudget(contextBudgetTestRoute(), "model", 250000); err == nil {
				t.Fatal("writer replaced an invalid store")
			}
		})
	}
}

func TestContextBudgetTransportAliasesUseSameWireIdentity(t *testing.T) {
	for _, pair := range [][2]string{{"anthropic", "anthropic_messages"}, {"openai", "openai_chat"}, {"gemini", "gemini_native"}, {"vertex", "vertex_anthropic"}, {"bedrock", "bedrock_anthropic"}} {
		left, right := contextBudgetTestRoute(), contextBudgetTestRoute()
		left.Transport, right.Transport = pair[0], strings.ToUpper(pair[1])
		leftKey, _ := ContextBudgetRouteKey(left, "model")
		rightKey, _ := ContextBudgetRouteKey(right, "model")
		if leftKey == "" || leftKey != rightKey {
			t.Fatalf("equivalent wire aliases did not normalize: %v", pair)
		}
	}
}
