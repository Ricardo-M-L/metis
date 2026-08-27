package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

const memoryE2ETarget = "TARGET-RECALL: the user's Quartz Heron workstation codename is Nimbus Finch."

// memoryRepositoryE2EProvider serves both memory-side Complete calls and the
// actual agent Stream call. Keeping one provider at the boundary lets this
// test exercise the same distill -> restart -> rerank -> request flow used by
// a real session without reaching an external API.
type memoryRepositoryE2EProvider struct {
	mu              sync.Mutex
	streamRequests  []llm.Request
	distillRequests int
	distillErr      error
}

func (*memoryRepositoryE2EProvider) Name() string          { return "memory-repository-e2e" }
func (*memoryRepositoryE2EProvider) ModelID() string       { return "memory-repository-e2e-model" }
func (*memoryRepositoryE2EProvider) MaxContextTokens() int { return 200_000 }

func (p *memoryRepositoryE2EProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	switch {
	case strings.Contains(req.System, "extract durable facts"):
		p.mu.Lock()
		p.distillRequests++
		distillErr := p.distillErr
		p.mu.Unlock()
		if distillErr != nil {
			return nil, distillErr
		}
		body, err := json.Marshal([]map[string]any{{
			"type":    memory.TypeUser,
			"content": memoryE2ETarget,
			"tags":    []string{"quartz", "heron", "workstation", "codename"},
		}})
		if err != nil {
			return nil, err
		}
		return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: string(body)}}}, nil

	case strings.Contains(req.System, "rank archival memory passages"):
		prompt := requestText(req)
		for _, line := range strings.Split(prompt, "\n") {
			if !strings.Contains(line, "TARGET-RECALL") {
				continue
			}
			var index int
			if _, err := fmt.Sscanf(strings.TrimSpace(line), "[%d]", &index); err != nil {
				return nil, fmt.Errorf("parse target candidate index from %q: %w", line, err)
			}
			return &llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("[%d]", index)}}}, nil
		}
		return nil, errors.New("target passage missing from rerank candidates")
	default:
		return nil, fmt.Errorf("unexpected Complete request system prompt: %q", req.System)
	}
}

func (p *memoryRepositoryE2EProvider) distillRequestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.distillRequests
}

func (p *memoryRepositoryE2EProvider) setDistillError(err error) {
	p.mu.Lock()
	p.distillErr = err
	p.mu.Unlock()
}

func (p *memoryRepositoryE2EProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.streamRequests = append(p.streamRequests, req)
	p.mu.Unlock()
	return &memoryRepositoryE2EStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "I recalled the cross-session workstation codename."},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

func (p *memoryRepositoryE2EProvider) lastStreamRequest(t *testing.T) llm.Request {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.streamRequests) == 0 {
		t.Fatal("provider did not receive an agent Stream request")
	}
	return p.streamRequests[len(p.streamRequests)-1]
}

type memoryRepositoryE2EStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *memoryRepositoryE2EStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*memoryRepositoryE2EStream) Close() error { return nil }

func requestText(req llm.Request) string {
	var text strings.Builder
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
				text.WriteByte('\n')
			}
		}
	}
	return text.String()
}

func archivalPassageByID(t *testing.T, manager *memory.MemoryManager, id string) memory.Passage {
	t.Helper()
	passages, err := manager.Archival().Search(memory.SearchOptions{SortBy: "recent"})
	if err != nil {
		t.Fatalf("search archival memory: %v", err)
	}
	for _, passage := range passages {
		if passage.ID == id {
			return passage
		}
	}
	t.Fatalf("archival passage %q not found", id)
	return memory.Passage{}
}

func readRecallMessages(t *testing.T, root string) []memory.Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "recall", "messages.jsonl"))
	if err != nil {
		t.Fatalf("read recall messages: %v", err)
	}
	var messages []memory.Message
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var message memory.Message
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("decode recall message %q: %v", line, err)
		}
		messages = append(messages, message)
	}
	return messages
}

