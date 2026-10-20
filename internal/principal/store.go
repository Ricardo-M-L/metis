package principal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Store struct {
	root string
	now  func() time.Time
	mu   sync.Mutex
	// Protected by the store/file lock. Intermediate immutable tree versions
	// need not reach disk when several old summaries migrate in one commit.
	historyPending map[string][]byte
}

func NewStore(root string) *Store {
	return &Store{root: root, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) Create(name string, definition Definition) (Job, error) {
	hash, err := HashDefinition(definition)
	if err != nil {
		return Job{}, err
	}
	if name == "" || len(name) > 128 {
		return Job{}, errors.New("persistent job name is required (at most 128 bytes)")
	}
	j := Job{Version: SchemaVersion, ID: randomID("job-"), Name: name, Status: Active, ConfigSHA256: hash, Definition: definition, CreatedAt: s.now(), UpdatedAt: s.now()}
	j.SessionID = uuid.NewString()
	j.SessionDir = filepath.Join(s.root, j.ID, "sessions")
	j.MemoryDir = filepath.Join(s.root, j.ID, "memory")
	err = s.withLock(func() error { return s.write(j, true) })
	if err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

func (s *Store) Get(id string) (Job, error) {
	var j Job
	err := s.withLock(func() error { var err error; j, err = s.read(id); return err })
	return clone(j), err
}
func (s *Store) List() ([]Job, error) {
	var jobs []Job
	err := s.withLock(func() error {
		entries, err := os.ReadDir(s.root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			j, err := s.read(entry.Name()[:len(entry.Name())-5])
			if err != nil {
				return err
			}
			jobs = append(jobs, clone(j))
		}
		return nil
	})
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	if jobs == nil {
		jobs = []Job{}
	}
	return jobs, err
}

func (s *Store) SetStatus(id string, status Status) (Job, error) {
	return s.update(id, func(j *Job) error {
		if status != Active && status != Paused && status != Retired {
			return errors.New("invalid persistent job status")
		}
		if j.Status == Retired && status != Retired {
			return errors.New("retired persistent job cannot be resumed")
		}
		j.Status = status
		return nil
	})
}

func (s *Store) RegisterRun(binding Binding, runID string) (Job, error) {
	if !validID(runID) {
		return Job{}, errors.New("invalid persistent job run identity")
	}
	return s.update(binding.JobID, func(j *Job) error {
		if err := checkBinding(*j, binding); err != nil {
			return err
		}
		for _, r := range j.Runs {
			if r.RunID == runID {
				return nil
			}
		}
		if _, found, err := s.findArchived(*j, runID); err != nil {
			return err
		} else if found {
			return nil
		}
		if j.Status != Active {
			return ErrInactive
		}
		if j.exhausted() {
			return ErrBudget
		}
		j.Runs = append(j.Runs, RunUsage{RunID: runID, Status: "active", UpdatedAt: s.now()})
		return nil
	})
}

// RecordRunUsage replaces an absolute run projection. This operation is
// idempotent, including after a crashed worker is restarted. Only the bound
// coordinator journal can supply these totals; callers never add an estimate
// to a physical request a second time.
func (s *Store) RecordRunUsage(binding Binding, usage RunUsage) (Job, error) {
	if !validID(usage.RunID) || usage.SourceRevision == 0 || !validUSD(usage.SpentUSD) || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0 || usage.UnresolvedRequests < 0 {
		return Job{}, errors.New("invalid persistent job run usage")
	}
	return s.update(binding.JobID, func(j *Job) error {
		if err := checkBinding(*j, binding); err != nil {
			return err
		}
		found := false
		for i, r := range j.Runs {
			if r.RunID == usage.RunID {
				if usage.SourceRevision < r.SourceRevision {
					return nil
				}
				if usage.SourceRevision == r.SourceRevision {
					oldAt := r.UpdatedAt
					usage.UpdatedAt = oldAt
					if usage != r {
						return errors.New("same coordinator revision has conflicting job accounting projections")
					}
					return nil
				}
				usage.UpdatedAt = s.now()
				j.Runs[i] = usage
				found = true
				break
			}
		}
		if !found {
			old, archived, err := s.findArchived(*j, usage.RunID)
			if err != nil {
				return err
			}
			if !archived {
				return errors.New("run is not registered with persistent job")
			}
			if usage.SourceRevision < old.SourceRevision {
				return nil
			}
			if usage.SourceRevision == old.SourceRevision {
				usage.UpdatedAt = old.UpdatedAt
				if usage != old {
					return errors.New("same coordinator revision has conflicting job accounting projections")
				}
				return nil
			}
			usage.UpdatedAt = s.now()
			if settledRun(usage) {
				j.RunHistory, err = s.putArchived(*j, j.RunHistory, usage)
			} else {
				j.RunHistory, err = s.deleteArchived(*j, j.RunHistory, usage.RunID)
				j.Runs = append(j.Runs, usage)
			}
			if err != nil {
				return err
			}
		}
		return recount(j)
	})
}

func (s *Store) ClaimExecution(binding Binding, executionID, worker string, ttl time.Duration) (Lease, error) {
	if !validID(executionID) || worker == "" || len(worker) > 256 {
		return Lease{}, errors.New("persistent job execution and worker identities are required")
	}
	if ttl < time.Second || ttl > 24*time.Hour {
		return Lease{}, errors.New("persistent job lease must be between 1s and 24h")
	}
	var lease Lease
	_, err := s.update(binding.JobID, func(j *Job) error {
		if err := checkBinding(*j, binding); err != nil {
			return err
		}
		if j.Status != Active {
			return ErrInactive
		}
		if j.exhausted() {
			return ErrBudget
		}
		if j.UnresolvedRequests > 0 {
			return ErrUsageUnknown
		}
		if j.Lease != nil && j.Lease.ExpiresAt.After(s.now()) {
			return ErrBusy
		}
		lease = Lease{Token: randomID("lease-"), ExecutionID: executionID, Worker: worker, ExpiresAt: s.now().Add(ttl)}
		j.Lease = &lease
		return nil
	})
	return lease, err
}

func (s *Store) CheckExecution(binding Binding, lease Lease) (Job, error) {
	j, err := s.Get(binding.JobID)
	if err != nil {
		return Job{}, err
	}
	if err = checkBinding(j, binding); err != nil {
		return Job{}, err
	}
	if err = checkLease(j, lease, s.now()); err != nil {
		return Job{}, err
	}
	if j.Status != Active {
		return Job{}, ErrInactive
	}
	if j.exhausted() {
		return Job{}, ErrBudget
	}
	if j.UnresolvedRequests > 0 {
		return Job{}, ErrUsageUnknown
	}
	return j, nil
}
func (s *Store) RenewExecution(binding Binding, lease Lease, ttl time.Duration) error {
	if ttl < time.Second || ttl > 24*time.Hour {
		return errors.New("persistent job lease must be between 1s and 24h")
	}
	_, err := s.update(binding.JobID, func(j *Job) error {
		if err := checkBinding(*j, binding); err != nil {
			return err
		}
		if err := checkLease(*j, lease, s.now()); err != nil {
			return err
		}
		if j.Status != Active {
			return ErrInactive
		}
		j.Lease.ExpiresAt = s.now().Add(ttl)
		return nil
	})
	return err
}
func (s *Store) ReleaseExecution(binding Binding, lease Lease) error {
	_, err := s.update(binding.JobID, func(j *Job) error {
		if err := checkBinding(*j, binding); err != nil {
			return err
		}
		if j.Lease == nil {
			return nil
		}
		if j.Lease.Token != lease.Token {
			return ErrLeaseLost
		}
		j.Lease = nil
		return nil
	})
	return err
}

// KeepExecution cancels running work on loss of ownership or a lifecycle
// change. Request dispatch separately checks the durable status and budget.
func (s *Store) KeepExecution(ctx context.Context, binding Binding, lease Lease, ttl time.Duration) (context.Context, func()) {
	workCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(min(ttl/3, 2*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := s.RenewExecution(binding, lease, ttl); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	var once sync.Once
	return workCtx, func() { once.Do(func() { cancel(nil); <-done; _ = s.ReleaseExecution(binding, lease) }) }
}

func checkBinding(j Job, b Binding) error {
	if err := ValidateBinding(&b, j.Definition.Workspace); err != nil {
		return err
	}
	if j.ID != b.JobID || j.ConfigSHA256 != b.ConfigSHA256 {
		return errors.New("persistent job task configuration differs from its principal")
	}
	return nil
}
func checkLease(j Job, l Lease, now time.Time) error {
	if j.Lease == nil || j.Lease.Token != l.Token || j.Lease.ExecutionID != l.ExecutionID || !j.Lease.ExpiresAt.After(now) {
		return ErrLeaseLost
	}
	return nil
}
func recount(j *Job) error {
	if err := validHistory(j.RunHistory); err != nil {
		return err
	}
	j.SpentUSD = j.RunHistory.SpentUSD
	j.InputTokens = j.RunHistory.InputTokens
	j.OutputTokens = j.RunHistory.OutputTokens
	j.UnresolvedRequests = 0
	seen := make(map[string]bool, len(j.Runs))
	for _, r := range j.Runs {
		if !validID(r.RunID) || seen[r.RunID] || !validUSD(r.SpentUSD) || r.InputTokens < 0 || r.OutputTokens < 0 || r.CacheReadTokens < 0 || r.CacheWriteTokens < 0 || r.UnresolvedRequests < 0 {
			return errors.New("invalid persistent job run accounting projection")
		}
		switch r.Status {
		case "active", "blocked", "completed", "cancelled":
		default:
			return errors.New("invalid persistent job run lifecycle projection")
		}
		seen[r.RunID] = true
		if r.InputTokens > math.MaxInt64-r.CacheReadTokens || r.InputTokens+r.CacheReadTokens > math.MaxInt64-r.CacheWriteTokens {
			return errors.New("persistent job token accounting overflow")
		}
		input := r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens
		if j.InputTokens > math.MaxInt64-input || j.OutputTokens > math.MaxInt64-r.OutputTokens {
			return errors.New("persistent job token accounting overflow")
		}
		j.InputTokens += input
		j.OutputTokens += r.OutputTokens
		j.SpentUSD += r.SpentUSD
		if j.UnresolvedRequests > math.MaxInt-r.UnresolvedRequests {
			return errors.New("persistent job unresolved request accounting overflow")
		}
		j.UnresolvedRequests += r.UnresolvedRequests
	}
	if !validUSD(j.SpentUSD) {
		return errors.New("persistent job spend accounting overflow")
	}
	return nil
}
func (s *Store) update(id string, mutate func(*Job) error) (Job, error) {
	var result Job
	err := s.withLock(func() error {
		j, err := s.read(id)
		if err != nil {
			return err
		}
		s.historyPending = make(map[string][]byte)
		defer func() { s.historyPending = nil }()
		if err = mutate(&j); err != nil {
			return err
		}
		if err = s.archiveSettled(&j); err != nil {
			return err
		}
		if err = recount(&j); err != nil {
			return err
		}
		j.UpdatedAt = s.now()
		if err = s.flushHistory(j); err != nil {
			return err
		}
		if err = s.write(j, false); err != nil {
			return err
		}
		result = clone(j)
		return nil
	})
	return result, err
}
func clone(j Job) Job {
	data, _ := json.Marshal(j)
	var independent Job
	_ = json.Unmarshal(data, &independent)
	return independent
}
func (s *Store) path(id string) (string, error) {
	if !validID(id) {
		return "", ErrNotFound
	}
	return filepath.Join(s.root, id+".json"), nil
}
func (s *Store) read(id string) (Job, error) {
	p, err := s.path(id)
	if err != nil {
		return Job{}, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Job{}, errors.New("persistent job state must be a regular file")
	}
	if info.Size() > 8<<20 {
		return Job{}, errors.New("persistent job state exceeds 8 MiB")
	}
	f, err := os.Open(p)
	if err != nil {
		return Job{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err = json.Unmarshal(data, &j); err != nil {
		return Job{}, err
	}
	if (j.Version != SchemaVersion && j.Version != 1) || j.ID != id {
		return Job{}, errors.New("persistent job state identity or schema mismatch")
	}
	if j.Version == 1 && j.RunHistory != (RunHistory{}) {
		return Job{}, errors.New("legacy persistent job cannot contain archived history")
	}
	if j.RunHistory.Count > 0 {
		if _, err = s.readHistoryNode(j, j.RunHistory); err != nil {
			return Job{}, err
		}
	}
	if _, err := uuid.Parse(j.SessionID); err != nil || j.SessionDir != filepath.Join(s.root, id, "sessions") {
		return Job{}, errors.New("persistent job session identity mismatch")
	}
	if j.MemoryDir != filepath.Join(s.root, id, "memory") {
		return Job{}, errors.New("persistent job memory identity mismatch")
	}
	if err = ValidateBinding(ptr(j.Binding()), ""); err != nil {
		return Job{}, err
	}
	if j.Status != Active && j.Status != Paused && j.Status != Retired {
		return Job{}, errors.New("invalid persisted job status")
	}
	before := j
	if err = recount(&j); err != nil {
		return Job{}, err
	}
	if before.SpentUSD != j.SpentUSD || before.InputTokens != j.InputTokens || before.OutputTokens != j.OutputTokens || before.UnresolvedRequests != j.UnresolvedRequests {
		return Job{}, errors.New("persistent job accounting totals differ from run projections")
	}
	j.Version = SchemaVersion
	return j, nil
}
func ptr(b Binding) *Binding { return &b }
func (s *Store) write(j Job, create bool) error {
	p, err := s.path(j.ID)
	if err != nil {
		return err
	}
	if create {
		if _, err := os.Lstat(p); err == nil {
			return errors.New("persistent job identity collision")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return errors.New("persistent job active metadata exceeds 8 MiB; settle or cancel unfinished runs before assigning more work")
	}
	f, err := os.CreateTemp(s.root, ".job-*.tmp")
	if err != nil {
		return err
	}
	pTemp := f.Name()
	defer os.Remove(pTemp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(pTemp, p); err != nil {
		return err
	}
	dir, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) withLock(fn func() error) error {
	if s == nil || !filepath.IsAbs(s.root) {
		return errors.New("persistent job root must be absolute")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("persistent job root must be a directory, never a symlink")
	}
	if err = os.Chmod(s.root, 0700); err != nil {
		return err
	}
	f, err := openLock(filepath.Join(s.root, ".lock"))
	if err != nil {
		return err
	}
	defer f.Close()
	linked, err := os.Lstat(f.Name())
	opened, statErr := f.Stat()
	if err != nil || statErr != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return errors.New("persistent job lock file identity mismatch")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		locked, err := tryLock(f)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("persistent job lock timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer unlock(f)
	return fn()
}
