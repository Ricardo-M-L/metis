package principal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A deterministic treap gives the immutable history logarithmic lookup and
// updates without a growing index in the job JSON. Child references include
// their totals, so checking a node also checks the cumulative accounting root.
// New nodes are synced before the job manifest can reference them. A failed
// manifest write leaves only unreachable immutable files, never partial spend.
type historyNode struct {
	Version      int        `json:"version"`
	JobID        string     `json:"job_id"`
	ConfigSHA256 string     `json:"config_sha256"`
	Usage        RunUsage   `json:"usage"`
	Left         RunHistory `json:"left"`
	Right        RunHistory `json:"right"`
}

func settledRun(r RunUsage) bool {
	return r.SourceRevision > 0 && r.UnresolvedRequests == 0 && (r.Status == "completed" || r.Status == "cancelled")
}

func validHistory(h RunHistory) error {
	if h.Count == 0 {
		if h != (RunHistory{}) {
			return errors.New("empty persistent job history has nonempty metadata")
		}
		return nil
	}
	if len(h.SHA256) != 64 || !validUSD(h.SpentUSD) || h.InputTokens < 0 || h.OutputTokens < 0 {
		return errors.New("invalid persistent job history metadata")
	}
	if _, err := hex.DecodeString(h.SHA256); err != nil {
		return errors.New("invalid persistent job history digest")
	}
	return nil
}

func (s *Store) archiveSettled(j *Job) error {
	settled := 0
	for _, r := range j.Runs {
		if settledRun(r) {
			settled++
		}
	}
	if settled <= HotRunHistoryLimit {
		return nil
	}
	keep := make([]RunUsage, 0, len(j.Runs)-(settled-HotRunHistoryLimit))
	for _, r := range j.Runs {
		if settled > HotRunHistoryLimit && settledRun(r) {
			if _, duplicate, err := s.findArchived(*j, r.RunID); err != nil {
				return err
			} else if duplicate {
				return errors.New("persistent job run appears in both hot and archived history")
			}
			var err error
			j.RunHistory, err = s.putArchived(*j, j.RunHistory, r)
			if err != nil {
				return err
			}
			settled--
		} else {
			keep = append(keep, r)
		}
	}
	j.Runs = keep
	return nil
}

// RefreshRuns returns only projections which can still require coordinator
// refresh. Settled historical runs are corrected explicitly through
// RecordRunUsage when a newer authoritative receipt arrives.
func (s *Store) RefreshRuns(jobID string) ([]RunUsage, error) {
	var runs []RunUsage
	err := s.withLock(func() error {
		j, err := s.read(jobID)
		if err != nil {
			return err
		}
		for _, r := range j.Runs {
			if !settledRun(r) {
				runs = append(runs, r)
			}
		}
		return nil
	})
	return runs, err
}

// GetRunUsage looks up either hot or archived history. Missing or corrupt
// archive nodes produce errors; they never turn a registered run into a new
// run that could execute or be charged for a second time.
func (s *Store) GetRunUsage(jobID, runID string) (RunUsage, error) {
	var result RunUsage
	if !validID(runID) {
		return result, ErrNotFound
	}
	err := s.withLock(func() error {
		j, err := s.read(jobID)
		if err != nil {
			return err
		}
		for _, r := range j.Runs {
			if r.RunID == runID {
				result = r
				return nil
			}
		}
		var found bool
		result, found, err = s.findArchived(j, runID)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return nil
	})
	return result, err
}

