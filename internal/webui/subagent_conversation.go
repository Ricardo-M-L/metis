package webui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

type subAgentUsageView struct {
	Available                bool   `json:"available"`
	InputTokens              *int64 `json:"inputTokens"`
	OutputTokens             *int64 `json:"outputTokens"`
	TotalTokens              *int64 `json:"totalTokens"`
	CacheReadInputTokens     *int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens *int64 `json:"cacheCreationInputTokens"`
}

type subAgentConversationCompleteness struct {
	State             string `json:"state"`
	ContextResets     int    `json:"contextResets"`
	LiveTailAvailable bool   `json:"liveTailAvailable"`
	IncompleteTail    bool   `json:"incompleteTail"`
	Reason            string `json:"reason,omitempty"`
}

type subAgentConversationView struct {
	Agent         subAgentDetailView                  `json:"agent"`
	Messages      []llm.Message                       `json:"messages"`
	StartIndex    int                                 `json:"startIndex"`
	TotalMessages int                                 `json:"totalMessages"`
	HasMore       bool                                `json:"hasMore"`
	NextBefore    *int                                `json:"nextBefore"`
	Revision      string                              `json:"revision"`
	Segments      []agent.SubAgentConversationSegment `json:"segments"`
	Completeness  subAgentConversationCompleteness    `json:"completeness"`
	Usage         subAgentUsageView                   `json:"usage"`
	LiveTools     []agent.SubAgentActivityCall        `json:"liveTools"`
	Activity      subAgentActivityView                `json:"activity"`
}

type subAgentActivityView struct {
	Available bool   `json:"available"`
	Partial   bool   `json:"partial"`
	Reason    string `json:"reason,omitempty"`
}

// Directory pages bound expensive header/body reads. The cursor advances over
// directory names, including other parents' entries, without disclosing them.
const subAgentDirectoryScanLimit = 512

type subAgentDirectoryCursor struct {
	After string `json:"after"`
}

type subAgentPersistedView struct {
	Revision string
	View     subAgentDetailView
}

// Retain metadata only. Long tool bodies belong to the on-demand conversation
// response, rather than every status/list refresh or a process-wide cache.
func (s *Server) persistedSubAgentView(sessionID, agentID string) (subAgentDetailView, bool) {
	if s == nil || s.store == nil {
		return subAgentDetailView{}, false
	}
	revision := agent.SubAgentConversationRevision(s.store.Dir, agentID)
	if revision == "" {
		return subAgentDetailView{}, false
	}
	s.subAgentReadMu.Lock()
	cached, found := s.subAgentReadCache[agentID]
	s.subAgentReadMu.Unlock()
	if found && cached.Revision == revision && cached.View.SessionID == sessionID {
		return cached.View, true
	}
	conversation, err := agent.LoadSubAgentConversation(s.store.Dir, agentID)
	if err != nil || conversation.Header.SubAgentOf != sessionID {
		return subAgentDetailView{}, false
	}
	view := subAgentDetailView{
		SessionID: sessionID, Name: conversation.Header.TeammateName, AgentID: agentID,
		Model:  conversation.Header.Model,
		Status: "unknown", StartedAt: conversation.Header.CreatedAt, ConversationRevision: conversation.Revision,
	}
	if terminal := conversation.Terminal; terminal != nil {
		view.Status, view.Background, view.EndedAt = terminal.Status, terminal.Background, terminal.EndedAt
		view.Output, view.OutputTruncated = trimSubAgentDetailOutput(terminal.Output)
		view.Result, view.ResultTruncated = trimSubAgentDetailOutput(terminal.Result)
		view.StopHint, view.ExitError = terminal.StopHint, terminal.ExitError
		if elapsed := terminal.EndedAt.Sub(conversation.Header.CreatedAt); elapsed > 0 {
			view.ElapsedMS = elapsed.Milliseconds()
		}
	}
	// The task is the first visible request; project only user messages until
	// it is found, without repeatedly cloning every long tool result.
	for _, message := range conversation.Messages {
		if message.Role == llm.RoleUser {
			visible, _ := subAgentPresentationConversation([]llm.Message{message}, nil)
			if view.Task = subAgentTask(visible); view.Task != "" {
				break
			}
		}
	}
	s.subAgentReadMu.Lock()
	if s.subAgentReadCache == nil {
		s.subAgentReadCache = make(map[string]subAgentPersistedView)
	}
	if len(s.subAgentReadCache) >= 128 {
		for id := range s.subAgentReadCache {
			delete(s.subAgentReadCache, id)
			break
		}
	}
	s.subAgentReadCache[agentID] = subAgentPersistedView{Revision: conversation.Revision, View: view}
	s.subAgentReadMu.Unlock()
	return view, true
}

