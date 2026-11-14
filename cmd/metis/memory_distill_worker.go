package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

const memoryDistillWorkerEnv = "METIS_MEMORY_DISTILL_WORKER"

// Queue diagnostics store only this static stage, never configuration errors
// that can contain private profile names, paths, endpoints, or credentials.
// The underlying error remains available to errors.Is/As in the caller.
type memoryDistillationSetupError struct {
	stage string
	err   error
}

func (*memoryDistillationSetupError) Error() string {
	return "memory distillation worker setup failed"
}

func (e *memoryDistillationSetupError) Unwrap() error { return e.err }
func (e *memoryDistillationSetupError) DistillationFailureCode() string {
	return e.stage
}

func memoryDistillationSetupFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &memoryDistillationSetupError{stage: stage, err: err}
}

// Launching a worker is a local hand-off, not a join of its provider calls.
// The queue's cross-process lock owns execution; this lock only closes the
// check/start race between foreground boundaries in the same process.
var memoryDistillLaunchMu sync.Mutex
var memoryDistillLaunching = make(map[string]<-chan struct{})

type memoryDistillLaunchDeps struct {
	getenv       func(string) string
	executable   func() (string, error)
	hasPending   func(string) (bool, error)
	workerActive func(string) (bool, error)
	start        func(string, string) (<-chan struct{}, error)
}

type memoryDistillResumeDeps struct {
	getenv  func(string) string
	release func(string, string) (int, error)
	start   func(string) error
}

// Startup recovers abandoned successful exchanges whose foreground owner
// exited before cadence or a headless boundary could release them. Staged
// work owned by a live foreground process remains behind its cadence gate.
// A normal launcher and the detached child never release staged work.
func resumeMemoryDistillationWorker(root string) error {
	return resumeMemoryDistillationWorkerWith(root, memoryDistillResumeDeps{
		getenv: os.Getenv,
		release: func(root, _ string) (int, error) {
			return agent.RecoverDurableDistillationQueue(root)
		},
		start: startMemoryDistillationWorker,
	})
}

func resumeMemoryDistillationWorkerWith(root string, deps memoryDistillResumeDeps) error {
	root = strings.TrimSpace(root)
	if root == "" || deps.getenv(memoryDistillWorkerEnv) == "1" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return errors.New("memory distillation queue requires an absolute root")
	}
	if _, err := deps.release(root, ""); err != nil {
		return fmt.Errorf("release memory distillation recovery queue: %w", err)
	}
	return deps.start(root)
}

func startMemoryDistillationWorker(root string) error {
	return startMemoryDistillationWorkerWith(root, memoryDistillLaunchDeps{
		getenv: os.Getenv, executable: os.Executable,
		hasPending:   agent.DurableDistillationQueueHasPending,
		workerActive: agent.DurableDistillationQueueWorkerActive,
		start:        startDetachedMemoryDistillationWorker,
	})
}

func startMemoryDistillationWorkerWith(root string, deps memoryDistillLaunchDeps) error {
	root = strings.TrimSpace(root)
	if root == "" || deps.getenv(memoryDistillWorkerEnv) == "1" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return errors.New("memory distillation queue requires an absolute root")
	}
	memoryDistillLaunchMu.Lock()
	defer memoryDistillLaunchMu.Unlock()
	if done, started := memoryDistillLaunching[root]; started {
		select {
		case <-done:
			delete(memoryDistillLaunching, root)
		default:
			return nil
		}
	}
	pending, err := deps.hasPending(root)
	if err != nil || !pending {
		return err
	}
	active, err := deps.workerActive(root)
	if err != nil || active {
		return err
	}
	executable, err := deps.executable()
	if err != nil {
		return fmt.Errorf("locate memory distillation worker: %w", err)
	}
	// A package test executable has no CLI dispatcher. Tests exercise the
	// injected launcher seam; ordinary runtime fixtures must never spawn a
	// second test process with private conversation text on disk.
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe")
	if strings.HasSuffix(base, ".test") {
		return nil
	}
	done, err := deps.start(executable, root)
	if err != nil {
		return fmt.Errorf("start memory distillation worker (queue retained): %w", err)
	}
	memoryDistillLaunching[root] = done
	return nil
}

func startDetachedMemoryDistillationWorker(executable, root string) (<-chan struct{}, error) {
	cmd, err := memoryDistillationWorkerCommand(executable, root)
	if err != nil {
		return nil, err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Reap while this foreground process remains alive. Go does not terminate
	// an exec.Command child on parent exit, and its new process session keeps
	// terminal interrupts from cancelling the persisted queue hand-off.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()
	return done, nil
}

func memoryDistillationWorkerCommand(executable, root string) (*exec.Cmd, error) {
	// Resolve while still in the foreground's configuration context. Passing a
	// relative METIS_HOME or its lexical symlink to a fresh process would let
	// that process select a different configuration/credential namespace.
	configHome, err := config.VerifiedHome()
	if err != nil {
		return nil, memoryDistillationSetupFailure("config_namespace", err)
	}
	cmd := exec.Command(executable, "__memory-distill-worker", "--root", root)
	cmd.Env = memoryDistillationWorkerEnvironment(os.Environ(), configHome)
	configureMemoryDistillationProcess(cmd)
	return cmd, nil
}

func memoryDistillationWorkerEnvironment(env []string, configHome string) []string {
	out := make([]string, 0, len(env)+3)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, memoryDistillWorkerEnv) || strings.EqualFold(name, "METIS_NO_UPDATE_CHECK") || strings.EqualFold(name, "METIS_HOME") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, memoryDistillWorkerEnv+"=1", "METIS_NO_UPDATE_CHECK=1", "METIS_HOME="+configHome)
}

