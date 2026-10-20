// Package projectcoord provides METIS's durable, project-scoped work graph.
//
// Session Task* entries are useful while one conversation is open, but a
// project coordinator needs state that survives a session switch, a Desktop
// restart, and independent worker processes.  This package owns that state.
// It deliberately has no LLM dependency: the CLI, the desktop backend, and
// LLM-facing tools can all drive the same graph without creating a runtime
// import cycle.
package projectcoord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/execution"
	"github.com/Ricardo-M-L/metis/internal/principal"
)

const (
	stateVersion          = 5 // older writers must refuse archived operation references they cannot preserve
	handoffStateVersion   = 4
	principalStateVersion = 3
	ledgerStateVersion    = 2
	legacyStateVersion    = 1
	defaultLease          = 15 * time.Minute
	defaultMaxAttempts    = 2 // one original attempt plus one constrained recovery attempt
	maxEvents             = 200
	maxPersistedOutput    = 512 << 10
	maxDependencyOutput   = 24 << 10
	lockWait              = 3 * time.Second
)

var (
	// ErrRunNotFound is returned when an ID does not belong to the requested
	// workspace.  Keeping workspace in the lookup prevents a project run from
	// accidentally leaking into another project with the same METIS home.
	ErrRunNotFound = errors.New("project coordinator run not found")
	// ErrNoReadyWork means the graph has no claimable node.  It is distinct
	// from a failed storage operation so workers can go idle cleanly.
	ErrNoReadyWork = errors.New("project coordinator has no ready work")
)

// Phase is a broad project milestone.  Custom is available for a project
// whose workflow does not fit the default four-stage scaffold.
type Phase string

const (
	PhaseResearch       Phase = "research"
	PhaseSynthesis      Phase = "synthesis"
	PhaseImplementation Phase = "implementation"
	PhaseVerification   Phase = "verification"
	PhaseCustom         Phase = "custom"
)

// RunStatus describes the aggregate state of a project run.
type RunStatus string

const (
	RunActive    RunStatus = "active"
	RunBlocked   RunStatus = "blocked"
	RunCompleted RunStatus = "completed"
	RunCancelled RunStatus = "cancelled"
)

// ItemStatus is the lifecycle for one durable work item.
type ItemStatus string

const (
	ItemPending   ItemStatus = "pending"
	ItemReady     ItemStatus = "ready"
	ItemRunning   ItemStatus = "running"
	ItemBlocked   ItemStatus = "blocked"
	ItemCompleted ItemStatus = "completed"
	ItemFailed    ItemStatus = "failed"
	ItemCancelled ItemStatus = "cancelled"
)

const (
	// FailureLeaseExpired can recover when no unsettled physical provider
	// request or writing operation is left by the crashed worker.
	FailureLeaseExpired = "worker_lease_expired"
	// FailureProviderUsageUnresolved holds a work item until its earlier
	// physical request settles and the coordinator explicitly recovers it.
	FailureProviderUsageUnresolved = "provider_usage_unresolved"
	// FailureWorkerInterrupted marks an unfinished claim during a service stop.
	// It may be retried within the existing attempt budget.
	FailureWorkerInterrupted = "worker_interrupted"
	// FailureProviderStreamInterrupted is an explicit missing-completion marker
	// from a provider stream, eligible for the same bounded task retry policy.
	FailureProviderStreamInterrupted = "provider_stream_interrupted"
	// FailureWorkerExecution is the durable record used when a worker process
	// itself exits with an error.  It deliberately requires explicit recovery.
	FailureWorkerExecution = "worker_execution_failed"
	// FailureWorkerBlocked is recorded when a worker explicitly says it cannot
	// continue.  It is intentionally not retried automatically.
	FailureWorkerBlocked = "worker_reported_blocked"
)

// Failure records why a project work item did not finish.  It is separate
// from an LLM transcript so a new coordinator turn can make a deterministic
// decision without depending on an old context window.
type Failure struct {
	Code            string    `json:"code"`
	Summary         string    `json:"summary"`
	SuggestedAction string    `json:"suggested_action,omitempty"`
	Recoverable     bool      `json:"recoverable"`
	AutoRecovered   bool      `json:"auto_recovered"`
	ObservedAt      time.Time `json:"observed_at"`
}

// Lease identifies an active worker claim.  Claims expire so a killed CLI or
// Desktop process cannot leave a project permanently stuck in "running".
type Lease struct {
	Token     string    `json:"token"`
	Worker    string    `json:"worker"`
	ClaimedAt time.Time `json:"claimed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WorkItem is one node in the project graph.
type WorkItem struct {
	ID          string     `json:"id"`
	Phase       Phase      `json:"phase"`
	Subject     string     `json:"subject"`
	Prompt      string     `json:"prompt"`
	DependsOn   []string   `json:"depends_on,omitempty"`
	Status      ItemStatus `json:"status"`
	Owner       string     `json:"owner,omitempty"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	// ForcedRecoveries is deliberately capped at one. It permits a
	// coordinator to retry once after the normal attempt budget only after a
	// person/model has reviewed and changed the underlying condition; it must
	// never create an unbounded retry loop.
	ForcedRecoveries     int                `json:"forced_recoveries,omitempty"`
	LastRecoveryRevision uint64             `json:"last_recovery_revision,omitempty"`
	Lease                *Lease             `json:"lease,omitempty"`
	Output               string             `json:"output,omitempty"`
	Failure              *Failure           `json:"failure,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	UpdatedAt            time.Time          `json:"updated_at"`
	CompletedAt          time.Time          `json:"completed_at,omitempty"`
	AgentProfile         string             `json:"agent_profile,omitempty"`
	PrincipalBinding     *principal.Binding `json:"principal_binding,omitempty"`
	Handoff              *HandoffBinding    `json:"handoff,omitempty"`
	DeadlineAt           time.Time          `json:"deadline_at,omitempty"`
	MaxBudgetUSD         float64            `json:"max_budget_usd,omitempty"`
	SpentUSD             float64            `json:"spent_usd,omitempty"`
	Deliverables         []string           `json:"deliverables,omitempty"`
	Artifacts            []Artifact         `json:"artifacts,omitempty"`
	Usage                []AttemptUsage     `json:"usage,omitempty"`
	Acceptance           *Acceptance        `json:"acceptance,omitempty"`
	EventSource          string             `json:"event_source,omitempty"`
	EventID              string             `json:"event_id,omitempty"`
}

// Event is a bounded audit trail.  The full worker result remains on the
// work item; events make scheduling and recovery choices understandable at a
// glance in both the CLI and Desktop.
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	ItemID string    `json:"item_id,omitempty"`
	Detail string    `json:"detail"`
	Worker string    `json:"worker,omitempty"`
}

// Run is a durable project-level work graph.
type Run struct {
	Version          int                  `json:"version"`
	Revision         uint64               `json:"revision"`
	ID               string               `json:"id"`
	Workspace        string               `json:"workspace"`
	Goal             string               `json:"goal"`
	Status           RunStatus            `json:"status"`
	Environment      execution.Profile    `json:"environment"`
	Items            []WorkItem           `json:"items"`
	Events           []Event              `json:"events,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
	CompletedAt      time.Time            `json:"completed_at,omitempty"`
	DeadlineAt       time.Time            `json:"deadline_at,omitempty"`
	MaxBudgetUSD     float64              `json:"max_budget_usd,omitempty"`
	SpentUSD         float64              `json:"spent_usd,omitempty"`
	CheckCommands    []string             `json:"check_commands,omitempty"`
	Operations       []OperationRecord    `json:"operations,omitempty"`
	UsageRequests    []UsageRequestRecord `json:"usage_requests,omitempty"`
	OperationArchive *OperationArchive    `json:"operation_archive,omitempty"`
	archiveLookup    func(*OperationArchive, string) (*OperationRecord, error)
}

