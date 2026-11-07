package tasks

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ArchivedPlan preserves a replaced plan without turning unfinished work into
// completed work. Structured records remain addressable by their original IDs.
type ArchivedPlan struct {
	ID         string    `json:"id"`
	Items      []Item    `json:"items"`
	ArchivedAt time.Time `json:"archived_at"`
}

type planContextKey struct{}
type planBinding struct {
	mu     sync.RWMutex
	id     string
	frozen bool
}

func WithPlanID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, planContextKey{}, &planBinding{id: id})
}
func HasPlanContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(planContextKey{}).(*planBinding)
	return ok
}
func PlanIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if b, ok := ctx.Value(planContextKey{}).(*planBinding); ok {
		b.mu.RLock()
		defer b.mu.RUnlock()
		return b.id
	}
	return ""
}

// FreezePlanContext gives a spawned child a fixed scope. The parent may start
// another plan later, but the child's writes must not move into that plan.
func FreezePlanContext(ctx context.Context) context.Context {
	if !HasPlanContext(ctx) {
		return ctx
	}
	return context.WithValue(ctx, planContextKey{}, &planBinding{id: PlanIDFromContext(ctx), frozen: true})
}
func planContextFrozen(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	b, ok := ctx.Value(planContextKey{}).(*planBinding)
	return ok && b.frozen
}
func setContextPlanID(ctx context.Context, id string) {
	if ctx == nil {
		return
	}
	if b, ok := ctx.Value(planContextKey{}).(*planBinding); ok {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.frozen {
			b.id = id
		}
	}
}
func planIDForContext(ctx context.Context, fallback string) string {
	if id := PlanIDFromContext(ctx); id != "" {
		return id
	}
	return fallback
}

func ensurePlan(l *List) {
	if l.PlanID != "" {
		return
	}
	l.PlanID = uuid.NewString()
	// Untagged structured tasks belong to the legacy scope once, never to a
	// newly selected plan. No historical task file needs to be rewritten.
	l.LegacyPlanID = l.PlanID
}
func rotatePlan(l *List) {
	l.ArchivedPlans = append(l.ArchivedPlans, ArchivedPlan{ID: l.PlanID, Items: append([]Item(nil), l.Items...), ArchivedAt: time.Now()})
	l.PlanID = uuid.NewString()
	l.Items = nil
}
func planList(envelope *List, id string) (*List, error) {
	if id == envelope.PlanID {
		return &List{SessionID: envelope.SessionID, PlanID: id, Items: append([]Item(nil), envelope.Items...)}, nil
	}
	for _, p := range envelope.ArchivedPlans {
		if p.ID == id {
			return &List{SessionID: envelope.SessionID, PlanID: id, Items: append([]Item(nil), p.Items...)}, nil
		}
	}
	return nil, fmt.Errorf("tasks: plan %q is not part of this session", id)
}
func installPlanList(envelope, selected *List) {
	if selected.PlanID == envelope.PlanID {
		envelope.Items = selected.Items
		return
	}
	for i := range envelope.ArchivedPlans {
		if envelope.ArchivedPlans[i].ID == selected.PlanID {
			envelope.ArchivedPlans[i].Items = selected.Items
			return
		}
	}
}
func plansDisjoint(old, next []Item) bool {
	if len(old) == 0 || len(next) == 0 {
		return false
	}
	for _, item := range next {
		if findExisting(&List{Items: old}, item) != nil {
			return false
		}
	}
	return true
}

func CurrentPlanID(sessionID string) (string, error) {
	l, err := Load(sessionID)
	if err != nil {
		return "", err
	}
	return l.PlanID, nil
}
func EnsureCurrentPlan(sessionID string) (string, error) {
	unlock, err := lockTaskMutation(sessionID)
	if err != nil {
		return "", err
	}
	defer unlock()
	l, err := Load(sessionID)
	if err != nil {
		return "", err
	}
	if l.PlanID != "" {
		return l.PlanID, nil
	}
	ensurePlan(l)
	return l.PlanID, saveLocked(l)
}

