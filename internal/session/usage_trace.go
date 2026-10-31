package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ReadUsageEvents scans only persisted token rows, without loading an entire
// conversation into the trace index. The caller reconciles these observations
// with the authoritative session ledger; historical model identities can be
// absent and must not be inferred from the current session header.
func (s *TraceStore) ReadUsageEvents(sid string) ([]TraceEvent, error) {
	if s == nil || !validTraceSessionID(sid) {
		return nil, fmt.Errorf("trace: invalid session_id %q", sid)
	}
	s.mu.Lock()
	if writer := s.writers[sid]; writer != nil {
		if err := writer.w.Flush(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	f, err := os.Open(filepath.Join(s.dir, sid+".jsonl"))
	s.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	// A worker may be mid-append. Only complete newline-terminated records
	// are safe to observe; the next refresh will include the remaining bytes.
	scanner.Split(func(data []byte, _ bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		return 0, nil, nil
	})
	var out []TraceEvent
	seen := make(map[string]struct{})
	for scanner.Scan() {
		var ev TraceEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil || ev.ID == "" || ev.SessionID != sid || ev.Turn <= 0 || ev.Kind != "tokens" {
			continue
		}
		if _, ok := seen[ev.ID]; ok {
			continue
		}
		seen[ev.ID] = struct{}{}
		out = append(out, TraceEvent{ID: ev.ID, SessionID: ev.SessionID, Turn: ev.Turn, TS: ev.TS,
			Kind: ev.Kind, Text: ev.Text, UsageModel: ev.UsageModel, SubAgentOf: ev.SubAgentOf})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