func subAgentPageNumber(r *http.Request, key string, fallback, maximum int) (int, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback, true
	}
	number, err := strconv.Atoi(raw)
	return number, err == nil && number >= 0 && number <= maximum
}

func (s *Server) handleSubAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID := r.URL.Query().Get("sessionId")
	if !agent.ValidSubAgentID(sessionID) {
		writeError(w, http.StatusBadRequest, "invalid parent session id")
		return
	}
	limit, validLimit := subAgentPageNumber(r, "limit", 50, 100)
	cursor := subAgentDirectoryCursor{}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || !agent.ValidSubAgentID(cursor.After) {
			writeError(w, http.StatusBadRequest, "invalid directory cursor")
			return
		}
	}
	if !validLimit || limit == 0 {
		writeError(w, http.StatusBadRequest, "invalid directory pagination")
		return
	}
	items := make([]subAgentDetailView, 0, limit)
	seen := make(map[string]bool)
	appendItem := func(id string) bool {
		if seen[id] || !agent.ValidSubAgentID(id) {
			return false
		}
		view, ok := s.subAgentDetailView(sessionID, id)
		if !ok {
			return false
		}
		s.enrichSubAgentDetail(&view, true)
		// Body content is fetched on demand from /conversation. Compact list
		// refreshes must not resend many large terminal previews.
		view.Output, view.Result = "", ""
		if summary := []rune(view.Task); len(summary) > 240 {
			view.Task = string(summary[:240]) + "…"
		}
		items = append(items, view)
		seen[id] = true
		return true
	}
	var liveIDs []string
	{
		// Worker identities are parent-scoped and may appear before their first
		// disk checkpoint. Snapshot under the worker mutex, then perform I/O.
		s.workerTurnsMu.Lock()
		worker := s.workerSnapshots[sessionID]
		liveIDs = make([]string, 0, len(worker.status.Agents)+len(worker.unobserved))
		for _, child := range worker.status.Agents {
			liveIDs = append(liveIDs, child.AgentID)
		}
		for id := range worker.unobserved {
			liveIDs = append(liveIDs, id)
		}
		s.workerTurnsMu.Unlock()
		if s.roster != nil {
			for _, child := range s.roster.List() {
				if child != nil && s.subAgentBelongsToSession(sessionID, child.AgentID) {
					liveIDs = append(liveIDs, child.AgentID)
				}
			}
		}
		sort.Strings(liveIDs)
	}
	var ids []string
	if s.store != nil {
		var err error
		ids, err = agent.ListSubAgentTranscripts(s.store.Dir)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "unable to read sub-agent directory")
			return
		}
	}
	// Keyset order is independent of the live roster. A child moving out of
	// the worker's bounded retained set cannot shift another child's cursor.
	union := make([]string, 0, len(liveIDs)+len(ids))
	known := make(map[string]bool, len(liveIDs)+len(ids))
	for _, candidates := range [][]string{liveIDs, ids} {
		for _, id := range candidates {
			if !known[id] && agent.ValidSubAgentID(id) {
				union = append(union, id)
				known[id] = true
			}
		}
	}
	ids = union
	sort.Strings(ids)
	position := sort.SearchStrings(ids, cursor.After)
	if position < len(ids) && ids[position] == cursor.After {
		position++
	}
	inspected := 0
	for position < len(ids) && inspected < subAgentDirectoryScanLimit && len(items) < limit {
		id := ids[position]
		position++
		inspected++
		appendItem(id)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].AgentID < items[j].AgentID
		}
		return items[i].StartedAt.Before(items[j].StartedAt)
	})
	var nextCursor *string
	if position < len(ids) {
		encoded, _ := json.Marshal(subAgentDirectoryCursor{After: ids[position-1]})
		next := base64.RawURLEncoding.EncodeToString(encoded)
		nextCursor = &next
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": sessionID, "agents": items,
		"hasMore": nextCursor != nil, "nextCursor": nextCursor,
	})
}

