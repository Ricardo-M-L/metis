// Package handoff persists peer messages independently of a worker process.
// Delivery is acknowledged only after the receiver checkpoints its transcript.
package handoff

import (
	"context"
	"crypto/rand"
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
	"time"
	"unicode"
	"unicode/utf8"
)

// Version 3 also protects admission diagnostics from older writers. Versions 1
// and 2 remain readable, but must not claim fields they cannot preserve.
const SchemaVersion = 3
const DefaultLease = 30 * time.Second
const maxRecordBytes = 256 << 10

// Receipt growth is separately bounded; Send still has the smaller input cap.
const maxBusinessRecordBytes = 2 << 20

var (
	ErrConflict = errors.New("handoff idempotency key conflicts with an existing message")
	ErrLease    = errors.New("handoff delivery lease expired or belongs to another worker")
)

// Task is an optional structured handoff contract. Paths are references, not
// instructions to execute; the receiver must verify artifacts before use.
type Task struct {
	ID           string     `json:"id"`
	Objective    string     `json:"objective"`
	Artifacts    []Artifact `json:"artifacts,omitempty"`
	Acceptance   []string   `json:"acceptance,omitempty"`
	Deliverables []string   `json:"deliverables,omitempty"`
}
type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Envelope struct {
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Scope         string    `json:"scope"`
	SenderScope   string    `json:"sender_scope,omitempty"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	Body          string    `json:"body"`
	Task          *Task     `json:"task,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ContentSHA256 string    `json:"content_sha256"`
}

// Record is the durable outbox/inbox entry shared by both parties. A single
// atomic record avoids the crash window between writing separate queues.
type Record struct {
	Envelope
	State            string           `json:"state"` // pending, claimed, acknowledged
	Attempts         int              `json:"attempts"`
	Consumer         string           `json:"consumer,omitempty"`
	Token            string           `json:"token,omitempty"`
	LeaseExpires     time.Time        `json:"lease_expires,omitempty"`
	CheckpointSHA256 string           `json:"checkpoint_sha256,omitempty"`
	AcknowledgedAt   time.Time        `json:"acknowledged_at,omitempty"`
	Business         *BusinessReceipt `json:"business,omitempty"`
	Admission        *AdmissionStatus `json:"admission,omitempty"`
}

type SendRequest struct {
	Scope, From, To, Body string
	SenderScope           string
	Task                  *Task
	IdempotencyKey        string
}

type Store struct {
	root         string
	now          func() time.Time
	rootInfo     os.FileInfo
	messagesInfo os.FileInfo
	receiptsInfo os.FileInfo
}

func NewStore(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := secureDirectory(abs); err != nil {
		return nil, err
	}
	if err := secureDirectory(filepath.Join(abs, "messages")); err != nil {
		return nil, err
	}
	if err := secureDirectory(filepath.Join(abs, "receipts")); err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	messagesInfo, err := os.Lstat(filepath.Join(abs, "messages"))
	if err != nil {
		return nil, err
	}
	receiptsInfo, err := os.Lstat(filepath.Join(abs, "receipts"))
	if err != nil {
		return nil, err
	}
	return &Store{root: abs, now: time.Now, rootInfo: rootInfo, messagesInfo: messagesInfo, receiptsInfo: receiptsInfo}, nil
}
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// WorkspaceScope is stable across session/run rotation in the same workspace.
func WorkspaceScope(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(canonical))
	return "workspace-" + hex.EncodeToString(h[:16]), nil
}

func ValidateAddress(v string) error {
	if v == "" || v != strings.TrimSpace(v) || len(v) > 192 || !utf8.ValidString(v) {
		return errors.New("handoff address must be non-empty, trimmed, and at most 192 bytes")
	}
	for _, c := range v {
		if unicode.IsControl(c) {
			return errors.New("handoff address contains a control character")
		}
	}
	return nil
}

