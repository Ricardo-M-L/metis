package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const maxRewindStateBytes = 8 << 20

var ErrRewindStateWrite = errors.New("代码恢复记录无法保存，本次修改未执行")

type rewindCallContextKey struct{}
type rewindRecordedCall struct {
	Turn   int    `json:"turn"`
	Prefix string `json:"prefix"`
	Name   string `json:"name"`
	Input  string `json:"input"`
	Done   bool   `json:"done"`
}
type rewindStoredEntry struct {
	Hash       string            `json:"hash,omitempty"`
	Turn       int               `json:"turn"`
	Label      string            `json:"label"`
	Paths      map[string]string `json:"paths,omitempty"`
	Incomplete bool              `json:"incomplete,omitempty"`
	Pending    bool              `json:"pending,omitempty"`
	Scope      string            `json:"scope"`
	Prefix     string            `json:"prefix"`
}
type rewindStoredState struct {
	Version           int                           `json:"version"`
	SessionID         string                        `json:"sessionId"`
	Workspace         string                        `json:"workspace"`
	RestorePending    bool                          `json:"restorePending,omitempty"`
	Failure           string                        `json:"failure,omitempty"`
	Entries           []rewindStoredEntry           `json:"entries"`
	Calls             map[string]rewindRecordedCall `json:"calls"`
	DelegationUnknown bool                          `json:"delegationUnknown,omitempty"`
}

func rewindDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// A chain hash computes every user boundary in one pass. Serializing each
// prefix independently would make long-session loading quadratic.
func rewindPrefixes(history []llm.Message) map[int]string {
	prefixes := map[int]string{0: rewindDigest([]llm.Message(nil))}
	digest := sha256.New()
	turn := 0
	for _, message := range history {
		data, _ := json.Marshal(message)
		digest.Write(data)
		digest.Write([]byte{0})
		if _, ok := rewindPrompt(message); ok {
			turn++
			prefixes[turn] = hex.EncodeToString(digest.Sum(nil))
		}
	}
	return prefixes
}
func rewindPrefix(history []llm.Message, turn int) string { return rewindPrefixes(history)[turn] }
func canonicalRewindWorkspace(path string) string {
	if p, e := filepath.Abs(path); e == nil {
		path = p
	}
	if p, e := filepath.EvalSymlinks(path); e == nil {
		path = p
	}
	return filepath.Clean(path)
}
func validRewindHash(value string, optional bool) bool {
	if optional && value == "" {
		return true
	}
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func validRewindRelativePath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return path != "" && !filepath.IsAbs(filepath.FromSlash(path)) && clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") && clean != ".git" && !strings.HasPrefix(clean, ".git/")
}

