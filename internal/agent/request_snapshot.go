package agent

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// requestSnapshot owns the reusable header of an actual completed request.
// History is captured only when Fork is dispatched, after its tool_use exists;
// copying the entire transcript on every provider call would be unnecessary.
type requestSnapshot struct {
	system                string
	sections              []llm.SystemSection
	principalIdentity     string
	request               llm.Request
	anchor                contextRequestAnchor
	discovered            map[string]bool
	shortToolDescriptions bool
	contextCapabilities   llm.ContextCapabilities
	memory                memory.Repository
	contextWindow         int
	planMode              bool
	planPrompt            string
	runtimeState          *RuntimeStateSnapshot
}

func (l *Loop) rememberRequest(req llm.Request, anchor contextRequestAnchor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.historyRevision != anchor.historyRevision || l.routingRevision != anchor.routingRevision || identifyContextProvider(l.Provider) != anchor.providerID {
		return
	}
	req.Messages = nil
	snapshot := &requestSnapshot{
		request: cloneRequest(req), anchor: anchor, system: l.System, sections: slices.Clone(l.SystemSections), principalIdentity: l.principalIdentity,
		discovered: maps.Clone(l.discoveredMCP), shortToolDescriptions: l.ShortToolDescriptions,
		contextCapabilities: l.contextCapabilities, memory: l.Memory, contextWindow: l.ContextWindow, planMode: l.planMode, planPrompt: l.PlanSystemPrompt,
	}
	if l.runtimeStateReady {
		state := l.runtimeStateSnapshot
		snapshot.runtimeState = &state
	}
	l.lastRequest = snapshot
	// Only the initial warm request is pinned. Later tool discovery and live
	// registry changes must still be visible to the child.
	l.warmRequest = nil
}

func (l *Loop) parentSnapshot(toolName string) ParentSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	snap := ParentSnapshot{Messages: l.Messages, System: l.System, Model: l.Model}
	if toolName == "Fork" {
		snap.Messages = cloneRequestMessages(l.Messages)
		if previous := l.lastRequest; previous != nil && previous.anchor.historyRevision == l.historyRevision && previous.anchor.routingRevision == l.routingRevision && previous.anchor.providerID == identifyContextProvider(l.Provider) {
			snap.request = previous
		}
	}
	return snap
}

