package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestModelSwitchDropsEffortAbsentFromTargetModelSlots(t *testing.T) {
	s, loop := modelEffortSettingsFixture(t, true, nil)
	snapshot, err := config.ReadProviderSettings()
	if err != nil {
		t.Fatal(err)
	}
	var profile config.ProviderSettingsProvider
	for _, entry := range snapshot.Providers {
		if entry.ID == "private-route" {
			profile = entry
			break
		}
	}
	enabled := true
	profile.Models = append(profile.Models, config.ProviderModelSettings{ID: "private-high-only", SupportsReasoning: &enabled, ReasoningEfforts: map[string]string{"high": "high"}})
	if _, err := config.SaveProviderSettings(config.ProviderSettingsMutation{Action: "upsert", ExpectedRevision: snapshot.Revision, Provider: &profile}); err != nil {
		t.Fatal(err)
	}
	loop.SetEffort(llm.EffortMedium)
	if err := s.store.WriteHeaderFull(session.Header{ID: "private-effort-session", Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	s.buildProvider = func(provider, model string) (*rtpkg.ProviderBuild, error) {
		return &rtpkg.ProviderBuild{Provider: &activationTestProvider{name: provider, model: model}, Model: model}, nil
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/models", bytes.NewBufferString(`{"provider":"private-route","model":"private-high-only"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", rr.Code, rr.Body.String())
	}
	header, _, err := s.store.LoadHeader("private-effort-session")
	if err != nil || header.Model != "private-high-only" || header.Effort != "default" || loop.EffortValue() != llm.EffortDefault {
		t.Fatalf("target slot subset retained invalid medium: header=%+v loop=%q err=%v", header, loop.EffortValue(), err)
	}
}

func TestSessionRestoreDropsEffortRemovedFromSavedModelSlots(t *testing.T) {
	for _, alreadyActive := range []bool{false, true} {
		name := "session boundary"
		if alreadyActive {
			name = "active transcript refresh"
		}
		t.Run(name, func(t *testing.T) {
			s, loop := modelEffortSettingsFixture(t, true, map[string]string{"high": "high"})
			if err := s.store.WriteHeaderFull(session.Header{ID: "private-effort-session", Effort: "medium", System: "explicit prompt remains"}); err != nil {
				t.Fatal(err)
			}
			loop.SetEffort(llm.EffortHigh)
			if !alreadyActive {
				s.activeSessionID = "previous-effort-session"
			}
			header, history, err := s.store.Load("private-effort-session")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.activateSession(header.ID, header, history); err != nil {
				t.Fatal(err)
			}
			stored, _, err := s.store.LoadHeader(header.ID)
			if err != nil || stored.Effort != "default" || header.Effort != "default" || loop.EffortValue() != llm.EffortDefault {
				t.Fatalf("restore retained invalid medium: stored=%+v live=%q err=%v", stored, loop.EffortValue(), err)
			}
			if stored.System != "explicit prompt remains" || stored.Provider != "private-route" || stored.Model != "private-model-9287" {
				t.Fatalf("effort repair changed session route/prompt: %+v", stored)
			}
		})
	}
}

type effortHeaderInspectRunner struct {
	store      *session.Store
	seenEffort string
}

func (*effortHeaderInspectRunner) Eligible(*session.Header) bool { return true }
func (runner *effortHeaderInspectRunner) Run(_ context.Context, request IsolatedTurnRequest) (IsolatedTurnResult, error) {
	header, _, err := runner.store.LoadHeader(request.SessionID)
	if err != nil {
		return IsolatedTurnResult{}, err
	}
	runner.seenEffort = header.Effort
	return IsolatedTurnResult{Text: "completed"}, nil
}

func TestIsolatedTurnRepairsRemovedEffortSlotBeforeWorkerLoadsHeader(t *testing.T) {
	s, loop := modelEffortSettingsFixture(t, true, map[string]string{"high": "high"})
	if err := s.store.WriteHeaderFull(session.Header{ID: "private-effort-session", Effort: "medium", WorkDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	loop.SetEffort(llm.EffortHigh)
	runner := &effortHeaderInspectRunner{store: s.store}
	s.isolatedRunner = runner
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"sessionId":"private-effort-session","input":"inspect saved effort"}`)))
	if rr.Code != http.StatusOK || runner.seenEffort != "default" {
		t.Fatalf("worker received removed effort slot: HTTP=%d effort=%q %s", rr.Code, runner.seenEffort, rr.Body.String())
	}
	if loop.EffortValue() != llm.EffortHigh {
		t.Fatal("isolated header repair changed the parent Loop")
	}
}

func TestIsolatedTurnPreservesExplicitAllowedHighEffortForWorker(t *testing.T) {
	s, loop := modelEffortSettingsFixture(t, true, map[string]string{"high": "high"})
	if err := s.store.WriteHeaderFull(session.Header{ID: "private-effort-session", WorkDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	runner := &effortHeaderInspectRunner{store: s.store}
	s.isolatedRunner = runner
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/activate", bytes.NewBufferString(`{"id":"private-effort-session"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("activate effort owner: HTTP=%d %s", rr.Code, rr.Body.String())
	}
	selected := effortSettingsHTTP(t, s, http.MethodPost, `{"effort":"high"}`, http.StatusOK)
	if selected["applies"] != "next turn" || loop.EffortValue() != llm.EffortHigh {
		t.Fatalf("effort API did not select high: %#v", selected)
	}
	before, _, err := s.store.LoadHeader("private-effort-session")
	if err != nil || before.Effort != "high" {
		t.Fatalf("API did not durably save high: header=%+v err=%v", before, err)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/turns", bytes.NewBufferString(`{"sessionId":"private-effort-session","input":"inspect explicit high"}`)))
	if rr.Code != http.StatusOK || runner.seenEffort != "high" {
		t.Fatalf("admission removed allowed high: HTTP=%d workerEffort=%q %s", rr.Code, runner.seenEffort, rr.Body.String())
	}
	after, _, err := s.store.LoadHeader("private-effort-session")
	if err != nil || after.Effort != "high" || loop.EffortValue() != llm.EffortHigh {
		t.Fatalf("completed parent admission removed high: header=%+v loop=%q err=%v", after, loop.EffortValue(), err)
	}
}
