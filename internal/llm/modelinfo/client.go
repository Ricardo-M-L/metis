package modelinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Earlier schemas discarded profile or issuer evidence. Their records cannot
// safely be reclassified after the fact, so require endpoint rediscovery.
const cacheSchema = 4

type entry struct {
	binding   Binding
	models    []Model
	updatedAt time.Time
	version   uint64
}

type cacheRecord struct {
	Schema    int       `json:"schema"`
	Binding   Binding   `json:"binding"`
	UpdatedAt time.Time `json:"updated_at"`
	Models    []Model   `json:"models"`
}

// Client retains validated capabilities for each exact route. Refresh calls
// for one route serialize without blocking Get, and do not create goroutines.
type Client struct {
	mu           sync.RWMutex
	entries      map[string]entry
	cacheFiles   map[string]os.FileInfo
	refreshLocks map[string]chan struct{}
	cacheDir     string
	timeout      time.Duration
	ttl          time.Duration
	httpClient   *http.Client
}

// New loads existing capability cache records before returning. A corrupt or
// stale-format record is ignored so it cannot disable other endpoint bindings.
// An empty cacheDir selects a memory-only client.
func New(cacheDir string, opts ...Option) (*Client, error) {
	o := options{timeout: 5 * time.Second, ttl: 15 * time.Minute}
	for _, option := range opts {
		option(&o)
	}
	if o.timeout <= 0 || o.timeout > 30*time.Second || o.ttl < 0 {
		return nil, errors.New("model discovery: invalid timeout or TTL")
	}
	c := &Client{
		entries: make(map[string]entry), cacheFiles: make(map[string]os.FileInfo), refreshLocks: make(map[string]chan struct{}),
		cacheDir: cacheDir, timeout: o.timeout, ttl: o.ttl,
		httpClient: capabilityHTTPClient(o.httpClient, o.timeout),
	}
	if cacheDir != "" {
		if err := ensureCacheDir(cacheDir); err != nil {
			return nil, err
		}
		if err := c.load(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func ensureCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("model discovery: cannot create cache directory")
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("model discovery: unsafe cache directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return errors.New("model discovery: cannot protect cache directory")
	}
	return nil
}

func (c *Client) load() error {
	files, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return errors.New("model discovery: cannot read cache directory")
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		// Filenames are fixed-size binding hashes, not endpoint paths or IDs.
		if len(file.Name()) != 69 || strings.Trim(strings.TrimSuffix(file.Name(), ".json"), "0123456789abcdef") != "" {
			continue
		}
		key := strings.TrimSuffix(file.Name(), ".json")
		e, info, err := c.readCacheFile(key)
		if err != nil {
			continue
		}
		e.version = 1
		c.entries[key] = e
		c.cacheFiles[key] = info
	}
	return nil
}

func (c *Client) readCacheFile(key string) (entry, os.FileInfo, error) {
	name := filepath.Join(c.cacheDir, key+".json")
	st, err := os.Lstat(name)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > maxResponseBytes {
		return entry{}, nil, ErrInvalidResponse
	}
	f, err := os.Open(name)
	if err != nil {
		return entry{}, nil, errors.New("model discovery: cannot read capability cache")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return entry{}, nil, ErrInvalidResponse
	}
	data, err := io.ReadAll(io.LimitReader(f, maxResponseBytes+1))
	current, currentErr := os.Lstat(name)
	if err != nil || len(data) > maxResponseBytes || currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(st, current) || st.ModTime() != current.ModTime() || st.Size() != current.Size() {
		return entry{}, nil, ErrInvalidResponse
	}
	var record cacheRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&record); err != nil {
		return entry{}, nil, ErrInvalidResponse
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return entry{}, nil, ErrInvalidResponse
	}
	if record.Schema != cacheSchema || record.UpdatedAt.IsZero() || record.UpdatedAt.After(time.Now().Add(time.Minute)) || len(record.Models) > maxModels {
		return entry{}, nil, ErrInvalidResponse
	}
	b, recordKey, err := normalizeBinding(record.Binding)
	if err != nil || recordKey != key || !validCachedModels(record.Models) {
		return entry{}, nil, ErrInvalidResponse
	}
	sortModels(record.Models)
	return entry{binding: b, models: record.Models, updatedAt: record.UpdatedAt}, current, nil
}

