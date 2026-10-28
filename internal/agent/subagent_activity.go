package agent

// The activity sidecar is an advisory view of complete tool boundaries. It is
// separate from replay history: opening/closing a detail view never touches a
// running child, and losing this file must never alter tool/provider execution.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
	"github.com/google/uuid"
)

const (
	subAgentActivityDirname = "subagent-activity"
	// Limits reject a view explicitly; they never cut an input/result string.
	subAgentActivityMaxBytes = 128 << 20
	subAgentActivityMaxLine  = 32 << 20
	subAgentActivityMaxCalls = 10000
)

type SubAgentActivitySnapshot struct {
	Available       bool                   `json:"available"`
	Partial         bool                   `json:"partial"`
	Reason          string                 `json:"reason,omitempty"`
	ParentSessionID string                 `json:"parentSessionId"`
	Revision        string                 `json:"revision"`
	Calls           []SubAgentActivityCall `json:"calls"`
}

// ID is a local occurrence identity. TraceCallID, when supplied by the real
// dispatcher, also pairs this call with a subsequently checkpointed message.
// ToolUseID is only the provider's public reference and may legitimately repeat.
type SubAgentActivityCall struct {
	ID                string         `json:"id"`
	TraceCallID       string         `json:"traceCallId,omitempty"`
	TraceInvocationID string         `json:"traceInvocationId,omitempty"`
	ToolUseID         string         `json:"providerToolUseId"`
	ToolName          string         `json:"name"`
	Input             map[string]any `json:"input,omitempty"`
	Result            string         `json:"output"`
	IsError           bool           `json:"isError"`
	Status            string         `json:"status"` // running, completed, error, incomplete, result_only
	StartedAt         time.Time      `json:"startedAt"`
	EndedAt           *time.Time     `json:"endedAt,omitempty"`
}

type subAgentActivityRecord struct {
	Version         int                   `json:"version"`
	Kind            string                `json:"kind"`
	AgentID         string                `json:"agent_id"`
	ParentSessionID string                `json:"parent_session_id"`
	HeaderSHA256    string                `json:"header_sha256"`
	RunID           string                `json:"run_id"`
	Call            *SubAgentActivityCall `json:"call,omitempty"`
	Partial         bool                  `json:"partial,omitempty"`
	Reason          string                `json:"reason,omitempty"`
	At              time.Time             `json:"at"`
}

// Only failed writers are retained in memory, so a same-process API can report
// a write failure even when the disk cannot accept a failure marker. Entries
// are scoped to header+run and cleared by a replacement recorder.
type subAgentActivityFailure struct{ header, run string }

var subAgentActivityFailures sync.Map

type SubAgentActivityRecorder struct {
	mu                    sync.Mutex
	path                  string
	f                     *os.File
	owner                 subAgentActivityRecord
	next                  uint64
	pending               map[string]SubAgentActivityCall
	partial               bool
	writeErr              error
	closed                bool
	bytes                 int64
	scopeBound            bool
	parentToolUseID       string
	loopTraceInvocationID string
}

func subAgentActivityPath(sessionDir, agentID string) (string, error) {
	if sessionDir == "" || !ValidSubAgentID(agentID) {
		return "", fmt.Errorf("subagent activity: valid sessionDir + agentID required")
	}
	return filepath.Join(sessionDir, subAgentActivityDirname, agentID+".jsonl"), nil
}

func subAgentActivityHeader(sessionDir, agentID string) (pubsess.Header, string, error) {
	path, err := subAgentTranscriptPath(sessionDir, agentID)
	if err != nil {
		return pubsess.Header{}, "", err
	}
	if err := subAgentActivityCheckDir(filepath.Dir(path)); err != nil {
		return pubsess.Header{}, "", err
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return pubsess.Header{}, "", fmt.Errorf("subagent activity requires a regular transcript")
	}
	header, err := LoadSubAgentHeader(sessionDir, agentID)
	if err != nil {
		return pubsess.Header{}, "", err
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return pubsess.Header{}, "", err
	}
	hash := sha256.Sum256(raw)
	return header, hex.EncodeToString(hash[:]), nil
}

func subAgentActivityCheckDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("subagent activity directory must be a real directory")
	}
	return nil
}

