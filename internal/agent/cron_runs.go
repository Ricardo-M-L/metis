package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/google/uuid"
)

const (
	CronRunRetention   = 100
	CronRunOutputLimit = 64 * 1024
	cronRunFileLimit   = 512 * 1024
)

var ErrCronJobRunning = errors.New("cron job is already running")

// CronRunRecord is a durable outcome for every unattended fire, including
// startup failures and skipped overlapping invocations. SessionID points to
// the actual saved conversation for this fire, rather than the daemon itself.
type CronRunRecord struct {
	ID         string     `json:"id"`
	JobID      string     `json:"jobId"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	SessionID  string     `json:"sessionId,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	Error      string     `json:"error,omitempty"`
	Output     string     `json:"output,omitempty"`
}

// CronRun holds an OS execution lock until its terminal record is committed.
// The independent records lock is held only for short storage transactions.
type CronRun struct {
	mu     sync.Mutex
	dir    string
	lock   *os.File
	record CronRunRecord
}

func (s *CronService) Root() string { return s.root }

func safeCronRunID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func cronRunDir(root, jobID string) (string, error) {
	if root == "" || !safeCronRunID(jobID) {
		return "", errors.New("cron runs: invalid root or job id")
	}
	// Never follow a symlink within our owned storage tree. The configured
	// root itself may legitimately be located in a symlinked home directory.
	for _, dir := range []string{filepath.Join(root, "runs"), filepath.Join(root, "runs", jobID)} {
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("cron runs: unsafe storage directory")
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, "runs", jobID), nil
}

func withCronRunRecords(dir string, fn func() error) (err error) {
	f, err := openCronRunLock(filepath.Join(dir, ".records.lock"))
	if err != nil {
		return err
	}
	defer f.Close()
	if err = lockCronFile(f); err != nil {
		return err
	}
	defer unlockCronFile(f)
	return fn()
}

// BeginCronRun atomically admits a fire. A conflicting fire is recorded as
// skipped and returns ErrCronJobRunning. An explicit ID is never overwritten.
func BeginCronRun(root, jobID, runID, trigger string) (*CronRun, error) {
	if runID == "" {
		runID = uuid.NewString()
	}
	if !safeCronRunID(runID) {
		return nil, errors.New("cron runs: invalid run id")
	}
	if trigger != "manual" && trigger != "scheduled" {
		return nil, errors.New("cron runs: invalid trigger")
	}
	dir, err := cronRunDir(root, jobID)
	if err != nil {
		return nil, err
	}
	f, err := openCronRunLock(filepath.Join(dir, ".execution.lock"))
	if err != nil {
		return nil, err
	}
	acquired := false
	run := &CronRun{dir: dir, lock: f, record: CronRunRecord{ID: runID, JobID: jobID, Trigger: trigger, Status: "running", StartedAt: time.Now().UTC()}}
	err = withCronRunRecords(dir, func() error {
		// Inspectors briefly acquire execution ownership to recover crashes.
		// Serialize their probes with admission so this never mistakes a read
		// for an overlapping run. This try-lock cannot wait on a live owner.
		var lockErr error
		acquired, lockErr = tryLockCronRunFile(f)
		if lockErr != nil {
			return lockErr
		}
		if _, err := os.Lstat(filepath.Join(dir, runID+".json")); err == nil {
			return errors.New("cron runs: run id already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
		if acquired {
			if err := recoverCronRunsLocked(dir); err != nil {
				return err
			}
		} else {
			now := time.Now().UTC()
			run.record.Status, run.record.FinishedAt = "skipped", &now
			run.record.Error = ErrCronJobRunning.Error()
		}
		if err := writeCronRunLocked(dir, run.record); err != nil {
			return err
		}
		return pruneCronRunsLocked(dir)
	})
	if err != nil || !acquired {
		if acquired {
			_ = unlockCronFile(f)
		}
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrCronJobRunning
	}
	return run, nil
}

func (r *CronRun) SetSessionID(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return errors.New("cron run already finished")
	}
	if !safeCronRunID(id) {
		return errors.New("cron runs: invalid session id")
	}
	r.record.SessionID = id
	return withCronRunRecords(r.dir, func() error { return writeCronRunLocked(r.dir, r.record) })
}

// Finish also releases ownership when persistence fails; readers can then
// correctly classify the last durable running record as interrupted.
func (r *CronRun) Finish(status, summary, output string, runErr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return nil
	}
	if status != "succeeded" && status != "failed" && status != "cancelled" && status != "interrupted" && status != "skipped" {
		return errors.New("cron runs: invalid terminal status")
	}
	defer func() { _ = unlockCronFile(r.lock); _ = r.lock.Close(); r.lock = nil }()
	now := time.Now().UTC()
	r.record.Status, r.record.FinishedAt = status, &now
	r.record.Summary = boundedCronText(summary, 2048)
	r.record.Output = boundedCronText(output, CronRunOutputLimit)
	if runErr != nil {
		r.record.Error = boundedCronText(runErr.Error(), 4096)
	}
	return withCronRunRecords(r.dir, func() error {
		if err := writeCronRunLocked(r.dir, r.record); err != nil {
			return err
		}
		return pruneCronRunsLocked(r.dir)
	})
}