func readRecallSessions(t *testing.T, root string) []memory.Session {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "recall", "sessions.json"))
	if err != nil {
		t.Fatalf("read recall sessions: %v", err)
	}
	var sessions []memory.Session
	if err := json.Unmarshal(raw, &sessions); err != nil {
		t.Fatalf("decode recall sessions: %v", err)
	}
	return sessions
}

func TestMemoryRepositoryE2E_RestartRecallUsageAndProvenance(t *testing.T) {
	root := t.TempDir()
	provider := &memoryRepositoryE2EProvider{}

	// Session A records its completed exchange and distills the durable fact
	// through the repository API. This is the state a later process must see.
	sessionA, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create session A memory manager: %v", err)
	}
	const (
		sessionAID = "memory-e2e-session-a"
		messageAID = "memory-e2e-session-a/turn/1"
	)
	userA := "Please remember that the Quartz Heron workstation codename assigned to me is Nimbus Finch."
	assistantA := "Understood. I will remember that durable workstation codename for a future session."
	if err := sessionA.RecordTurn(context.Background(), sessionAID, messageAID, userA, assistantA); err != nil {
		t.Fatalf("record session A turn: %v", err)
	}
	if err := sessionA.DistillTurnWithMetadata(
		context.Background(), provider, sessionAID, messageAID, userA, assistantA,
	); err != nil {
		t.Fatalf("distill session A turn: %v", err)
	}

	seeded, err := sessionA.Archival().Search(memory.SearchOptions{Query: "TARGET-RECALL"})
	if err != nil || len(seeded) != 1 {
		t.Fatalf("distilled target passage missing: passages=%+v err=%v", seeded, err)
	}
	targetID := seeded[0].ID
	for _, passage := range []memory.Passage{
		{
			ID:      "memory-e2e-decoy-a",
			Content: "Quartz Heron workstation codename documentation uses an unrelated decoy value, Silver Maple.",
			Type:    memory.TypeProject,
			Tags:    []string{"quartz", "heron", "workstation", "codename"},
		},
		{
			ID:      "memory-e2e-decoy-b",
			Content: "Quartz Heron workstation codename migration notes mention another unrelated decoy, Amber Kite.",
			Type:    memory.TypeReference,
			Tags:    []string{"quartz", "heron", "workstation", "codename"},
		},
	} {
		if err := sessionA.Archival().Insert(passage); err != nil {
			t.Fatalf("insert archival decoy %q: %v", passage.ID, err)
		}
	}

	// A fresh manager models a process restart and a brand-new Desktop/CLI
	// session. No in-memory state from session A is reused here.
	sessionB, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("restart memory manager for session B: %v", err)
	}
	if got := archivalPassageByID(t, sessionB, targetID); got.SourceSessionID != sessionAID || got.SourceMessageID != messageAID {
		t.Fatalf("distilled provenance did not survive restart: %+v", got)
	}

	loop := NewLoop(provider, newRegistryWith(t), nil, nil, "BASE MEMORY E2E SYSTEM", 3)
	loop.SystemSections = []llm.SystemSection{{Name: "base", Body: "BASE MEMORY E2E SYSTEM", Cache: true}}
	loop.Memory = sessionB
	loop.AutoRetrieveK = 1
	loop.AutoRetrieveRerank = true
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: "memory-e2e-session-b"}
	}
	const query = "What is my Quartz Heron workstation codename?"
	loop.AppendUser(query)

	// Context/status estimation is a preview. Repeated UI polling must not
	// turn every rerank candidate into an observed memory use.
	for range 3 {
		if got := loop.EstimateRequestContextTokens(nil); got <= 0 {
			t.Fatalf("EstimateRequestContextTokens()=%d, want a positive estimate", got)
		}
	}
	for _, id := range []string{targetID, "memory-e2e-decoy-a", "memory-e2e-decoy-b"} {
		if got := archivalPassageByID(t, sessionB, id); got.UseCount != 0 || got.LastUsedAt != "" {
			t.Fatalf("estimate mutated retrieval metadata for %q: %+v", id, got)
		}
	}

	events := make(chan Event, 32)
	if err := loop.Run(context.Background(), events); err != nil {
		t.Fatalf("run session B: %v", err)
	}

	request := provider.lastStreamRequest(t)
	if strings.Contains(request.System, "<auto-retrieve") {
		t.Fatalf("query-specific recall leaked into system prompt: %q", request.System)
	}
	for _, section := range request.SystemSections {
		if section.Name == "auto-retrieve" || strings.Contains(section.Body, "<auto-retrieve") {
			t.Fatalf("query-specific recall leaked into system section: %+v", section)
		}
	}
	var userMessage *llm.Message
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == llm.RoleUser {
			userMessage = &request.Messages[i]
			break
		}
	}
	if userMessage == nil || len(userMessage.Content) < 2 {
		t.Fatalf("provider request has no user message with recall tail: %+v", request.Messages)
	}
	lastBlock := userMessage.Content[len(userMessage.Content)-1]
	if !lastBlock.Synthetic || !strings.Contains(lastBlock.Text, "<auto-retrieve") || !strings.Contains(lastBlock.Text, memoryE2ETarget) {
		t.Fatalf("recall is not the synthetic user tail: %+v", lastBlock)
	}

	// Only the reranker's final Top-1 was really attached. Candidate expansion
	// and token estimation must leave both decoys untouched.
	target := archivalPassageByID(t, sessionB, targetID)
	if target.UseCount != 1 || target.LastUsedAt == "" {
		t.Fatalf("selected target usage=%d last_used_at=%q, want 1 and non-empty", target.UseCount, target.LastUsedAt)
	}
	for _, id := range []string{"memory-e2e-decoy-a", "memory-e2e-decoy-b"} {
		decoy := archivalPassageByID(t, sessionB, id)
		if decoy.UseCount != 0 || decoy.LastUsedAt != "" {
			t.Fatalf("non-selected rerank candidate %q was marked used: %+v", id, decoy)
		}
	}

	// The clean Run path synchronously records the visible exchange. The
	// provider-only synthetic recall block must neither pollute user content
	// nor replace source identity on the persisted turn.
	messages := readRecallMessages(t, root)
	var sessionBMessages []memory.Message
	for _, message := range messages {
		if message.SessionID == "memory-e2e-session-b" {
			sessionBMessages = append(sessionBMessages, message)
		}
	}
	if len(sessionBMessages) != 2 {
		t.Fatalf("session B recall message count=%d, want 2: %+v", len(sessionBMessages), sessionBMessages)
	}
	sessionBSourceID := sessionBMessages[0].SourceMessageID
	if !strings.HasPrefix(sessionBSourceID, "memory-e2e-session-b/message/") {
		t.Fatalf("session B source-message identity is not durable: %q", sessionBSourceID)
	}
	for _, message := range sessionBMessages {
		if message.SourceMessageID != sessionBSourceID || message.Scope != "session" {
			t.Fatalf("session B message provenance missing: %+v", message)
		}
		if strings.Contains(message.Content, "<auto-retrieve") || strings.Contains(message.Content, "TARGET-RECALL") {
			t.Fatalf("synthetic recall leaked into persisted message: %+v", message)
		}
	}
	if sessionBMessages[0].Role != "user" || sessionBMessages[0].Content != query {
		t.Fatalf("persisted user message=%+v, want visible query %q", sessionBMessages[0], query)
	}
	if sessionBMessages[1].Role != "assistant" || !strings.Contains(sessionBMessages[1].Content, "cross-session") {
		t.Fatalf("persisted assistant message unexpected: %+v", sessionBMessages[1])
	}

	var sessionBMeta *memory.Session
	sessions := readRecallSessions(t, root)
	for i := range sessions {
		session := sessions[i]
		if session.ID == "memory-e2e-session-b" {
			sessionBMeta = &session
			break
		}
	}
	if sessionBMeta == nil {
		t.Fatal("session B recall metadata missing")
	}
	if sessionBMeta.MsgCount != 2 || sessionBMeta.SourceSessionID != "memory-e2e-session-b" ||
		sessionBMeta.SourceMessageID != sessionBSourceID || sessionBMeta.Scope != "session" {
		t.Fatalf("session B recall metadata provenance missing: %+v", *sessionBMeta)
	}
}

