// Package learning keeps proposed skill changes separate from the active
// catalog. A single atomic state record is the activation authority; model
// authors can propose content, but only host-evaluated versions can activate.
package learning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const MaxContentBytes = 256 * 1024
const maxStateBytes = 32 * 1024 * 1024

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)

type Version struct {
	ID          string    `json:"id"`
	Parent      string    `json:"parent,omitempty"`
	ParentEpoch uint64    `json:"parent_epoch"`
	Content     string    `json:"content"`
	Source      string    `json:"source"`
	CreatedAt   time.Time `json:"created_at"`
}

type State struct {
	Schema          int                `json:"schema"`
	Name            string             `json:"name"`
	Revision        uint64             `json:"revision"`
	ActivationEpoch uint64             `json:"activation_epoch"`
	ActiveVersion   string             `json:"active_version,omitempty"`
	BaselineVersion string             `json:"baseline_version,omitempty"`
	Versions        map[string]Version `json:"versions"`
	Evaluations     []Evaluation       `json:"evaluations,omitempty"`
	Transitions     []Transition       `json:"transitions,omitempty"`
	Quality         *QualityState      `json:"quality,omitempty"`
	Suspended       bool               `json:"suspended,omitempty"`
}

type Transition struct {
	Kind         string    `json:"kind"`
	From         string    `json:"from,omitempty"`
	To           string    `json:"to,omitempty"`
	EvaluationID string    `json:"evaluation_id,omitempty"`
	FeedbackID   string    `json:"feedback_id,omitempty"`
	Note         string    `json:"note,omitempty"`
	At           time.Time `json:"at"`
}

type Store struct{ dir string }

func NewStore(skillsDir string) *Store { return &Store{dir: filepath.Join(skillsDir, ".learning")} }
func (s *Store) Dir() string           { return s.dir }
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid learning skill name %q", name)
	}
	return nil
}
func Digest(content []byte) string       { sum := sha256.Sum256(content); return hex.EncodeToString(sum[:]) }
func (s *Store) path(name string) string { return filepath.Join(s.dir, name, "state.json") }