// ListRunUsagePage enumerates all registered runs in RunID order, including
// archived runs. The cursor is the last returned RunID. A final page returns
// an empty next cursor. Each page loads only its search paths and results.
func (s *Store) ListRunUsagePage(jobID, cursor string, limit int) ([]RunUsage, string, error) {
	if cursor != "" && !validID(cursor) || limit < 1 || limit > 1024 {
		return nil, "", errors.New("invalid persistent job history page")
	}
	var runs []RunUsage
	err := s.withLock(func() error {
		j, err := s.read(jobID)
		if err != nil {
			return err
		}
		hot := append([]RunUsage(nil), j.Runs...)
		sort.Slice(hot, func(i, k int) bool { return hot[i].RunID < hot[k].RunID })
		for _, r := range hot {
			if r.RunID > cursor {
				runs = append(runs, r)
				if len(runs) > limit {
					break
				}
			}
		}
		var archived []RunUsage
		if err = s.historyPage(j, j.RunHistory, cursor, limit+1, &archived); err != nil {
			return err
		}
		runs = append(runs, archived...)
		sort.Slice(runs, func(i, k int) bool { return runs[i].RunID < runs[k].RunID })
		for i := 1; i < len(runs); i++ {
			if runs[i].RunID == runs[i-1].RunID {
				return errors.New("persistent job run appears in both hot and archived history")
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(runs) > limit {
		return runs[:limit], runs[limit-1].RunID, nil
	}
	return runs, "", nil
}

// VerifyRunHistory checks every reachable archived record. Normal accounting
// and admission use the authenticated root totals and logarithmic lookups;
// this full audit is suitable for startup/maintenance, not every request.
func (s *Store) VerifyRunHistory(jobID string) error {
	return s.withLock(func() error {
		j, err := s.read(jobID)
		if err != nil {
			return err
		}
		return s.walkHistory(j, j.RunHistory, "", "", make(map[string]bool))
	})
}

// CompactRunHistory removes obsolete immutable versions only after verifying
// the complete committed tree. Under the same process/file lock, no reader or
// writer can still be using an older root. A crash during cleanup affects only
// unreachable nodes; missing committed nodes stop cleanup before any deletion.
func (s *Store) CompactRunHistory(jobID string) (int, error) {
	removed := 0
	err := s.withLock(func() error {
		j, err := s.read(jobID)
		if err != nil {
			return err
		}
		reachable := make(map[string]bool)
		if err = s.walkHistory(j, j.RunHistory, "", "", reachable); err != nil {
			return err
		}
		root := filepath.Join(s.root, j.ID, "run-history")
		jobInfo, err := os.Lstat(filepath.Join(s.root, j.ID))
		if errors.Is(err, os.ErrNotExist) && j.RunHistory.Count == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if !jobInfo.IsDir() || jobInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("persistent job history parent must not be a symlink")
		}
		info, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) && j.RunHistory.Count == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("persistent job history directory must not be a symlink")
		}
		shards, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, shard := range shards {
			if len(shard.Name()) != 2 {
				return errors.New("unrecognized persistent job history shard")
			}
			if _, err = hex.DecodeString(shard.Name()); err != nil || !shard.IsDir() || shard.Type()&os.ModeSymlink != 0 {
				return errors.New("invalid persistent job history shard")
			}
			dir := filepath.Join(root, shard.Name())
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				name := entry.Name()
				if strings.HasPrefix(name, ".history-") && strings.HasSuffix(name, ".tmp") {
					if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
						return errors.New("invalid persistent job history temporary file")
					}
					if err = os.Remove(filepath.Join(dir, name)); err != nil {
						return err
					}
					removed++
					continue
				}
				if len(name) != 69 || filepath.Ext(name) != ".json" || name[:2] != shard.Name() {
					return errors.New("unrecognized persistent job history file")
				}
				digest := name[:64]
				if _, err = hex.DecodeString(digest); err != nil || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
					return errors.New("invalid persistent job history file")
				}
				if !reachable[digest] {
					if err = os.Remove(filepath.Join(dir, name)); err != nil {
						return err
					}
					removed++
				}
			}
			d, err := os.Open(dir)
			if err != nil {
				return err
			}
			err = d.Sync()
			_ = d.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	return removed, err
}

func (s *Store) walkHistory(j Job, ref RunHistory, lower, upper string, seen map[string]bool) error {
	if ref.Count == 0 {
		return nil
	}
	if seen[ref.SHA256] {
		return errors.New("duplicate persistent job history node")
	}
	n, err := s.readHistoryNode(j, ref)
	if err != nil {
		return err
	}
	if lower != "" && n.Usage.RunID <= lower || upper != "" && n.Usage.RunID >= upper {
		return errors.New("persistent job history ordering mismatch")
	}
	seen[ref.SHA256] = true
	if err = s.walkHistory(j, n.Left, lower, n.Usage.RunID, seen); err != nil {
		return err
	}
	return s.walkHistory(j, n.Right, n.Usage.RunID, upper, seen)
}

func (s *Store) historyPage(j Job, ref RunHistory, cursor string, limit int, out *[]RunUsage) error {
	if ref.Count == 0 || len(*out) >= limit {
		return nil
	}
	n, err := s.readHistoryNode(j, ref)
	if err != nil {
		return err
	}
	if n.Usage.RunID > cursor {
		if err = s.historyPage(j, n.Left, cursor, limit, out); err != nil {
			return err
		}
		if len(*out) < limit {
			*out = append(*out, n.Usage)
		}
	}
	return s.historyPage(j, n.Right, cursor, limit, out)
}

func (s *Store) findArchived(j Job, runID string) (RunUsage, bool, error) {
	ref := j.RunHistory
	for ref.Count > 0 {
		n, err := s.readHistoryNode(j, ref)
		if err != nil {
			return RunUsage{}, false, err
		}
		switch {
		case runID == n.Usage.RunID:
			return n.Usage, true, nil
		case runID < n.Usage.RunID:
			ref = n.Left
		default:
			ref = n.Right
		}
	}
	return RunUsage{}, false, nil
}

func historyPriority(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]) + id
}