func TestMemoryRepositoryE2E_OneTurnBoundaryFlushesResidualForFreshSession(t *testing.T) {
	root := t.TempDir()
	provider := &memoryRepositoryE2EProvider{}

	sessionA, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create session A memory manager: %v", err)
	}
	loopA := NewLoop(provider, newRegistryWith(t), nil, nil, "BASE MEMORY E2E SYSTEM", 3)
	loopA.Memory = sessionA
	const sessionAID = "memory-boundary-session-a"
	loopA.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionAID}
	}
	loopA.AppendUser("Please remember that my Quartz Heron workstation codename is Nimbus Finch for future sessions.")
	if err := loopA.Run(context.Background(), make(chan Event, 32)); err != nil {
		t.Fatalf("run one-turn session A: %v", err)
	}
	if got := loopA.DistillEvery; got != 5 {
		t.Fatalf("NewLoop DistillEvery=%d, want explicit default 5", got)
	}
	if got := provider.distillRequestCount(); got != 0 {
		t.Fatalf("one-turn session distilled before boundary: calls=%d", got)
	}
	if got, err := sessionA.Archival().Search(memory.SearchOptions{Query: "TARGET-RECALL"}); err != nil || len(got) != 0 {
		t.Fatalf("residual fact exists before boundary flush: passages=%+v err=%v", got, err)
	}

	launches := make(chan int, 16)
	for range 16 {
		go func() { launches <- loopA.FlushPendingDistillation(sessionAID) }()
	}
	launched := 0
	for range 16 {
		launched += <-launches
	}
	if launched != 1 {
		t.Fatalf("concurrent FlushPendingDistillation launched=%d, want exactly 1", launched)
	}
	if err := loopA.WaitForDistillation(context.Background(), sessionAID); err != nil {
		t.Fatalf("join residual distillation: %v", err)
	}
	if got := loopA.FlushPendingDistillation(sessionAID); got != 0 {
		t.Fatalf("second residual flush launched duplicate jobs=%d", got)
	}
	if got := provider.distillRequestCount(); got != 1 {
		t.Fatalf("residual distillation calls=%d, want exactly 1", got)
	}

	// A fresh manager and Loop model the next Desktop/CLI session. It must
	// retrieve the boundary-flushed fact without sharing any in-memory state
	// from session A.
	sessionB, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create fresh session B memory manager: %v", err)
	}
	seeded, err := sessionB.Archival().Search(memory.SearchOptions{Query: "TARGET-RECALL"})
	if err != nil || len(seeded) != 1 {
		t.Fatalf("boundary-distilled target passage missing: passages=%+v err=%v", seeded, err)
	}
	if got := seeded[0]; got.SourceSessionID != sessionAID ||
		!strings.HasPrefix(got.SourceMessageID, sessionAID+"/message/") {
		t.Fatalf("boundary-distilled provenance missing: %+v", got)
	}

	loopB := NewLoop(provider, newRegistryWith(t), nil, nil, "BASE MEMORY E2E SYSTEM", 3)
	loopB.SystemSections = []llm.SystemSection{{Name: "base", Body: "BASE MEMORY E2E SYSTEM", Cache: true}}
	loopB.Memory = sessionB
	loopB.AutoRetrieveK = 1
	loopB.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: "memory-boundary-session-b"}
	}
	loopB.AppendUser("What is my Quartz Heron workstation codename?")
	if err := loopB.Run(context.Background(), make(chan Event, 32)); err != nil {
		t.Fatalf("run fresh session B: %v", err)
	}
	request := provider.lastStreamRequest(t)
	if !strings.Contains(requestText(request), memoryE2ETarget) {
		t.Fatalf("fresh session B did not receive boundary recall: %+v", request.Messages)
	}
}

