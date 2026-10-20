package tool

import (
	"context"
	"testing"
)

type operationContextExecutor struct{}

func (*operationContextExecutor) Execute(ctx context.Context, t Tool, input map[string]any, invoke func(context.Context) (*Result, error)) (*Result, error) {
	return invoke(ctx)
}

func TestOperationExecutorIsScopedAndSurvivesDetachedContext(t *testing.T) {
	executor := &operationContextExecutor{}
	ctx, cancel := context.WithCancel(WithOperationExecutor(context.Background(), executor))
	cancel()
	if OperationExecutorFromContext(context.WithoutCancel(ctx)) != executor {
		t.Fatal("detached child lost its durable operation boundary")
	}
	if OperationExecutorFromContext(context.Background()) != nil || OperationExecutorFromContext(nil) != nil {
		t.Fatal("durable boundary leaked into an unrelated context")
	}
	if OperationExecutorFromContext(WithOperationExecutor(ctx, nil)) != executor {
		t.Fatal("nil executor replaced an inherited boundary")
	}
}
