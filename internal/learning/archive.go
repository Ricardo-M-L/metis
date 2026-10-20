package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Archive files are immutable and fsynced before removing records from the
// active authority. An interrupted compaction can leave a redundant archive,
// but cannot discard its only durable evidence.
func writeLearningArchive(path string, data []byte) error {
	if old, err := readLearningArchive(path, len(data)+1); err == nil {
		if !bytes.Equal(old, data) {
			return errors.New("learning archive content conflict")
		}
		return syncLearningArchiveDirectories(filepath.Dir(path))
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".archive-*")
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
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncLearningArchiveDirectories(dir)
}

func syncLearningArchiveDirectories(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	// Persist any newly created archive directory entries before the caller
	// replaces hot state that used to contain the sole copy of these records.
	for parent := filepath.Dir(dir); ; parent = filepath.Dir(parent) {
		p, err := os.Open(parent)
		if err != nil {
			return err
		}
		err = errors.Join(p.Sync(), p.Close())
		if err != nil {
			return err
		}
		if filepath.Base(parent) == "archive" {
			p, err := os.Open(filepath.Dir(parent))
			if err != nil {
				return err
			}
			return errors.Join(p.Sync(), p.Close())
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}

func readLearningArchive(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("learning archive exceeds bounded record size")
	}
	return data, nil
}

type ArchiveResult struct {
	ArchivedVersions    int `json:"archived_versions"`
	ArchivedEvaluations int `json:"archived_evaluations"`
	ArchivedTransitions int `json:"archived_transitions"`
	RetainedVersions    int `json:"retained_versions"`
}

// Archive removes cold content from hot state, retaining the baseline, active
// version, immediate rollback target, and all current-epoch proposals. Evidence
// remains readable under .learning/NAME/archive; it never becomes a fresh
// untested candidate because Stage consults archived IDs as well.
func (s *Store) Archive(ctx context.Context, name string) (ArchiveResult, error) {
	var out ArchiveResult
	err := s.mutate(ctx, name, func(st *State) error {
		if st.Revision == 0 && len(st.Versions) == 0 {
			return os.ErrNotExist
		}
		var err error
		out, err = s.archiveState(st)
		return err
	})
	return out, err
}

func (s *Store) archiveState(st *State) (ArchiveResult, error) {
	var out ArchiveResult
	name := st.Name

	for _, t := range st.Transitions {
		if t.Kind != "promotion" || !controllerDigestValid(t.To) {
			continue
		}
		proof, _ := json.Marshal(t)
		if err := writeLearningArchive(filepath.Join(s.dir, name, "archive", "activations", t.To+".json"), proof); err != nil {
			// A version can be activated more than once. The first durable proof is
			// enough for host rollback, but never overwrite its original cause.
			if _, readErr := s.readActivationProof(name, t.To); readErr != nil {
				return out, err
			}
		}
	}
	keep := map[string]bool{st.ActiveVersion: true, st.BaselineVersion: true}
	if v, ok := st.Versions[st.ActiveVersion]; ok {
		keep[v.Parent] = true
	}
	for id, v := range st.Versions {
		if v.Parent == st.ActiveVersion && v.ParentEpoch == st.ActivationEpoch {
			keep[id] = true
		}
	}
	ids := make([]string, 0, len(st.Versions))
	for id := range st.Versions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if keep[id] {
			continue
		}
		data, _ := json.Marshal(st.Versions[id])
		if err := writeLearningArchive(s.versionArchivePath(name, id), data); err != nil {
			return out, err
		}
		delete(st.Versions, id)
		out.ArchivedVersions++
	}
	evaluations := st.Evaluations[:0]
	latestCurrent := map[string]string{}
	for _, e := range st.Evaluations {
		if e.BaselineVersion == st.ActiveVersion && e.BaselineEpoch == st.ActivationEpoch {
			latestCurrent[e.CandidateVersion] = e.ID
		}
	}
	for _, e := range st.Evaluations {
		if e.BaselineVersion == st.ActiveVersion && e.BaselineEpoch == st.ActivationEpoch && latestCurrent[e.CandidateVersion] == e.ID {
			evaluations = append(evaluations, e)
			continue
		}
		data, _ := json.Marshal(e)
		if err := writeLearningArchive(filepath.Join(s.dir, name, "archive", "evaluations", e.ID+".json"), data); err != nil {
			return out, err
		}
		out.ArchivedEvaluations++
	}
	st.Evaluations = evaluations
	// Keep a bounded transition tail; the full cold segment has a content hash.
	if len(st.Transitions) > 128 {
		old := st.Transitions[:len(st.Transitions)-128]
		data, _ := json.Marshal(old)
		if err := writeLearningArchive(filepath.Join(s.dir, name, "archive", "transitions", Digest(data)+".json"), data); err != nil {
			return out, err
		}
		out.ArchivedTransitions = len(old)
		st.Transitions = st.Transitions[len(st.Transitions)-128:]
	}
	out.RetainedVersions = len(st.Versions)
	return out, nil
}
func (s *Store) versionArchivePath(name, id string) string {
	return filepath.Join(s.dir, name, "archive", "versions", id+".json")
}