// Reload sees a newer cache published by another Metis process, without making
// a network request. Get remains a pure in-memory read. Unchanged files require
// only stat; reads are bounded and retain the previous snapshot on corruption.
func (c *Client) Reload(binding Binding) error {
	_, key, err := normalizeBinding(binding)
	if err != nil {
		return err
	}
	if c.cacheDir == "" {
		return nil
	}
	dir, err := os.Lstat(c.cacheDir)
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 || dir.Mode().Perm() != 0700 {
		return errors.New("model discovery: unsafe cache directory")
	}
	info, err := os.Lstat(filepath.Join(c.cacheDir, key+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("model discovery: cannot inspect capability cache")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxResponseBytes {
		return ErrInvalidResponse
	}
	c.mu.RLock()
	previousInfo := c.cacheFiles[key]
	c.mu.RUnlock()
	if previousInfo != nil && os.SameFile(previousInfo, info) && previousInfo.ModTime() == info.ModTime() && previousInfo.Size() == info.Size() && previousInfo.Mode() == info.Mode() {
		return nil
	}
	e, info, err := c.readCacheFile(key)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, found := c.entries[key]
	// Another process's older record must not roll back an already validated
	// snapshot. Cache timestamp determines freshness, not filesystem mtime.
	if !found || e.updatedAt.After(previous.updatedAt) {
		e.version = previous.version
		if !found || !reflect.DeepEqual(previous.models, e.models) {
			e.version++
		}
		c.entries[key] = e
	}
	c.cacheFiles[key] = info
	return nil
}

func validCachedModels(models []Model) bool {
	seen := make(map[string]bool, len(models))
	byID := make(map[string]Model, len(models))
	for _, m := range models {
		if !validLabel(m.ID, 512) || (m.Name != "" && !validLabel(m.Name, 512)) || seen[m.ID] {
			return false
		}
		if m.DisplayName != "" && !validLabel(m.DisplayName, 512) {
			return false
		}
		if m.OwnedBy == "" {
			if m.OwnerSource != "" || m.OwnerModelID != "" {
				return false
			}
		} else {
			if !validLabel(m.OwnedBy, 256) || !validLabel(m.OwnerModelID, 512) {
				return false
			}
			if m.OwnerSource != SourceEndpointPI && m.OwnerSource != SourceEndpointModels {
				return false
			}
		}
		if m.Source != SourceEndpointPI && m.Source != SourceEndpointModels && m.Source != SourceEndpointMerged {
			return false
		}
		if m.WindowKind != "" && m.WindowKind != WindowKindCapacity && m.WindowKind != WindowKindClientProfile {
			return false
		}
		if m.LimitEvidence != "" && m.LimitEvidence != LimitEvidenceServedCapacity {
			return false
		}
		if !validAssociatedLimits(m.AssocLimits) {
			return false
		}
		if m.AssocLimits != (AssociatedLimits{}) && (m.WindowKind != WindowKindClientProfile || m.AssocLimits.Source != SourceEndpointModels) {
			return false
		}
		if m.WindowKind == WindowKindClientProfile && (m.ContextWindow <= 0 || m.MaxContextWindow <= 0) {
			return false
		}
		if m.ContextWindow < 0 || m.ContextWindow > maxCapabilityTokens || m.MaxContextWindow < 0 || m.MaxContextWindow > maxCapabilityTokens || m.MaxInputTokens < 0 || m.MaxInputTokens > maxCapabilityTokens || m.MaxOutputTokens < 0 || m.MaxOutputTokens > maxCapabilityTokens {
			return false
		}
		if m.ContextWindow > 0 && m.MaxContextWindow > 0 && m.ContextWindow > m.MaxContextWindow {
			return false
		}
		seen[m.ID] = true
		byID[m.ID] = m
	}
	for _, m := range models {
		if m.OwnedBy == "" || m.OwnerModelID == m.ID {
			continue
		}
		// A cross-ID issuer statement is only valid together with the original
		// ordinary record on this binding. It does not authorize model rewrites.
		peer, found := byID[m.OwnerModelID]
		if !found || m.WindowKind != WindowKindClientProfile || m.OwnerSource != SourceEndpointModels || peer.OwnerSource != SourceEndpointModels || peer.OwnedBy != m.OwnedBy || peer.OwnerModelID != peer.ID || peer.DisplayName != m.ID {
			return false
		}
	}
	for _, m := range models {
		limits := m.AssocLimits
		if limits == (AssociatedLimits{}) {
			continue
		}
		if limits.ModelID == m.ID {
			if limits.Evidence == AssocEvidenceDeclaredServed && m.LimitEvidence != LimitEvidenceServedCapacity {
				return false
			}
			continue
		}
		peer, found := byID[limits.ModelID]
		if !found || peer.WindowKind == WindowKindClientProfile || peer.DisplayName != m.ID || m.OwnedBy == "" || peer.OwnedBy != m.OwnedBy || peer.OwnerModelID != peer.ID || peer.OwnerSource != SourceEndpointModels || associatedLimits(peer) != limits {
			return false
		}
		matches := 0
		for _, candidate := range models {
			if candidate.ID != m.ID && candidate.DisplayName == m.ID {
				matches++
			}
		}
		if matches != 1 {
			return false
		}
	}
	return true
}

func validAssociatedLimits(limits AssociatedLimits) bool {
	if limits == (AssociatedLimits{}) {
		return true
	}
	if limits.Source != SourceEndpointModels || !validLabel(limits.ModelID, 512) {
		return false
	}
	for _, n := range []int{limits.ContextWindow, limits.MaxContextWindow, limits.MaxInputTokens, limits.MaxOutputTokens} {
		if n < 0 || n > maxCapabilityTokens {
			return false
		}
	}
	if limits.ContextWindow > 0 && limits.MaxContextWindow > 0 && limits.ContextWindow > limits.MaxContextWindow {
		return false
	}
	switch limits.Evidence {
	case AssocEvidenceCapacityWindow:
		return (limits.ContextWindow > 0 || limits.MaxContextWindow > 0) && limits.MaxInputTokens == 0 && limits.MaxOutputTokens == 0
	case AssocEvidenceDeclaredServed:
		return limits.ContextWindow > 0 || limits.MaxContextWindow > 0 || limits.MaxInputTokens > 0 || limits.MaxOutputTokens > 0
	default:
		return false
	}
}

func sortModels(models []Model) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}

func (c *Client) write(key string, e entry) error {
	if c.cacheDir == "" {
		return nil
	}
	if err := ensureCacheDir(c.cacheDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheRecord{Schema: cacheSchema, Binding: e.binding, UpdatedAt: e.updatedAt, Models: e.models}, "", "  ")
	if err != nil {
		return errors.New("model discovery: cannot encode capability cache")
	}
	if len(data) > maxResponseBytes {
		return ErrResponseTooLarge
	}
	f, err := os.CreateTemp(c.cacheDir, ".capability-*")
	if err != nil {
		return errors.New("model discovery: cannot create capability cache")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return errors.New("model discovery: cannot protect capability cache")
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return errors.New("model discovery: cannot write capability cache")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errors.New("model discovery: cannot sync capability cache")
	}
	if err := f.Close(); err != nil {
		return errors.New("model discovery: cannot close capability cache")
	}
	if err := os.Rename(temp, filepath.Join(c.cacheDir, key+".json")); err != nil {
		return errors.New("model discovery: cannot publish capability cache")
	}
	return nil
}

// Refresh discovers and atomically publishes capabilities. A failed response
// leaves the previous snapshot available. Cache I/O failures are reported and
// do not publish a snapshot that could be lost across restart.
func (c *Client) Refresh(ctx context.Context, binding Binding, key string) ([]Model, error) {
	return c.refresh(ctx, binding, key, false)
}

func (c *Client) RefreshIfStale(ctx context.Context, binding Binding, key string) ([]Model, error) {
	return c.refresh(ctx, binding, key, true)
}

func (c *Client) refresh(ctx context.Context, binding Binding, authKey string, onlyStale bool) ([]Model, error) {
	b, key, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	callerCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onlyStale {
		// Corrupt caches do not prevent a bounded network refresh from repairing
		// them, and they never replace the last validated in-memory snapshot.
		_ = c.Reload(b)
	}
	c.mu.Lock()
	lock := c.refreshLocks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		c.refreshLocks[key] = lock
	}
	c.mu.Unlock()
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-lock }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	previous, found := c.entries[key]
	c.mu.RUnlock()
	if onlyStale && found && time.Since(previous.updatedAt) < c.ttl {
		return append([]Model(nil), previous.models...), nil
	}
	models, err := c.discover(ctx, callerCtx, b, authKey)
	if err != nil {
		return nil, err
	}
	if err := callerCtx.Err(); err != nil {
		return nil, err
	}
	sortModels(models)
	version := previous.version
	if !found || !reflect.DeepEqual(previous.models, models) {
		version++
	}
	e := entry{binding: b, models: models, updatedAt: time.Now().UTC(), version: version}
	if err := c.write(key, e); err != nil {
		return nil, err
	}
	c.mu.Lock()
	// Reload can publish another process's snapshot while this request is in
	// flight. Publish versions against the current snapshot, never a stale
	// version captured before the HTTP request.
	current, currentFound := c.entries[key]
	if currentFound && current.updatedAt.After(e.updatedAt) {
		models = current.models
	} else {
		e.version = current.version
		if !currentFound || !reflect.DeepEqual(current.models, models) {
			e.version++
		}
		c.entries[key] = e
	}
	c.mu.Unlock()
	return append([]Model(nil), models...), nil
}

func (c *Client) Get(binding Binding, modelID string) (Model, bool) {
	_, key, err := normalizeBinding(binding)
	if err != nil || !validLabel(modelID, 512) {
		return Model{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.entries[key].models {
		if m.ID == modelID {
			return m, true
		}
	}
	return Model{}, false
}

func (c *Client) Models(binding Binding) []Model {
	_, key, err := normalizeBinding(binding)
	if err != nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Model(nil), c.entries[key].models...)
}

func (c *Client) Version(binding Binding) uint64 {
	_, key, err := normalizeBinding(binding)
	if err != nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[key].version
}