func (s *Store) putArchived(j Job, ref RunHistory, usage RunUsage) (RunHistory, error) {
	if ref.Count == 0 {
		return s.writeHistoryNode(j, historyNode{Usage: usage})
	}
	n, err := s.readHistoryNode(j, ref)
	if err != nil {
		return RunHistory{}, err
	}
	if usage.RunID == n.Usage.RunID {
		n.Usage = usage
		return s.writeHistoryNode(j, n)
	}
	if usage.RunID < n.Usage.RunID {
		n.Left, err = s.putArchived(j, n.Left, usage)
		if err != nil {
			return RunHistory{}, err
		}
		left, err := s.readHistoryNode(j, n.Left)
		if err != nil {
			return RunHistory{}, err
		}
		if historyPriority(left.Usage.RunID) < historyPriority(n.Usage.RunID) {
			n.Left = left.Right
			left.Right, err = s.writeHistoryNode(j, n)
			if err != nil {
				return RunHistory{}, err
			}
			return s.writeHistoryNode(j, left)
		}
	} else {
		n.Right, err = s.putArchived(j, n.Right, usage)
		if err != nil {
			return RunHistory{}, err
		}
		right, err := s.readHistoryNode(j, n.Right)
		if err != nil {
			return RunHistory{}, err
		}
		if historyPriority(right.Usage.RunID) < historyPriority(n.Usage.RunID) {
			n.Right = right.Left
			right.Left, err = s.writeHistoryNode(j, n)
			if err != nil {
				return RunHistory{}, err
			}
			return s.writeHistoryNode(j, right)
		}
	}
	return s.writeHistoryNode(j, n)
}

func (s *Store) deleteArchived(j Job, ref RunHistory, runID string) (RunHistory, error) {
	if ref.Count == 0 {
		return ref, errors.New("registered history run disappeared")
	}
	n, err := s.readHistoryNode(j, ref)
	if err != nil {
		return RunHistory{}, err
	}
	switch {
	case runID == n.Usage.RunID:
		return s.mergeHistory(j, n.Left, n.Right)
	case runID < n.Usage.RunID:
		n.Left, err = s.deleteArchived(j, n.Left, runID)
	default:
		n.Right, err = s.deleteArchived(j, n.Right, runID)
	}
	if err != nil {
		return RunHistory{}, err
	}
	return s.writeHistoryNode(j, n)
}

func (s *Store) mergeHistory(j Job, left, right RunHistory) (RunHistory, error) {
	if left.Count == 0 {
		return right, nil
	}
	if right.Count == 0 {
		return left, nil
	}
	l, err := s.readHistoryNode(j, left)
	if err != nil {
		return RunHistory{}, err
	}
	r, err := s.readHistoryNode(j, right)
	if err != nil {
		return RunHistory{}, err
	}
	if historyPriority(l.Usage.RunID) < historyPriority(r.Usage.RunID) {
		l.Right, err = s.mergeHistory(j, l.Right, right)
		if err != nil {
			return RunHistory{}, err
		}
		return s.writeHistoryNode(j, l)
	}
	r.Left, err = s.mergeHistory(j, left, r.Left)
	if err != nil {
		return RunHistory{}, err
	}
	return s.writeHistoryNode(j, r)
}

func historySummary(n historyNode) (RunHistory, error) {
	if !settledRun(n.Usage) || !validID(n.Usage.RunID) {
		return RunHistory{}, errors.New("only settled terminal runs may be archived")
	}
	// Reuse the same overflow and lifecycle validation as the hot manifest.
	j := Job{Runs: []RunUsage{n.Usage}}
	if err := recount(&j); err != nil {
		return RunHistory{}, err
	}
	for _, child := range []RunHistory{n.Left, n.Right} {
		if err := validHistory(child); err != nil {
			return RunHistory{}, err
		}
		if j.InputTokens > math.MaxInt64-child.InputTokens || j.OutputTokens > math.MaxInt64-child.OutputTokens {
			return RunHistory{}, errors.New("persistent job archived token accounting overflow")
		}
		j.InputTokens += child.InputTokens
		j.OutputTokens += child.OutputTokens
		j.SpentUSD += child.SpentUSD
	}
	if !validUSD(j.SpentUSD) || n.Left.Count > math.MaxUint64-n.Right.Count || n.Left.Count+n.Right.Count == math.MaxUint64 {
		return RunHistory{}, errors.New("persistent job archived accounting overflow")
	}
	return RunHistory{Count: 1 + n.Left.Count + n.Right.Count, SpentUSD: j.SpentUSD, InputTokens: j.InputTokens, OutputTokens: j.OutputTokens}, nil
}