func TestMemoryRepositoryE2E_BoundaryFlushesAllFourResidualTurns(t *testing.T) {
	root := t.TempDir()
	provider := &memoryRepositoryE2EProvider{}
	manager, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create memory manager: %v", err)
	}
	loop := NewLoop(provider, newRegistryWith(t), nil, nil, "BASE MEMORY E2E SYSTEM", 3)
	loop.Memory = manager
	const sessionID = "memory-four-residual-turns"
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionID}
	}
	for turn := 1; turn <= 4; turn++ {
		loop.AppendUser(fmt.Sprintf(
			"Turn %d: keep this durable Quartz Heron workstation preference available to future sessions.", turn,
		))
		if err := loop.Run(context.Background(), make(chan Event, 32)); err != nil {
			t.Fatalf("run session A turn %d: %v", turn, err)
		}
	}
	if got := provider.distillRequestCount(); got != 0 {
		t.Fatalf("four-turn session distilled before cadence/boundary: calls=%d", got)
	}
	if got := loop.FlushPendingDistillation(sessionID); got != 4 {
		t.Fatalf("four-turn residual flush launched=%d, want 4", got)
	}
	if err := loop.WaitForDistillation(context.Background(), sessionID); err != nil {
		t.Fatalf("join four residual distillations: %v", err)
	}
	if got := provider.distillRequestCount(); got != 4 {
		t.Fatalf("four-turn residual provider calls=%d, want 4", got)
	}
	passages, err := manager.Archival().Search(memory.SearchOptions{Query: "TARGET-RECALL"})
	if err != nil || len(passages) != 4 {
		t.Fatalf("boundary passages=%d, want 4: %+v err=%v", len(passages), passages, err)
	}
	wantSource := make(map[string]bool, 4)
	for _, passage := range passages {
		if !strings.HasPrefix(passage.SourceMessageID, sessionID+"/message/") {
			t.Fatalf("residual source-message identity is not durable: %+v", passage)
		}
		wantSource[passage.SourceMessageID] = true
	}
	if len(wantSource) != 4 {
		t.Fatalf("residual source-message identities=%v, want 4 unique IDs", wantSource)
	}
}

