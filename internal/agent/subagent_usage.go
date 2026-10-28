package agent

// Sub-agent usage is a separate, append-only receipt journal. The transcript
// format stays readable by older Metis versions. Every consumed provider stream
// has one identity; repeated absolute usage frames replace that receipt rather
// than being added again. These are provider-reported tokens, not an invoice or
// an estimate of requests for which the provider returned no usage.

import (
	"bufio"
	"bytes"
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

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
	"github.com/google/uuid"
)

// SubAgentUsageSnapshot contains disjoint input/output/cache buckets. Available
// means at least one provider usage receipt exists, including a reported zero.
// LLMElapsedMillis sums measured stream attempts, not the child's wall time.
type SubAgentUsageSnapshot struct {
	Available                bool
	ParentSessionID          string
	Revision                 string
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
	LLMElapsedMillis         int64
}

type subAgentUsageRecord struct {
	Version                  int    `json:"version"`
	AgentID                  string `json:"agent_id"`
	ParentSessionID          string `json:"parent_session_id"`
	HeaderSHA256             string `json:"header_sha256"`
	ContinuationFrom         string `json:"continuation_from,omitempty"`
	ReceiptID                string `json:"receipt_id,omitempty"`
	Reported                 bool   `json:"reported,omitempty"`
	InputTokens              int64  `json:"input_tokens,omitempty"`
	OutputTokens             int64  `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens,omitempty"`
	LLMElapsedMillis         int64  `json:"llm_elapsed_millis,omitempty"`
}

// SubAgentUsageRecorder is scoped to the immutable transcript header. A late
// producer therefore cannot move its receipts to a replacement transcript.
type SubAgentUsageRecorder struct {
	path  string
	owner subAgentUsageRecord
}