func (s *Server) handleSubAgentConversation(w http.ResponseWriter, r *http.Request, sessionID, agentID string) {
	limit, validLimit := subAgentPageNumber(r, "limit", 100, 200)
	before, validBefore := subAgentPageNumber(r, "before", int(^uint(0)>>1), int(^uint(0)>>1))
	if !validLimit || limit == 0 || !validBefore {
		writeError(w, http.StatusBadRequest, "invalid conversation pagination")
		return
	}
	detail, ok := s.subAgentDetailView(sessionID, agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "sub-agent not found")
		return
	}
	response := subAgentConversationView{
		Agent: detail, Messages: make([]llm.Message, 0), Segments: make([]agent.SubAgentConversationSegment, 0),
		Completeness: subAgentConversationCompleteness{State: "unavailable", Reason: "history_not_recorded"},
		LiveTools:    make([]agent.SubAgentActivityCall, 0),
	}
	recordedResults := make(map[string]bool)
	if s.store != nil {
		conversation, err := agent.LoadSubAgentConversation(s.store.Dir, agentID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			if !s.subAgentBelongsToSession(sessionID, agentID) {
				writeError(w, http.StatusNotFound, "sub-agent not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "unable to read recorded sub-agent conversation")
			return
		}
		if err == nil {
			if conversation.Header.SubAgentOf != sessionID {
				writeError(w, http.StatusNotFound, "sub-agent not found")
				return
			}
			messages, segments := subAgentPresentationConversation(conversation.Messages, conversation.Segments)
			for _, message := range conversation.Messages {
				for _, block := range message.Content {
					if block.Type == "tool_result" && block.TraceCallID != "" {
						recordedResults[block.TraceCallID] = true
					}
				}
			}
			// Describe the bytes actually read, even if the producer appended
			// another checkpoint before this response finished formatting.
			response.Agent.ConversationRevision = conversation.Revision
			response.Agent.Model = conversation.Header.Model
			response.TotalMessages = len(messages)
			if before == int(^uint(0)>>1) {
				before = len(messages)
			} else if before > len(messages) {
				writeError(w, http.StatusBadRequest, "conversation cursor exceeds recorded history")
				return
			}
			response.StartIndex = max(0, before-limit)
			response.Messages = messages[response.StartIndex:before]
			response.HasMore = response.StartIndex > 0
			if response.HasMore {
				next := response.StartIndex
				response.NextBefore = &next
			}
			response.Segments = segments
			response.Completeness.State = "recorded"
			response.Completeness.Reason = "recorded_history_only"
			response.Completeness.ContextResets = conversation.ContextResets
			response.Completeness.IncompleteTail = conversation.IncompleteTail
			if conversation.ContextResets > 0 {
				response.Completeness.State = "context_reset"
			}
			response.Agent.Task = subAgentTask(messages)
		}
		activity, err := agent.LoadSubAgentActivity(s.store.Dir, agentID)
		if err == nil && activity.ParentSessionID == sessionID {
			response.Activity = subAgentActivityView{Available: activity.Available, Partial: activity.Partial, Reason: activity.Reason}
			response.Completeness.LiveTailAvailable = activity.Available
			if activity.Available || activity.Partial {
				response.Agent.activityRevision = activity.Revision
			}
			for _, call := range activity.Calls {
				// Provider tool IDs can repeat. Only the dispatcher's exact
				// occurrence identity proves this result is already recorded.
				if call.TraceCallID != "" && recordedResults[call.TraceCallID] {
					continue
				}
				call.Input = agent.PresentationToolInput(call.Input)
				response.LiveTools = append(response.LiveTools, call)
			}
		} else if err != nil {
			response.Activity = subAgentActivityView{Partial: true, Reason: "activity_unavailable"}
		}
	}
	s.enrichSubAgentDetail(&response.Agent, false)
	response.Revision, response.Usage = response.Agent.ConversationRevision, response.Agent.Usage
	if s.store != nil {
		if header, err := agent.LoadSubAgentHeader(s.store.Dir, agentID); err == nil && header.SubAgentOf != sessionID {
			writeError(w, http.StatusNotFound, "sub-agent not found")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) enrichSubAgentDetail(view *subAgentDetailView, includeTask bool) {
	if view == nil {
		return
	}
	view.ParentSessionID = view.SessionID
	view.Ownership = "parent"
	view.CanAcceptDirectInput = false
	view.DirectInputBlockedReason = parentOwnedInputReason
	if s == nil || s.store == nil {
		return
	}
	if view.ConversationRevision == "" {
		view.ConversationRevision = agent.SubAgentConversationRevision(s.store.Dir, view.AgentID)
	}
	usage, err := agent.LoadSubAgentUsage(s.store.Dir, view.AgentID)
	if err == nil && usage.Available && usage.ParentSessionID == view.SessionID {
		input, output, cacheRead, cacheCreate := usage.InputTokens, usage.OutputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens
		total := input + output + cacheRead + cacheCreate
		view.Usage = subAgentUsageView{Available: true, InputTokens: &input, OutputTokens: &output, TotalTokens: &total, CacheReadInputTokens: &cacheRead, CacheCreationInputTokens: &cacheCreate}
		view.ConversationRevision += ":" + usage.Revision
	}
	activityRevision := view.activityRevision
	if activityRevision == "" {
		activityRevision = agent.SubAgentActivityRevision(s.store.Dir, view.AgentID)
	}
	if activityRevision != "" {
		view.ConversationRevision += ":activity:" + activityRevision
	}
	if includeTask {
		if persisted, ok := s.persistedSubAgentView(view.SessionID, view.AgentID); ok {
			view.Task = persisted.Task
		}
	}
}

func subAgentTask(messages []llm.Message) string {
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		var parts []string
		for _, block := range message.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

var subAgentInternalSectionPatterns = func() []*regexp.Regexp {
	tags := []string{"system-reminder", "memory-context", "auto-retrieve", "peer_message", "task-context", "project-context", "job_notification", "sub_agent_idle", "memory_consolidation_done", "monitor_event", "post_compact_context", "metis-internal-review"}
	patterns := make([]*regexp.Regexp, 0, len(tags))
	for _, tag := range tags {
		patterns = append(patterns, regexp.MustCompile(`(?is)<`+tag+`(?:\s[^>]*)?>.*?(?:</`+tag+`\s*>|$)`))
	}
	return patterns
}()

// Match the parent transcript's visibleTranscriptText boundary, in addition
// to using its credential-safe history projection. Tool result bodies are
// preserved verbatim; internal instruction envelopes apply to prose only.
func subAgentPresentationConversation(messages []llm.Message, segments []agent.SubAgentConversationSegment) ([]llm.Message, []agent.SubAgentConversationSegment) {
	visible := make([]llm.Message, 0, len(messages))
	projected := agent.PresentationHistory(messages)
	visibleSegments := make([]agent.SubAgentConversationSegment, 0, len(segments))
	nextSegment := 0
	for i, message := range projected {
		for nextSegment < len(segments) && segments[nextSegment].StartIndex == i {
			segment := segments[nextSegment]
			segment.StartIndex = len(visible)
			visibleSegments = append(visibleSegments, segment)
			nextSegment++
		}
		if message.Role == llm.RoleSystem {
			continue
		}
		blocks := make([]llm.ContentBlock, 0, len(message.Content))
		for _, block := range message.Content {
			if block.Type == "text" || block.Type == "thinking" {
				for _, pattern := range subAgentInternalSectionPatterns {
					block.Text = pattern.ReplaceAllString(block.Text, "")
				}
				if strings.TrimSpace(block.Text) == "" {
					continue
				}
			}
			blocks = append(blocks, block)
		}
		if len(blocks) > 0 {
			message.Content = blocks
			visible = append(visible, message)
		}
	}
	for nextSegment < len(segments) {
		segment := segments[nextSegment]
		segment.StartIndex = len(visible)
		visibleSegments = append(visibleSegments, segment)
		nextSegment++
	}
	return visible, visibleSegments
}