// CreateInput describes a new project run.  If Items is empty METIS creates
// a four-stage research → synthesis → implementation → verification scaffold.
type CreateInput struct {
	Workspace     string
	Goal          string
	Items         []WorkItemInput
	DeadlineAt    time.Time
	MaxBudgetUSD  float64
	CheckCommands []string
}

// WorkItemInput is the write-only input shape used when a coordinator expands
// a project plan.  ID is optional for AddWorkItem and mandatory only when a
// caller wants another item to depend on a stable, human-chosen identifier.
type WorkItemInput struct {
	ID               string
	Phase            Phase
	Subject          string
	Prompt           string
	DependsOn        []string
	MaxAttempts      int
	AgentProfile     string
	PrincipalBinding *principal.Binding
	Handoff          *HandoffBinding
	DeadlineAt       time.Time
	MaxBudgetUSD     float64
	Deliverables     []string
}

// ClaimOptions controls an atomic worker claim.
type ClaimOptions struct {
	Worker string
	Lease  time.Duration
}

// Store persists project runs underneath one METIS home.  The package uses a
// short-lived filesystem lock in addition to its process-local mutex because
// independent `metis coordinator run` workers may claim work concurrently.
type Store struct {
	root            string
	now             func() time.Time
	executionMemory execution.Memory
	probeContext    context.Context
	mu              sync.Mutex
	lockWait        time.Duration
	principalCheck  func(*principal.Binding) error
}

// Option configures a Store.
type Option func(*Store)

// WithPrincipalCheck gates claims before changing attempt state. The callback
// must only inspect the principal store, never re-enter this work graph.
func WithPrincipalCheck(check func(*principal.Binding) error) Option {
	return func(s *Store) { s.principalCheck = check }
}

// WithExecutionMemory allows coordinator prompts and status output to include
// the same durable environment rule that the Agent tool uses for recovery.
func WithExecutionMemory(memory execution.Memory) Option {
	return func(s *Store) { s.executionMemory = memory }
}

// WithProbeContext binds only environment discovery to its owner's lifetime.
// Independent CLI workers use their own default Store and can still record
// usage or an interrupted claim after the parent service has been cancelled.
func WithProbeContext(ctx context.Context) Option {
	return func(s *Store) {
		if ctx != nil {
			s.probeContext = ctx
		}
	}
}

// withClock is intentionally package-private; tests need deterministic lease
// expiry while production should always use the real UTC clock.
func withClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// NewStore returns a lazy store.  It does not create directories until the
// first project run is written.
func NewStore(root string, options ...Option) *Store {
	s := &Store{
		root:         strings.TrimSpace(root),
		now:          func() time.Time { return time.Now().UTC() },
		lockWait:     lockWait,
		probeContext: context.Background(),
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// Root returns the configured persistence root, primarily for diagnostics.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Create initializes a new project run and its first ready work item.
func (s *Store) Create(input CreateInput) (Run, error) {
	if err := validateContract(input.MaxBudgetUSD, input.CheckCommands, nil); err != nil {
		return Run{}, err
	}
	if s == nil {
		return Run{}, errors.New("project coordinator store is unavailable")
	}
	workspace, err := execution.CanonicalWorkspace(input.Workspace)
	if err != nil {
		return Run{}, err
	}
	goal := strings.TrimSpace(input.Goal)
	if goal == "" {
		return Run{}, errors.New("project goal is required")
	}
	items := input.Items
	if len(items) == 0 {
		items = DefaultPlan(goal)
	}
	materialized, err := materializeItems(items, s.now())
	if err != nil {
		return Run{}, err
	}
	if err := validateGraph(materialized); err != nil {
		return Run{}, err
	}
	for _, item := range materialized {
		if err := principal.ValidateBinding(item.PrincipalBinding, workspace); err != nil {
			return Run{}, err
		}
	}

	environment, err := s.environmentFor(workspace)
	if err != nil {
		return Run{}, err
	}
	now := s.now()
	run := Run{
		Version:       stateVersion,
		Revision:      1,
		ID:            newID("project"),
		Workspace:     workspace,
		Goal:          goal,
		Status:        RunActive,
		Environment:   environment,
		Items:         materialized,
		CreatedAt:     now,
		UpdatedAt:     now,
		DeadlineAt:    input.DeadlineAt,
		MaxBudgetUSD:  input.MaxBudgetUSD,
		CheckCommands: append([]string(nil), input.CheckCommands...),
	}
	run.reconcile(now)
	run.addEvent(now, "run_created", "", "Created durable project work graph", "")

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeRun(workspace, run, true); err != nil {
		return Run{}, err
	}
	return cloneRun(run), nil
}

// DefaultPlan produces a conservative sequential scaffold.  A coordinator can
// add parallel research or implementation items with AddWorkItem after it has
// inspected the project; METIS does not pretend to know a task decomposition
// before it has any evidence.
func DefaultPlan(_ string) []WorkItemInput {
	return []WorkItemInput{
		{
			ID:      "research",
			Phase:   PhaseResearch,
			Subject: "Inspect the project and execution environment",
			Prompt:  "Inspect the repository, available tools, workspace constraints, and relevant code. Report concrete findings, risks, and a proposed implementation path.",
		},
		{
			ID:        "synthesis",
			Phase:     PhaseSynthesis,
			Subject:   "Turn findings into an implementation plan",
			Prompt:    "Read the research evidence, choose a minimal safe plan, identify any work that can proceed in parallel, and state verification criteria.",
			DependsOn: []string{"research"},
		},
		{
			ID:        "implementation",
			Phase:     PhaseImplementation,
			Subject:   "Implement the approved project change",
			Prompt:    "Use the synthesis plan and project conventions to make the change. Preserve unrelated work and record the files changed plus any checks run.",
			DependsOn: []string{"synthesis"},
		},
		{
			ID:        "verification",
			Phase:     PhaseVerification,
			Subject:   "Verify the project result independently",
			Prompt:    "Review the implementation against the goal and run the smallest meaningful validation. Report evidence, remaining limits, and a clear pass or block outcome.",
			DependsOn: []string{"implementation"},
		},
	}
}

// List returns all runs for one workspace, newest first.  It also reclaims
// expired worker leases while loading each run.
func (s *Store) List(workspace string) ([]Run, error) {
	canonical, err := execution.CanonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	dir, err := s.projectDir(canonical)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Run{}, nil
		}
		return nil, err
	}
	runs := make([]Run, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			continue
		}
		run, err := s.Get(canonical, id)
		if errors.Is(err, ErrRunNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].UpdatedAt.After(runs[j].UpdatedAt) })
	return runs, nil
}