func subAgentUsageHeaderHash(header pubsess.Header) string {
	raw, _ := json.Marshal(header)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func subAgentUsagePath(sessionDir, agentID string) string {
	return filepath.Join(sessionDir, "subagent-usage", agentID+".jsonl")
}

// NewSubAgentUsageRecorder binds future receipts to the currently saved header.
// A resume that rewrites the transcript may retain receipts only when the
// previous snapshot proves the same agent and parent. Durable resume keeps its
// original header and automatically continues the same receipt namespace.
func NewSubAgentUsageRecorder(sessionDir, agentID string, previous *pubsess.Header) (*SubAgentUsageRecorder, error) {
	if !ValidSubAgentID(agentID) {
		return nil, fmt.Errorf("invalid subagent usage ID")
	}
	header, err := LoadSubAgentHeader(sessionDir, agentID)
	if err != nil {
		return nil, err
	}
	r := &SubAgentUsageRecorder{path: subAgentUsagePath(sessionDir, agentID), owner: subAgentUsageRecord{
		Version: 1, AgentID: agentID, ParentSessionID: header.SubAgentOf, HeaderSHA256: subAgentUsageHeaderHash(header),
	}}
	if previous != nil && previous.ID == agentID && previous.SubAgentOf == header.SubAgentOf {
		from := subAgentUsageHeaderHash(*previous)
		if from != r.owner.HeaderSHA256 {
			record := r.owner
			record.ContinuationFrom = from
			if err := r.append(record); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

type subAgentUsageContextKey struct{}

// WithSubAgentUsageRecorder also shadows an inherited recorder when r is nil.
// A grandchild without persistence must never charge the parent's receipt set.
func WithSubAgentUsageRecorder(ctx context.Context, r *SubAgentUsageRecorder) context.Context {
	return context.WithValue(ctx, subAgentUsageContextKey{}, r)
}

func observeSubAgentUsageStream(ctx context.Context, stream llm.StreamReader) llm.StreamReader {
	recorder, _ := ctx.Value(subAgentUsageContextKey{}).(*SubAgentUsageRecorder)
	if recorder == nil || stream == nil {
		return stream
	}
	record := recorder.owner
	record.ReceiptID = uuid.NewString()
	return &subAgentUsageStream{StreamReader: stream, recorder: recorder, record: record, start: time.Now()}
}

func finishSubAgentUsageStream(stream llm.StreamReader) {
	if observed, ok := stream.(*subAgentUsageStream); ok {
		observed.mu.Lock()
		observed.finishLocked()
		observed.mu.Unlock()
	}
}

type subAgentUsageStream struct {
	llm.StreamReader
	mu       sync.Mutex
	recorder *SubAgentUsageRecorder
	record   subAgentUsageRecord
	start    time.Time
	finished bool
}

func (s *subAgentUsageStream) Recv() (llm.StreamEvent, error) {
	event, err := s.StreamReader.Recv()
	s.mu.Lock()
	if !s.finished {
		// applyStreamEvent validates the entire frame before accepting any of it.
		usage := usageTotals{in: int(s.record.InputTokens), out: int(s.record.OutputTokens), cacheRead: int(s.record.CacheReadInputTokens), cacheCreate: int(s.record.CacheCreationInputTokens)}
		if usage.applyStreamEvent(event) == nil {
			reported := event.HasInputTokens || event.HasOutputTokens || event.HasCacheReadInputTokens || event.HasCacheCreationInputTokens || event.InputTokens > 0 || event.OutputTokens > 0 || event.CacheReadInputTokens > 0 || event.CacheCreationInputTokens > 0
			if reported {
				next := s.record
				next.Reported = true
				next.InputTokens, next.OutputTokens = int64(usage.in), int64(usage.out)
				next.CacheReadInputTokens, next.CacheCreationInputTokens = int64(usage.cacheRead), int64(usage.cacheCreate)
				if next != s.record {
					s.record = next
					s.persistLocked()
				}
			}
		}
		if err != nil || event.Type == "message_stop" {
			s.finishLocked()
		}
	}
	s.mu.Unlock()
	// Receipt I/O is advisory and never changes provider errors or budget writes.
	return event, err
}

func (s *subAgentUsageStream) Close() error {
	err := s.StreamReader.Close()
	s.mu.Lock()
	s.finishLocked()
	s.mu.Unlock()
	return err
}

func (s *subAgentUsageStream) persistLocked() {
	s.record.LLMElapsedMillis = time.Since(s.start).Milliseconds()
	_ = s.recorder.append(s.record)
}

func (s *subAgentUsageStream) finishLocked() {
	if !s.finished {
		s.finished = true
		if s.record.Reported {
			s.persistLocked()
		}
	}
}

var subAgentUsageWriteLocks sync.Map

func (r *SubAgentUsageRecorder) append(record subAgentUsageRecord) error {
	lockValue, _ := subAgentUsageWriteLocks.LoadOrStore(r.path, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// This lease is separate from the transcript writer's lifetime lease. It
	// serializes tail repair + append across processes as well as goroutines.
	for attempts := 0; ; attempts++ {
		err = trySubAgentTranscriptLock(f)
		if err == nil || err != ErrSubAgentTranscriptBusy || attempts == 99 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		return err
	}
	if err := trimSubAgentUsageTail(f); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if n, err := f.Write(encoded); err != nil {
		return err
	} else if n != len(encoded) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := syncSubAgentDirectory(dir); err != nil {
		return err
	}
	return syncSubAgentDirectory(filepath.Dir(dir))
}

func trimSubAgentUsageTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	// Receipts are small, but scanning backwards in fixed chunks also handles
	// arbitrary interrupted junk without allocating the entire journal.
	end := info.Size()
	var chunk [4096]byte
	for end > 0 {
		start := end - int64(len(chunk))
		if start < 0 {
			start = 0
		}
		part := chunk[:end-start]
		if _, err := f.ReadAt(part, start); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(part, '\n'); i >= 0 {
			return f.Truncate(start + int64(i) + 1)
		}
		end = start
	}
	return f.Truncate(0)
}

type subAgentUsageReadCache struct {
	mu       sync.Mutex
	info     os.FileInfo
	offset   int64
	receipts map[string]subAgentUsageRecord
	links    map[string]subAgentUsageRecord
	header   string
	snapshot SubAgentUsageSnapshot
}

var subAgentUsageReadCaches sync.Map

// LoadSubAgentUsage confirms the current transcript's ownership on every read.
// Journal data is cached by inode/size/mtime and only new complete records are
// decoded while it grows, so live polling never repeatedly scans its history.
func LoadSubAgentUsage(sessionDir, agentID string) (SubAgentUsageSnapshot, error) {
	var result SubAgentUsageSnapshot
	if !ValidSubAgentID(agentID) {
		return result, fmt.Errorf("invalid subagent usage ID")
	}
	header, err := LoadSubAgentHeader(sessionDir, agentID)
	if err != nil {
		return result, err
	}
	result.ParentSessionID = header.SubAgentOf
	headerHash := subAgentUsageHeaderHash(header)
	result.Revision = headerHash
	path := subAgentUsagePath(sessionDir, agentID)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return result, err
	}
	value, _ := subAgentUsageReadCaches.LoadOrStore(path, &subAgentUsageReadCache{})
	cache := value.(*subAgentUsageReadCache)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	changed := cache.info == nil || !os.SameFile(cache.info, info) || cache.info.Size() != info.Size() || !cache.info.ModTime().Equal(info.ModTime())
	if !changed && cache.header == headerHash {
		return cache.snapshot, nil
	}
	if changed {
		if cache.info == nil || !os.SameFile(cache.info, info) || info.Size() < cache.info.Size() || info.Size() == cache.info.Size() {
			cache.offset = 0
			cache.receipts = make(map[string]subAgentUsageRecord)
			cache.links = make(map[string]subAgentUsageRecord)
		}
		if _, err := f.Seek(cache.offset, io.SeekStart); err != nil {
			return result, err
		}
		reader := bufio.NewReader(io.LimitReader(f, info.Size()-cache.offset))
		for {
			line, readErr := reader.ReadBytes('\n')
			if readErr != nil {
				if readErr != io.EOF {
					return result, readErr
				}
				break // incomplete crash tail is not a receipt
			}
			cache.offset += int64(len(line))
			var record subAgentUsageRecord
			if json.Unmarshal(line, &record) != nil || record.Version != 1 || record.AgentID != agentID || record.HeaderSHA256 == "" || record.InputTokens < 0 || record.OutputTokens < 0 || record.CacheReadInputTokens < 0 || record.CacheCreationInputTokens < 0 || record.LLMElapsedMillis < 0 {
				continue
			}
			if record.ContinuationFrom != "" {
				cache.links[record.HeaderSHA256] = record
			} else if record.ReceiptID != "" && record.Reported {
				cache.receipts[record.HeaderSHA256+":"+record.ReceiptID] = record
			}
		}
		cache.info = info
	}
	allowed := map[string]bool{headerHash: true}
	for current := headerHash; ; {
		link, ok := cache.links[current]
		if !ok || link.ParentSessionID != header.SubAgentOf || allowed[link.ContinuationFrom] {
			break
		}
		allowed[link.ContinuationFrom] = true
		current = link.ContinuationFrom
	}
	for _, receipt := range cache.receipts {
		if !allowed[receipt.HeaderSHA256] || receipt.ParentSessionID != header.SubAgentOf {
			continue
		}
		result.Available = true
		result.InputTokens += receipt.InputTokens
		result.OutputTokens += receipt.OutputTokens
		result.CacheReadInputTokens += receipt.CacheReadInputTokens
		result.CacheCreationInputTokens += receipt.CacheCreationInputTokens
		result.LLMElapsedMillis += receipt.LLMElapsedMillis
	}
	result.Revision = fmt.Sprintf("%s:%d:%d", headerHash, info.Size(), info.ModTime().UnixNano())
	cache.header, cache.snapshot = headerHash, result
	return result, nil
}
