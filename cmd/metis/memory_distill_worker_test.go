package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func memoryDistillLauncherFixture(t *testing.T) (memoryDistillLaunchDeps, *atomic.Int32, chan struct{}) {
	t.Helper()
	starts := new(atomic.Int32)
	done := make(chan struct{})
	deps := memoryDistillLaunchDeps{
		getenv:       func(string) string { return "" },
		executable:   func() (string, error) { return "/opt/metis", nil },
		hasPending:   func(string) (bool, error) { return true, nil },
		workerActive: func(string) (bool, error) { return false, nil },
		start: func(executable, root string) (<-chan struct{}, error) {
			if executable != "/opt/metis" || !filepath.IsAbs(root) {
				t.Errorf("launch executable=%q root=%q", executable, root)
			}
			starts.Add(1)
			return done, nil
		},
	}
	return deps, starts, done
}

func TestMemoryDistillationLauncherSkipsEmptyActiveAndRecursiveWork(t *testing.T) {
	for _, reason := range []string{"empty root", "no pending", "active worker", "worker child", "test executable"} {
		t.Run(reason, func(t *testing.T) {
			deps, starts, done := memoryDistillLauncherFixture(t)
			defer close(done)
			root := t.TempDir()
			switch reason {
			case "empty root":
				root = ""
			case "no pending":
				deps.hasPending = func(string) (bool, error) { return false, nil }
			case "active worker":
				deps.workerActive = func(string) (bool, error) { return true, nil }
			case "worker child":
				deps.getenv = func(string) string { return "1" }
				deps.hasPending = func(string) (bool, error) { t.Fatal("child inspected queue"); return false, nil }
			case "test executable":
				deps.executable = func() (string, error) { return "/tmp/metis.test", nil }
			}
			if err := startMemoryDistillationWorkerWith(root, deps); err != nil {
				t.Fatal(err)
			}
			if starts.Load() != 0 {
				t.Fatalf("started %d workers", starts.Load())
			}
		})
	}
}

func TestMemoryDistillationLauncherDeduplicatesUntilChildExits(t *testing.T) {
	deps, starts, done := memoryDistillLauncherFixture(t)
	root := t.TempDir()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := startMemoryDistillationWorkerWith(root, deps); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("launch race spawned %d children", starts.Load())
	}
	close(done)
	if err := startMemoryDistillationWorkerWith(root, deps); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 {
		t.Fatalf("completed child prevented restart: starts=%d", starts.Load())
	}
}

func TestMemoryDistillationLauncherFailureAllowsRetry(t *testing.T) {
	deps, starts, done := memoryDistillLauncherFixture(t)
	defer close(done)
	root := t.TempDir()
	want := errors.New("spawn refused")
	deps.start = func(string, string) (<-chan struct{}, error) { return nil, want }
	if err := startMemoryDistillationWorkerWith(root, deps); !errors.Is(err, want) {
		t.Fatalf("launch error = %v", err)
	}
	deps.start = func(string, string) (<-chan struct{}, error) { starts.Add(1); return done, nil }
	if err := startMemoryDistillationWorkerWith(root, deps); err != nil || starts.Load() != 1 {
		t.Fatalf("retry error=%v starts=%d", err, starts.Load())
	}
}

func TestMemoryDistillationResumeReleasesBeforeStartingWorker(t *testing.T) {
	root := t.TempDir()
	var calls []string
	deps := memoryDistillResumeDeps{
		getenv: func(string) string { return "" },
		release: func(gotRoot, session string) (int, error) {
			if gotRoot != root || session != "" {
				t.Fatalf("recovery released root=%q session=%q", gotRoot, session)
			}
			calls = append(calls, "release")
			return 3, nil
		},
		start: func(gotRoot string) error {
			if gotRoot != root {
				t.Fatalf("recovery started root=%q", gotRoot)
			}
			calls = append(calls, "start")
			return nil
		},
	}
	if err := resumeMemoryDistillationWorkerWith(root, deps); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "release,start" {
		t.Fatalf("recovery calls = %v", calls)
	}
	want := errors.New("recovery storage unavailable")
	deps.release = func(string, string) (int, error) { return 0, want }
	deps.start = func(string) error { t.Fatal("started worker after release failed"); return nil }
	if err := resumeMemoryDistillationWorkerWith(root, deps); !errors.Is(err, want) {
		t.Fatalf("release error = %v", err)
	}
}