func TestMemoryRepositoryE2E_DefaultCadenceDrainsWholeFiveTurnWindow(t *testing.T) {
	root := t.TempDir()
	provider := &memoryRepositoryE2EProvider{}
	manager, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create memory manager: %v", err)
	}
	loop := NewLoop(provider, newRegistryWith(t), nil, nil, "BASE MEMORY E2E SYSTEM", 3)
	loop.Memory = manager
	const sessionID = "memory-five-turn-cadence"
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionID}
	}
	for turn := 1; turn <= DefaultDistillEvery; turn++ {
		loop.AppendUser(fmt.Sprintf("Cadence turn %d carries a durable Quartz Heron workstation preference.", turn))
		if err := loop.Run(context.Background(), make(chan Event, 32)); err != nil {
			t.Fatalf("run cadence turn %d: %v", turn, err)
		}
	}
	if err := loop.WaitForDistillation(context.Background(), sessionID); err != nil {
		t.Fatalf("join cadence batch: %v", err)
	}
	if got := provider.distillRequestCount(); got != DefaultDistillEvery {
		t.Fatalf("cadence provider calls=%d, want complete %d-turn window", got, DefaultDistillEvery)
	}
	if got := loop.FlushPendingDistillation(sessionID); got != 0 {
		t.Fatalf("cadence left residual jobs=%d", got)
	}
}