// RestoreFork installs a warm request without changing the parent's prefix.
// The caller closes outstanding tool calls before restoration: ordinary
// Restore repairs orphans, so restoring an open call and then adding its real
// placeholder would insert two results for the same tool invocation.
func (l *Loop) RestoreFork(snap ParentSnapshot, history []llm.Message) {
	l.Restore(cloneRequestMessages(history))
	if snap.request == nil {
		return // Legacy embedders supply history/system/model only.
	}
	source := snap.request
	req := cloneRequest(source.request)
	if l.Registry != nil {
		req.Tools = slices.DeleteFunc(req.Tools, func(spec llm.ToolSpec) bool { _, ok := l.Registry.GetModelEntry(spec.Name); return !ok })
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Provider = source.anchor.provider
	l.Model = req.Model
	l.System = source.system
	l.SystemSections = slices.Clone(source.sections)
	l.principalIdentity = source.principalIdentity
	l.ResponseFormat = req.ResponseFormat
	l.ContextWindow = source.contextWindow
	l.contextCapabilities = source.contextCapabilities
	l.Memory = source.memory
	l.ShortToolDescriptions = source.shortToolDescriptions
	l.planMode, l.PlanSystemPrompt = source.planMode, source.planPrompt
	l.discoveredMCP = maps.Clone(source.discovered)
	l.discoveredMCPHydrated = true
	l.cacheAffinityID = req.SessionID
	l.warmRequest = &req
	l.warmRequestAnchor = &source.anchor
	l.SetEffort(req.Effort)
	// Runtime-owned sections must be regenerated, not duplicated, once the
	// child changes its own plan/permission state. The first request below
	// still uses the exact parent's already-rendered sections.
	if source.runtimeState != nil && len(req.SystemSections) > 0 {
		l.SystemSections = slices.DeleteFunc(l.SystemSections, func(s llm.SystemSection) bool {
			return s.Name == "runtime_state" || s.Name == "plan_mode"
		})
		state := *source.runtimeState
		l.inheritedRuntimeState = &state
	}
}

func (l *Loop) warmToolSpecs() ([]llm.ToolSpec, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.warmRequest == nil {
		return nil, false
	}
	return cloneRequestTools(l.warmRequest.Tools), true
}

// applyWarmRequestLocked keeps preview/estimation calls pure. The pin is
// released only after a provider response succeeds, never by a UI read.
func (l *Loop) applyWarmRequestLocked(req llm.Request) llm.Request {
	if l.warmRequest == nil {
		return req
	}
	if anchor := l.warmRequestAnchor; anchor != nil && anchor.memoryPresent {
		generation, body, valid := l.memorySnapshotLocked()
		if !valid || !anchor.memoryValid || generation != anchor.memoryGeneration || body != anchor.memoryBody {
			l.warmRequest = nil
			return req
		}
	}
	if l.inheritedRuntimeState != nil && l.Gate != nil && l.inheritedRuntimeState.PermissionMode != "" && permission.CanonicalMode(l.inheritedRuntimeState.PermissionMode) != l.Gate.Mode() {
		l.warmRequest = nil
		return req
	}
	warm := cloneRequest(*l.warmRequest)
	warm.Messages = req.Messages
	if req.MaxTokens > 0 && (warm.MaxTokens <= 0 || req.MaxTokens < warm.MaxTokens) {
		warm.MaxTokens = req.MaxTokens
	}
	return warm
}

func cloneRequest(req llm.Request) llm.Request {
	req.SystemSections = slices.Clone(req.SystemSections)
	req.StopSequences = slices.Clone(req.StopSequences)
	req.Tools = cloneRequestTools(req.Tools)
	req.Messages = cloneRequestMessages(req.Messages)
	if req.ResponseFormat != nil {
		format := *req.ResponseFormat
		format.JSONSchema = cloneRequestMap(format.JSONSchema)
		req.ResponseFormat = &format
	}
	return req
}

func cloneRequestTools(specs []llm.ToolSpec) []llm.ToolSpec {
	out := slices.Clone(specs)
	for i := range out {
		out[i].InputSchema = cloneRequestMap(out[i].InputSchema)
	}
	return out
}

func cloneRequestMessages(messages []llm.Message) []llm.Message {
	out := slices.Clone(messages)
	for i := range out {
		out[i].Content = cloneRequestBlocks(out[i].Content)
	}
	return out
}

func cloneRequestBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := slices.Clone(blocks)
	for i := range out {
		out[i].ToolInput = cloneRequestMap(out[i].ToolInput)
		out[i].Presentation = cloneRequestMap(out[i].Presentation)
		out[i].ProviderHint = maps.Clone(out[i].ProviderHint)
		if out[i].ContextUpdate != nil {
			update := *out[i].ContextUpdate
			update.State = slices.Clone(update.State)
			update.Changed = slices.Clone(update.Changed)
			update.Removed = slices.Clone(update.Removed)
			out[i].ContextUpdate = &update
		}
		out[i].ToolResultBlocks = cloneRequestBlocks(out[i].ToolResultBlocks)
	}
	return out
}

func cloneRequestMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = cloneRequestValue(value)
	}
	return out
}

func cloneRequestValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return cloneRequestMap(v)
	case map[string]string:
		return maps.Clone(v)
	case []any:
		out := slices.Clone(v)
		for i := range out {
			out[i] = cloneRequestValue(out[i])
		}
		return out
	case []map[string]any:
		out := slices.Clone(v)
		for i := range out {
			out[i] = cloneRequestMap(out[i])
		}
		return out
	case []string:
		return slices.Clone(v)
	case json.RawMessage:
		return slices.Clone(v)
	default:
		// Plugin schemas may use typed slices (for example []int enums).
		// Preserve their numeric types rather than a JSON float64 round-trip.
		original := reflect.ValueOf(value)
		if original.IsValid() && original.Kind() == reflect.Slice && !original.IsNil() {
			out := reflect.MakeSlice(original.Type(), original.Len(), original.Len())
			for i := 0; i < original.Len(); i++ {
				if cloned := cloneRequestValue(original.Index(i).Interface()); cloned != nil {
					out.Index(i).Set(reflect.ValueOf(cloned))
				}
				// MakeSlice already supplied the correctly typed zero value for
				// null interfaces; reflect.ValueOf(nil) cannot be passed to Set.
			}
			return out.Interface()
		}
		return value
	}
}