// LoadRewindState reconstructs durable file mappings without touching files,
// activating providers, or creating a shadow repository. Missing old mappings
// never authorize a restore merely because a shadow HEAD happens to exist.
func (l *Loop) LoadRewindState() error { return l.loadRewindState(false) }
func (l *Loop) loadRewindState(allowUnpaired bool) error {
	if l == nil || l.Checkpointer == nil {
		return nil
	}
	history := l.History()
	prefixes := rewindPrefixes(history)
	l.ckptPersistMu.Lock()
	defer l.ckptPersistMu.Unlock()
	path := l.Checkpointer.RewindMetadataPath()
	if l.ckptLoadedPath != path {
		l.ckptLoadedPath = path
		l.ckptCalls = make(map[string]rewindRecordedCall)
		data, err := readRewindState(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			l.ckptPersistErr = fmt.Errorf("rewind: 无法读取代码恢复记录: %w", err)
			return l.ckptPersistErr
		}
		if err == nil {
			var stored rewindStoredState
			if json.Unmarshal(data, &stored) != nil || stored.Version != 1 || stored.SessionID != l.Checkpointer.ControlStatus().SessionID || stored.Workspace != canonicalRewindWorkspace(l.Checkpointer.ControlStatus().Workspace) || len(stored.Entries) > 4096 || len(stored.Calls) > 32768 {
				l.ckptPersistErr = errors.New("rewind: 代码恢复记录损坏或不属于当前会话/工作区")
				return l.ckptPersistErr
			}
			if stored.Failure != "" || stored.RestorePending {
				l.ckptPersistErr = errors.New("rewind: 上次代码恢复或记录写入未完成，仅支持会话回退")
				return l.ckptPersistErr
			}
			l.ckptDelegateUnknown = stored.DelegationUnknown
			entries := make([]ckptEntry, 0, len(stored.Entries))
			lastTurn := -1
			for _, entry := range stored.Entries {
				if entry.Turn < 0 || entry.Turn < lastTurn || !validRewindHash(entry.Hash, true) || entry.Prefix == "" || entry.Prefix != prefixes[entry.Turn+1] {
					l.ckptPersistErr = errors.New("rewind: 代码恢复记录与当前对话不匹配，仅支持会话回退")
					return l.ckptPersistErr
				}
				lastTurn = entry.Turn
				for name, state := range entry.Paths {
					if !validRewindRelativePath(name) || state == "" {
						l.ckptPersistErr = errors.New("rewind: 代码恢复文件记录无效")
						return l.ckptPersistErr
					}
				}
				entries = append(entries, ckptEntry{hash: entry.Hash, restoreToTurns: entry.Turn, label: entry.Label, managedPaths: entry.Paths, incomplete: entry.Incomplete, pending: entry.Pending, scope: entry.Scope, prefix: entry.Prefix})
			}
			l.ckptMu.Lock()
			// A caller that already made a new in-memory snapshot must not be replaced
			// by an older file. Normal runtime rebinding starts with an empty stack.
			if len(l.ckptStack) == 0 {
				l.ckptStack = entries
				if len(entries) > 0 {
					l.ckptSnappedAt = entries[len(entries)-1].restoreToTurns + 1
				}
			}
			l.ckptMu.Unlock()
			l.ckptCalls = stored.Calls
			if l.ckptCalls == nil {
				l.ckptCalls = make(map[string]rewindRecordedCall)
			}
		}
	}
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "tool_use" && unknownCheckpointMutationTools[block.ToolName] {
				l.ckptDelegateUnknown = true
			}
		}
	}
	if l.ckptPersistErr != nil {
		return l.ckptPersistErr
	}
	l.ckptMu.Lock()
	for _, entry := range l.ckptStack {
		if entry.prefix != "" && entry.prefix != prefixes[entry.restoreToTurns+1] {
			l.ckptPersistErr = errors.New("rewind: 对话已改变，原代码恢复记录不可复用")
			break
		}
	}
	if gap := rewindCallsGap(history, l.ckptCalls, allowUnpaired); l.ckptPersistErr == nil && gap > 0 {
		// Keep a durable protection gap at the latest boundary. A new later user
		// turn may capture a fresh baseline, but an older point cannot cross the gap.
		turn := gap
		if turn > 0 {
			found := false
			for i := range l.ckptStack {
				if l.ckptStack[i].restoreToTurns == turn-1 {
					l.ckptStack[i].incomplete = true
					found = true
				}
			}
			if !found {
				l.ckptStack = append(l.ckptStack, ckptEntry{restoreToTurns: turn - 1, label: "unverified tool history", incomplete: true, prefix: prefixes[turn]})
			}
			sort.SliceStable(l.ckptStack, func(i, j int) bool { return l.ckptStack[i].restoreToTurns < l.ckptStack[j].restoreToTurns })
		}
	}
	l.ckptMu.Unlock()
	return l.ckptPersistErr
}

