package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

const (
	subAgentHistoryMaxBytes  = 16 << 20
	subAgentHistoryMaxScan   = 4096
	subAgentHistoryMaxText   = 64 << 10
	subAgentHistoryScanBytes = 64 << 20
)

var errSubAgentQueryNotFound = errors.New("sub-agent not found in the current parent session")
var errSubAgentQueryReadBudget = errors.New("historical query byte budget reached")

// SubAgentQueryScope binds read-only historical tools to one direct parent.
// A configured scope never falls back to unscoped Roster access.
type SubAgentQueryScope struct {
	SessionDir      string
	ParentSessionID string
}

// subAgentQueryScope is immutable per registry generation. A non-nil scope
// with an empty parent must fail closed until startup rebinding completes.
type subAgentQueryScope struct {
	sessionDir string
	parentID   string
}

type subAgentHistoryRecord struct {
	id, name, status, loadState string
	executionPhase              string
	output, stopReason          string
	diagnostic                  string
	started, ended              time.Time
	background                  bool
	partial, incompleteTail     bool
	continuationAttempts        int
	canResume                   bool
}

// Stop hints in old transcripts are not necessarily host-authored. Keep only
// known machine codes; raw provider errors belong neither in a reason nor in a
// parent-facing diagnostic. Typed live errors are classified separately.
func safeSubAgentHistoryReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || agent.IsIncompleteStopReason(reason) || agent.IsContentFilterStopReason(reason) {
		return reason
	}
	switch reason {
	case "end_turn", "stop", "stop_sequence", "tool_use", "tool_calls", "no_tool_calls", "cancelled", "ctx_cancelled", "permission_denied", "panic", "setup_failed", "admission_failed", "api_error", "execution_error", "timeout":
		return reason
	}
	if strings.HasPrefix(reason, "timeout ") {
		if duration, err := time.ParseDuration(strings.TrimPrefix(reason, "timeout ")); err == nil && duration > 0 {
			return "timeout " + duration.String()
		}
	}
	return "execution_error"
}

func queryText(s string, limit int) string {
	s = security.RedactSubprocessText(s)
	return clipQueryText(s, limit)
}

func clipQueryText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func queryLine(s string, limit int) string {
	return strings.Join(strings.Fields(queryText(s, limit)), " ")
}

func subAgentQueryError(code, message string) *tools.Result {
	return &tools.Result{Output: code + ": " + queryText(message, 1024), IsError: true,
		Meta: map[string]any{"query_state": code}, Presentation: map[string]any{"queryState": code}}
}

// Root-relative opens confine even a concurrent symlink replacement. Lstat
// checks also reject links whose targets would remain inside the root.
func (s subAgentQueryScope) openRoot() (*os.Root, error) {
	if s.sessionDir == "" || !agent.ValidSubAgentID(s.parentID) {
		return nil, errors.New("historical query scope is not bound to a parent session")
	}
	info, err := os.Lstat(s.sessionDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("session storage must be a regular directory, not a symbolic link")
	}
	parent, err := os.OpenRoot(s.sessionDir)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	openedSession, statErr := parent.Stat(".")
	currentSession, pathErr := os.Lstat(s.sessionDir)
	if statErr != nil || pathErr != nil || currentSession.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, openedSession) || !os.SameFile(openedSession, currentSession) {
		return nil, errors.New("session storage changed while opening")
	}
	before, err := parent.Lstat(agent.SubAgentTranscriptDirname)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("sub-agent storage must be a regular directory, not a symbolic link")
	}
	root, err := parent.OpenRoot(agent.SubAgentTranscriptDirname)
	if err != nil {
		return nil, err
	}
	opened, statErr := root.Stat(".")
	after, pathErr := parent.Lstat(agent.SubAgentTranscriptDirname)
	if statErr != nil || pathErr != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		root.Close()
		return nil, errors.New("sub-agent storage changed while opening")
	}
	return root, nil
}