// Get loads one project run and persists any automatic lease-expiry recovery.
func (s *Store) Get(workspace, runID string) (Run, error) {
	return s.withRun(workspace, runID, func(_ *Run) (bool, error) { return false, nil })
}

// AddWorkItem expands an existing graph.  The new node is validated against
// every existing dependency before anything reaches disk.
func (s *Store) AddWorkItem(workspace, runID string, input WorkItemInput) (WorkItem, error) {
	var added WorkItem
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		if run.Status == RunCompleted || run.Status == RunCancelled {
			return false, fmt.Errorf("cannot add work to %s project run", run.Status)
		}
		if strings.TrimSpace(input.ID) == "" {
			input.ID = newID("item")
		}
		items, err := materializeItems([]WorkItemInput{input}, s.now())
		if err != nil {
			return false, err
		}
		candidate := append(append([]WorkItem(nil), run.Items...), items[0])
		if err := principal.ValidateBinding(items[0].PrincipalBinding, run.Workspace); err != nil {
			return false, err
		}
		if err := validateGraph(candidate); err != nil {
			return false, err
		}
		run.Items = candidate
		added = cloneItem(items[0])
		run.UpdatedAt = s.now()
		run.addEvent(run.UpdatedAt, "work_added", added.ID, added.Subject, "")
		return true, nil
	})
	if err == nil {
		if item, itemErr := updated.item(added.ID); itemErr == nil {
			added = cloneItem(*item)
		}
	}
	return added, err
}

// ClaimNext atomically reserves the oldest ready work item for one worker.
// Separate processes cannot claim the same item because the read-modify-write
// sequence is protected by the run's filesystem lock.
func (s *Store) ClaimNext(workspace, runID string, options ClaimOptions) (WorkItem, error) {
	worker := strings.TrimSpace(options.Worker)
	if worker == "" || len(worker) > 256 {
		return WorkItem{}, errors.New("worker name is required and must fit within 256 bytes")
	}
	lease := options.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	if lease < time.Second || lease > 24*time.Hour {
		return WorkItem{}, fmt.Errorf("lease must be between 1s and 24h")
	}
	var claimed WorkItem
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		if run.Status == RunCancelled || run.Status == RunCompleted {
			return false, ErrNoReadyWork
		}
		var heldPrincipal error
		for i := range run.Items {
			item := &run.Items[i]
			if item.Status != ItemReady {
				continue
			}
			if item.PrincipalBinding != nil && s.principalCheck != nil {
				if err := s.principalCheck(item.PrincipalBinding); err != nil {
					if errors.Is(err, principal.ErrInactive) || errors.Is(err, principal.ErrBudget) || errors.Is(err, principal.ErrUsageUnknown) || errors.Is(err, principal.ErrBusy) {
						heldPrincipal = err
						continue
					}
					return false, err
				}
			}
			now := s.now()
			item.Status = ItemRunning
			item.Owner = worker
			item.Attempts++
			item.Lease = &Lease{Token: newID("claim"), Worker: worker, ClaimedAt: now, ExpiresAt: now.Add(lease)}
			item.Usage = append(item.Usage, AttemptUsage{Token: item.Lease.Token, Worker: worker})
			item.UpdatedAt = now
			run.UpdatedAt = now
			run.addEvent(now, "work_claimed", item.ID, item.Subject, worker)
			claimed = cloneItem(*item)
			return true, nil
		}
		if heldPrincipal != nil {
			return false, heldPrincipal
		}
		return false, ErrNoReadyWork
	})
	return claimed, err
}

