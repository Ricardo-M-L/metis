package builtin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestInvocationAuthorizerDoesNotExpirePendingApproval(t *testing.T) {
	authorizer := newInvocationAuthorizer[string]()
	ctx := tools.WithInvocationID(context.Background(), "slow-user-approval")
	authorizer.record(ctx, "prepared")

	// Simulate a batch ASK that waited far longer than the historical 2-minute
	// TTL before the user approved it. No wall-clock sleep keeps this deterministic.
	authorizer.mu.Lock()
	binding := authorizer.pending["slow-user-approval"]
	binding.created = time.Now().Add(-24 * time.Hour)
	authorizer.pending["slow-user-approval"] = binding
	authorizer.mu.Unlock()

	got, hasID, found := authorizer.consume(ctx)
	if !hasID || !found || got != "prepared" {
		t.Fatalf("consume after delayed approval = %q, %v, %v; want prepared, true, true", got, hasID, found)
	}
}

func TestInvocationAuthorizerCannotCrossInvocationIDs(t *testing.T) {
	authorizer := newInvocationAuthorizer[string]()
	denied := tools.WithInvocationID(context.Background(), "denied-call")
	authorizer.record(denied, "denied binding")

	got, hasID, found := authorizer.consume(tools.WithInvocationID(context.Background(), "later-call"))
	if !hasID || found || got != "" {
		t.Fatalf("later invocation consumed denied binding: %q, %v, %v", got, hasID, found)
	}
	if got, _, found := authorizer.consume(denied); !found || got != "denied binding" {
		t.Fatalf("exact invocation did not retain its binding: %q, found=%v", got, found)
	}
}

func TestInvocationAuthorizerCapsAbandonedBindings(t *testing.T) {
	authorizer := newInvocationAuthorizer[int]()
	for i := 0; i <= maxPendingInvocationBindings; i++ {
		authorizer.record(tools.WithInvocationID(context.Background(), fmt.Sprintf("call-%05d", i)), i)
	}
	authorizer.mu.Lock()
	if got := len(authorizer.pending); got != maxPendingInvocationBindings {
		t.Fatalf("pending bindings = %d, want cap %d", got, maxPendingInvocationBindings)
	}
	authorizer.mu.Unlock()

	if _, _, found := authorizer.consume(tools.WithInvocationID(context.Background(), "call-00000")); found {
		t.Fatal("oldest abandoned binding was not evicted")
	}
	if got, _, found := authorizer.consume(tools.WithInvocationID(context.Background(), fmt.Sprintf("call-%05d", maxPendingInvocationBindings))); !found || got != maxPendingInvocationBindings {
		t.Fatalf("newest binding was evicted: got %d, found=%v", got, found)
	}
}

func TestGrepApprovalKeyIsFixedLengthAndInputSensitive(t *testing.T) {
	base := map[string]any{
		"root": "/repo", "pattern": "needle", "glob": "*.go", "max": 20, "offset": 2,
	}
	key := grepApprovalKey(base, "/repo")
	if len(key) != sha256.Size*2 {
		t.Fatalf("key length = %d, want %d hex chars", len(key), sha256.Size*2)
	}
	copyInput := map[string]any{
		"offset": 2, "max": 20, "glob": "*.go", "pattern": "needle", "root": "/repo",
	}
	if got := grepApprovalKey(copyInput, "/repo"); got != key {
		t.Fatalf("equal input produced different key: %q != %q", got, key)
	}
	for field, value := range map[string]any{
		"root": "/other", "pattern": "other", "glob": "*.md", "max": 21, "offset": 3,
	} {
		changed := map[string]any{
			"root": "/repo", "pattern": "needle", "glob": "*.go", "max": 20, "offset": 2,
		}
		changed[field] = value
		effectiveRoot := "/repo"
		if field == "root" {
			effectiveRoot = "/other"
		}
		if got := grepApprovalKey(changed, effectiveRoot); got == key {
			t.Fatalf("changing %s did not change approval key", field)
		}
	}
	huge := map[string]any{"pattern": strings.Repeat("x", 1<<20), "glob": strings.Repeat("?", 1<<20)}
	if got := grepApprovalKey(huge, "/repo"); len(got) != sha256.Size*2 {
		t.Fatalf("huge input key length = %d", len(got))
	}
}