func (s subAgentQueryScope) openOwned(root *os.Root, id string) (*os.File, os.FileInfo, pubsess.Header, error) {
	if !agent.ValidSubAgentID(id) {
		return nil, nil, pubsess.Header{}, errors.New("invalid sub-agent ID")
	}
	name := id + ".jsonl"
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, pubsess.Header{}, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, pubsess.Header{}, errors.New("stored sub-agent must be a regular file, not a symbolic link")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, pubsess.Header{}, err
	}
	info, statErr := f.Stat()
	after, pathErr := root.Lstat(name)
	if statErr != nil || pathErr != nil || after.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !os.SameFile(before, info) || !os.SameFile(info, after) {
		f.Close()
		return nil, nil, pubsess.Header{}, errors.New("stored sub-agent changed while opening")
	}
	var header struct {
		Type   string          `json:"type"`
		Header *pubsess.Header `json:"header"`
	}
	// Read just the immutable first record before admitting the owner's body.
	var first []byte
	complete := false
	buf := make([]byte, 1024)
	for len(first) < 64<<10 {
		n, readErr := f.Read(buf[:min(len(buf), (64<<10)-len(first))])
		first = append(first, buf[:n]...)
		if index := bytes.IndexByte(first, '\n'); index >= 0 {
			first = first[:index]
			complete = true
			break
		}
		if readErr != nil {
			f.Close()
			return nil, nil, pubsess.Header{}, errors.New("stored sub-agent has no complete bounded first header")
		}
	}
	if !complete || json.Unmarshal(first, &header) != nil || header.Type != "header" || header.Header == nil || header.Header.ID != id {
		f.Close()
		return nil, nil, pubsess.Header{}, errors.New("stored sub-agent has an invalid first header")
	}
	if header.Header.SubAgentOf != s.parentID {
		f.Close()
		return nil, nil, pubsess.Header{}, errSubAgentQueryNotFound
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, pubsess.Header{}, errors.New("stored sub-agent is not seekable")
	}
	return f, info, *header.Header, nil
}

type subAgentQueryTerminal struct {
	agent.SubAgentTerminal
	ContinuationAttempts int `json:"continuationAttempts,omitempty"`
}

