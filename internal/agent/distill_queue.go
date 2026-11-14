package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memdir"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/processutil"
)

const distillationQueueDirectory = "distillation-queue"

// DistillationQueueJob is a durable hand-off, never a provider client or a
// credential snapshot. A worker reconstructs the frozen route from local
// configuration and validates the workspace namespace before using it.
type DistillationQueueJob struct {
	Version          int       `json:"version"`
	ID               string    `json:"id"`
	RepositoryRoot   string    `json:"repository_root"`
	Scope            string    `json:"scope"`
	WorkspacePath    string    `json:"workspace_path"`
	ConfigHome       string    `json:"config_home"`
	SessionID        string    `json:"session_id"`
	SourceMessageID  string    `json:"source_message_id"`
	SourceRequestID  string    `json:"source_request_id"`
	ProviderName     string    `json:"provider_name"`
	ModelID          string    `json:"model_id"`
	UserMessage      string    `json:"user_message,omitempty"`
	AssistantMessage string    `json:"assistant_message,omitempty"`
	Status           string    `json:"status"`
	Ready            bool      `json:"ready"`
	OwnerPID         int       `json:"owner_pid,omitempty"`
	Attempts         int       `json:"attempts"`
	LeaseUntil       time.Time `json:"lease_until,omitempty"`
	NextAttemptAt    time.Time `json:"next_attempt_at,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	CompletedAt      time.Time `json:"completed_at,omitempty"`
	// A classified reason preserves retry evidence without writing arbitrary
	// provider errors, which can contain credentials, to another disk log.
	LastFailure string `json:"last_failure,omitempty"`
}

func durableDistillationManager(repository memory.Repository) (*memory.MemoryManager, bool) {
	manager, ok := repository.(*memory.MemoryManager)
	if !ok {
		if binding, explicitCapability := repository.(interface{ DurableMemoryManager() *memory.MemoryManager }); explicitCapability {
			manager = binding.DurableMemoryManager()
			ok = manager != nil
		}
	}
	return manager, ok && manager != nil && strings.TrimSpace(manager.Root()) != ""
}

// DurableDistillationRoot is empty for custom repositories whose persistence
// and reconstruction semantics are unknown. Standard workspace bindings can
// explicitly expose their concrete manager; other wrappers retain legacy joins.
func (l *Loop) DurableDistillationRoot() string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	manager, ok := durableDistillationManager(l.Memory)
	enabled := l.DistillEvery > 0
	l.mu.RUnlock()
	if !ok || !enabled {
		return ""
	}
	root, err := filepath.Abs(manager.Root())
	if err != nil {
		return ""
	}
	return root
}

// PersistPendingDistillation releases a successful boundary's durable jobs.
// Successful exchanges were already fsynced before Run returned; readiness is
// separately persisted so an active worker cannot bypass the normal cadence.
func (l *Loop) PersistPendingDistillation(sessionID string) (bool, error) {
	if l == nil {
		return false, nil
	}
	l.mu.RLock()
	manager, durable := durableDistillationManager(l.Memory)
	enabled := l.DistillEvery > 0
	l.mu.RUnlock()
	if !durable || !enabled {
		return durable, nil
	}
	_, err := ReleaseDurableDistillationQueue(manager.Root(), sessionID)
	if err == nil {
		l.startReleasedDistillationWorker(manager.Root())
	}
	return true, err
}

func (l *Loop) startReleasedDistillationWorker(root string) {
	if l.DurableDistillationLauncher != nil {
		if absolute, err := filepath.Abs(root); err == nil {
			_ = l.DurableDistillationLauncher(absolute)
		}
	}
}

// ReleaseDurableDistillationQueue is called at cadence, successful session
// boundaries, and explicit startup recovery. Merely starting a worker never
// releases staged exchanges from another still-running foreground session.
func ReleaseDurableDistillationQueue(root, sessionID string) (int, error) {
	return releaseDurableDistillationQueue(root, sessionID, false)
}

// RecoverDurableDistillationQueue releases abandoned staged exchanges only.
// PID reuse is deliberately conservative: it can delay recovery, never admit
// another live foreground's work early. An explicit session boundary can
// always release that work; old jobs with no owner PID remain recoverable.
func RecoverDurableDistillationQueue(root string) (int, error) {
	return releaseDurableDistillationQueue(root, "", true)
}

func releaseDurableDistillationQueue(root, sessionID string, abandonedOnly bool) (int, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	released := 0
	err = withDistillationQueueLock(root, func() error {
		jobs, err := listDistillationQueueJobs(root)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job.Status != "pending" || job.Ready || (sessionID != "" && job.SessionID != sessionID) {
				continue
			}
			if abandonedOnly && job.OwnerPID > 0 && processutil.Alive(job.OwnerPID) {
				continue
			}
			job.Ready = true
			if err := writeDistillationQueueJob(root, job); err != nil {
				return err
			}
			released++
		}
		return nil
	})
	return released, err
}

func enqueueDurableDistillation(snapshot distillSnapshot) (bool, error) {
	manager, ok := durableDistillationManager(snapshot.repository)
	if !ok {
		return false, nil
	}
	if snapshot.provider == nil || strings.TrimSpace(snapshot.sessionID) == "" || strings.TrimSpace(snapshot.sourceMessageID) == "" {
		return true, nil
	}
	if !distillationMayAddInformation(snapshot) {
		return true, nil
	}
	if err := manager.CheckSourceSession(snapshot.sessionID); err != nil {
		return true, err
	}
	root, err := filepath.Abs(manager.Root())
	if err != nil {
		return true, err
	}
	workspace := snapshot.workspacePath
	if workspace == "" {
		workspace, err = os.Getwd()
		if err != nil {
			return true, err
		}
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return true, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(workspace); resolveErr == nil {
		workspace = resolved
	}
	if scope := manager.WorkspaceScope(); scope != "" {
		actual, scopeErr := memory.WorkspaceScope(workspace)
		if scopeErr != nil {
			return true, scopeErr
		}
		if actual != scope {
			return true, errors.New("memory distillation workspace does not match repository scope")
		}
	}
	user, assistant := memdir.Redact(snapshot.userMsg), memdir.Redact(snapshot.assistantMsg)
	if user.Reject || assistant.Reject {
		return true, memory.ErrSensitiveMemory
	}
	providerName := snapshot.providerName
	if providerName == "" {
		providerName = snapshot.provider.Name()
	}
	modelID := snapshot.modelID
	if modelID == "" {
		modelID = snapshot.provider.ModelID()
	}
	configHome, err := config.VerifiedHome()
	if err != nil {
		return true, err
	}
	job := DistillationQueueJob{Version: 1, RepositoryRoot: root, Scope: manager.WorkspaceScope(), WorkspacePath: workspace,
		ConfigHome: configHome,
		SessionID:  snapshot.sessionID, SourceMessageID: snapshot.sourceMessageID, SourceRequestID: snapshot.sourceRequestID,
		ProviderName: providerName, ModelID: modelID, UserMessage: user.Redacted, AssistantMessage: assistant.Redacted,
		Status: "pending", Ready: !snapshot.waitForCadence, CreatedAt: time.Now().UTC()}
	job.ID = distillationQueueJobID(job)
	if snapshot.waitForCadence {
		job.OwnerPID = os.Getpid()
	}
	return true, withDistillationQueueLock(root, func() error {
		// DeleteSession writes its tombstone before taking this same queue
		// lock. Rechecking here closes preflight -> enqueue -> delete races.
		if err := manager.CheckSourceSession(snapshot.sessionID); err != nil {
			return err
		}
		_, readErr := readDistillationQueueJob(root, job.ID)
		if readErr == nil {
			return nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		return writeDistillationQueueJob(root, job)
	})
}

func distillationQueueJobID(job DistillationQueueJob) string {
	identity, _ := json.Marshal([]string{job.RepositoryRoot, job.Scope, job.SessionID, job.SourceMessageID, job.SourceRequestID, job.ConfigHome})
	sum := sha256.Sum256(identity)
	return hex.EncodeToString(sum[:])
}

// Only a complete, exact restatement of already durable bodies is skipped.
// We deliberately do not classify short preferences or questions by length,
// punctuation, greetings, or a phrase blacklist.
func distillationMayAddInformation(snapshot distillSnapshot) bool {
	user, assistant := normalizeDistillationContent(snapshot.userMsg), normalizeDistillationContent(snapshot.assistantMsg)
	if user == "" || assistant == "" {
		return false
	}
	passages, err := snapshot.repository.Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil {
		return true
	}
	known := normalizeDistillationContent(snapshot.repository.BuildContext())
	for _, passage := range passages {
		known += "\n" + normalizeDistillationContent(passage.Content)
	}
	return !strings.Contains(known, user) || !strings.Contains(known, assistant)
}

func normalizeDistillationContent(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(value))
}

func withDistillationQueueLock(root string, run func() error) error {
	file, err := memdir.OpenPrivateRegularFile(root, filepath.Join(distillationQueueDirectory, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = trySubAgentTranscriptLock(file)
		if err == nil {
			return run()
		}
		if !errors.Is(err, ErrSubAgentTranscriptBusy) || time.Now().After(deadline) {
			return fmt.Errorf("lock memory distillation queue: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readDistillationQueueJob(root, id string) (DistillationQueueJob, error) {
	var job DistillationQueueJob
	if len(id) != sha256.Size*2 {
		return job, errors.New("invalid memory distillation identity")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return job, errors.New("invalid memory distillation identity")
	}
	data, err := memdir.ReadPrivateRegularFile(root, filepath.Join(distillationQueueDirectory, id+".json"), 16<<20)
	if err != nil {
		return job, err
	}
	if err := json.Unmarshal(data, &job); err != nil {
		return job, err
	}
	if job.Version != 1 || job.ID != id || distillationQueueJobID(job) != id || filepath.Clean(job.RepositoryRoot) != filepath.Clean(root) {
		return job, errors.New("memory distillation job identity mismatch")
	}
	return job, nil
}

func writeDistillationQueueJob(root string, job DistillationQueueJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return memdir.AtomicWritePrivateFile(root, filepath.Join(distillationQueueDirectory, job.ID+".json"), append(data, '\n'), 0o600)
}

func listDistillationQueueJobs(root string) ([]DistillationQueueJob, error) {
	entries, err := os.ReadDir(filepath.Join(root, distillationQueueDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []DistillationQueueJob
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		job, err := readDistillationQueueJob(root, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	return jobs, nil
}

func DurableDistillationQueueHasPending(root string) (bool, error) {
	jobs, err := listDistillationQueueJobs(root)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if (job.Status == "pending" && job.Ready) || job.Status == "running" {
			return true, nil
		}
	}
	return false, nil
}

func DurableDistillationQueueWorkerActive(root string) (bool, error) {
	file, err := memdir.OpenPrivateRegularFile(root, filepath.Join(distillationQueueDirectory, "worker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = trySubAgentTranscriptLock(file)
	if errors.Is(err, ErrSubAgentTranscriptBusy) {
		return true, nil
	}
	return false, err
}

// RunDurableDistillationQueue owns one cross-process lock for its lifetime.
// Acquiring it proves any old running lease is abandoned (including SIGKILL),
// so restart recovery does not have to wait for a stale wall-clock deadline.
// Failed work remains pending with backoff; each worker has a finite budget.
func RunDurableDistillationQueue(ctx context.Context, root string, resolve func(context.Context, DistillationQueueJob) (memory.Repository, llm.Provider, error)) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := memdir.OpenPrivateRegularFile(root, filepath.Join(distillationQueueDirectory, "worker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := trySubAgentTranscriptLock(lock); errors.Is(err, ErrSubAgentTranscriptBusy) {
		return nil
	} else if err != nil {
		return err
	}
	workerCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	idleUntil := time.Now().Add(300 * time.Millisecond)
	attemptsThisLaunch := make(map[string]int)
	for {
		var job *DistillationQueueJob
		var next time.Time
		err := withDistillationQueueLock(root, func() error {
			jobs, err := listDistillationQueueJobs(root)
			if err != nil {
				return err
			}
			for i := range jobs {
				candidate := jobs[i]
				if candidate.Status != "pending" && candidate.Status != "running" {
					continue
				}
				if candidate.Status == "pending" && !candidate.Ready {
					continue
				}
				if attemptsThisLaunch[candidate.ID] >= 3 {
					continue
				}
				if candidate.Status == "pending" && time.Now().Before(candidate.NextAttemptAt) {
					if next.IsZero() || candidate.NextAttemptAt.Before(next) {
						next = candidate.NextAttemptAt
					}
					continue
				}
				candidate.Status, candidate.LeaseUntil = "running", time.Now().UTC().Add(45*time.Second)
				candidate.Attempts++
				if err := writeDistillationQueueJob(root, candidate); err != nil {
					return err
				}
				job = &candidate
				attemptsThisLaunch[candidate.ID]++
				break
			}
			return nil
		})
		if err != nil {
			return err
		}
		if job == nil {
			if next.IsZero() && time.Now().After(idleUntil) {
				return nil
			}
			wait := 100 * time.Millisecond
			if !next.IsZero() && time.Until(next) < wait {
				wait = max(time.Until(next), time.Millisecond)
			}
			select {
			case <-workerCtx.Done():
				return workerCtx.Err()
			case <-time.After(wait):
			}
			continue
		}
		idleUntil = time.Now().Add(300 * time.Millisecond)
		jobCtx, jobCancel := context.WithTimeout(workerCtx, 30*time.Second)
		// Revocation is checked before workspace/provider resolution. Deleted
		// sources must be scrubbed even when their old workspace or credentials
		// can no longer be reconstructed.
		processErr := memory.CheckSourceSessionAtRoot(root, job.SessionID)
		var repository memory.Repository
		var provider llm.Provider
		if processErr == nil {
			repository, provider, processErr = resolve(jobCtx, *job)
		}
		if processErr == nil && provider == nil {
			processErr = errors.New("memory distillation provider unavailable")
		}
		if processErr == nil {
			processErr = validateDistillationJobRepository(*job, repository)
		}
		if processErr == nil {
			if guard, ok := repository.(interface{ CheckSourceSession(string) error }); ok {
				// This is provider-work admission, not a lock held over network
				// I/O. A later deletion cannot unsend admitted input; it scrubs
				// queued text and repository tombstones reject every late write.
				processErr = guard.CheckSourceSession(job.SessionID)
			}
		}
		if processErr == nil {
			processErr = repository.DistillTurnWithMetadata(jobCtx, provider, job.SessionID, job.SourceMessageID, job.UserMessage, job.AssistantMessage)
		}
		jobCancel()
		if err := withDistillationQueueLock(root, func() error {
			current, err := readDistillationQueueJob(root, job.ID)
			if err != nil {
				return err
			}
			if current.Status == "revoked" {
				// DeleteSession may have scrubbed this job while its provider
				// ignored cancellation. Never restore the worker's frozen text.
				return nil
			}
			job.LeaseUntil = time.Time{}
			if processErr == nil || errors.Is(processErr, memory.ErrSessionDeleted) {
				job.Status, job.CompletedAt = "completed", time.Now().UTC()
				if processErr != nil {
					job.Status = "revoked"
				}
				job.UserMessage, job.AssistantMessage, job.LastFailure = "", "", ""
			} else {
				job.Status, job.LastFailure = "pending", distillationFailureCode(processErr)
				backoff := time.Second << min(job.Attempts, 8)
				job.NextAttemptAt = time.Now().UTC().Add(backoff)
			}
			return writeDistillationQueueJob(root, *job)
		}); err != nil {
			return err
		}
		if workerCtx.Err() != nil {
			return workerCtx.Err()
		}
		// Per-job launch budgets leave other due work runnable even when an old
		// job has accumulated failures across many process restarts.
	}
}

func distillationFailureCode(err error) string {
	var providerErr *memory.DistillationProviderError
	if errors.As(err, &providerErr) {
		return "provider_error"
	}
	var classified interface{ DistillationFailureCode() string }
	if errors.As(err, &classified) {
		switch code := classified.DistillationFailureCode(); code {
		case "config_namespace", "workspace", "provider_configuration", "repository":
			return code
		}
	}
	return "processing_error"
}

func validateDistillationJobRepository(job DistillationQueueJob, repository memory.Repository) error {
	if repository == nil {
		return errors.New("memory distillation repository unavailable")
	}
	root, err := filepath.Abs(repository.Root())
	if err != nil {
		return err
	}
	if filepath.Clean(root) != filepath.Clean(job.RepositoryRoot) {
		return errors.New("memory distillation root mismatch")
	}
	if scope, ok := repository.(interface{ WorkspaceScope() string }); ok && scope.WorkspaceScope() != job.Scope {
		return errors.New("memory distillation scope mismatch")
	}
	if job.Scope != "" {
		actual, err := memory.WorkspaceScope(job.WorkspacePath)
		if err != nil {
			return err
		}
		if actual != job.Scope {
			return errors.New("memory distillation workspace identity changed")
		}
	}
	return nil
}
