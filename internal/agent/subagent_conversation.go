package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

// SubAgentConversation is a read-only view of everything the transcript has
// recorded. Unlike the resume snapshot, it retains the history preceding a
// compaction or edit. Reset snapshots are explicit context segments: their
// messages must not be mistaken for newly generated or deduplicated turns.
type SubAgentConversation struct {
	Header         pubsess.Header
	Messages       []llm.Message
	Terminal       *SubAgentTerminal
	Segments       []SubAgentConversationSegment
	ContextResets  int
	IncompleteTail bool
	Revision       string
}

type SubAgentConversationSegment struct {
	StartIndex int    `json:"startIndex"`
	Reason     string `json:"reason"`
}

// ValidSubAgentID confines all transcript readers to one filename. IDs are
// opaque, so do not assume that only the current agt-<hex> generator exists.
func ValidSubAgentID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 128 || !utf8.ValidString(id) || id != strings.TrimSpace(id) || strings.ContainsAny(id, `/\\`) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func subAgentTranscriptPath(sessionDir, agentID string) (string, error) {
	if sessionDir == "" || !ValidSubAgentID(agentID) {
		return "", fmt.Errorf("subagent transcript: valid sessionDir + agentID required")
	}
	return filepath.Join(sessionDir, SubAgentTranscriptDirname, agentID+".jsonl"), nil
}

func subAgentFileRevision(info os.FileInfo) string {
	return fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano())
}

func subAgentHeaderRevision(header pubsess.Header) string {
	encoded, _ := json.Marshal(header)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:12])
}

// SubAgentConversationRevision is cheap enough for the live detail sampler.
// It does not read message bodies or obtain the producer's writer lease.
func SubAgentConversationRevision(sessionDir, agentID string) string {
	path, err := subAgentTranscriptPath(sessionDir, agentID)
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	header, err := LoadSubAgentHeader(sessionDir, agentID)
	if err != nil {
		return ""
	}
	return subAgentFileRevision(info) + "-" + subAgentHeaderRevision(header)
}

// LoadSubAgentConversation reads a bounded-at-open snapshot without mutating
// the producer or its recovery history. A partial final line is unacknowledged
// and ignored; complete malformed records fail rather than presenting invented
// history. Legacy files may already have lost history before this read, so this
// API promises recorded content, not a complete lifecycle audit.
func LoadSubAgentConversation(sessionDir, agentID string) (*SubAgentConversation, error) {
	return loadSubAgentConversation(sessionDir, agentID, nil)
}

func loadSubAgentConversation(sessionDir, agentID string, afterHeader func()) (*SubAgentConversation, error) {
	path, err := subAgentTranscriptPath(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("subagent transcript is not a regular file")
	}
	conversation := &SubAgentConversation{
		Messages: make([]llm.Message, 0),
		Segments: []SubAgentConversationSegment{{StartIndex: 0, Reason: "initial"}},
		Revision: subAgentFileRevision(info),
	}
	reader := bufio.NewReader(io.LimitReader(f, info.Size()))
	headerSeen, currentLength := false, 0
	var initialHeader pubsess.Header
	var execution *SubAgentExecutionConfig
	var followups subAgentFollowupReplay
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if readErr != io.EOF {
				return nil, fmt.Errorf("read subagent conversation: %w", readErr)
			}
			conversation.IncompleteTail = len(line) > 0
			break
		}
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		var entry subagentEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("subagent transcript line %d: %w", lineNumber, err)
		}
		if !headerSeen && (entry.Type != "header" || entry.Header == nil || entry.Header.ID != agentID) {
			return nil, fmt.Errorf("subagent transcript line %d: invalid first header", lineNumber)
		}
		switch entry.Type {
		case "header":
			if entry.Header == nil {
				return nil, fmt.Errorf("subagent transcript line %d: missing header", lineNumber)
			}
			if !headerSeen {
				conversation.Header, headerSeen = *entry.Header, true
				initialHeader = *entry.Header
				if afterHeader != nil {
					afterHeader()
				}
			} else {
				mergeSubagentHeader(&conversation.Header, entry.Header)
			}
		case "message":
			if entry.Message == nil {
				return nil, fmt.Errorf("subagent transcript line %d: missing message", lineNumber)
			}
			conversation.Messages = append(conversation.Messages, *entry.Message)
			currentLength++
		case "checkpoint":
			checkpoint := entry.Checkpoint
			if checkpoint == nil || checkpoint.Offset < 0 || (checkpoint.Reset && checkpoint.Offset != 0) {
				return nil, fmt.Errorf("subagent transcript line %d: invalid checkpoint", lineNumber)
			}
			if checkpoint.Reset {
				currentLength = 0
				conversation.ContextResets++
				conversation.Segments = append(conversation.Segments, SubAgentConversationSegment{StartIndex: len(conversation.Messages), Reason: "context_reset"})
			}
			if checkpoint.Offset != currentLength {
				return nil, fmt.Errorf("subagent transcript line %d: checkpoint offset %d differs from history length %d", lineNumber, checkpoint.Offset, currentLength)
			}
			conversation.Messages = append(conversation.Messages, checkpoint.Messages...)
			followups.pending = pendingSubAgentFollowups(checkpoint.Messages, followups.pending)
			currentLength += len(checkpoint.Messages)
			conversation.Terminal = nil
		case "terminal":
			if entry.Terminal == nil {
				return nil, fmt.Errorf("subagent transcript line %d: missing terminal", lineNumber)
			}
			terminal := *entry.Terminal
			conversation.Terminal = &terminal
		case "handoff":
			if entry.Handoff == nil || entry.Handoff.Scope == "" || entry.Handoff.Recipient == "" {
				return nil, fmt.Errorf("subagent transcript line %d: invalid handoff", lineNumber)
			}
		case "execution":
			var err error
			execution, err = readSubAgentExecution(execution, entry.Execution)
			if err != nil {
				return nil, fmt.Errorf("subagent transcript line %d: %w", lineNumber, err)
			}
		case "followup":
			if err := followups.enqueue(entry.Followup); err != nil {
				return nil, fmt.Errorf("subagent transcript line %d: %w", lineNumber, err)
			}
		case "followup_ack":
			if err := followups.acknowledge(entry.FollowupAck); err != nil {
				return nil, fmt.Errorf("subagent transcript line %d: %w", lineNumber, err)
			}
		default:
			return nil, fmt.Errorf("subagent transcript line %d: unknown entry type %q", lineNumber, entry.Type)
		}
	}
	if !headerSeen {
		return nil, fmt.Errorf("subagent transcript has no header")
	}
	// Ordinary resume currently truncates the same inode. Verify both the
	// opened descriptor and its pathname still name this immutable run, so
	// a concurrent replacement cannot combine an old owner with a new body.
	line, err := bufio.NewReader(io.NewSectionReader(f, 0, 64<<10)).ReadBytes('\n')
	var current subagentEntry
	if err != nil || json.Unmarshal(line, &current) != nil || current.Type != "header" || current.Header == nil || subAgentHeaderRevision(*current.Header) != subAgentHeaderRevision(initialHeader) {
		return nil, fmt.Errorf("subagent transcript changed while reading")
	}
	pathHeader, err := LoadSubAgentHeader(sessionDir, agentID)
	if err != nil || subAgentHeaderRevision(pathHeader) != subAgentHeaderRevision(initialHeader) {
		return nil, fmt.Errorf("subagent transcript replaced while reading")
	}
	conversation.Revision += "-" + subAgentHeaderRevision(initialHeader)
	return conversation, nil
}
