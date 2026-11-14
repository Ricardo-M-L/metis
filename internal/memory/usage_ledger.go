package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Ricardo-M-L/metis/internal/memdir"
)

const usageLedgerPath = "usage/events.jsonl"

type usageEntry struct {
	Key string `json:"key"`
}
type usageEvent struct {
	Version  int          `json:"version"`
	ID       string       `json:"event_id"`
	At       string       `json:"at"`
	Entries  []usageEntry `json:"passages"`
	Checksum string       `json:"checksum"`
}
type usageTotal struct {
	count int
	last  string
}

// usageLedger caches only access metadata. Its lock never acquires repository
// or corpus locks, so callers may safely decorate a guarded recall snapshot.
type usageLedger struct {
	mu      sync.Mutex
	root    string
	info    os.FileInfo
	offset  int64
	anchor  []byte
	badTail bool
	totals  map[string]usageTotal
	events  map[string]string
}

func newUsageLedger(root string) *usageLedger { return &usageLedger{root: root} }

// Create the metadata directory before any corpus fingerprint is captured.
// First use must not change the root directory mtime and rebuild the corpus.
func (ledger *usageLedger) initialize() error {
	return withRepositoryLock(ledger.root, func() error {
		file, err := memdir.OpenPrivateRegularFile(ledger.root, usageLedgerPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := file.Sync(); err != nil {
			return err
		}
		if err := syncUsageDirectory(ledger.root, "usage"); err != nil {
			return err
		}
		return syncUsageDirectory(ledger.root, ".")
	})
}

func passageUsageKey(p Passage) string {
	kind, created := "archive", p.CreatedAt
	if isTopicPassage(p) {
		kind, created = "topic", ""
	}
	data, _ := json.Marshal([]string{kind, p.ID, created, p.SourceSessionID, p.Scope})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func eventSelection(event usageEvent) string {
	data, _ := json.Marshal(event.Entries)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func eventChecksum(event usageEvent) string {
	event.Checksum = ""
	data, _ := json.Marshal(event)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (ledger *usageLedger) reset() {
	ledger.info, ledger.offset, ledger.badTail, ledger.anchor = nil, 0, false, nil
	ledger.totals, ledger.events = make(map[string]usageTotal), make(map[string]string)
}

func (ledger *usageLedger) loadFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if ledger.info != nil && os.SameFile(ledger.info, info) && ledger.info.Size() == info.Size() && ledger.info.ModTime().Equal(info.ModTime()) {
		return nil
	}
	if ledger.info == nil || !os.SameFile(ledger.info, info) || info.Size() < ledger.info.Size() || ledger.badTail || info.Size() == ledger.info.Size() {
		ledger.reset()
	}
	if len(ledger.anchor) > 0 {
		anchor := make([]byte, len(ledger.anchor))
		if _, err := file.ReadAt(anchor, ledger.offset-int64(len(anchor))); err != nil || !bytes.Equal(anchor, ledger.anchor) {
			ledger.reset()
		}
	}
	data, err := io.ReadAll(io.NewSectionReader(file, ledger.offset, info.Size()-ledger.offset))
	if err != nil {
		return err
	}
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			ledger.badTail = true
			break
		}
		line := data[:end]
		var event usageEvent
		parseErr := json.Unmarshal(line, &event)
		if parseErr == nil {
			parseErr = validateUsageEvent(event)
		}
		if parseErr != nil {
			// A killed append can leave one incomplete final transaction. Never
			// accept a valid-looking prefix of a batch or skip interior damage.
			if len(bytes.TrimSpace(data[end+1:])) != 0 {
				ledger.reset()
				return fmt.Errorf("memory: corrupt usage ledger before tail: %w", parseErr)
			}
			ledger.badTail = true
			break
		}
		if err := ledger.apply(event); err != nil {
			ledger.reset()
			return err
		}
		ledger.offset += int64(end + 1)
		data = data[end+1:]
	}
	ledger.info = info
	return ledger.captureAnchor(file)
}

// Verify the append boundary before extending an offset cache. A same-inode
// truncate/rewrite that grows the file must not be mistaken for append-only I/O.
func (ledger *usageLedger) captureAnchor(file *os.File) error {
	size := ledger.offset
	if size > 512 {
		size = 512
	}
	ledger.anchor = make([]byte, size)
	if size == 0 {
		return nil
	}
	_, err := file.ReadAt(ledger.anchor, ledger.offset-size)
	return err
}

func validateUsageEvent(event usageEvent) error {
	if event.Version != 1 || event.ID == "" || len(event.Entries) == 0 || event.Checksum != eventChecksum(event) {
		return errors.New("invalid usage transaction")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.At); err != nil {
		return err
	}
	previous := ""
	for _, entry := range event.Entries {
		if len(entry.Key) != sha256.Size*2 || entry.Key <= previous {
			return errors.New("invalid usage selection")
		}
		if _, err := hex.DecodeString(entry.Key); err != nil {
			return err
		}
		previous = entry.Key
	}
	return nil
}

func (ledger *usageLedger) apply(event usageEvent) error {
	selection := eventSelection(event)
	if existing, ok := ledger.events[event.ID]; ok {
		if existing != selection {
			return errors.New("memory: usage event id reused for another selection")
		}
		return nil
	}
	ledger.events[event.ID] = selection
	for _, entry := range event.Entries {
		total := ledger.totals[entry.Key]
		total.count++
		total.last = latestUsageTime(total.last, event.At)
		ledger.totals[entry.Key] = total
	}
	return nil
}

func latestUsageTime(left, right string) string {
	l, le := time.Parse(time.RFC3339Nano, left)
	r, re := time.Parse(time.RFC3339Nano, right)
	if re == nil && (le != nil || r.After(l)) {
		return right
	}
	return left
}

func (ledger *usageLedger) decorate(passages []Passage) error {
	if ledger == nil || len(passages) == 0 {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	file, err := memdir.OpenPrivateRegularFile(ledger.root, usageLedgerPath, os.O_RDONLY, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		ledger.reset()
	} else if err != nil {
		return err
	} else {
		defer file.Close()
		if err := ledger.loadFile(file); err != nil {
			return err
		}
	}
	for i := range passages {
		p := &passages[i]
		if !p.usageDecorated {
			p.usageBaseCount, p.usageBaseLast, p.usageDecorated = p.UseCount, p.LastUsedAt, true
		}
		total := ledger.totals[passageUsageKey(*p)]
		p.UseCount = p.usageBaseCount + total.count
		p.LastUsedAt = latestUsageTime(p.usageBaseLast, total.last)
	}
	return nil
}

// appendLocked commits one entire selection before returning. The caller holds
// the process + cross-process repository lock; failed tails are truncated only
// here, never by a read-only search.
func (ledger *usageLedger) appendLocked(event usageEvent) error {
	if ledger == nil {
		return errors.New("memory: missing usage ledger")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	file, err := memdir.OpenPrivateRegularFile(ledger.root, usageLedgerPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := ledger.loadFile(file); err != nil {
		return err
	}
	if existing, ok := ledger.events[event.ID]; ok {
		if existing != eventSelection(event) {
			return errors.New("memory: usage event id reused for another selection")
		}
		// A previous call may have written the complete transaction and then
		// failed fsync. A retry must confirm durability before reporting success.
		if err := file.Sync(); err != nil {
			return err
		}
		if err := syncUsageDirectory(ledger.root, "usage"); err != nil {
			return err
		}
		return syncUsageDirectory(ledger.root, ".")
	}
	if ledger.badTail {
		if err := file.Truncate(ledger.offset); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			ledger.info = nil
			return err
		}
		ledger.badTail = false
	}
	event.Checksum = eventChecksum(event)
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		ledger.info = nil
		return err
	}
	if err := file.Sync(); err != nil {
		ledger.info = nil
		return err
	}
	// Persist both directory entries when the first append creates usage/.
	if err := syncUsageDirectory(ledger.root, "usage"); err != nil {
		ledger.info = nil
		return err
	}
	if err := syncUsageDirectory(ledger.root, "."); err != nil {
		ledger.info = nil
		return err
	}
	if err := ledger.apply(event); err != nil {
		ledger.info = nil
		return err
	}
	ledger.offset += int64(len(data))
	ledger.info, err = file.Stat()
	if err != nil {
		return err
	}
	return ledger.captureAnchor(file)
}

func syncUsageDirectory(root, relative string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, _, err := openAuthoritativeRoot(root)
	if err != nil {
		return err
	}
	defer handle.Close()
	info, err := handle.Lstat(relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("memory: unsafe usage directory")
	}
	file, err := handle.Open(filepath.FromSlash(strings.TrimSpace(relative)))
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("memory: usage directory changed while opening")
	}
	if err := file.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