func validateRequest(q SendRequest) error {
	for _, v := range []string{q.Scope, q.From, q.To} {
		if err := ValidateAddress(v); err != nil {
			return err
		}
	}
	if q.SenderScope != "" {
		if err := ValidateAddress(q.SenderScope); err != nil {
			return err
		}
	}
	if q.To == "*" {
		return errors.New("broadcast must resolve explicit recipients before persistence")
	}
	if strings.TrimSpace(q.Body) == "" || len(q.Body) > 128<<10 || !utf8.ValidString(q.Body) {
		return errors.New("handoff body must be non-empty and at most 128 KiB")
	}
	if len(q.IdempotencyKey) > 512 {
		return errors.New("handoff idempotency key exceeds 512 bytes")
	}
	if q.Task != nil {
		if err := ValidateAddress(q.Task.ID); err != nil {
			return fmt.Errorf("task id: %w", err)
		}
		if strings.TrimSpace(q.Task.Objective) == "" || len(q.Task.Objective) > 16<<10 || !utf8.ValidString(q.Task.Objective) {
			return errors.New("task objective must be non-empty and at most 16 KiB")
		}
		if len(q.Task.Artifacts) > 64 || len(q.Task.Acceptance) > 64 || len(q.Task.Deliverables) > 64 {
			return errors.New("handoff task has more than 64 artifacts, deliverables or acceptance checks")
		}
		for _, a := range q.Task.Artifacts {
			if strings.TrimSpace(a.Path) == "" || len(a.Path) > 4096 || !validSHA(a.SHA256) || !utf8.ValidString(a.Path) {
				return errors.New("handoff artifacts require a path and SHA-256")
			}
		}
		seenDeliverables := map[string]bool{}
		for _, p := range q.Task.Deliverables {
			if !validDeliverable(p) || seenDeliverables[p] {
				return errors.New("handoff deliverables require distinct managed relative paths")
			}
			seenDeliverables[p] = true
		}
		for _, a := range q.Task.Acceptance {
			if strings.TrimSpace(a) == "" || len(a) > 4096 || !utf8.ValidString(a) {
				return errors.New("handoff acceptance check must be non-empty and at most 4096 bytes")
			}
		}
	}
	return nil
}

// ValidateEnvelope validates a durable message before another host subsystem
// binds it to work. Version 1 remains readable; versions 2 and later may carry
// managed deliverables. Unknown versions are rejected so older writers cannot
// erase fields introduced by a newer host.
func ValidateEnvelope(e Envelope) error {
	if (e.Version != 1 && e.Version != 2 && e.Version != SchemaVersion) || !validID(e.ID) || e.CreatedAt.IsZero() {
		return errors.New("invalid handoff record version or identity")
	}
	if e.Version == 1 && e.Task != nil && len(e.Task.Deliverables) != 0 {
		return errors.New("legacy handoff version cannot contain managed deliverables")
	}
	q := SendRequest{Scope: e.Scope, SenderScope: e.SenderScope, From: e.From, To: e.To, Body: e.Body, Task: e.Task}
	if err := validateRequest(q); err != nil {
		return err
	}
	if e.ContentSHA256 != contentSHA(q) {
		return errors.New("handoff content checksum differs")
	}
	return nil
}

