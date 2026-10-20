package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/learning"
)

func (s *Store) Dir() string                    { return s.dir }
func (s *Store) LearningStore() *learning.Store { return learning.NewStore(s.dir) }

// ParseManifest rejects unknown fields, empty commands and mismatched names.
// Its original bytes, rather than a later reserialization, identify a version.
func ParseManifest(name string, content []byte) (Workflow, error) {
	if err := learning.ValidateName(name); err != nil {
		return Workflow{}, err
	}
	if len(content) == 0 || len(content) > learning.MaxContentBytes {
		return Workflow{}, errors.New("workflow manifest must contain 1..262144 bytes")
	}
	var wf Workflow
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wf); err != nil {
		return wf, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return wf, errors.New("workflow manifest must contain one JSON object")
	}
	if wf.Name != name {
		return wf, errors.New("workflow manifest name differs from selected name")
	}
	if len(wf.Steps) == 0 || len(wf.Steps) > 64 {
		return wf, errors.New("workflow requires 1..64 steps")
	}
	seen := map[string]bool{}
	for _, step := range wf.Steps {
		if strings.TrimSpace(step.Name) == "" || seen[step.Name] || len(step.Name) > 128 || strings.TrimSpace(step.Command) == "" || len(step.Command) > 8192 {
			return wf, errors.New("workflow requires unique step names and bounded nonempty commands")
		}
		seen[step.Name] = true
	}
	wf.VersionSHA256 = learning.Digest(content)
	return wf, nil
}

func (s *Store) StageManifest(ctx context.Context, name string, content []byte, source string) (learning.Version, error) {
	if _, err := ParseManifest(name, content); err != nil {
		return learning.Version{}, err
	}
	baseline, err := os.ReadFile(s.path(name))
	if err != nil && !os.IsNotExist(err) {
		return learning.Version{}, err
	}
	if len(baseline) > 0 {
		if _, err := ParseManifest(name, baseline); err != nil {
			return learning.Version{}, fmt.Errorf("existing workflow baseline: %w", err)
		}
	}
	return s.LearningStore().Stage(ctx, name, baseline, content, source)
}

// Propose is the model-facing save path. It never changes the active recipe.
func (s *Store) Propose(ctx context.Context, wf Workflow) (learning.Version, error) {
	data, err := json.Marshal(wf)
	if err != nil {
		return learning.Version{}, err
	}
	return s.StageManifest(ctx, wf.Name, data, "model-workflow-proposal")
}
