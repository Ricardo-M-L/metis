package main

import (
	"context"
	"errors"

	"github.com/Ricardo-M-L/metis/internal/principal"
)

// resolveProjectPrincipalBinding is shared by normal and Desktop tool
// registries. A model chooses a saved identity, never supplies its own role,
// credential reference, permission policy or lifetime budget snapshot.
func resolveProjectPrincipalBinding(ctx context.Context, workspace, jobID string) (*principal.Binding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical, err := projectCoordinatorWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	job, err := persistentJobStore().Get(jobID)
	if err != nil {
		return nil, err
	}
	if canonical != job.Definition.Workspace {
		return nil, errors.New("persistent job workspace differs from project")
	}
	binding := job.Binding()
	if err := checkPersistentJobReady(&binding); err != nil {
		return nil, err
	}
	return &binding, nil
}