// NewSubAgentActivityRecorder must be called after creating/resuming the child
// transcript. A new run atomically replaces its own sidecar. An older producer
// retains its old inode, so its late writes cannot contaminate this run.
func NewSubAgentActivityRecorder(sessionDir, agentID string) (*SubAgentActivityRecorder, error) {
	path, err := subAgentActivityPath(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	header, hash, err := subAgentActivityHeader(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	// A failed resume setup must not expose a previous run that happens to
	// retain the same immutable transcript header.
	subAgentActivityFailures.Store(path, subAgentActivityFailure{header: hash})
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := subAgentActivityCheckDir(dir); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".activity-*.tmp")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
			_ = os.Remove(tmp)
			subAgentActivityFailures.Store(path, subAgentActivityFailure{header: hash})
		}
	}()
	r := &SubAgentActivityRecorder{path: path, f: f, pending: make(map[string]SubAgentActivityCall), owner: subAgentActivityRecord{
		Version: 1, AgentID: agentID, ParentSessionID: header.SubAgentOf, HeaderSHA256: hash, RunID: uuid.NewString(),
	}}
	if err := r.appendLocked("run", nil, ""); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	if err := syncSubAgentDirectory(dir); err != nil {
		return nil, err
	}
	subAgentActivityFailures.Delete(path)
	keep = true
	return r, nil
}

type subAgentActivityContextKey struct{}

// A nil recorder shadows inheritance: an unpersisted grandchild must never
// write its boundaries into its parent's sidecar. A real recorder binds once
// to this child's immutable loop identity and public parent reference. Native
// child events are already stamped with both by Loop.emit; nonempty parent
// references do not indicate that the event was forwarded from a descendant.
func WithSubAgentActivityRecorder(ctx context.Context, recorder *SubAgentActivityRecorder) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if recorder != nil {
		invocation := TraceInvocationIDFromContext(ctx)
		if invocation == "" {
			// Direct Agent.Execute embeddings may bypass dispatch, which normally
			// supplies this identity. Give their child loop the same strict scope.
			invocation = NewTraceInvocationID()
			ctx = WithTraceInvocationID(ctx, invocation)
		}
		recorder.mu.Lock()
		if !recorder.scopeBound {
			recorder.scopeBound = true
			recorder.parentToolUseID = ParentToolUseIDFromContext(ctx)
			recorder.loopTraceInvocationID = invocation
		}
		recorder.mu.Unlock()
	}
	return context.WithValue(ctx, subAgentActivityContextKey{}, recorder)
}

func ObserveSubAgentActivity(ctx context.Context, event Event) error {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(subAgentActivityContextKey{}).(*SubAgentActivityRecorder)
	return recorder.Observe(event)
}

func CloseSubAgentActivity(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(subAgentActivityContextKey{}).(*SubAgentActivityRecorder)
	return recorder.Close()
}

// Observe consumes only native complete tool boundaries, before best-effort
// forwarding truncates/drops them. It never executes tools. Callers must treat
// its errors as advisory, without changing provider results or budget writes.
func (r *SubAgentActivityRecorder) Observe(event Event) error {
	if r == nil || (event.Kind != EventToolStart && event.Kind != EventToolResult) {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scopeBound {
		// Ordinary tools inherit the loop invocation. A native Agent/Fork/Ralph
		// call instead carries its child's new invocation and names this loop as
		// TraceParentInvocationID. A descendant's or sibling's tool matches
		// neither, even when the provider reuses the same public parent ID.
		nativeInvocation := event.TraceInvocationID == r.loopTraceInvocationID
		nativeChildCall := isTraceChildTool(event.ToolName) && event.TraceParentInvocationID == r.loopTraceInvocationID
		if event.SubAgentParentID != r.parentToolUseID || (!nativeInvocation && !nativeChildCall) {
			return nil
		}
	} else if event.SubAgentParentID != "" {
		// Unbound recorders support explicit local synthetic/legacy events only;
		// actual child runners always bind their scope through the context helper.
		return nil
	}
	if r.closed {
		return fmt.Errorf("subagent activity recorder is closed")
	}
	if r.writeErr != nil {
		return r.writeErr
	}
	now := time.Now().UTC()
	if event.Kind == EventToolStart {
		if r.next >= subAgentActivityMaxCalls {
			return r.failLocked(fmt.Errorf("subagent activity call limit exceeded"))
		}
		r.next++
		call := SubAgentActivityCall{
			ID: fmt.Sprintf("%s-%d", r.owner.RunID, r.next), TraceCallID: event.TraceCallID,
			TraceInvocationID: event.TraceInvocationID, ToolUseID: event.ToolUseID, ToolName: event.ToolName,
			Input: clonePresentation(event.ToolInput), Status: "running", StartedAt: now,
		}
		if err := r.appendLocked("start", &call, ""); err != nil {
			return err
		}
		r.pending[call.ID] = call
		return nil
	}
	if event.ToolResult == nil {
		r.partial = true
		return r.appendLocked("partial", nil, "missing_tool_result")
	}
	// Match a real trace occurrence first. Legacy events without a trace are
	// paired only when exactly one pending call fits; ambiguity stays explicit.
	var matched SubAgentActivityCall
	count := 0
	for _, pending := range r.pending {
		matches := false
		if event.TraceCallID != "" {
			matches = pending.TraceCallID == event.TraceCallID && pending.ToolUseID == event.ToolUseID && pending.ToolName == event.ToolName
		} else {
			matches = pending.TraceCallID == "" && pending.ToolUseID == event.ToolUseID && pending.ToolName == event.ToolName
		}
		if matches {
			matched, count = pending, count+1
		}
	}
	reason := ""
	if count != 1 {
		r.partial = true
		r.next++
		matched = SubAgentActivityCall{
			ID: fmt.Sprintf("%s-%d", r.owner.RunID, r.next), TraceCallID: event.TraceCallID,
			TraceInvocationID: event.TraceInvocationID, ToolUseID: event.ToolUseID, ToolName: event.ToolName,
			Status: "result_only",
		}
		reason = "unmatched_tool_result"
	} else {
		matched.Status = "completed"
		if event.ToolResult.IsError {
			matched.Status = "error"
		}
	}
	matched.Result, matched.IsError, matched.EndedAt = event.ToolResult.Output, event.ToolResult.IsError, &now
	if err := r.appendLocked("result", &matched, reason); err != nil {
		return err
	}
	delete(r.pending, matched.ID)
	return nil
}

func (r *SubAgentActivityRecorder) failLocked(err error) error {
	r.writeErr = err
	subAgentActivityFailures.Store(r.path, subAgentActivityFailure{r.owner.HeaderSHA256, r.owner.RunID})
	return err
}

func (r *SubAgentActivityRecorder) appendLocked(kind string, call *SubAgentActivityCall, reason string) error {
	record := r.owner
	record.Kind, record.Call, record.At = kind, call, time.Now().UTC()
	record.Partial, record.Reason = r.partial, reason
	line, err := json.Marshal(record)
	if err != nil {
		return r.failLocked(fmt.Errorf("encode subagent activity: %w", err))
	}
	line = append(line, '\n')
	if len(line) > subAgentActivityMaxLine || r.bytes+int64(len(line)) > subAgentActivityMaxBytes {
		return r.failLocked(fmt.Errorf("subagent activity size limit exceeded"))
	}
	n, err := r.f.Write(line)
	r.bytes += int64(n)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = r.f.Sync()
	}
	if err != nil {
		return r.failLocked(fmt.Errorf("write subagent activity: %w", err))
	}
	return nil
}