// BeginUserWork is called only at a new human input boundary. Follow-up turns
// keep unfinished work; completed work is archived before the next request.
// Synthetic wakeups and child runs must retain their existing plan binding.
func BeginUserWork(sessionID string) (string, error) {
	unlock, err := lockTaskMutation(sessionID)
	if err != nil {
		return "", err
	}
	defer unlock()
	l, err := Load(sessionID)
	if err != nil {
		return "", err
	}
	ensurePlan(l)
	items := planningItemsForList(l, l.PlanID, false)
	// A legacy compact list is the last explicit plan. Untagged unmatched
	// Task* rows from older work must not prevent that completed plan from
	// being archived at the next human request.
	if l.PlanID == l.LegacyPlanID && len(l.Items) > 0 {
		var current []*Task
		if store := TaskStoreForSession(sessionID); store != nil {
			for _, task := range store.List(false) {
				if task.PlanID == l.PlanID || findExisting(&List{Items: l.Items}, Item{Content: task.Subject}) != nil {
					current = append(current, task)
				}
			}
		}
		items = mergePlanningItems(l.Items, current)
	}
	completed := len(items) > 0
	for _, item := range items {
		if item.Status != "completed" {
			completed = false
			break
		}
	}
	if completed {
		rotatePlan(l)
	}
	return l.PlanID, saveLocked(l)
}

func effectiveTaskPlan(t *Task, l *List) string {
	if t.PlanID != "" {
		return t.PlanID
	}
	return l.LegacyPlanID
}
func filterTasksForPlan(all []*Task, l *List, id string) []*Task {
	// Files with no plan metadata keep their historical read semantics until
	// the first mutation establishes the legacy scope.
	if l.PlanID == "" {
		return all
	}
	out := make([]*Task, 0, len(all))
	for _, t := range all {
		if effectiveTaskPlan(t, l) == id {
			out = append(out, t)
		}
	}
	return out
}
func planningItemsForList(l *List, id string, snapshot bool) []Item {
	selected := l
	if l.PlanID != "" {
		var err error
		selected, err = planList(l, id)
		if err != nil {
			return nil
		}
	}
	var structured []*Task
	if snapshot {
		structured = NewTaskStore(l.SessionID).List(false)
	} else if store := TaskStoreForSession(l.SessionID); store != nil {
		structured = store.List(false)
	}
	return mergePlanningItems(selected.Items, filterTasksForPlan(structured, l, id))
}
func PlanningItemsForContext(ctx context.Context) ([]Item, error) {
	sid := SessionIDFromContext(ctx)
	l, err := Load(sid)
	if err != nil {
		return nil, err
	}
	return planningItemsForList(l, planIDForContext(ctx, l.PlanID), false), nil
}
func TasksForContext(ctx context.Context, all []*Task) ([]*Task, error) {
	l, err := Load(SessionIDFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return filterTasksForPlan(all, l, planIDForContext(ctx, l.PlanID)), nil
}
func TaskPlanID(sessionID string, task *Task) (string, error) {
	l, err := Load(sessionID)
	if err != nil {
		return "", err
	}
	return effectiveTaskPlan(task, l), nil
}
func PlanHistory(sessionID string) ([]ArchivedPlan, error) {
	l, err := Load(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]ArchivedPlan, 0, len(l.ArchivedPlans))
	for _, p := range l.ArchivedPlans {
		p.Items = planningItemsForList(l, p.ID, true)
		out = append(out, p)
	}
	return out, nil
}

// ActivatePlanForContext explicitly resumes an archived plan. Ordinary task
// reads/updates never activate history as a side effect.
func ActivatePlanForContext(ctx context.Context, id string) error {
	if planContextFrozen(ctx) {
		return fmt.Errorf("tasks: a child cannot change the current work plan")
	}
	unlock, err := lockTaskMutation(SessionIDFromContext(ctx))
	if err != nil {
		return err
	}
	defer unlock()
	l, err := Load(SessionIDFromContext(ctx))
	if err != nil {
		return err
	}
	if id == l.PlanID {
		setContextPlanID(ctx, id)
		return nil
	}
	selected, err := planList(l, id)
	if err != nil {
		return err
	}
	archive := make([]ArchivedPlan, 0, len(l.ArchivedPlans))
	for _, p := range l.ArchivedPlans {
		if p.ID != id {
			archive = append(archive, p)
		}
	}
	archive = append(archive, ArchivedPlan{ID: l.PlanID, Items: append([]Item(nil), l.Items...), ArchivedAt: time.Now()})
	l.ArchivedPlans = archive
	l.PlanID = id
	l.Items = selected.Items
	if err := saveLocked(l); err != nil {
		return err
	}
	setContextPlanID(ctx, id)
	return nil
}