func cmdMemoryDistillWorker(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("__memory-distill-worker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "private memory repository root")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid memory distillation worker arguments")
	}
	if flags.NArg() != 0 || strings.TrimSpace(*root) == "" || !filepath.IsAbs(*root) {
		return errors.New("memory distillation worker requires an absolute --root")
	}
	configHome, err := freezeMemoryDistillationConfigHome()
	if err != nil {
		return err
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		return err
	}
	defer func() { _ = os.Chdir(oldCWD) }()
	// Bound one detached worker's lifetime. Failed jobs remain durable and
	// retryable; a later foreground startup resumes the queue without spawning
	// another generation of workers from this child.
	workerCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return agent.RunDurableDistillationQueue(workerCtx, filepath.Clean(*root), func(ctx context.Context, job agent.DistillationQueueJob) (memory.Repository, llm.Provider, error) {
		return resolveMemoryDistillationJobAtHome(ctx, filepath.Clean(*root), configHome, job)
	})
}

// The command calls this before changing to any queued workspace. The
// absolute environment value keeps legacy config/auth surfaces aligned with
// VerifiedHome even when the foreground used a relative METIS_HOME.
func freezeMemoryDistillationConfigHome() (string, error) {
	configHome, err := config.VerifiedHome()
	if err != nil {
		return "", memoryDistillationSetupFailure("config_namespace", err)
	}
	if !filepath.IsAbs(configHome) {
		return "", memoryDistillationSetupFailure("config_namespace", errors.New("memory distillation requires an absolute configuration home"))
	}
	if err := os.Setenv("METIS_HOME", configHome); err != nil {
		return "", memoryDistillationSetupFailure("config_namespace", err)
	}
	return configHome, nil
}

func resolveMemoryDistillationJob(ctx context.Context, root string, job agent.DistillationQueueJob) (memory.Repository, llm.Provider, error) {
	configHome, err := freezeMemoryDistillationConfigHome()
	if err != nil {
		return nil, nil, err
	}
	return resolveMemoryDistillationJobAtHome(ctx, root, configHome, job)
}

func resolveMemoryDistillationJobAtHome(ctx context.Context, root, configHome string, job agent.DistillationQueueJob) (memory.Repository, llm.Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, memoryDistillationSetupFailure("processing_error", err)
	}
	if !filepath.IsAbs(job.ConfigHome) || filepath.Clean(job.ConfigHome) != filepath.Clean(configHome) {
		return nil, nil, memoryDistillationSetupFailure("config_namespace", errors.New("memory distillation configuration namespace changed"))
	}
	currentHome, err := config.VerifiedHome()
	if err != nil {
		return nil, nil, memoryDistillationSetupFailure("config_namespace", err)
	}
	if currentHome != configHome {
		return nil, nil, memoryDistillationSetupFailure("config_namespace", errors.New("memory distillation configuration namespace changed"))
	}
	if !filepath.IsAbs(job.WorkspacePath) {
		return nil, nil, memoryDistillationSetupFailure("workspace", errors.New("memory distillation job has no absolute workspace"))
	}
	if job.Scope != "" {
		scope, err := memory.WorkspaceScope(job.WorkspacePath)
		if err != nil {
			return nil, nil, memoryDistillationSetupFailure("workspace", err)
		}
		if scope != job.Scope {
			return nil, nil, memoryDistillationSetupFailure("workspace", errors.New("memory distillation workspace scope changed"))
		}
	}
	if strings.TrimSpace(job.ProviderName) == "" {
		return nil, nil, memoryDistillationSetupFailure("provider_configuration", errors.New("memory distillation job has no provider identity"))
	}
	if err := os.Chdir(job.WorkspacePath); err != nil {
		return nil, nil, memoryDistillationSetupFailure("workspace", err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, memoryDistillationSetupFailure("config_namespace", err)
	}
	// Never prompt in a detached process. Revoked/absent workspace trust means
	// only user-owned provider configuration can supply endpoint/credentials.
	if err := config.ApplyProviderPolicyForWorkspace(cfg, currentWorkspaceTrusted()); err != nil {
		return nil, nil, memoryDistillationSetupFailure("provider_configuration", err)
	}
	provider, err := rtpkg.BuildProviderWithoutPreconnect(cfg, job.ProviderName, job.ModelID)
	if err != nil {
		return nil, nil, memoryDistillationSetupFailure("provider_configuration", err)
	}
	var repository *memory.MemoryManager
	if job.Scope == "" {
		repository, err = memory.NewMemoryManager(root)
	} else {
		repository, err = memory.NewMemoryManagerForWorkspace(root, job.WorkspacePath)
	}
	if err != nil {
		return nil, nil, memoryDistillationSetupFailure("repository", err)
	}
	return repository, provider.Provider, nil
}