func TestMemoryRepositoryE2E_FailedBoundaryDistillationIsReportedAndRetryable(t *testing.T) {
	root := t.TempDir()
	provider := &memoryRepositoryE2EProvider{}
	manager, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create memory manager: %v", err)
	}
	loop := NewLoop(provider, newRegistryWith(t), nil, nil, "BASE MEMORY E2E SYSTEM", 3)
	loop.Memory = manager
	const sessionID = "memory-boundary-retry"
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionID}
	}
	loop.AppendUser("Please remember this durable Quartz Heron workstation preference after a transient failure.")
	if err := loop.Run(context.Background(), make(chan Event, 32)); err != nil {
		t.Fatalf("run session A: %v", err)
	}

	distillErr := errors.New("transient distillation outage")
	provider.setDistillError(distillErr)
	if got := loop.FlushPendingDistillation(sessionID); got != 1 {
		t.Fatalf("first boundary flush launched=%d, want 1", got)
	}
	if err := loop.WaitForDistillation(context.Background(), sessionID); !errors.Is(err, distillErr) {
		t.Fatalf("failed boundary wait error=%v, want %v", err, distillErr)
	}

	provider.setDistillError(nil)
	if got := loop.FlushPendingDistillation(sessionID); got != 1 {
		t.Fatalf("retry boundary flush launched=%d, want 1", got)
	}
	if err := loop.WaitForDistillation(context.Background(), sessionID); err != nil {
		t.Fatalf("retry boundary wait: %v", err)
	}
	if got := provider.distillRequestCount(); got != 2 {
		t.Fatalf("distillation attempts=%d, want 2", got)
	}
	if got, err := manager.Archival().Search(memory.SearchOptions{Query: "TARGET-RECALL"}); err != nil || len(got) != 1 {
		t.Fatalf("retried distilled passage missing: passages=%+v err=%v", got, err)
	}
}

func TestCompletedTurnSourceIdentitySurvivesUndoAndHistoryReplacement(t *testing.T) {
	root := t.TempDir()
	manager, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create memory manager: %v", err)
	}
	loop := NewLoop(&memoryRepositoryE2EProvider{}, nil, nil, nil, "", 3)
	loop.Memory = manager
	loop.DistillEvery = 0
	const sessionID = "memory-source-id-stability"
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: sessionID}
	}
	appendExchange := func() {
		loop.AppendUser("Remember the same durable sentence; this text intentionally repeats after undo.")
		loop.mu.Lock()
		loop.Messages = append(loop.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "I recorded the same durable response for this completed exchange."}}})
		loop.mu.Unlock()
	}
	record := func(label string) {
		if _, err := loop.recordCompletedTurn(context.Background()); err != nil {
			t.Fatalf("record %s: %v", label, err)
		}
	}

	appendExchange()
	record("before undo")
	if !loop.UndoLastTurn() {
		t.Fatal("undo completed exchange")
	}
	appendExchange()
	record("after undo")
	// A compaction-style history replacement can reduce CountTurns too. Source
	// identity must remain unique even when the visible transcript becomes empty.
	loop.Restore(nil)
	appendExchange()
	record("after history replacement")

	messages := readRecallMessages(t, root)
	if len(messages) != 6 {
		t.Fatalf("recall messages=%d, want 6: %+v", len(messages), messages)
	}
	ids := make(map[string]bool, 3)
	for i := 0; i < len(messages); i += 2 {
		id := messages[i].SourceMessageID
		if !strings.HasPrefix(id, sessionID+"/message/") || messages[i+1].SourceMessageID != id {
			t.Fatalf("unstable exchange identity at %d: %+v %+v", i, messages[i], messages[i+1])
		}
		ids[id] = true
	}
	if len(ids) != 3 {
		t.Fatalf("source IDs reused across undo/history replacement: %v", ids)
	}
}

func TestRecordCompletedTurnExcludesSyntheticRecallContent(t *testing.T) {
	root := t.TempDir()
	manager, err := memory.NewMemoryManager(root)
	if err != nil {
		t.Fatalf("create memory manager: %v", err)
	}
	loop := NewLoop(&memoryRepositoryE2EProvider{}, nil, nil, nil, "", 3)
	loop.Memory = manager
	loop.DistillEvery = 0
	loop.CurrentStateSnapshot = func() RuntimeStateSnapshot {
		return RuntimeStateSnapshot{SessionID: "memory-synthetic-filter"}
	}
	loop.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "text", Text: "This is the person-authored durable request."},
			{Type: "text", Text: "SYNTHETIC-RECALL-MUST-NOT-PERSIST", Synthetic: true},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "This is the successful assistant response to persist."}}},
	}
	if _, err := loop.recordCompletedTurn(context.Background()); err != nil {
		t.Fatalf("record completed turn: %v", err)
	}
	messages := readRecallMessages(t, root)
	if len(messages) != 2 || messages[0].Content != "This is the person-authored durable request." {
		t.Fatalf("persisted user message contains synthetic content: %+v", messages)
	}
}