// A pre-dispatch load may ignore the not-yet-executed latest batch. A resumed
// unfinished tool has an orphan-repair result, so it is never mistaken for a
// fresh dispatch. Every completed historical mutation needs exact attribution.
func rewindCallsGap(history []llm.Message, calls map[string]rewindRecordedCall, allowUnpaired bool) int {
	prefixes := rewindPrefixes(history)
	type use struct {
		key, name, input, prefix string
		turn                     int
		paired                   bool
	}
	var uses []use
	queues := map[string][]int{}
	turn := 0
	for _, message := range history {
		if _, ok := rewindPrompt(message); ok {
			turn++
		}
		for _, block := range message.Content {
			if block.Type == "tool_use" && checkpointMutationTool(block.ToolName) {
				key := block.TraceCallID
				if key == "" {
					key = "tool:" + block.ToolUseID
				}
				uses = append(uses, use{key: key, name: block.ToolName, input: rewindDigest(block.ToolInput), prefix: prefixes[turn], turn: turn})
				queues[block.ToolUseID] = append(queues[block.ToolUseID], len(uses)-1)
			} else if block.Type == "tool_result" {
				q := queues[block.ToolUseID]
				for i, index := range q {
					if !uses[index].paired && (block.TraceCallID == "" || uses[index].key == block.TraceCallID || strings.HasPrefix(uses[index].key, "tool:")) {
						uses[index].paired = true
						queues[block.ToolUseID] = append(q[:i], q[i+1:]...)
						break
					}
				}
			}
		}
	}
	gap := 0
	for _, use := range uses {
		record, ok := calls[use.key]
		if (!ok || !record.Done || record.Turn != use.turn || record.Prefix != use.prefix || record.Name != use.name || record.Input != use.input) && !(allowUnpaired && !use.paired) {
			if use.turn > gap {
				gap = use.turn
			}
		}
	}
	return gap
}

func (l *Loop) rewindPersistenceError() error {
	l.ckptPersistMu.Lock()
	defer l.ckptPersistMu.Unlock()
	if l.ckptPersistErr != nil {
		return l.ckptPersistErr
	}
	if l.ckptDelegateUnknown {
		return errors.New("委派任务的文件修改边界尚未确认，此会话仅支持会话回退")
	}
	return nil
}
func (l *Loop) recordRewindCall(ctx context.Context, name string, input map[string]any, done bool, stageErr error) error {
	if l == nil || l.Checkpointer == nil {
		return nil
	}
	history := l.History()
	turn := l.CountTurns()
	prefixes := rewindPrefixes(history)
	prefix := prefixes[turn]
	key, _ := ctx.Value(rewindCallContextKey{}).(string)
	if key == "" {
		key = "direct:" + fmt.Sprint(turn) + ":" + name + ":" + rewindDigest(input)
	}
	l.ckptPersistMu.Lock()
	defer l.ckptPersistMu.Unlock()
	if l.ckptCalls == nil {
		l.ckptCalls = make(map[string]rewindRecordedCall)
	}
	l.ckptCalls[key] = rewindRecordedCall{Turn: turn, Prefix: prefix, Name: name, Input: rewindDigest(input), Done: done && stageErr == nil}
	l.ckptMu.Lock()
	for i := range l.ckptStack {
		if l.ckptStack[i].prefix == "" {
			l.ckptStack[i].prefix = prefixes[l.ckptStack[i].restoreToTurns+1]
		}
		if l.ckptStack[i].restoreToTurns == turn-1 {
			pending := false
			for _, call := range l.ckptCalls {
				if call.Turn == turn && !call.Done {
					pending = true
				}
			}
			l.ckptStack[i].pending = pending
			if stageErr != nil {
				l.ckptStack[i].incomplete = true
			}
		}
	}
	l.ckptMu.Unlock()
	return l.writeRewindStateLocked(history)
}
func (l *Loop) persistRewindState() error {
	if l == nil || l.Checkpointer == nil {
		return nil
	}
	history := l.History()
	l.ckptPersistMu.Lock()
	defer l.ckptPersistMu.Unlock()
	return l.writeRewindStateLocked(history)
}
func (l *Loop) writeRewindStateLocked(history []llm.Message) error {
	prefixes := rewindPrefixes(history)
	status := l.Checkpointer.ControlStatus()
	stored := rewindStoredState{Version: 1, SessionID: status.SessionID, Workspace: canonicalRewindWorkspace(status.Workspace), RestorePending: l.ckptRestorePending, DelegationUnknown: l.ckptDelegateUnknown, Calls: make(map[string]rewindRecordedCall)}
	if l.ckptPersistErr != nil {
		stored.Failure = l.ckptPersistErr.Error()
	}
	for key, call := range l.ckptCalls {
		if call.Prefix != "" && call.Prefix == prefixes[call.Turn] {
			stored.Calls[key] = call
		}
	}
	l.ckptMu.Lock()
	for i := range l.ckptStack {
		entry := &l.ckptStack[i]
		if entry.prefix == "" {
			entry.prefix = prefixes[entry.restoreToTurns+1]
		}
		stored.Entries = append(stored.Entries, rewindStoredEntry{Hash: entry.hash, Turn: entry.restoreToTurns, Label: entry.label, Paths: cloneRewindPaths(entry.managedPaths), Incomplete: entry.incomplete, Pending: entry.pending, Scope: entry.scope, Prefix: entry.prefix})
	}
	l.ckptMu.Unlock()
	data, err := json.Marshal(stored)
	if err != nil || len(data) > maxRewindStateBytes || len(stored.Entries) > 4096 || len(stored.Calls) > 32768 {
		// A bounded tombstone is sufficient to prove that code recovery is
		// unavailable. Reaching capacity must not stop an otherwise valid chat.
		l.ckptPersistErr = errors.New("rewind: 代码恢复记录超过容量限制，仅支持会话回退")
		stored.Failure = l.ckptPersistErr.Error()
		stored.Entries = nil
		stored.Calls = nil
		data, err = json.Marshal(stored)
	}
	if err == nil {
		err = writeRewindState(l.Checkpointer.RewindMetadataPath(), data)
	}
	l.ckptWriteErr = err
	if err != nil {
		l.ckptPersistErr = fmt.Errorf("rewind: 无法保存代码恢复记录，仅支持会话回退: %w", err)
		return l.ckptPersistErr
	}
	l.ckptCalls = stored.Calls
	return l.ckptPersistErr
}