func boundedCronText(s string, limit int) string {
	s = security.RedactSubprocessText(s)
	if len(s) <= limit {
		return s
	}
	s = s[:limit-len("\n[truncated]")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "\n[truncated]"
}

func writeCronRunLocked(dir string, record CronRunRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > cronRunFileLimit {
		return errors.New("cron runs: record too large")
	}
	f, err := os.CreateTemp(dir, ".run-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, record.ID+".json")); err != nil {
		return err
	}
	// Strengthen rename durability on platforms supporting directory fsync;
	// Windows may reject it after the record was successfully committed.
	if parent, err := os.Open(dir); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return nil
}

func readCronRunFile(dir, id string) (*CronRunRecord, error) {
	path := filepath.Join(dir, id+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > cronRunFileLimit {
		return nil, errors.New("cron runs: unsafe or oversized record")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var record CronRunRecord
	if err := json.NewDecoder(io.LimitReader(f, cronRunFileLimit+1)).Decode(&record); err != nil {
		return nil, err
	}
	if record.ID != id || record.JobID != filepath.Base(dir) {
		return nil, errors.New("cron runs: record identity mismatch")
	}
	return &record, nil
}

func readAllCronRunsLocked(dir string) ([]CronRunRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	records := make([]CronRunRecord, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !safeCronRunID(id) {
			continue
		}
		r, err := readCronRunFile(dir, id)
		if err != nil {
			return nil, fmt.Errorf("read cron run %s: %w", id, err)
		}
		records = append(records, *r)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].ID > records[j].ID
		}
		return records[i].StartedAt.After(records[j].StartedAt)
	})
	return records, nil
}

func recoverCronRunsLocked(dir string) error {
	records, err := readAllCronRunsLocked(dir)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Status != "running" {
			continue
		}
		now := time.Now().UTC()
		r.Status, r.FinishedAt, r.Error = "interrupted", &now, "Execution owner exited before recording an outcome"
		if err := writeCronRunLocked(dir, r); err != nil {
			return err
		}
	}
	return nil
}

func pruneCronRunsLocked(dir string) error {
	records, err := readAllCronRunsLocked(dir)
	if err != nil {
		return err
	}
	remaining := CronRunRetention
	for _, r := range records {
		if r.Status == "running" {
			remaining--
		}
	}
	for _, r := range records {
		if r.Status == "running" {
			continue
		}
		if remaining <= 0 {
			if err := os.Remove(filepath.Join(dir, r.ID+".json")); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		remaining--
	}
	return nil
}

// Inspecting a live owner never marks it interrupted. A nonblocking OS lock,
// rather than a PID/timestamp heuristic, also handles PID reuse and SIGKILL.
func withRecoveredCronRuns(root, jobID string, fn func(string) error) error {
	dir, err := cronRunDir(root, jobID)
	if err != nil {
		return err
	}
	f, err := openCronRunLock(filepath.Join(dir, ".execution.lock"))
	if err != nil {
		return err
	}
	defer f.Close()
	return withCronRunRecords(dir, func() error {
		acquired, err := tryLockCronRunFile(f)
		if err != nil {
			return err
		}
		if acquired {
			defer unlockCronFile(f)
		}
		if acquired {
			if err := recoverCronRunsLocked(dir); err != nil {
				return err
			}
		}
		return fn(dir)
	})
}

func ListCronRuns(root, jobID string, limit int) ([]CronRunRecord, error) {
	if limit <= 0 || limit > CronRunRetention {
		limit = CronRunRetention
	}
	var records []CronRunRecord
	err := withRecoveredCronRuns(root, jobID, func(dir string) error {
		var err error
		records, err = readAllCronRunsLocked(dir)
		if len(records) > limit {
			records = records[:limit]
		}
		// Long output belongs to the detail route, not every list refresh.
		for i := range records {
			records[i].Output = ""
		}
		return err
	})
	return records, err
}

func ReadCronRun(root, jobID, runID string) (*CronRunRecord, error) {
	if !safeCronRunID(runID) {
		return nil, errors.New("cron runs: invalid run id")
	}
	var record *CronRunRecord
	err := withRecoveredCronRuns(root, jobID, func(dir string) error { var err error; record, err = readCronRunFile(dir, runID); return err })
	return record, err
}

func CronJobRunning(root, jobID string) (bool, error) {
	dir, err := cronRunDir(root, jobID)
	if err != nil {
		return false, err
	}
	f, err := openCronRunLock(filepath.Join(dir, ".execution.lock"))
	if err != nil {
		return false, err
	}
	defer f.Close()
	running := false
	err = withCronRunRecords(dir, func() error {
		acquired, err := tryLockCronRunFile(f)
		if acquired {
			_ = unlockCronFile(f)
		}
		running = !acquired
		return err
	})
	return running, err
}

// WithCronJobIdle serializes a job deletion against admission without
// creating a run record. Lock order matches Begin + RunNow: execution first,
// short job-storage transaction second.
func WithCronJobIdle(root, jobID string, fn func() error) error {
	dir, err := cronRunDir(root, jobID)
	if err != nil {
		return err
	}
	f, err := openCronRunLock(filepath.Join(dir, ".execution.lock"))
	if err != nil {
		return err
	}
	defer f.Close()
	err = withCronRunRecords(dir, func() error {
		acquired, err := tryLockCronRunFile(f)
		if err != nil {
			return err
		}
		if !acquired {
			return ErrCronJobRunning
		}
		return nil
	})
	if err != nil {
		return err
	}
	defer unlockCronFile(f)
	return fn()
}