// Close marks unmatched starts incomplete, never successful. It belongs to the
// child runner (including background runners), not the parent tool's return.
func (r *SubAgentActivityRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.writeErr
	}
	r.closed = true
	if len(r.pending) != 0 {
		r.partial = true
	}
	if r.writeErr == nil {
		_ = r.appendLocked("close", nil, "")
	}
	closeErr := r.f.Close()
	if r.writeErr != nil {
		return r.writeErr
	}
	return closeErr
}

// SubAgentActivityRevision lets live list samplers avoid decoding tool bodies
// when nothing changed. Normal reads inspect only the transcript header and
// file metadata. A failed-writer marker needs one small sidecar header read to
// distinguish an old producer's failure from the current run.
func SubAgentActivityRevision(sessionDir, agentID string) string {
	path, err := subAgentActivityPath(sessionDir, agentID)
	if err != nil {
		return ""
	}
	_, hash, err := subAgentActivityHeader(sessionDir, agentID)
	if err != nil {
		return ""
	}
	failureValue, failed := subAgentActivityFailures.Load(path)
	if failed && failureValue == (subAgentActivityFailure{header: hash}) {
		return hash
	}
	if err := subAgentActivityCheckDir(filepath.Dir(path)); err != nil {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	revision := hash + ":" + subAgentFileRevision(info)
	if failed {
		failure := failureValue.(subAgentActivityFailure)
		if failure.header == hash {
			f, err := os.Open(path)
			if err == nil {
				line, readErr := bufio.NewReader(io.LimitReader(f, 64<<10)).ReadBytes('\n')
				_ = f.Close()
				var first subAgentActivityRecord
				if readErr == nil && json.Unmarshal(line, &first) == nil && first.RunID == failure.run {
					revision += ":write-error"
				}
			}
		}
	}
	return revision
}

// LoadSubAgentActivity takes a bounded-at-open, read-only snapshot. A missing
// sidecar is unavailable (for example an older runtime); malformed complete
// records fail visibly. An unacknowledged crash tail is ignored and signalled.
func LoadSubAgentActivity(sessionDir, agentID string) (SubAgentActivitySnapshot, error) {
	result := SubAgentActivitySnapshot{Calls: make([]SubAgentActivityCall, 0)}
	path, err := subAgentActivityPath(sessionDir, agentID)
	if err != nil {
		return result, err
	}
	header, hash, err := subAgentActivityHeader(sessionDir, agentID)
	if err != nil {
		return result, err
	}
	result.ParentSessionID, result.Revision = header.SubAgentOf, hash
	if failure, ok := subAgentActivityFailures.Load(path); ok && failure == (subAgentActivityFailure{header: hash}) {
		result.Partial, result.Reason = true, "initialization_error"
		return result, nil
	}
	if err := subAgentActivityCheckDir(filepath.Dir(path)); err != nil {
		if os.IsNotExist(err) {
			result.Reason = "missing"
			return result, nil
		}
		return result, err
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		result.Reason = "missing"
		return result, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return result, fmt.Errorf("subagent activity is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		return result, fmt.Errorf("subagent activity changed while opening")
	}
	result.Revision += ":" + subAgentFileRevision(info)
	if info.Size() > subAgentActivityMaxBytes {
		result.Partial, result.Reason = true, "size_limit"
		return result, nil
	}
	reader := bufio.NewReader(io.LimitReader(f, info.Size()))
	indices := make(map[string]int)
	runID, closed := "", false
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if readErr != io.EOF {
				return result, fmt.Errorf("read subagent activity: %w", readErr)
			}
			if len(line) != 0 {
				result.Partial, result.Reason = true, "incomplete_tail"
			}
			break
		}
		if len(line) > subAgentActivityMaxLine {
			return result, fmt.Errorf("subagent activity line %d exceeds size limit", lineNumber)
		}
		var record subAgentActivityRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return result, fmt.Errorf("subagent activity line %d: %w", lineNumber, err)
		}
		if record.Version != 1 || record.AgentID != agentID || record.HeaderSHA256 != hash || record.ParentSessionID != header.SubAgentOf {
			result.Available, result.Partial, result.Reason, result.Calls = false, true, "owner_mismatch", []SubAgentActivityCall{}
			return result, nil
		}
		if lineNumber == 1 {
			if record.Kind != "run" || record.RunID == "" {
				return result, fmt.Errorf("subagent activity has no valid run header")
			}
			runID, result.Available = record.RunID, true
			continue
		}
		if record.RunID != runID || closed {
			return result, fmt.Errorf("subagent activity line %d belongs to another or closed run", lineNumber)
		}
		if record.Partial || record.Reason != "" {
			result.Partial = true
			if record.Reason != "" {
				result.Reason = record.Reason
			}
		}
		switch record.Kind {
		case "start", "result":
			if record.Call == nil || record.Call.ID == "" {
				return result, fmt.Errorf("subagent activity line %d has no call", lineNumber)
			}
			call := *record.Call
			index, exists := indices[call.ID]
			if record.Kind == "start" {
				if exists || call.Status != "running" || call.EndedAt != nil {
					return result, fmt.Errorf("subagent activity line %d has an invalid start", lineNumber)
				}
			} else if exists {
				start := result.Calls[index]
				if start.Status != "running" || start.TraceCallID != call.TraceCallID || start.ToolUseID != call.ToolUseID || start.ToolName != call.ToolName || call.EndedAt == nil || (call.Status != "completed" && call.Status != "error") {
					return result, fmt.Errorf("subagent activity line %d has an invalid result pairing", lineNumber)
				}
			} else if call.Status != "result_only" || call.EndedAt == nil {
				return result, fmt.Errorf("subagent activity line %d has an unscoped result", lineNumber)
			}
			if !exists {
				if len(result.Calls) >= subAgentActivityMaxCalls {
					result.Partial, result.Reason = true, "call_limit"
					return result, nil
				}
				indices[call.ID] = len(result.Calls)
				result.Calls = append(result.Calls, call)
			} else {
				result.Calls[index] = call
			}
		case "partial":
			result.Partial = true
		case "close":
			closed = true
			for i := range result.Calls {
				if result.Calls[i].Status == "running" {
					result.Calls[i].Status = "incomplete"
					result.Calls[i].EndedAt = &record.At
				}
			}
		default:
			return result, fmt.Errorf("subagent activity line %d has unknown kind %q", lineNumber, record.Kind)
		}
	}
	if runID == "" {
		result.Available, result.Partial, result.Reason = false, true, "incomplete_header"
	}
	if failure, ok := subAgentActivityFailures.Load(path); ok && failure == (subAgentActivityFailure{hash, runID}) {
		result.Partial, result.Reason = true, "write_error"
		result.Revision += ":write-error"
	}
	// The transcript can be replaced while a bounded snapshot is being read.
	// Recheck its immutable identity before returning any saved tool values.
	_, currentHash, err := subAgentActivityHeader(sessionDir, agentID)
	if err != nil {
		return SubAgentActivitySnapshot{}, err
	}
	if currentHash != hash {
		result.Available, result.Partial, result.Reason, result.Calls = false, true, "owner_mismatch", []SubAgentActivityCall{}
	}
	return result, nil
}