// Complete records durable evidence and opens dependent work items.
func (s *Store) Complete(ctx context.Context, workspace, runID, itemID string, input CompletionInput) (WorkItem, error) {
	worker, output := input.Worker, input.Output
	if err := validateCompletionOutput(output); err != nil {
		return WorkItem{}, err
	}
	before, err := s.Get(workspace, runID)
	if err != nil {
		return WorkItem{}, err
	}
	claimed, err := before.item(itemID)
	if err != nil {
		return WorkItem{}, err
	}
	if err := claimed.assertClaim(worker, input.Token); err != nil {
		return WorkItem{}, err
	}
	if err := before.assertOperationsResolved(itemID); err != nil {
		return WorkItem{}, err
	}
	if deadline := WorkDeadline(before, *claimed); !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	acceptance, artifacts, acceptanceErr := verifyCompletion(ctx, before, *claimed)
	var completed WorkItem
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		if err := item.assertClaim(worker, input.Token); err != nil {
			return false, err
		}
		if err := run.assertOperationsResolved(item.ID); err != nil {
			return false, err
		}
		if !sameContract(before, *claimed, *run, *item) {
			return false, errors.New("completion contract changed during verification")
		}
		item.Acceptance = &acceptance
		item.Output = boundText(output, maxPersistedOutput)
		if acceptanceErr != nil {
			run.addEvent(s.now(), "acceptance_rejected", itemID, acceptance.Summary, worker)
			return true, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		now := s.now()
		item.Status = ItemCompleted
		item.Output = boundText(output, maxPersistedOutput)
		item.Artifacts = artifacts
		item.Failure = nil
		item.Lease = nil
		item.Owner = ""
		item.CompletedAt = now
		item.UpdatedAt = now
		run.UpdatedAt = now
		run.addEvent(now, "work_completed", item.ID, item.Subject, strings.TrimSpace(worker))
		completed = cloneItem(*item)
		return true, nil
	})
	if err == nil && acceptanceErr != nil {
		return WorkItem{}, acceptanceErr
	}
	if err == nil {
		if item, itemErr := updated.item(completed.ID); itemErr == nil {
			completed = cloneItem(*item)
		}
	}
	return completed, err
}

// Fail records a concrete reason. Only explicit interruption/environment
// recovery codes are automatically requeued, and even those can use at most
// the work item's configured attempt budget.  Unknown model/provider errors
// remain visible and require a deliberate recovery decision.
func (s *Store) Fail(workspace, runID, itemID, worker, token string, failure Failure, output string) (WorkItem, error) {
	failure.Code = strings.TrimSpace(failure.Code)
	failure.Summary = strings.TrimSpace(failure.Summary)
	if failure.Code == "" {
		failure.Code = FailureWorkerExecution
	}
	if failure.Summary == "" {
		failure.Summary = "Worker did not complete the project work item."
	}
	var result WorkItem
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		if err := item.assertClaim(worker, token); err != nil {
			return false, err
		}
		now := s.now()
		failure.ObservedAt = now
		failure.Recoverable = autoRecoverable(failure.Code)
		if failure.Code == FailureProviderUsageUnresolved {
			failure.AutoRecovered = false
		}
		if failure.Recoverable && run.assertUsageResolved(item.ID) != nil {
			failure = providerUsageUnresolvedFailure(now)
		}
		item.Output = boundText(output, maxPersistedOutput)
		item.Failure = &failure
		item.Lease = nil
		item.Owner = ""
		item.UpdatedAt = now
		if failure.Recoverable && item.Attempts < item.MaxAttempts {
			item.Status = ItemPending
			item.Failure.AutoRecovered = true
			run.addEvent(now, "work_auto_recovered", item.ID, failure.Summary, strings.TrimSpace(worker))
		} else {
			item.Status = ItemFailed
			run.addEvent(now, "work_failed", item.ID, failure.Summary, strings.TrimSpace(worker))
		}
		if isExecutionConstraint(failure.Code) {
			run.Environment.LastFailure = &execution.Failure{
				Code:            execution.FailureCode(failure.Code),
				Summary:         failure.Summary,
				SuggestedAction: failure.SuggestedAction,
				AutoRecovered:   item.Failure.AutoRecovered,
			}
		}
		run.UpdatedAt = now
		result = cloneItem(*item)
		return true, nil
	})
	if err == nil {
		if item, itemErr := updated.item(result.ID); itemErr == nil {
			result = cloneItem(*item)
		}
		s.rememberExecutionConstraint(updated)
	}
	return result, err
}

// Recover requeues a failed or dependency-blocked item after a coordinator
// has changed the situation (for example, initialized Git or supplied a
// missing credential).  force permits one explicit retry after the normal
// attempt budget; no implicit infinite retry path exists.
func (s *Store) Recover(workspace, runID, itemID, note string, force bool) (WorkItem, error) {
	var recovered WorkItem
	updated, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		if item.Status != ItemFailed && item.Status != ItemBlocked {
			return false, fmt.Errorf("work item %q is %s, not failed or blocked", item.ID, item.Status)
		}
		if err := run.assertOperationsResolved(itemID); err != nil {
			return false, err
		}
		if err := run.assertUsageResolved(itemID); err != nil {
			return false, err
		}
		if item.Attempts >= item.MaxAttempts {
			if !force {
				return false, fmt.Errorf("work item %q exhausted %d attempts; use force after fixing the cause", item.ID, item.MaxAttempts)
			}
			if item.ForcedRecoveries >= 1 {
				return false, fmt.Errorf("work item %q already used its one forced retry; add replacement work after changing the plan", item.ID)
			}
			item.ForcedRecoveries++
		}
		now := s.now()
		item.Status = ItemPending
		item.Lease = nil
		item.Owner = ""
		item.UpdatedAt = now
		if item.Failure != nil {
			item.Failure.AutoRecovered = false
		}
		run.UpdatedAt = now
		detail := strings.TrimSpace(note)
		if detail == "" {
			detail = "Coordinator requeued the work after reviewing the failure."
		}
		item.LastRecoveryRevision = run.Revision + 1
		run.addEvent(now, "work_requeued", item.ID, detail, "")
		recovered = cloneItem(*item)
		return true, nil
	})
	if err == nil {
		if item, itemErr := updated.item(recovered.ID); itemErr == nil {
			recovered = cloneItem(*item)
		}
	}
	return recovered, err
}

