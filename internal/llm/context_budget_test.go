package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestContextBudgetPolicyCurrentNeverLooksUpAndRefreshIsCoherent(t *testing.T) {
	var calls atomic.Int32
	var saved atomic.Int64
	saved.Store(550000)
	policy := NewContextBudgetPolicy(ContextBudgetPolicyOptions{LegacyWindow: 100000, InitialWindow: 100000, InitialSource: ContextBudgetLegacyProvider,
		Lookup: func(context.Context) (int, bool, error) {
			calls.Add(1)
			return int(saved.Load()), true, nil
		},
	})
	for range 1000 {
		if window, source := policy.Current(); window != 100000 || source != ContextBudgetLegacyProvider {
			t.Fatalf("unrefreshed policy changed: %d %q", window, source)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("status reads perform budget I/O")
	}
	if err := policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if window, source := policy.Current(); window != 550000 || source != ContextBudgetPerModel {
		t.Fatalf("refresh did not atomically publish tuple: %d %q", window, source)
	}
	saved.Store(0)
	if err := policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if window, source := policy.Current(); window != 0 || source != ContextBudgetAutomatic {
		t.Fatalf("automatic record inherited legacy value: %d %q", window, source)
	}
}

func TestContextBudgetPolicyRejectsFailedOrCancelledRefresh(t *testing.T) {
	testErr := errors.New("store is corrupt")
	for _, failure := range []string{"lookup-error", "cancelled-during-lookup", "negative"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			policy := NewContextBudgetPolicy(ContextBudgetPolicyOptions{InitialWindow: 500000, InitialSource: ContextBudgetPerModel,
				Lookup: func(context.Context) (int, bool, error) {
					switch failure {
					case "lookup-error":
						return 0, false, testErr
					case "cancelled-during-lookup":
						cancel()
						return 0, true, nil
					default:
						return -1, true, nil
					}
				},
			})
			err := policy.Refresh(ctx)
			if err == nil || (failure != "cancelled-during-lookup" && !errors.Is(err, ErrContextBudgetPolicy)) {
				t.Fatalf("unsafe refresh did not fail closed: %v", err)
			}
			if window, source := policy.Current(); window != 500000 || source != ContextBudgetPerModel {
				t.Fatalf("failed refresh replaced safe policy: %d %q", window, source)
			}
		})
	}
}

func TestContextBudgetPolicyParallelRefreshAndReads(t *testing.T) {
	var next atomic.Int64
	policy := NewContextBudgetPolicy(ContextBudgetPolicyOptions{InitialWindow: 0, InitialSource: ContextBudgetAutomatic,
		Lookup: func(context.Context) (int, bool, error) { return int(next.Load()), true, nil },
	})
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 500 {
				window, source := policy.Current()
				if (window == 0 && source != ContextBudgetAutomatic) || (window > 0 && source != ContextBudgetPerModel) {
					t.Errorf("torn budget tuple: %d %q", window, source)
					return
				}
			}
		}()
	}
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := range 100 {
				next.Store(int64(200000 + i))
				if err := policy.Refresh(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
}

func TestContextBudgetPolicyMissingRecordRetainsCapturedLegacy(t *testing.T) {
	policy := NewContextBudgetPolicy(ContextBudgetPolicyOptions{LegacyWindow: 100000, InitialWindow: 500000, InitialSource: ContextBudgetPerModel,
		Lookup: func(context.Context) (int, bool, error) { return 0, false, nil },
	})
	if err := policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if window, source := policy.Current(); window != 100000 || source != ContextBudgetLegacyProvider {
		t.Fatalf("missing-record compatibility changed: %d %q", window, source)
	}
}

func TestContextBudgetPolicyOwnerDoesNotMixCapturedBudgetSource(t *testing.T) {
	owner := ContextBudgetOwner{ProviderID: "private-gateway", Model: "exact-model", RouteKey: "fixture-route-digest"}
	policy := NewContextBudgetPolicy(ContextBudgetPolicyOptions{Owner: owner, InitialWindow: 100000, InitialSource: ContextBudgetLegacyProvider,
		Lookup: func(context.Context) (int, bool, error) { return 0, true, nil },
	})
	// Model/status readers can finish resolving a prior selection while a new
	// request boundary adopts an automatic budget. Ownership stamping may not
	// reinterpret that already resolved window with the new policy's source.
	capturedWindow, capturedSource := policy.Current()
	if err := policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := policy.Apply(ApplyContextBudgetSource(ContextCapabilities{Window: capturedWindow}, capturedSource))
	if got.Window != 100000 || got.BudgetSource != ContextBudgetLegacyProvider || got.ProviderID != owner.ProviderID || got.Model != owner.Model || got.RouteKey != owner.RouteKey {
		t.Fatalf("ownership stamp mixed budget generations: %+v", got)
	}
}