func TestMemoryDistillationResumeChildCannotReleaseStagedJobs(t *testing.T) {
	deps := memoryDistillResumeDeps{
		getenv: func(string) string { return "1" },
		release: func(string, string) (int, error) {
			t.Fatal("worker child released staged jobs")
			return 0, nil
		},
		start: func(string) error { t.Fatal("worker child recursively started recovery"); return nil },
	}
	if err := resumeMemoryDistillationWorkerWith(t.TempDir(), deps); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryDistillationWorkerEnvironmentReplacesControlFlags(t *testing.T) {
	env := memoryDistillationWorkerEnvironment([]string{"PRESERVED=value", memoryDistillWorkerEnv + "=0", "METIS_NO_UPDATE_CHECK=0", "METIS_HOME=relative", "METIS_HOME=/retargetable-link"}, "/canonical/config-home")
	joined := strings.Join(env, "\n")
	for _, entry := range []string{"PRESERVED=value", memoryDistillWorkerEnv + "=1", "METIS_NO_UPDATE_CHECK=1", "METIS_HOME=/canonical/config-home"} {
		if strings.Count(joined, entry) != 1 {
			t.Fatalf("environment did not preserve exactly one %q: %v", entry, env)
		}
	}
	if strings.Contains(joined, "=0") {
		t.Fatalf("stale control flag: %v", env)
	}
	if strings.Count(joined, "METIS_HOME=") != 1 {
		t.Fatal("worker inherited a second configuration home")
	}
}

func TestMemoryDistillationWorkerRejectsUnscopedRelativeInputs(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	configHome, err := config.VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--root", "relative"}, {"--root", t.TempDir(), "extra"}, {"--unknown"}} {
		if err := cmdMemoryDistillWorker(context.Background(), args); err == nil {
			t.Fatalf("accepted arguments %v", args)
		}
	}
	for _, job := range []agent.DistillationQueueJob{
		{ConfigHome: configHome, WorkspacePath: "relative", ProviderName: "openai"},
		{ConfigHome: configHome, WorkspacePath: t.TempDir(), ProviderName: "openai", Scope: "wrong-workspace"},
		{ConfigHome: configHome, WorkspacePath: t.TempDir()},
	} {
		if _, _, err := resolveMemoryDistillationJob(context.Background(), t.TempDir(), job); err == nil {
			t.Fatalf("accepted invalid workspace identity: %+v", job)
		}
	}
}