func readRewindState(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRewindStateBytes {
		return nil, errors.New("invalid rewind metadata file")
	}
	root, err := openRewindMetadataRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, errors.New("rewind metadata changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRewindStateBytes+1))
	if len(data) > maxRewindStateBytes {
		return nil, errors.New("rewind metadata exceeds size limit")
	}
	return data, err
}
func writeRewindState(path string, data []byte) error {
	parent := filepath.Dir(path)
	// Check the two untrusted storage components before MkdirAll, not only
	// after it: a session-dir symlink must not create .git in its target.
	for _, component := range []string{filepath.Dir(parent), parent} {
		if info, err := os.Lstat(component); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return errors.New("rewind metadata directory is a symlink or not a directory")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	root, err := openRewindMetadataRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(filepath.Base(path)); err == nil && !info.Mode().IsRegular() {
		return errors.New("invalid rewind metadata destination")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp := ".rewind-" + NewTraceInvocationID()
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = root.Rename(temp, filepath.Base(path)); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func openRewindMetadataRoot(parent string) (*os.Root, error) {
	// macOS /var and /tmp are normal system aliases. Reject symlinked session
	// storage and .git themselves, rather than rejecting those system aliases.
	for _, path := range []string{filepath.Dir(parent), parent} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("rewind metadata directory is a symlink or not a directory")
		}
	}
	return os.OpenRoot(parent)
}

func cloneRewindPaths(paths map[string]string) map[string]string {
	if paths == nil {
		return nil
	}
	copy := make(map[string]string, len(paths))
	for key, value := range paths {
		copy[key] = value
	}
	return copy
}
func (l *Loop) fenceRewindPersistence(history []llm.Message) error {
	if l.Checkpointer == nil {
		return ErrCheckpointUnavailable
	}
	l.ckptPersistMu.Lock()
	defer l.ckptPersistMu.Unlock()
	l.ckptRestorePending = true
	return l.writeRewindStateLocked(history)
}
func (l *Loop) finishRewindPersistence() error {
	history := l.History()
	l.ckptPersistMu.Lock()
	defer l.ckptPersistMu.Unlock()
	l.ckptRestorePending = false
	return l.writeRewindStateLocked(history)
}
