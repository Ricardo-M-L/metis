package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RecoverSubAgentTerminal may only be called after the owning worker has been
// joined. It appends a missing failure/cancellation terminal without replacing
// a runner's durable result or truncating its transcript history.
func RecoverSubAgentTerminal(sessionDir, parentSessionID, agentID string, terminal SubAgentTerminal) (*SubAgentSnapshot, error) {
	if parentSessionID == "" || agentID == "" || agentID != strings.TrimSpace(agentID) || strings.ContainsAny(agentID, "/\\") ||
		(terminal.Status != StatusKilled.String() && terminal.Status != StatusFailed.String()) {
		return nil, errors.New("invalid sub-agent terminal recovery")
	}
	snapshot, err := LoadSubAgentSnapshot(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	if snapshot.Header.ID != agentID || snapshot.Header.SubAgentOf != parentSessionID {
		return nil, errors.New("sub-agent terminal recovery owner mismatch")
	}
	if snapshot.Terminal != nil {
		return snapshot, nil
	}
	path := filepath.Join(sessionDir, SubAgentTranscriptDirname, agentID+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("append recovered sub-agent terminal: %w", err)
	}
	writer := &SubAgentTranscript{path: path, f: file}
	writeErr := writer.AppendTerminal(terminal)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := writer.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	snapshot.Terminal = &terminal
	return snapshot, nil
}
