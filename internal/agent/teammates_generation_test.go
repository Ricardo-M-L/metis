package agent

import "testing"

func registerRosterGeneration(t *testing.T, roster *Roster, name, id string) *Teammate {
	t.Helper()
	worker := &Teammate{Name: name, AgentID: id, Status: StatusRunning}
	if err := roster.Register(worker); err != nil {
		t.Fatal(err)
	}
	return worker
}

func finishRosterGeneration(roster *Roster, worker *Teammate, result string) {
	worker.AppendText(result)
	worker.Finish(StatusCompleted, result, nil, "end_turn")
	roster.UnregisterTeammate(worker)
}

func TestRosterGenerationLatestFinishedLookup(t *testing.T) {
	roster := NewRoster(0)
	first := registerRosterGeneration(t, roster, "worker", "agt-stable")
	finishRosterGeneration(roster, first, "first-generation-output")
	second := registerRosterGeneration(t, roster, "worker", "agt-stable")
	finishRosterGeneration(roster, second, "second-generation-output")

	byID, found := roster.LookupByAgentID("agt-stable")
	if !found || byID != second {
		t.Fatalf("ID lookup returned stale generation: got=%p first=%p latest=%p found=%v", byID, first, second, found)
	}
	byName, _, found := roster.LookupRecentlyFinished("worker")
	if !found || byName != second {
		t.Fatalf("finished name lookup returned stale generation: got=%p latest=%p found=%v", byName, second, found)
	}
	if got := byID.Snapshot(); got.Result != "second-generation-output" || got.Output != "second-generation-output" {
		t.Fatalf("latest generation result unavailable: %#v", got)
	}
	// Finished discovery must not widen the live lookup used by peer messaging.
	if _, found := roster.Lookup("worker"); found {
		t.Fatal("finished teammate became live-addressable")
	}
}

func TestRosterGenerationListPrefersLiveThenLatestFinished(t *testing.T) {
	roster := NewRoster(0)
	first := registerRosterGeneration(t, roster, "worker", "agt-stable")
	finishRosterGeneration(roster, first, "first")
	second := registerRosterGeneration(t, roster, "worker", "agt-stable")

	if byID, found := roster.LookupByAgentID("agt-stable"); !found || byID != second {
		t.Fatalf("live generation not preferred: got=%p latest=%p found=%v", byID, second, found)
	}
	if listed := roster.List(); len(listed) != 1 || listed[0] != second {
		t.Fatalf("list contains stale generation beside live replacement: %#v", listed)
	}
	finishRosterGeneration(roster, second, "second")
	if listed := roster.List(); len(listed) != 1 || listed[0] != second {
		t.Fatalf("list did not retain only latest completed generation: %#v", listed)
	}
}

func TestRosterGenerationListPreservesDifferentIDsWithSameName(t *testing.T) {
	roster := NewRoster(0)
	first := registerRosterGeneration(t, roster, "worker", "agt-first")
	finishRosterGeneration(roster, first, "first independent child")
	second := registerRosterGeneration(t, roster, "worker", "agt-second")
	finishRosterGeneration(roster, second, "second independent child")

	listed := roster.List()
	if len(listed) != 2 {
		t.Fatalf("distinct child IDs collapsed by display name: %#v", listed)
	}
	seen := map[*Teammate]bool{}
	for _, worker := range listed {
		seen[worker] = true
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("list lost a distinct child: %#v", listed)
	}
	for id, want := range map[string]*Teammate{"agt-first": first, "agt-second": second} {
		if got, found := roster.LookupByAgentID(id); !found || got != want {
			t.Fatalf("independent child %s unavailable: got=%p want=%p found=%v", id, got, want, found)
		}
	}
	if got, _, found := roster.LookupRecentlyFinished("worker"); !found || got != second {
		t.Fatalf("same-name lookup did not choose latest child: got=%p want=%p found=%v", got, second, found)
	}
}

func TestRosterGenerationLateUnregisterCannotRemoveReplacement(t *testing.T) {
	roster := NewRoster(0)
	first := registerRosterGeneration(t, roster, "worker", "agt-stable")
	finishRosterGeneration(roster, first, "first")
	second := registerRosterGeneration(t, roster, "worker", "agt-stable")
	cancelled := false
	second.SetCancel(func() { cancelled = true })

	// A delayed old finalizer must only close its own lifecycle handle.
	roster.UnregisterTeammate(first)
	if got, found := roster.Lookup("worker"); !found || got != second || roster.Count() != 1 {
		t.Fatalf("old finalizer removed current generation: got=%p latest=%p found=%v count=%d", got, second, found, roster.Count())
	}
	select {
	case <-second.done:
		t.Fatal("old finalizer closed new generation's done channel")
	default:
	}
	if cancelled {
		t.Fatal("old finalizer canceled new generation")
	}
	if got := second.Snapshot(); got.Status != StatusRunning {
		t.Fatalf("old finalizer changed current generation state: %#v", got)
	}
	if listed := roster.List(); len(listed) != 1 || listed[0] != second {
		t.Fatalf("late finalizer reintroduced stale generation into list: %#v", listed)
	}
	finishRosterGeneration(roster, second, "second")
}
