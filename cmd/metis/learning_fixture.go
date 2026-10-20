package main

import (
	"errors"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

type workflowLearningFixtures struct {
	Path        string
	SHA256      string
	InputSHA256 string
	Fixtures    workflow.EvaluationFixtures
}

func loadWorkflowLearningFixtures(path string, suite learning.Suite) (*workflowLearningFixtures, error) {
	if path == "" {
		return nil, nil
	}
	data, err := readLearningBounded(path, workflow.MaxEvaluationFixtureBytes)
	if err != nil {
		return nil, err
	}
	f, err := decodeWorkflowLearningFixtures(data, suite)
	if err != nil {
		return nil, err
	}
	f.Path = path
	return f, nil
}

func decodeWorkflowLearningFixtures(data []byte, suite learning.Suite) (*workflowLearningFixtures, error) {
	fixtures, err := workflow.DecodeEvaluationFixtures(data)
	if err != nil {
		return nil, err
	}
	if len(fixtures.Cases) != len(suite.Cases) {
		return nil, errors.New("fixture suite must cover exactly the host evaluation case IDs")
	}
	for _, c := range suite.Cases {
		if _, ok := fixtures.Cases[c.ID]; !ok {
			return nil, fmt.Errorf("host fixture missing for case %s", c.ID)
		}
	}
	return &workflowLearningFixtures{SHA256: learning.EvaluationFixtureDigest(data), InputSHA256: learning.Digest(data), Fixtures: fixtures}, nil
}

func (f *workflowLearningFixtures) ValidateInputs() error {
	if f == nil {
		return nil
	}
	if f.Path == "" {
		return nil
	} // controller supplied immutable, digest-checked policy bytes
	data, err := readLearningBounded(f.Path, workflow.MaxEvaluationFixtureBytes)
	if err != nil {
		return err
	}
	if learning.Digest(data) != f.InputSHA256 {
		return errors.New("host fixture suite changed")
	}
	return nil
}

func workflowLearningFixtureProfileDigest(profile string, fixtures *workflowLearningFixtures) string {
	if fixtures == nil {
		return profile
	}
	return learning.EvaluationProfileDigest(profile, fixtures.SHA256)
}
