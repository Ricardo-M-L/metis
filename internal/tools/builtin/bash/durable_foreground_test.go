package bash

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type foregroundDurabilityMarker struct{}

func (foregroundDurabilityMarker) Execute(ctx context.Context, _ pubtool.Tool, _ map[string]any, invoke func(context.Context) (*pubtool.Result, error)) (*pubtool.Result, error) {
	return invoke(ctx)
}

func TestDurableBashWaitsForConfirmedCompletionInsteadOfReturningJobID(t *testing.T) {
	old := AutoBackgroundThreshold
	AutoBackgroundThreshold = 25 * time.Millisecond
	t.Cleanup(func() { AutoBackgroundThreshold = old })
	registry := jobs.NewRegistry(t.TempDir())
	defer registry.ResetAndWait(time.Second)
	tool := Bash{gate: permission.New(permission.ModeBypass), Jobs: registry}
	ctx := pubtool.WithOperationExecutor(context.Background(), foregroundDurabilityMarker{})
	for _, command := range []string{`printf 'start\n'; sleep 0.20; printf 'confirmed\n'`, `sleep 0.20; printf 'confirmed\n'`} {
		result, err := tool.Execute(ctx, map[string]any{"command": command, "timeout_ms": float64(2000)})
		if err != nil || result == nil || result.IsError || !strings.Contains(result.Output, "confirmed") {
			t.Fatalf("no terminal command receipt: result=%+v error=%v", result, err)
		}
		if result.Presentation["kind"] == "background_job" || len(registry.List()) != 0 {
			t.Fatal("durable operation returned an unconfirmed background job")
		}
	}
}