func (s subAgentQueryScope) readRecord(root *os.Root, id string, live *agent.Teammate, bodyBudget ...*int64) (subAgentHistoryRecord, error) {
	f, before, header, err := s.openOwned(root, id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && live != nil {
			return s.liveRecord(id, live, true)
		}
		return subAgentHistoryRecord{}, err
	}
	defer f.Close()
	record := subAgentHistoryRecord{id: id, name: header.TeammateName, status: "unknown", loadState: "not_loaded", started: header.CreatedAt}
	if live != nil {
		return s.liveRecord(id, live, false)
	}
	if before.Size() > subAgentHistoryMaxBytes {
		return record, fmt.Errorf("stored sub-agent exceeds the %d-byte historical query limit; use Desktop details", subAgentHistoryMaxBytes)
	}
	if len(bodyBudget) > 0 && bodyBudget[0] != nil {
		if before.Size() > *bodyBudget[0] {
			return record, errSubAgentQueryReadBudget
		}
		*bodyBudget[0] -= before.Size()
	}
	data, err := io.ReadAll(io.LimitReader(f, before.Size()))
	if err != nil || int64(len(data)) != before.Size() {
		return record, errors.New("stored sub-agent could not be read completely")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		record.incompleteTail = true
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	var terminal *subAgentQueryTerminal
	length := 0
	for index, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry struct {
			Type       string                 `json:"type"`
			Header     *pubsess.Header        `json:"header,omitempty"`
			Message    *llm.Message           `json:"message,omitempty"`
			Terminal   *subAgentQueryTerminal `json:"terminal,omitempty"`
			Checkpoint *struct {
				Offset   int           `json:"offset"`
				Reset    bool          `json:"reset"`
				Messages []llm.Message `json:"messages"`
			} `json:"checkpoint,omitempty"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return record, fmt.Errorf("stored sub-agent contains an invalid record at line %d", index+1)
		}
		switch entry.Type {
		case "header":
			if entry.Header == nil || (entry.Header.ID != "" && entry.Header.ID != id) || (entry.Header.SubAgentOf != "" && entry.Header.SubAgentOf != s.parentID) {
				return record, errors.New("stored sub-agent identity changed")
			}
			// Later canonical header records patch mutable metadata. Ownership
			// remains bound to the immutable first header checked by openOwned.
			if entry.Header.TeammateName != "" {
				record.name = entry.Header.TeammateName
			}
		case "terminal":
			if entry.Terminal == nil {
				return record, errors.New("stored sub-agent terminal is missing")
			}
			terminal = entry.Terminal
		case "message":
			if entry.Message == nil {
				return record, errors.New("stored sub-agent message is missing")
			}
			length++
			updateHistoricalText(&record, *entry.Message)
		case "checkpoint":
			cp := entry.Checkpoint
			if cp == nil || cp.Offset < 0 || (cp.Reset && cp.Offset != 0) {
				return record, errors.New("stored sub-agent checkpoint is invalid")
			}
			if cp.Reset {
				length, record.output = 0, ""
			}
			if cp.Offset != length {
				return record, errors.New("stored sub-agent checkpoint history does not match its offset")
			}
			for _, message := range cp.Messages {
				updateHistoricalText(&record, message)
			}
			length += len(cp.Messages)
			terminal = nil
		case "execution", "handoff", "followup", "followup_ack":
			// These records never change the output's terminal authority.
		default:
			return record, errors.New("stored sub-agent contains an unsupported record type")
		}
	}
	after, statErr := f.Stat()
	current, pathErr := root.Lstat(id + ".jsonl")
	if statErr != nil || pathErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || !os.SameFile(after, current) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return record, errors.New("stored sub-agent changed while reading; retry the query")
	}
	if terminal != nil {
		status := agent.StatusFailed
		switch terminal.Status {
		case "completed":
			status = agent.StatusCompleted
		case "killed":
			status = agent.StatusKilled
		case "failed":
		default:
			return record, errors.New("stored sub-agent terminal status is invalid")
		}
		record.status = terminal.Status
		record.output = terminal.Output
		if terminal.Result != "" {
			record.output = terminal.Result
		}
		// ExitError may contain a complete provider HTTP body. Never stringify
		// or parse it: cold queries classify only persisted host machine codes.
		record.stopReason, record.diagnostic, record.canResume = subAgentFailureDetails(agent.TeammateSnapshot{
			Status: status, StopHint: safeSubAgentHistoryReason(terminal.StopHint),
		})
		record.ended, record.background = terminal.EndedAt, terminal.Background
		record.continuationAttempts = terminal.ContinuationAttempts
	} else {
		record.diagnostic = "No terminal receipt is recorded. This producer is not loaded in this process; its saved snapshot does not prove completion or current execution."
	}
	record.partial = record.status != "completed" && record.output != ""
	return record, nil
}

func (s subAgentQueryScope) liveRecord(id string, live *agent.Teammate, requireOwner bool) (subAgentHistoryRecord, error) {
	snapshot := live.Snapshot()
	if snapshot.AgentID != id || ((requireOwner || snapshot.ParentSessionID != "") && snapshot.ParentSessionID != s.parentID) {
		return subAgentHistoryRecord{}, errSubAgentQueryNotFound
	}
	record := subAgentHistoryRecord{
		id: id, name: snapshot.Name, status: snapshot.Status.String(), loadState: "live",
		output: snapshot.Output, stopReason: snapshot.StopHint, started: snapshot.Started,
		ended: snapshot.EndTime, background: snapshot.Background, executionPhase: snapshot.ExecutionPhase,
		continuationAttempts: snapshot.ContinuationAttempts,
	}
	if !snapshot.Status.IsActive() && snapshot.Result != "" {
		record.output = snapshot.Result
	}
	snapshot.StopHint = safeSubAgentHistoryReason(snapshot.StopHint)
	record.stopReason, record.diagnostic, record.canResume = subAgentFailureDetails(snapshot)
	record.partial = snapshot.Status != agent.StatusCompleted && record.output != ""
	return record, nil
}

func updateHistoricalText(record *subAgentHistoryRecord, message llm.Message) {
	if message.Role != llm.RoleAssistant {
		return
	}
	var parts []string
	for _, block := range message.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) > 0 {
		record.output = strings.Join(parts, "")
	}
}

type subAgentHistoryCatalog struct {
	records          []subAgentHistoryRecord
	readErrors       int
	scanIncomplete   bool
	budgetIncomplete bool
}

func (s subAgentQueryScope) catalog(ctx context.Context, roster *agent.Roster) (subAgentHistoryCatalog, error) {
	var result subAgentHistoryCatalog
	live := make(map[string]*agent.Teammate)
	if roster != nil {
		for _, teammate := range roster.List() {
			live[teammate.AgentID] = teammate
		}
	}
	root, err := s.openRoot()
	if errors.Is(err, fs.ErrNotExist) {
		for id, teammate := range live {
			if record, ownerErr := s.liveRecord(id, teammate, true); ownerErr == nil {
				result.records = append(result.records, record)
			}
		}
		sortHistoryRecords(result.records)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return result, err
	}
	defer dir.Close()
	seen := make(map[string]bool)
	visited := 0
	remainingBytes := int64(subAgentHistoryScanBytes)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			visited++
			if visited > subAgentHistoryMaxScan {
				result.scanIncomplete = true
				break
			}
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".jsonl")
			record, recordErr := s.readRecord(root, id, live[id], &remainingBytes)
			if recordErr != nil {
				if errors.Is(recordErr, errSubAgentQueryReadBudget) {
					result.budgetIncomplete = true
					break
				}
				if !errors.Is(recordErr, errSubAgentQueryNotFound) {
					result.readErrors++
				}
				continue
			}
			result.records = append(result.records, record)
			seen[id] = true
		}
		if result.scanIncomplete || result.budgetIncomplete || errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return result, readErr
		}
	}
	// Live entries beyond the scan bound still require a persisted owner
	// check; they cannot bypass a session boundary by remaining in Roster.
	for id, teammate := range live {
		if seen[id] {
			continue
		}
		record, err := s.readRecord(root, id, teammate)
		if err == nil {
			result.records = append(result.records, record)
		}
	}
	sortHistoryRecords(result.records)
	return result, nil
}

func sortHistoryRecords(records []subAgentHistoryRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].started.Equal(records[j].started) {
			return records[i].id < records[j].id
		}
		return records[i].started.After(records[j].started)
	})
}

func (record subAgentHistoryRecord) presentation() map[string]any {
	return map[string]any{
		"agentId": record.id, "name": queryLine(record.name, 160), "status": record.status,
		"loadState": record.loadState, "stopReason": queryLine(record.stopReason, 128),
		"executionPhase": record.executionPhase,
		"diagnostic":     record.diagnostic, "partialOutput": record.partial,
		"continuationAttempts": record.continuationAttempts,
		"canResume":            record.status == "completed" || record.canResume,
	}
}

func (record subAgentHistoryRecord) summary() string {
	duration := time.Since(record.started)
	if !record.ended.IsZero() {
		duration = record.ended.Sub(record.started)
	}
	if record.started.IsZero() || duration < 0 {
		duration = 0
	}
	line := fmt.Sprintf("agent_id=%s name=%q status=%s load_state=%s elapsed=%s background=%v", record.id, queryLine(record.name, 160), record.status, record.loadState, duration.Round(time.Second), record.background)
	if record.executionPhase != "" {
		line += " execution_phase=" + queryLine(record.executionPhase, 64)
	}
	if record.stopReason != "" {
		line += " stop_reason=" + queryLine(record.stopReason, 128)
	}
	if record.diagnostic != "" {
		line += fmt.Sprintf(" diagnostic=%q", queryLine(record.diagnostic, 256))
	}
	return line
}

func queryPageInt(input map[string]any, key string, fallback, maximum int) (int, error) {
	raw, exists := input[key]
	if !exists {
		return fallback, nil
	}
	value := -1
	switch v := raw.(type) {
	case int:
		value = v
	case float64:
		if v >= 0 && v <= float64(maximum) && v == float64(int(v)) {
			value = int(v)
		}
	}
	if value < 0 || value > maximum || (key == "limit" && value == 0) {
		minimum := 0
		if key == "limit" {
			minimum = 1
		}
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, minimum, maximum)
	}
	return value, nil
}

func (s SubAgentList) executeHistorical(ctx context.Context, input map[string]any) (*tools.Result, error) {
	limit, err := queryPageInt(input, "limit", 50, 100)
	if err != nil {
		return subAgentQueryError("read_error", err.Error()), nil
	}
	offset, err := queryPageInt(input, "offset", 0, subAgentHistoryMaxScan)
	if err != nil {
		return subAgentQueryError("read_error", err.Error()), nil
	}
	catalog, err := s.history.catalog(ctx, s.roster)
	if err != nil {
		return subAgentQueryError("read_error", "Cannot inspect saved sub-agents: "+err.Error()), nil
	}
	var lines []string
	end := min(len(catalog.records), offset+limit)
	for i := min(offset, len(catalog.records)); i < end; i++ {
		record := catalog.records[i]
		line := record.summary()
		if record.output != "" {
			line += fmt.Sprintf(" result=%q", queryLine(record.output, 160))
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "(no matching sub-agents in this parent-session query page)")
	}
	lines = append(lines, fmt.Sprintf("query_page offset=%d shown=%d known_total=%d", offset, max(0, end-min(offset, len(catalog.records))), len(catalog.records)))
	if end < len(catalog.records) {
		lines = append(lines, fmt.Sprintf("more_saved_records=true next_offset=%d; use SubAgentList with offset or SubAgentOutput with agent_id", end))
	}
	if catalog.scanIncomplete || catalog.budgetIncomplete || catalog.readErrors > 0 {
		lines = append(lines, fmt.Sprintf("history_incomplete=true scan_limit_reached=%v byte_budget_reached=%v read_errors=%d; omitted records are not evidence of completion or deletion; query a known agent_id directly", catalog.scanIncomplete, catalog.budgetIncomplete, catalog.readErrors))
	}
	return &tools.Result{Output: strings.Join(lines, "\n"), Meta: map[string]any{"history_incomplete": catalog.scanIncomplete || catalog.budgetIncomplete || catalog.readErrors > 0, "known_total": len(catalog.records), "next_offset": end}}, nil
}

func (s SubAgentOutput) executeHistorical(ctx context.Context, input map[string]any) (*tools.Result, error) {
	id, _ := input["agent_id"].(string)
	name, _ := input["name"].(string)
	if id == "" && strings.TrimSpace(name) == "" {
		return subAgentQueryError("read_error", "either agent_id or name is required"), nil
	}
	var record subAgentHistoryRecord
	if id != "" {
		if !agent.ValidSubAgentID(id) {
			return subAgentQueryError("read_error", "invalid sub-agent ID"), nil
		}
		var live *agent.Teammate
		if s.roster != nil {
			live, _ = s.roster.LookupByAgentID(id)
		}
		root, err := s.history.openRoot()
		if errors.Is(err, fs.ErrNotExist) {
			if live == nil {
				return subAgentQueryError("not_found", errSubAgentQueryNotFound.Error()), nil
			}
			record, err = s.history.liveRecord(id, live, true)
		} else if err != nil {
			return subAgentQueryError("read_error", err.Error()), nil
		} else {
			defer root.Close()
			record, err = s.history.readRecord(root, id, live)
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errSubAgentQueryNotFound) {
			return subAgentQueryError("not_found", errSubAgentQueryNotFound.Error()), nil
		}
		if err != nil {
			return subAgentQueryError("read_error", err.Error()), nil
		}
	} else {
		if len(name) > 256 {
			return subAgentQueryError("read_error", "name is too long; use agent_id"), nil
		}
		catalog, err := s.history.catalog(ctx, s.roster)
		if err != nil {
			return subAgentQueryError("read_error", err.Error()), nil
		}
		var matches []subAgentHistoryRecord
		for _, candidate := range catalog.records {
			if candidate.name == name {
				matches = append(matches, candidate)
			}
		}
		if len(matches) > 1 {
			return subAgentQueryError("ambiguous", "multiple saved sub-agents have this name; use the stable agent_id from SubAgentList"), nil
		}
		if catalog.scanIncomplete || catalog.budgetIncomplete || catalog.readErrors > 0 {
			return subAgentQueryError("read_error", "historical name lookup is incomplete; query the exact agent_id instead"), nil
		}
		if len(matches) == 0 {
			return subAgentQueryError("not_found", errSubAgentQueryNotFound.Error()), nil
		}
		record = matches[0]
	}
	if err := ctx.Err(); err != nil {
		return subAgentQueryError("read_error", "sub-agent query was cancelled"), nil
	}
	text := record.summary() + "\npartial_output=" + fmt.Sprint(record.partial) + " continuation_attempts=" + fmt.Sprint(record.continuationAttempts) + "\n"
	if record.stopReason != "" {
		text += "stop_reason=" + queryLine(record.stopReason, 128) + "\n"
	}
	if record.diagnostic != "" {
		text += "---diagnostic---\n" + queryText(record.diagnostic, 1024) + "\n"
	}
	if record.incompleteTail {
		text += "incomplete_tail=true (unacknowledged final bytes were ignored)\n"
	}
	safeOutput := security.RedactSubprocessText(record.output)
	outputTruncated := len(safeOutput) > subAgentHistoryMaxText
	if safeOutput != "" {
		text += "---output---\n" + clipQueryText(safeOutput, subAgentHistoryMaxText)
		if outputTruncated {
			text += "\noutput_truncated=true; use Desktop details for the remaining saved text"
		}
	}
	return &tools.Result{Output: strings.TrimSpace(text), Meta: map[string]any{"query_state": record.loadState, "output_truncated": outputTruncated}, Presentation: map[string]any{"subagent": record.presentation(), "queryState": record.loadState}}, nil
}