func contentSHA(q SendRequest) string {
	b, _ := json.Marshal(struct {
		Scope, SenderScope, From, To, Body string
		Task                               *Task
	}{q.Scope, q.SenderScope, q.From, q.To, q.Body, q.Task})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) Send(ctx context.Context, q SendRequest) (Record, bool, error) {
	if err := validateRequest(q); err != nil {
		return Record{}, false, err
	}
	id := ""
	if q.IdempotencyKey != "" {
		b, _ := json.Marshal([]string{q.Scope, q.SenderScope, q.From, q.To, q.IdempotencyKey})
		h := sha256.Sum256(b)
		id = "msg-" + hex.EncodeToString(h[:])
	} else {
		var err error
		id, err = randomID("msg-", 32)
		if err != nil {
			return Record{}, false, err
		}
	}
	var rec Record
	duplicate := false
	err := s.locked(ctx, func() error {
		existing, err := s.read(id)
		if err == nil {
			if existing.ContentSHA256 != contentSHA(q) {
				return ErrConflict
			}
			rec = existing
			duplicate = true
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		rec = Record{Envelope: Envelope{Version: SchemaVersion, ID: id, Scope: q.Scope, SenderScope: q.SenderScope, From: q.From, To: q.To, Body: q.Body, Task: q.Task, CreatedAt: s.now().UTC(), ContentSHA256: contentSHA(q)}, State: "pending"}
		return s.write(rec)
	})
	return rec, duplicate, err
}

// Claim selects the oldest unacknowledged message. Reopening the store does
// not reset leases; another worker may reclaim only after the old lease ends.
func (s *Store) Claim(ctx context.Context, scope, to, consumer string, lease time.Duration) (*Record, error) {
	return s.claim(ctx, scope, to, consumer, lease, false)
}

// ClaimNotifications excludes structured work so an unrelated worker cannot
// consume an unassigned task while draining its notification mailbox.
func (s *Store) ClaimNotifications(ctx context.Context, scope, to, consumer string, lease time.Duration) (*Record, error) {
	return s.claim(ctx, scope, to, consumer, lease, true)
}

func (s *Store) claim(ctx context.Context, scope, to, consumer string, lease time.Duration, notificationsOnly bool) (*Record, error) {
	for _, v := range []string{scope, to, consumer} {
		if err := ValidateAddress(v); err != nil {
			return nil, err
		}
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	if lease > 5*time.Minute {
		return nil, errors.New("handoff lease exceeds 5 minutes")
	}
	var claimed *Record
	err := s.locked(ctx, func() error {
		recs, err := s.list(scope, to, false)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		for _, r := range recs {
			if r.Business != nil || (notificationsOnly && r.Task != nil) || r.State == "acknowledged" || (r.State == "claimed" && now.Before(r.LeaseExpires)) {
				continue
			}
			token, err := randomID("claim-", 16)
			if err != nil {
				return err
			}
			r.State = "claimed"
			r.Consumer = consumer
			r.Token = token
			r.LeaseExpires = now.Add(lease)
			r.Attempts++
			r.Admission = nil
			if err := s.write(r); err != nil {
				return err
			}
			claimed = &r
			return nil
		}
		return nil
	})
	return claimed, err
}

func (s *Store) Renew(ctx context.Context, id, token string, lease time.Duration) error {
	if lease <= 0 {
		lease = DefaultLease
	}
	if lease > 5*time.Minute {
		return errors.New("handoff lease exceeds 5 minutes")
	}
	return s.locked(ctx, func() error {
		r, err := s.read(id)
		if err != nil {
			return err
		}
		if err = s.verifyLease(r, token); err != nil {
			return err
		}
		r.LeaseExpires = s.now().UTC().Add(lease)
		return s.write(r)
	})
}

// Ack records a successful, durable transcript checkpoint. It does not mean
// the recipient finished the requested work, only that the message is saved.
func (s *Store) Ack(ctx context.Context, id, token, checkpointSHA string) error {
	if !validSHA(checkpointSHA) {
		return errors.New("handoff acknowledgement requires a transcript checkpoint SHA-256")
	}
	return s.locked(ctx, func() error {
		r, err := s.read(id)
		if err != nil {
			return err
		}
		if r.State == "acknowledged" && r.Token == token && r.CheckpointSHA256 == checkpointSHA {
			return nil
		}
		if err = s.verifyLease(r, token); err != nil {
			return err
		}
		r.State = "acknowledged"
		r.CheckpointSHA256 = checkpointSHA
		r.AcknowledgedAt = s.now().UTC()
		return s.write(r)
	})
}

func (s *Store) Release(ctx context.Context, id, token string) error {
	return s.locked(ctx, func() error {
		r, err := s.read(id)
		if err != nil {
			return err
		}
		if err = s.verifyLease(r, token); err != nil {
			return err
		}
		r.State = "pending"
		r.Consumer = ""
		r.Token = ""
		r.LeaseExpires = time.Time{}
		return s.write(r)
	})
}
func (s *Store) verifyLease(r Record, token string) error {
	if r.State != "claimed" || token == "" || r.Token != token || !s.now().Before(r.LeaseExpires) {
		return ErrLease
	}
	return nil
}
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	var r Record
	err := s.locked(ctx, func() error { var err error; r, err = s.read(id); return err })
	return r, err
}
func (s *Store) List(ctx context.Context, scope, to string) ([]Record, error) {
	var r []Record
	err := s.locked(ctx, func() error { var err error; r, err = s.list(scope, to, true); return err })
	return r, err
}

func validSHA(v string) bool {
	if len(v) != 64 {
		return false
	}
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 32
}
func validID(v string) bool {
	return strings.HasPrefix(v, "msg-") && validSHA(strings.TrimPrefix(v, "msg-"))
}
func randomID(prefix string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
func (s *Store) read(id string) (Record, error) {
	if !validID(id) {
		return Record{}, errors.New("invalid handoff message id")
	}
	p := filepath.Join(s.root, "receipts", id+".json")
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		p = filepath.Join(s.root, "messages", id+".json")
		info, err = os.Lstat(p)
	}
	if err != nil {
		return Record{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Record{}, errors.New("refusing non-regular handoff record")
	}
	f, err := openRecord(p)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return Record{}, err
	}
	if !os.SameFile(info, opened) {
		return Record{}, errors.New("handoff record changed during open")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBusinessRecordBytes+1))
	if err != nil {
		return Record{}, err
	}
	if len(b) > maxBusinessRecordBytes {
		return Record{}, errors.New("handoff record exceeds size limit")
	}
	var r Record
	if err = json.Unmarshal(b, &r); err != nil {
		return Record{}, err
	}
	if r.Business == nil && len(b) > maxRecordBytes {
		return Record{}, errors.New("handoff input exceeds size limit")
	}
	if r.ID != id {
		return Record{}, errors.New("invalid handoff record version or identity")
	}
	if r.Version == 1 && (r.Business != nil || (r.Task != nil && len(r.Task.Deliverables) != 0)) {
		return Record{}, errors.New("legacy handoff version cannot contain business receipts or deliverables")
	}
	if r.Version < 3 && r.Admission != nil {
		return Record{}, errors.New("legacy handoff version cannot contain admission diagnostics")
	}
	if err = validateAdmission(r); err != nil {
		return Record{}, err
	}
	if err = validateBusinessReceipt(r.Business, r.Task); err != nil {
		return Record{}, err
	}
	if r.Business != nil && (r.Scope != "jobs" || r.To != "job:"+r.Business.Assignment.JobID) {
		return Record{}, errors.New("business assignment does not match job mailbox")
	}
	if r.Business != nil && r.Business.State == BusinessCompleted && r.State != "acknowledged" {
		return Record{}, errors.New("business completion has no transport checkpoint")
	}
	if err = ValidateEnvelope(r.Envelope); err != nil {
		return Record{}, err
	}
	if r.State != "pending" && r.State != "claimed" && r.State != "acknowledged" {
		return Record{}, errors.New("invalid handoff delivery state")
	}
	if r.State == "claimed" && (r.Token == "" || r.Consumer == "" || r.LeaseExpires.IsZero()) {
		return Record{}, errors.New("incomplete handoff claim")
	}
	if r.State == "acknowledged" && (!validSHA(r.CheckpointSHA256) || r.AcknowledgedAt.IsZero()) {
		return Record{}, errors.New("incomplete handoff acknowledgement")
	}
	return r, nil
}
func (s *Store) list(scope, to string, includeReceipts bool) ([]Record, error) {
	out := make([]Record, 0)
	dirs := []string{"messages"}
	if includeReceipts {
		dirs = append(dirs, "receipts")
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(s.root, dir))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			r, err := s.read(strings.TrimSuffix(e.Name(), ".json"))
			if err != nil {
				return nil, err
			}
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if !includeReceipts && r.State == "acknowledged" {
				continue
			}
			if (scope == "" || r.Scope == scope) && (to == "" || r.To == to) {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
func secureDirectory(p string) error {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlinked handoff directory")
	}
	return os.Chmod(p, 0o700)
}
func (s *Store) write(r Record) error {
	r.Version = SchemaVersion
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	limit := maxRecordBytes
	if r.Business != nil {
		limit = maxBusinessRecordBytes
	}
	if len(b) > limit {
		return errors.New("handoff record exceeds size limit")
	}
	dir := filepath.Join(s.root, "messages")
	if r.State == "acknowledged" {
		dir = filepath.Join(s.root, "receipts")
	}
	f, err := os.CreateTemp(dir, ".handoff-*")
	if err != nil {
		return err
	}
	p := f.Name()
	defer os.Remove(p)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(p, filepath.Join(dir, r.ID+".json")); err != nil {
		return err
	}
	if err = syncDirectory(dir); err != nil {
		return err
	}
	if r.State == "acknowledged" {
		// Commit ACK and later business projections directly to the same authoritative
		// receipt. A crash before inbox cleanup leaves a stale hot record, but read
		// always prefers this already-synced receipt and Claim cannot redeliver it.
		messages := filepath.Join(s.root, "messages")
		if err = os.Remove(filepath.Join(messages, r.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDirectory(messages)
	}
	return nil
}

func (s *Store) locked(ctx context.Context, fn func() error) error {
	if s == nil {
		return errors.New("handoff store unavailable")
	}
	if err := s.verifyDirectories(); err != nil {
		return err
	}
	lock, err := acquireLock(ctx, filepath.Join(s.root, ".lock"))
	if err != nil {
		return err
	}
	defer lock.release()
	if err := s.verifyDirectories(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (s *Store) verifyDirectories() error {
	for _, p := range []struct {
		path string
		info os.FileInfo
	}{{s.root, s.rootInfo}, {filepath.Join(s.root, "messages"), s.messagesInfo}, {filepath.Join(s.root, "receipts"), s.receiptsInfo}} {
		info, err := os.Lstat(p.path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, p.info) {
			return errors.New("handoff directory was replaced or symlinked")
		}
	}
	return nil
}