// BuildWorkerPrompt creates the context handed to a real worker run.  It
// includes the current environment capability profile and only the bounded
// outputs of direct prerequisites, making a restart deterministic without
// copying a whole prior conversation into the next context window.
func BuildWorkerPrompt(run Run, item WorkItem) string {
	var b strings.Builder
	b.WriteString("You are executing one durable METIS project work item.\n\n")
	b.WriteString("Durable operation rules: Bash commands, including finite tests and acceptance checks, must finish in the foreground. Omit run_in_background, await_completion and required_for_completion, or set all three to false; general background-job guidance does not apply to this worker. Use timeout_ms for a bounded foreground command. Confirmed writes can be replayed after recovery without repeating their effects. Do not disguise the same external action by changing its command or arguments. An uncertain operation must be inspected and resolved by the host before this item can resume.\n\n")
	if !WorkDeadline(run, item).IsZero() {
		fmt.Fprintf(&b, "Absolute task deadline: %s\n", WorkDeadline(run, item).Format(time.RFC3339))
	}
	if remaining := RemainingBudget(run, item); remaining > 0 {
		fmt.Fprintf(&b, "Remaining estimated USD budget: %.6f\n", remaining)
	}
	if len(item.Deliverables) > 0 {
		fmt.Fprintf(&b, "Required output files: %s\n", strings.Join(item.Deliverables, ", "))
	}
	if item.Handoff != nil {
		fmt.Fprintf(&b, "Structured handoff: %s (task %s), content SHA-256: %s\n", item.Handoff.MessageID, item.Handoff.TaskID, item.Handoff.ContentSHA256)
		for _, artifact := range item.Handoff.InputArtifacts {
			fmt.Fprintf(&b, "Verified input artifact: %s SHA-256=%s\n", artifact.Path, artifact.SHA256)
		}
	}
	if item.EventID != "" {
		fmt.Fprintf(&b, "Triggering event: %s / %s\n", item.EventSource, item.EventID)
	}
	if item.Phase == PhaseVerification {
		fmt.Fprintf(&b, "Host acceptance checks configured: %d. The host reruns these checks after your response; prose cannot substitute for passing checks.\n", len(run.CheckCommands))
	}
	fmt.Fprintf(&b, "Project goal: %s\n", run.Goal)
	fmt.Fprintf(&b, "Workspace: %s\n", run.Workspace)
	fmt.Fprintf(&b, "Phase: %s\n", item.Phase)
	fmt.Fprintf(&b, "Work item: %s (%s)\n\n", item.Subject, item.ID)
	b.WriteString("Execution environment:\n")
	if run.Environment.IsGitRepository {
		fmt.Fprintf(&b, "- Git repository: yes (%s)\n", run.Environment.GitRoot)
	} else {
		b.WriteString("- Git repository: no; do not require Git worktree isolation. Use the existing working directory when it is safe to do so.\n")
	}
	if run.Environment.IsLinkedWorktree {
		b.WriteString("- This workspace is already a linked Git worktree; do not nest another worktree beneath it.\n")
	}
	if failure := run.Environment.LastFailure; failure != nil {
		fmt.Fprintf(&b, "- Remembered environment rule: %s", failure.Summary)
		if failure.SuggestedAction != "" {
			fmt.Fprintf(&b, " Suggested action: %s", failure.SuggestedAction)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nYour task:\n")
	b.WriteString(item.Prompt)
	b.WriteString("\n\nDirect prerequisite evidence:\n")
	if len(item.DependsOn) == 0 {
		b.WriteString("- none\n")
	}
	for _, dependencyID := range item.DependsOn {
		dependency, err := run.item(dependencyID)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n[%s — %s]\n", dependency.ID, dependency.Subject)
		if dependency.Output == "" {
			b.WriteString("(no recorded output)\n")
		} else {
			b.WriteString(boundText(dependency.Output, maxDependencyOutput))
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nWork carefully in the stated workspace. Preserve unrelated changes. Before finishing, run the smallest meaningful validation for this work item. If an external fact prevents progress, end your final answer with `METIS_PROJECT_STATUS: blocked` and explain the concrete cause; otherwise include the evidence you produced.\n")
	return b.String()
}

func (s *Store) withRun(workspace, runID string, mutate func(*Run) (bool, error)) (Run, error) {
	if s == nil {
		return Run{}, errors.New("project coordinator store is unavailable")
	}
	canonical, err := execution.CanonicalWorkspace(workspace)
	if err != nil {
		return Run{}, err
	}
	if !validID(runID) {
		return Run{}, ErrRunNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.lockRun(canonical, runID)
	if err != nil {
		return Run{}, err
	}
	defer release()
	run, err := s.readRun(canonical, runID)
	if err != nil {
		return Run{}, err
	}
	environment, envErr := s.environmentFor(canonical)
	if envErr != nil {
		return Run{}, envErr
	}
	changed := false
	if run.Revision == 0 {
		changed = true
	}
	if run.Version != stateVersion {
		run.Version = stateVersion
		changed = true
	}
	if !sameEnvironment(run.Environment, environment) {
		run.Environment = environment
		changed = true
	}
	if run.reconcile(s.now()) {
		changed = true
	}
	archived, err := s.compactOperations(canonical, &run)
	if err != nil {
		return Run{}, err
	}
	changed = changed || archived
	mutated, err := mutate(&run)
	if err != nil {
		if changed {
			if run.Revision == ^uint64(0) {
				return Run{}, errors.New("project coordinator revision overflow")
			}
			run.Revision++
			if writeErr := s.writeRun(canonical, run, false); writeErr != nil {
				return Run{}, writeErr
			}
		}
		return Run{}, err
	}
	if run.reconcile(s.now()) {
		changed = true
	}
	if changed || mutated {
		if run.Revision == ^uint64(0) {
			return Run{}, errors.New("project coordinator revision overflow")
		}
		run.Revision++
		if err := s.writeRun(canonical, run, false); err != nil {
			return Run{}, err
		}
	}
	return cloneRun(run), nil
}

func (s *Store) environmentFor(workspace string) (execution.Profile, error) {
	profile, err := execution.ProbeContext(s.probeContext, workspace)
	if err != nil {
		return execution.Profile{}, err
	}
	if s.executionMemory == nil {
		return profile, nil
	}
	remembered, found, err := s.executionMemory.Load(workspace)
	if err != nil {
		return execution.Profile{}, err
	}
	if found && remembered.LastFailure != nil {
		profile.LastFailure = remembered.LastFailure
	}
	return profile, nil
}

func (s *Store) projectDir(workspace string) (string, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return "", errors.New("project coordinator persistence root is unavailable")
	}
	if !filepath.IsAbs(s.root) {
		return "", fmt.Errorf("project coordinator persistence root %q must be absolute", s.root)
	}
	canonical, err := execution.CanonicalWorkspace(workspace)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(s.root, hex.EncodeToString(sum[:16])), nil
}

func (s *Store) runPath(workspace, runID string) (string, error) {
	dir, err := s.projectDir(workspace)
	if err != nil {
		return "", err
	}
	if !validID(runID) {
		return "", ErrRunNotFound
	}
	return filepath.Join(dir, runID+".json"), nil
}

func (s *Store) readRun(workspace, runID string) (Run, error) {
	path, err := s.runPath(workspace, runID)
	if err != nil {
		return Run{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Run{}, ErrRunNotFound
		}
		return Run{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Run{}, fmt.Errorf("project coordinator run %q is not a regular file", runID)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Run{}, err
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return Run{}, fmt.Errorf("decode project coordinator run %q: %w", runID, err)
	}
	canonical, err := execution.CanonicalWorkspace(workspace)
	if err != nil {
		return Run{}, err
	}
	if (run.Version != stateVersion && run.Version != handoffStateVersion && run.Version != principalStateVersion && run.Version != ledgerStateVersion && run.Version != legacyStateVersion) || run.ID != runID || run.Workspace != canonical {
		return Run{}, fmt.Errorf("project coordinator run identity mismatch")
	}
	if err := validateContract(run.MaxBudgetUSD, run.CheckCommands, nil); err != nil {
		return Run{}, fmt.Errorf("invalid persisted project contract: %w", err)
	}
	if err := validateGraph(run.Items); err != nil {
		return Run{}, fmt.Errorf("project coordinator run graph is invalid: %w", err)
	}
	for _, item := range run.Items {
		if item.LastRecoveryRevision > run.Revision {
			return Run{}, errors.New("work recovery revision exceeds journal revision")
		}
		if err := principal.ValidateBinding(item.PrincipalBinding, canonical); err != nil {
			return Run{}, err
		}
	}
	if err := validateOperations(run); err != nil {
		return Run{}, fmt.Errorf("project coordinator operation ledger is invalid: %w", err)
	}
	if err := s.validateOperationArchive(canonical, run); err != nil {
		return Run{}, fmt.Errorf("project coordinator operation archive is invalid: %w", err)
	}
	run.archiveLookup = func(archive *OperationArchive, id string) (*OperationRecord, error) {
		return s.archivedOperation(canonical, runID, archive, id)
	}
	if err := validateUsageLedger(run); err != nil {
		return Run{}, fmt.Errorf("project coordinator usage ledger is invalid: %w", err)
	}
	return run, nil
}

func (s *Store) writeRun(workspace string, run Run, create bool) error {
	dir, err := s.projectDir(workspace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.root, 0o700); err != nil {
		return err
	}
	path, err := s.runPath(workspace, run.ID)
	if err != nil {
		return err
	}
	if create {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("project coordinator run ID collision")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".run-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncOperationArchiveDirectory(dir)
}

func (s *Store) lockRun(workspace, runID string) (func(), error) {
	dir, err := s.projectDir(workspace)
	if err != nil {
		return nil, err
	}
	locks := filepath.Join(dir, ".locks")
	if err := ensureProjectCoordinatorLockDirectory(locks); err != nil {
		return nil, err
	}
	lock, err := acquireProjectCoordinatorLock(filepath.Join(locks, runID+".lock"), s.lockWait)
	if err != nil {
		return nil, err
	}
	return lock.release, nil
}

func (r *Run) item(id string) (*WorkItem, error) {
	for i := range r.Items {
		if r.Items[i].ID == id {
			return &r.Items[i], nil
		}
	}
	return nil, fmt.Errorf("project work item %q not found", id)
}

func (i *WorkItem) assertOwner(worker string) error {
	if i.Status != ItemRunning {
		return fmt.Errorf("work item %q is %s, not running", i.ID, i.Status)
	}
	worker = strings.TrimSpace(worker)
	if worker == "" || worker != i.Owner {
		return fmt.Errorf("work item %q is claimed by %q", i.ID, i.Owner)
	}
	return nil
}

func validateCompletionOutput(output string) error {
	if strings.TrimSpace(output) == "" {
		return errors.New("completion requires non-empty result evidence")
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(strings.Trim(strings.TrimSpace(line), "`"))
		if strings.HasPrefix(line, "metis_project_status: blocked") || strings.HasPrefix(line, "acceptance_incomplete:") {
			return errors.New("completion rejected: worker explicitly reported incomplete or blocked acceptance")
		}
	}
	return nil
}

func (r *Run) reconcile(now time.Time) bool {
	if r.Status == RunCancelled {
		return false
	}
	changed := r.reconcileOperations(now)
	if r.reconcileLimits(now) {
		changed = true
	}
	for i := range r.Items {
		item := &r.Items[i]
		// Older journals may already have requeued an interrupted request.
		// Hold that dispatchable state before ClaimNext can spend a replacement
		// attempt; dependency reconciliation must not make it ready again.
		if (item.Status == ItemPending || item.Status == ItemReady || item.Status == ItemBlocked) && r.assertUsageResolved(item.ID) != nil {
			failure := providerUsageUnresolvedFailure(now)
			item.Status, item.Failure = ItemFailed, &failure
			item.Owner, item.Lease, item.UpdatedAt = "", nil, now
			r.addEvent(now, FailureProviderUsageUnresolved, item.ID, failure.Summary, "")
			changed = true
			continue
		}
		if item.Status != ItemRunning || item.Lease == nil || item.Lease.ExpiresAt.After(now) {
			continue
		}
		item.Owner = ""
		item.Lease = nil
		item.UpdatedAt = now
		if r.assertUsageResolved(item.ID) != nil {
			failure := providerUsageUnresolvedFailure(now)
			item.Status, item.Failure = ItemFailed, &failure
			r.addEvent(now, FailureProviderUsageUnresolved, item.ID, failure.Summary, "")
			r.addEvent(now, "work_failed", item.ID, "Worker lease expired with unsettled provider usage; automatic recovery is held.", "")
			changed = true
			continue
		}
		item.Failure = &Failure{
			Code:        FailureLeaseExpired,
			Summary:     "Worker lease expired before this work item reported a result.",
			Recoverable: true,
			ObservedAt:  now,
		}
		if item.Attempts < item.MaxAttempts {
			item.Status = ItemPending
			item.Failure.AutoRecovered = true
			item.Failure.SuggestedAction = "The item was returned to the ready queue for another worker."
			r.addEvent(now, "worker_lease_expired", item.ID, item.Failure.Summary, "")
		} else {
			item.Status = ItemFailed
			item.Failure.AutoRecovered = false
			item.Failure.SuggestedAction = "Review the worker crash, fix the cause, then use the one explicit retry or add replacement work."
			r.addEvent(now, "worker_lease_expired", item.ID, item.Failure.Summary, "")
			r.addEvent(now, "work_failed", item.ID, "Worker lease recovery attempt budget exhausted.", "")
		}
		changed = true
	}

	// A dependency can complete in the same transaction that makes its direct
	// child ready.  Looping up to the number of nodes handles a short chain
	// without recursion and always terminates because statuses only advance.
	for pass := 0; pass < len(r.Items); pass++ {
		passChanged := false
		for i := range r.Items {
			item := &r.Items[i]
			if item.Status != ItemPending && item.Status != ItemBlocked {
				continue
			}
			deps := r.dependencyState(item.DependsOn)
			next := item.Status
			switch {
			case deps.failed:
				next = ItemBlocked
			case deps.complete:
				next = ItemReady
			default:
				next = ItemPending
			}
			if next != item.Status {
				item.Status = next
				item.UpdatedAt = now
				passChanged = true
				changed = true
			}
		}
		if !passChanged {
			break
		}
	}

	previous := r.Status
	allDone := len(r.Items) > 0
	hasLive := false
	hasBlocked := false
	for _, item := range r.Items {
		if item.Status != ItemCompleted && item.Status != ItemCancelled {
			allDone = false
		}
		switch item.Status {
		case ItemPending, ItemReady, ItemRunning:
			hasLive = true
		case ItemBlocked, ItemFailed:
			hasBlocked = true
		}
	}
	switch {
	case allDone:
		r.Status = RunCompleted
		if r.CompletedAt.IsZero() {
			r.CompletedAt = now
		}
	case hasLive:
		r.Status = RunActive
		r.CompletedAt = time.Time{}
	case hasBlocked:
		r.Status = RunBlocked
		r.CompletedAt = time.Time{}
	default:
		r.Status = RunActive
	}
	if r.Status != previous {
		changed = true
	}
	if changed {
		r.UpdatedAt = now
	}
	return changed
}

type dependencyStatus struct {
	complete bool
	failed   bool
}

func (r *Run) dependencyState(ids []string) dependencyStatus {
	if len(ids) == 0 {
		return dependencyStatus{complete: true}
	}
	result := dependencyStatus{complete: true}
	for _, id := range ids {
		item, err := r.item(id)
		if err != nil {
			return dependencyStatus{failed: true}
		}
		switch item.Status {
		case ItemFailed, ItemBlocked, ItemCancelled:
			result.failed = true
			result.complete = false
			return result
		case ItemCompleted:
			// still complete unless another dependency is unresolved.
		default:
			result.complete = false
		}
	}
	return result
}

func (r *Run) addEvent(at time.Time, kind, itemID, detail, worker string) {
	r.Events = append(r.Events, Event{At: at, Kind: kind, ItemID: itemID, Detail: boundText(strings.TrimSpace(detail), 2048), Worker: worker})
	if len(r.Events) > maxEvents {
		r.Events = append([]Event(nil), r.Events[len(r.Events)-maxEvents:]...)
	}
}

func materializeItems(inputs []WorkItemInput, now time.Time) ([]WorkItem, error) {
	items := make([]WorkItem, 0, len(inputs))
	for _, input := range inputs {
		if err := validateContract(input.MaxBudgetUSD, nil, input.Deliverables); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(input.ID)
		if id == "" {
			id = newID("item")
		}
		if !validID(id) {
			return nil, fmt.Errorf("invalid work item ID %q", id)
		}
		phase := input.Phase
		if phase == "" {
			phase = PhaseCustom
		}
		if !validPhase(phase) {
			return nil, fmt.Errorf("invalid project phase %q", phase)
		}
		subject := strings.TrimSpace(input.Subject)
		if subject == "" {
			return nil, fmt.Errorf("work item %q needs a subject", id)
		}
		prompt := strings.TrimSpace(input.Prompt)
		if prompt == "" {
			return nil, fmt.Errorf("work item %q needs a prompt", id)
		}
		maxAttempts := input.MaxAttempts
		if maxAttempts == 0 {
			maxAttempts = defaultMaxAttempts
		}
		if maxAttempts < 1 || maxAttempts > 10 {
			return nil, fmt.Errorf("work item %q max attempts must be between 1 and 10", id)
		}
		depends := uniqueIDs(input.DependsOn)
		for _, dep := range depends {
			if !validID(dep) || dep == id {
				return nil, fmt.Errorf("work item %q has invalid dependency %q", id, dep)
			}
		}
		items = append(items, WorkItem{
			ID:               id,
			Phase:            phase,
			Subject:          subject,
			Prompt:           prompt,
			DependsOn:        depends,
			Status:           ItemPending,
			MaxAttempts:      maxAttempts,
			CreatedAt:        now,
			UpdatedAt:        now,
			AgentProfile:     input.AgentProfile,
			PrincipalBinding: clonePrincipalBinding(input.PrincipalBinding),
			Handoff:          cloneHandoffBinding(input.Handoff),
			DeadlineAt:       input.DeadlineAt,
			MaxBudgetUSD:     input.MaxBudgetUSD,
			Deliverables:     append([]string(nil), input.Deliverables...),
		})
	}
	return items, nil
}

func validateGraph(items []WorkItem) error {
	if len(items) == 0 {
		return errors.New("project run has no work items")
	}
	byID := make(map[string]WorkItem, len(items))
	for _, item := range items {
		if err := validateHandoffBinding(item); err != nil {
			return err
		}
		if err := principal.ValidateBinding(item.PrincipalBinding, ""); err != nil {
			return err
		}
		if item.PrincipalBinding != nil && item.AgentProfile != item.PrincipalBinding.Definition.Profile.Name {
			return errors.New("work item profile name differs from frozen principal binding")
		}
		if err := validateContract(item.MaxBudgetUSD, nil, item.Deliverables); err != nil {
			return fmt.Errorf("invalid work item contract: %w", err)
		}
		if !validID(item.ID) || !validPhase(item.Phase) || strings.TrimSpace(item.Subject) == "" || strings.TrimSpace(item.Prompt) == "" || item.MaxAttempts < 1 || item.MaxAttempts > 10 || item.Attempts < 0 || item.ForcedRecoveries < 0 || item.ForcedRecoveries > 1 {
			return fmt.Errorf("work item %q is invalid", item.ID)
		}
		if _, exists := byID[item.ID]; exists {
			return fmt.Errorf("duplicate work item ID %q", item.ID)
		}
		byID[item.ID] = item
	}
	for _, item := range items {
		if item.Handoff != nil {
			verifier, ok := byID[item.ID+"-verify"]
			if !ok || verifier.Phase != PhaseVerification || verifier.PrincipalBinding != nil || verifier.Handoff != nil || len(verifier.DependsOn) != 1 || verifier.DependsOn[0] != item.ID {
				return errors.New("handoff requires its fixed independent verification item")
			}
		}
		seenDeps := map[string]struct{}{}
		for _, dep := range item.DependsOn {
			if dep == item.ID {
				return fmt.Errorf("work item %q depends on itself", item.ID)
			}
			if _, duplicate := seenDeps[dep]; duplicate {
				return fmt.Errorf("work item %q repeats dependency %q", item.ID, dep)
			}
			seenDeps[dep] = struct{}{}
			if _, found := byID[dep]; !found {
				return fmt.Errorf("work item %q depends on unknown item %q", item.ID, dep)
			}
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("project work graph has a dependency cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dep := range byID[id].DependsOn {
			if err := walk(dep); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := walk(id); err != nil {
			return err
		}
	}
	return nil
}

func autoRecoverable(code string) bool {
	return code == FailureLeaseExpired || code == FailureWorkerInterrupted || code == FailureProviderStreamInterrupted || code == string(execution.FailureWorktreeRequiresGit)
}

func providerUsageUnresolvedFailure(now time.Time) Failure {
	return Failure{
		Code:            FailureProviderUsageUnresolved,
		Summary:         "A previous physical provider request has unsettled usage; no replacement attempt was queued.",
		SuggestedAction: "Inspect the persisted request, settle its bound usage receipt or complete its observed usage, then explicitly recover this work item.",
		ObservedAt:      now,
	}
}

func isExecutionConstraint(code string) bool {
	switch execution.FailureCode(code) {
	case execution.FailureWorktreeRequiresGit, execution.FailureNestedWorktree:
		return true
	default:
		return false
	}
}

// rememberExecutionConstraint mirrors a known environment fact into the
// workspace-level execution store. The run file is already the source of
// truth for this transition, so a secondary-memory write failure must not
// report the work-item transition as failed after it was safely persisted.
func (s *Store) rememberExecutionConstraint(run Run) {
	if s == nil || s.executionMemory == nil || run.Environment.LastFailure == nil {
		return
	}
	failure := run.Environment.LastFailure
	if !isExecutionConstraint(string(failure.Code)) {
		return
	}
	_ = s.executionMemory.Record(run.Environment, *failure)
}

func validPhase(phase Phase) bool {
	switch phase {
	case PhaseResearch, PhaseSynthesis, PhaseImplementation, PhaseVerification, PhaseCustom:
		return true
	default:
		return false
	}
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 96 || strings.Contains(id, "..") {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func uniqueIDs(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func newID(prefix string) string {
	var random [5]byte
	if _, err := rand.Read(random[:]); err != nil {
		// A timestamp is still unique enough for a local coordinator if the
		// OS entropy source is temporarily unavailable.  It is an ID, not a
		// security token.
		return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(random[:]))
}

func boundText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit < 64 {
		return value[:limit]
	}
	head := limit * 3 / 4
	tail := limit - head - len("\n… METIS truncated preserved project output …\n")
	if tail < 0 {
		tail = 0
	}
	return value[:head] + "\n… METIS truncated preserved project output …\n" + value[len(value)-tail:]
}

func sameEnvironment(a, b execution.Profile) bool {
	if a.Workspace != b.Workspace || a.GitRoot != b.GitRoot || a.IsGitRepository != b.IsGitRepository || a.IsLinkedWorktree != b.IsLinkedWorktree {
		return false
	}
	if a.LastFailure == nil || b.LastFailure == nil {
		return a.LastFailure == nil && b.LastFailure == nil
	}
	return a.LastFailure.Code == b.LastFailure.Code && a.LastFailure.Occurrences == b.LastFailure.Occurrences && a.LastFailure.Summary == b.LastFailure.Summary
}

func cloneRun(run Run) Run {
	clone := run
	clone.Items = make([]WorkItem, len(run.Items))
	for i := range run.Items {
		clone.Items[i] = cloneItem(run.Items[i])
	}
	clone.Events = append([]Event(nil), run.Events...)
	clone.CheckCommands = append([]string(nil), run.CheckCommands...)
	clone.Operations = cloneOperations(run.Operations)
	clone.UsageRequests = cloneUsageRequests(run.UsageRequests)
	if run.OperationArchive != nil {
		archive := *run.OperationArchive
		clone.OperationArchive = &archive
	}
	if run.Environment.LastFailure != nil {
		failure := *run.Environment.LastFailure
		clone.Environment.LastFailure = &failure
	}
	return clone
}

func cloneItem(item WorkItem) WorkItem {
	clone := item
	clone.PrincipalBinding = clonePrincipalBinding(item.PrincipalBinding)
	clone.Handoff = cloneHandoffBinding(item.Handoff)
	clone.DependsOn = append([]string(nil), item.DependsOn...)
	clone.Deliverables = append([]string(nil), item.Deliverables...)
	clone.Artifacts = append([]Artifact(nil), item.Artifacts...)
	clone.Usage = append([]AttemptUsage(nil), item.Usage...)
	if item.Acceptance != nil {
		acceptance := *item.Acceptance
		acceptance.Checks = append([]CheckResult(nil), item.Acceptance.Checks...)
		clone.Acceptance = &acceptance
	}
	if item.Lease != nil {
		lease := *item.Lease
		clone.Lease = &lease
	}
	if item.Failure != nil {
		failure := *item.Failure
		clone.Failure = &failure
	}
	return clone
}

func clonePrincipalBinding(binding *principal.Binding) *principal.Binding {
	if binding == nil {
		return nil
	}
	data, _ := json.Marshal(binding)
	var copied principal.Binding
	_ = json.Unmarshal(data, &copied)
	return &copied
}
