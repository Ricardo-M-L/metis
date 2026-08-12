package bash

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestBashProcessGuardAppliesBeforePermissionAndExecution(t *testing.T) {
	t.Parallel()
	tool := New(permission.New(permission.ModeBypassPermissions), config.ToolBashSettings{Shell: "/bin/sh"})
	in := map[string]any{"command": `ps aux | grep metis | awk '{print $2}' | xargs kill -9`}

	got, source := tool.CanUse(context.Background(), in)
	if got != tools.PermissionDeny || !strings.Contains(source, "BashKill(job_id)") {
		t.Fatalf("CanUse = %v (%q), want hard deny directing BashKill(job_id)", got, source)
	}

	res, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || !res.IsError || !strings.Contains(res.Output, "BashKill(job_id)") {
		t.Fatalf("Execute result = %+v, want blocked result directing BashKill(job_id)", res)
	}
}