func (s *Store) historyPath(j Job, digest string, create bool) (string, error) {
	if !validID(j.ID) || len(digest) != 64 {
		return "", errors.New("invalid persistent job archive identity")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", err
	}
	dir := s.root
	for _, part := range []string{j.ID, "run-history", digest[:2]} {
		dir = filepath.Join(dir, part)
		if create {
			if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", fmt.Errorf("persistent job archive directory unavailable: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("persistent job archive directory must not be a symlink")
		}
	}
	return filepath.Join(dir, digest+".json"), nil
}

func (s *Store) readHistoryNode(j Job, ref RunHistory) (historyNode, error) {
	var n historyNode
	if err := validHistory(ref); err != nil || ref.Count == 0 {
		return n, errors.New("invalid persistent job history node reference")
	}
	data, pending := s.historyPending[ref.SHA256]
	if !pending {
		p, err := s.historyPath(j, ref.SHA256, false)
		if err != nil {
			return n, err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return n, fmt.Errorf("persistent job history node unavailable: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 16<<10 {
			return n, errors.New("persistent job history node must be a regular file below 16 KiB")
		}
		data, err = os.ReadFile(p)
		if err != nil {
			return n, err
		}
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return n, errors.New("persistent job history checksum mismatch")
	}
	if err := json.Unmarshal(data, &n); err != nil {
		return n, err
	}
	if n.Version != 1 || n.JobID != j.ID || n.ConfigSHA256 != j.ConfigSHA256 {
		return n, errors.New("persistent job history identity or schema mismatch")
	}
	summary, err := historySummary(n)
	if err != nil {
		return n, err
	}
	summary.SHA256 = ref.SHA256
	if summary != ref {
		return n, errors.New("persistent job history totals mismatch")
	}
	return n, nil
}

func (s *Store) writeHistoryNode(j Job, n historyNode) (RunHistory, error) {
	n.Version, n.JobID, n.ConfigSHA256 = 1, j.ID, j.ConfigSHA256
	ref, err := historySummary(n)
	if err != nil {
		return RunHistory{}, err
	}
	data, err := json.Marshal(n)
	if err != nil {
		return RunHistory{}, err
	}
	digest := sha256.Sum256(data)
	ref.SHA256 = hex.EncodeToString(digest[:])
	if s.historyPending != nil {
		s.historyPending[ref.SHA256] = data
		return ref, nil
	}
	dirs := make(map[string]bool)
	if err = s.persistHistoryNode(j, ref, data, dirs); err != nil {
		return RunHistory{}, err
	}
	if err = syncHistoryDirs(dirs); err != nil {
		return RunHistory{}, err
	}
	return ref, nil
}

func (s *Store) flushHistory(j Job) error {
	dirs := make(map[string]bool)
	flushed := make(map[string]bool)
	var flush func(RunHistory) error
	flush = func(ref RunHistory) error {
		data, pending := s.historyPending[ref.SHA256]
		if !pending || flushed[ref.SHA256] {
			return nil
		}
		n, err := s.readHistoryNode(j, ref)
		if err != nil {
			return err
		}
		if err = flush(n.Left); err != nil {
			return err
		}
		if err = flush(n.Right); err != nil {
			return err
		}
		if err = s.persistHistoryNode(j, ref, data, dirs); err != nil {
			return err
		}
		flushed[ref.SHA256] = true
		return nil
	}
	if err := flush(j.RunHistory); err != nil {
		return err
	}
	return syncHistoryDirs(dirs)
}

func (s *Store) persistHistoryNode(j Job, ref RunHistory, data []byte, dirs map[string]bool) error {
	p, err := s.historyPath(j, ref.SHA256, true)
	if err != nil {
		return err
	}
	// Even an existing immutable file may have been left by an interrupted
	// pre-commit flush. Sync its directory ancestry before reusing it.
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		dirs[dir] = true
		if dir == s.root {
			break
		}
	}
	if info, err := os.Lstat(p); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(len(data)) {
			return errors.New("invalid existing persistent job history node")
		}
		existing, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, data) {
			return errors.New("persistent job history checksum mismatch")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".history-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
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
	if err = os.Rename(f.Name(), p); err != nil {
		return err
	}
	return nil
}

func syncHistoryDirs(dirs map[string]bool) error {
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, k int) bool { return len(ordered[i]) > len(ordered[k]) })
	for _, dir := range ordered {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