// Read never creates or changes a state. Every persisted version is digest
// checked so a content edit cannot inherit an earlier version's evaluation.
func (s *Store) Read(name string) (State, error) {
	if err := ValidateName(name); err != nil {
		return State{}, err
	}
	f, err := os.Open(s.path(name))
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return State{}, err
	}
	if len(data) > maxStateBytes {
		return State{}, errors.New("learning state exceeds storage limit")
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	if (state.Schema != 1 && state.Schema != 2) || state.Name != name || state.Versions == nil {
		return State{}, errors.New("invalid learning state")
	}
	for id, v := range state.Versions {
		if id != v.ID || Digest([]byte(v.Content)) != id || len(v.Content) > MaxContentBytes {
			return State{}, errors.New("learning content digest mismatch")
		}
	}
	for _, id := range []string{state.ActiveVersion, state.BaselineVersion} {
		if id != "" {
			if _, ok := state.Versions[id]; !ok {
				return State{}, errors.New("learning state references absent version")
			}
		}
	}
	if err := validateQualityState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *Store) List() ([]State, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var states []State
	for _, e := range entries {
		if !e.IsDir() || ValidateName(e.Name()) != nil {
			continue
		}
		st, err := s.Read(e.Name())
		if err != nil {
			return nil, err
		}
		states = append(states, st)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states, nil
}

// Stage freezes the original active manifest on the first proposal. A new
// skill has no active version until its candidate passes a host suite.
func (s *Store) Stage(ctx context.Context, name string, baseline, candidate []byte, source string) (Version, error) {
	if err := ValidateName(name); err != nil {
		return Version{}, err
	}
	if len(candidate) == 0 || len(candidate) > MaxContentBytes || len(baseline) > MaxContentBytes {
		return Version{}, errors.New("learning manifest must contain 1..262144 bytes")
	}
	var result Version
	err := s.mutate(ctx, name, func(st *State) error {
		if st.Revision == 0 && len(baseline) > 0 {
			id := Digest(baseline)
			st.BaselineVersion = id
			st.ActiveVersion = id
			st.ActivationEpoch = 1
			st.Versions[id] = Version{ID: id, Content: string(baseline), Source: "baseline", CreatedAt: time.Now().UTC()}
		}
		id := Digest(candidate)
		if id == st.ActiveVersion {
			return errors.New("candidate is identical to the active version")
		}
		if v, ok := st.Versions[id]; ok {
			if v.Parent != st.ActiveVersion || v.ParentEpoch != st.ActivationEpoch {
				return errors.New("candidate belongs to an earlier active version; stage new content")
			}
			result = v
			return nil
		}
		if v, err := s.readArchivedVersion(name, id); err == nil {
			if v.Parent != st.ActiveVersion || v.ParentEpoch != st.ActivationEpoch {
				return errors.New("candidate belongs to an earlier archived activation; stage new content")
			}
			st.Versions[id] = v
			result = v
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if len(st.Versions) >= 128 {
			if _, err := s.archiveState(st); err != nil {
				return err
			}
			if len(st.Versions) >= 128 {
				return errors.New("learning has 128 current proposals; evaluate or reconcile pending candidates before proposing more")
			}
		}
		result = Version{ID: id, Parent: st.ActiveVersion, ParentEpoch: st.ActivationEpoch, Content: string(candidate), Source: source, CreatedAt: time.Now().UTC()}
		st.Versions[id] = result
		return nil
	})
	return result, err
}

func (s *Store) mutate(ctx context.Context, name string, fn func(*State) error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dir := filepath.Join(s.dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(ctx, filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.Read(name)
	if os.IsNotExist(err) {
		st = State{Schema: 2, Name: name, Versions: map[string]Version{}}
	} else if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(st.Evaluations) >= 240 || len(st.Transitions) > 256 {
		if _, err := s.archiveState(&st); err != nil {
			return err
		}
	}
	if err := fn(&st); err != nil {
		return err
	}
	if len(st.Versions) > 128 {
		if _, err := s.archiveState(&st); err != nil {
			return err
		}
		if len(st.Versions) > 128 {
			return errors.New("too many current learning proposals; reconcile pending candidates")
		}
	}
	// Schema 1 is readable for migration, but old CLIs must reject any state
	// carrying quality gates or archived evidence rather than dropping them.
	st.Schema = 2
	st.Revision++
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return errors.New("learning state exceeds storage limit")
	}
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(temp, s.path(name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) Promote(ctx context.Context, name, version, evaluationID string) error {
	return s.mutate(ctx, name, func(st *State) error {
		if st.Suspended || (st.Quality != nil && st.Quality.Paused) {
			return errors.New("learning quality guard paused; host review and resume required")
		}
		v, ok := st.Versions[version]
		if !ok {
			return errors.New("candidate version absent")
		}
		if v.Parent != st.ActiveVersion || v.ParentEpoch != st.ActivationEpoch {
			return errors.New("active version changed since candidate was staged")
		}
		var accepted *Evaluation
		for i := range st.Evaluations {
			e := &st.Evaluations[i]
			if e.ID == evaluationID {
				accepted = e
				break
			}
		}
		if accepted == nil {
			if archived, err := s.readArchivedEvaluation(name, evaluationID); err == nil {
				accepted = &archived
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if accepted == nil || !accepted.Passed || accepted.CandidateVersion != version || accepted.BaselineVersion != st.ActiveVersion || accepted.BaselineEpoch != st.ActivationEpoch {
			return errors.New("promotion requires a passing evaluation for the current baseline and exact candidate")
		}
		if err := accepted.Verify(); err != nil {
			return err
		}
		st.Transitions = append(st.Transitions, Transition{Kind: "promotion", From: st.ActiveVersion, To: version, EvaluationID: evaluationID, At: time.Now().UTC()})
		st.ActiveVersion = version
		st.ActivationEpoch++
		return nil
	})
}

// Rollback can restore the frozen baseline or a previously activated version,
// never a merely staged or failing candidate.
func (s *Store) Rollback(ctx context.Context, name, version string) error {
	return s.mutate(ctx, name, func(st *State) error {
		if _, ok := st.Versions[version]; !ok {
			v, err := s.readArchivedVersion(name, version)
			if err != nil {
				return errors.New("rollback version absent")
			}
			st.Versions[version] = v
		}
		allowed := version == st.BaselineVersion
		for _, t := range st.Transitions {
			if t.Kind == "promotion" && t.To == version {
				allowed = true
			}
		}
		if !allowed {
			_, err := s.readActivationProof(name, version)
			allowed = err == nil
		}
		if !allowed {
			return errors.New("rollback target was never active")
		}
		if st.ActiveVersion == version {
			return nil
		}
		st.Transitions = append(st.Transitions, Transition{Kind: "rollback", From: st.ActiveVersion, To: version, At: time.Now().UTC()})
		st.ActiveVersion = version
		st.ActivationEpoch++
		return nil
	})
}