func TestMemoryDistillationResolverUsesFrozenWorkspaceProviderAndScope(t *testing.T) {
	home, workspace, elsewhere, root := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_HOME", home)
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	t.Setenv("METIS_DISTILL_TEST_KEY", "fixture-not-a-real-credential")
	t.Chdir(elsewhere)
	configHome, err := config.VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `[provider]
default = "frozen-route"
[provider.custom.frozen-route]
transport = "openai_chat"
base_url = "http://127.0.0.1:1/v1"
model = "configured-model"
api_key_env = "METIS_DISTILL_TEST_KEY"
`
	if err := os.WriteFile(filepath.Join(workspace, ".metis", "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := addTrustedDir(workspace); err != nil {
		t.Fatal(err)
	}
	scope, err := memory.WorkspaceScope(workspace)
	if err != nil {
		t.Fatal(err)
	}
	job := agent.DistillationQueueJob{ConfigHome: configHome, WorkspacePath: workspace, Scope: scope, ProviderName: "frozen-route", ModelID: "frozen-model"}
	repository, provider, err := resolveMemoryDistillationJob(context.Background(), root, job)
	if err != nil {
		t.Fatal(err)
	}
	if repository.Root() != root || provider.ModelID() != "frozen-model" {
		t.Fatalf("restored repository=%q model=%q", repository.Root(), provider.ModelID())
	}
	actualCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	expectedCWD, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if actualCWD != expectedCWD {
		t.Fatalf("worker used cwd=%q, expected %q", actualCWD, expectedCWD)
	}
	// A second job must resolve from its own directory, without inheriting the
	// first job's project provider table or namespace.
	job.WorkspacePath = elsewhere
	job.Scope, err = memory.WorkspaceScope(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveMemoryDistillationJob(context.Background(), root, job); err == nil {
		t.Fatal("second job inherited the first workspace's provider configuration")
	}
}

func TestMemoryDistillationResolverDoesNotTrustUnknownProjectProvider(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	workspace := t.TempDir()
	t.Chdir(workspace)
	configHome, err := config.VerifiedHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".metis", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".metis", "config.toml"), []byte(`[provider.custom.untrusted-route]
transport = "openai_chat"
base_url = "http://127.0.0.1:1/v1"
model = "untrusted"
api_key_env = "PRIVATE_USER_KEY"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	job := agent.DistillationQueueJob{ConfigHome: configHome, WorkspacePath: workspace, ProviderName: "untrusted-route", ModelID: "frozen-model"}
	if _, _, err := resolveMemoryDistillationJob(context.Background(), t.TempDir(), job); err == nil {
		t.Fatal("untrusted project supplied a provider to the detached worker")
	}
}

func TestMemoryDistillationResolverRejectsMissingOrDifferentConfigurationHome(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	configHome, err := freezeMemoryDistillationConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	for _, home := range []string{"", "relative", t.TempDir()} {
		job := agent.DistillationQueueJob{ConfigHome: home, WorkspacePath: workspace, ProviderName: "openai"}
		if _, _, err := resolveMemoryDistillationJobAtHome(context.Background(), t.TempDir(), configHome, job); err == nil {
			t.Fatalf("accepted configuration namespace %q", home)
		} else {
			assertMemoryDistillationFailureCode(t, err, "config_namespace")
		}
	}
}

func assertMemoryDistillationFailureCode(t *testing.T, err error, want string) {
	t.Helper()
	var classified interface{ DistillationFailureCode() string }
	if !errors.As(err, &classified) || classified.DistillationFailureCode() != want {
		t.Fatalf("error type=%T stage=%v, want %q", err, classified, want)
	}
	if err.Error() != "memory distillation worker setup failed" {
		t.Fatalf("error exposed a non-static diagnostic: %q", err.Error())
	}
}

func TestMemoryDistillationResolverPreservesWorkspaceCauseWithoutLeakingPath(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	configHome, err := freezeMemoryDistillationConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	job := agent.DistillationQueueJob{
		ConfigHome: configHome, WorkspacePath: filepath.Join(t.TempDir(), "private-client-workspace"), ProviderName: "unresolved-profile",
	}
	_, _, err = resolveMemoryDistillationJobAtHome(context.Background(), t.TempDir(), configHome, job)
	assertMemoryDistillationFailureCode(t, err, "workspace")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("workspace cause no longer supports errors.Is")
	}
	var pathError *os.PathError
	if !errors.As(err, &pathError) || pathError.Path != job.WorkspacePath {
		t.Fatal("workspace cause no longer supports errors.As")
	}
}

func TestMemoryDistillationResolverSetupFailuresNeverContactProvider(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	t.Setenv("METIS_HOME", t.TempDir())
	t.Setenv("METIS_CATALOG_DISABLE", "1")
	t.Setenv("METIS_DISTILL_TEST_KEY", "fixture-not-a-real-credential")
	t.Chdir(t.TempDir())
	configHome, err := freezeMemoryDistillationConfigHome()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`[provider]
default = "available-profile"
[provider.custom.available-profile]
transport = "openai_chat"
base_url = %q
model = "fixture-model"
api_key_env = "METIS_DISTILL_TEST_KEY"
`, server.URL)
	if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	blockedRoot := filepath.Join(t.TempDir(), "private-repository-path")
	if err := os.WriteFile(blockedRoot, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ profile, root, stage string }{
		{"private-missing-profile", t.TempDir(), "provider_configuration"},
		{"available-profile", blockedRoot, "repository"},
	} {
		job := agent.DistillationQueueJob{ConfigHome: configHome, WorkspacePath: workspace, ProviderName: tc.profile, ModelID: "fixture-model"}
		_, _, err := resolveMemoryDistillationJobAtHome(context.Background(), tc.root, configHome, job)
		assertMemoryDistillationFailureCode(t, err, tc.stage)
	}
	if requests.Load() != 0 {
		t.Fatalf("setup failures sent %d provider HTTP requests", requests.Load())
	}
}