func (s *Store) readArchivedEvaluation(name, id string) (Evaluation, error) {
	var e Evaluation
	if !controllerDigestValid(id) {
		return e, errors.New("invalid archived evaluation ID")
	}
	data, err := readLearningArchive(filepath.Join(s.dir, name, "archive", "evaluations", id+".json"), maxStateBytes)
	if err != nil {
		return e, err
	}
	if json.Unmarshal(data, &e) != nil || e.ID != id || e.SkillName != name {
		return Evaluation{}, errors.New("invalid archived evaluation")
	}
	digest := e.ID
	e.ID = ""
	canonical, _ := json.Marshal(e)
	e.ID = digest
	if Digest(canonical) != digest {
		return Evaluation{}, errors.New("archived evaluation digest mismatch")
	}
	return e, nil
}

// Evaluation retrieves immutable evidence from either hot state or its cold
// archive. Promotion still verifies the host suite, environment and active CAS.
func (s *Store) Evaluation(name, id string) (Evaluation, error) {
	st, err := s.Read(name)
	if err != nil {
		return Evaluation{}, err
	}
	for _, e := range st.Evaluations {
		if e.ID == id {
			return e, nil
		}
	}
	return s.readArchivedEvaluation(name, id)
}
func (s *Store) readArchivedVersion(name, id string) (Version, error) {
	var v Version
	if !controllerDigestValid(id) {
		return v, errors.New("invalid archived version ID")
	}
	data, err := readLearningArchive(s.versionArchivePath(name, id), MaxContentBytes*6+4096)
	if err != nil {
		return v, err
	}
	if json.Unmarshal(data, &v) != nil || v.ID != id || len(v.Content) > MaxContentBytes || Digest([]byte(v.Content)) != id {
		return Version{}, errors.New("archived learning content digest mismatch")
	}
	return v, nil
}

func (s *Store) readActivationProof(name, id string) (Transition, error) {
	var t Transition
	if !controllerDigestValid(id) {
		return t, errors.New("invalid activation proof ID")
	}
	data, err := readLearningArchive(filepath.Join(s.dir, name, "archive", "activations", id+".json"), 8192)
	if err != nil {
		return t, err
	}
	if len(data) > 8192 || json.Unmarshal(data, &t) != nil || t.Kind != "promotion" || t.To != id || !controllerDigestValid(t.EvaluationID) {
		return Transition{}, errors.New("invalid archived activation proof")
	}
	return t, nil
}

type ControllerArchiveResult struct {
	Archived int `json:"archived"`
	Retained int `json:"retained"`
}

// ArchiveReceipts keeps unsettled reservations hot and preserves charged
// attempts for the current policy revision. Archival never refunds a query.
func (c *Controller) ArchiveReceipts(ctx context.Context) (ControllerArchiveResult, error) {
	unlock, err := c.locked(ctx)
	if err != nil {
		return ControllerArchiveResult{}, err
	}
	defer unlock()
	st, err := c.Read()
	if err != nil {
		return ControllerArchiveResult{}, err
	}
	out, err := c.archiveReceiptsState(&st, 0)
	if err != nil {
		return out, err
	}
	return out, c.save(st)
}
func (c *Controller) archiveReceiptsState(st *ControllerState, retain int) (ControllerArchiveResult, error) {
	out := ControllerArchiveResult{}
	remaining := len(st.Receipts)
	hot := make([]ControllerReceipt, 0, min(remaining, retain))
	if st.ArchivedBudget == nil {
		st.ArchivedBudget = map[string]ControllerArchivedBudget{}
	}
	for _, r := range st.Receipts {
		if r.Status == "reserved" || remaining <= retain {
			hot = append(hot, r)
			remaining--
			continue
		}
		data, _ := json.Marshal(r)
		if err := writeLearningArchive(filepath.Join(c.dir, "archive", "receipts", r.ID+".json"), data); err != nil {
			return out, err
		}
		if p, ok := st.Policies[r.PolicyKey]; ok && p.Revision == r.PolicyRevision {
			b := st.ArchivedBudget[r.PolicyKey]
			b.PolicyRevision = p.Revision
			b.Attempts++
			b.ChargedTokens = controllerAddTokens(b.ChargedTokens, r.ChargedTokens)
			st.ArchivedBudget[r.PolicyKey] = b
		}
		out.Archived++
		remaining--
		st.ArchivedReceipts++
	}
	st.Receipts = hot
	out.Retained = len(hot)
	return out, nil
}
func (c *Controller) readArchivedReceipt(id string) (ControllerReceipt, error) {
	var r ControllerReceipt
	if !controllerDigestValid(id) {
		return r, errors.New("invalid archived controller receipt ID")
	}
	data, err := readLearningArchive(filepath.Join(c.dir, "archive", "receipts", id+".json"), 16384)
	if err != nil {
		return r, err
	}
	if len(data) > 16384 || json.Unmarshal(data, &r) != nil || r.ID != id || id != controllerReceiptDigest(r.PolicyKey, r.PolicyRevision, r.ActiveEpoch, r.CandidateSHA256) || r.Status == "reserved" || r.PolicyRevision == 0 || r.ChargedTokens < 0 || !controllerDigestValid(r.PolicySHA256) {
		return ControllerReceipt{}, errors.New("invalid archived controller receipt")
	}
	return r, nil
}
