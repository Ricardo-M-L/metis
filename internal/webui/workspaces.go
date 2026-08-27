package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/session"
)

const workspaceRegistryVersion = 1

type workspaceRecord struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Name      string    `json:"name,omitempty"`
	Order     int       `json:"order"`
	Removed   bool      `json:"removed,omitempty"`
	AddedAt   time.Time `json:"addedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type workspaceRegistry struct {
	Version    int               `json:"version"`
	Workspaces []workspaceRecord `json:"workspaces"`
}

type workspaceView struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	Order      int    `json:"order"`
	Registered bool   `json:"registered"`
	Available  bool   `json:"available"`
	Active     bool   `json:"active"`
}

func workspaceRegistryPath() string {
	return filepath.Join(config.Home(), "workspaces.json")
}

// workspacePathKey returns the stable canonical path used for identity. A
// missing historical path keeps its cleaned absolute spelling; an available
// symlink resolves to the same identity as its target.
func workspacePathKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	return abs
}

func workspaceIDForPath(raw string) string {
	path := workspacePathKey(raw)
	if path == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:12])
}

func validateWorkspacePath(raw string) (string, error) {
	path := workspacePathKey(raw)
	if path == "" {
		return "", errors.New("workspace path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", errors.New("workspace path is not accessible")
	}
	if !info.IsDir() {
		return "", errors.New("workspace path is not a directory")
	}
	return path, nil
}

func validateWorkspaceName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("workspace name is required")
	}
	if len([]rune(name)) > 80 || strings.ContainsAny(name, "\r\n") {
		return "", errors.New("workspace name must be single-line and at most 80 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("workspace name contains control characters")
		}
	}
	return name, nil
}

func loadWorkspaceRegistry() (workspaceRegistry, error) {
	reg := workspaceRegistry{Version: workspaceRegistryVersion}
	data, err := os.ReadFile(workspaceRegistryPath())
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return reg, err
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return workspaceRegistry{Version: workspaceRegistryVersion}, err
	}
	if reg.Version != workspaceRegistryVersion {
		return workspaceRegistry{Version: workspaceRegistryVersion}, errors.New("unsupported workspace registry version")
	}
	seen := make(map[string]struct{}, len(reg.Workspaces))
	for i := range reg.Workspaces {
		path := workspacePathKey(reg.Workspaces[i].Path)
		id := workspaceIDForPath(path)
		if path == "" || id == "" {
			return workspaceRegistry{Version: workspaceRegistryVersion}, errors.New("invalid workspace registry path")
		}
		if _, duplicate := seen[id]; duplicate {
			return workspaceRegistry{Version: workspaceRegistryVersion}, errors.New("duplicate workspace registry path")
		}
		seen[id] = struct{}{}
		reg.Workspaces[i].Path = path
		reg.Workspaces[i].ID = id
	}
	return reg, nil
}

func saveWorkspaceRegistry(reg workspaceRegistry) (err error) {
	reg.Version = workspaceRegistryVersion
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	dir := config.Home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".workspaces-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, workspaceRegistryPath())
}

func workspaceDisplayName(path, configured string) string {
	if name := strings.TrimSpace(configured); name != "" {
		return name
	}
	if base := filepath.Base(path); base != "." && base != string(filepath.Separator) && base != "" {
		return base
	}
	return path
}

func workspaceAvailable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (s *Server) knownWorkspacePaths() map[string]string {
	paths := make(map[string]string)
	if cwd, err := os.Getwd(); err == nil {
		if path := workspacePathKey(cwd); path != "" {
			paths[workspaceIDForPath(path)] = path
		}
	}
	if s.store == nil {
		return paths
	}
	entries, err := s.store.ListResumable(session.ResumeListOptions{})
	if err != nil {
		return paths
	}
	for _, entry := range entries {
		hdr, _, err := s.store.LoadHeader(entry.ID)
		if err != nil || hdr == nil || hdr.WorkDir == "" {
			continue
		}
		if path := workspacePathKey(hdr.WorkDir); path != "" {
			paths[workspaceIDForPath(path)] = path
		}
	}
	return paths
}

func (s *Server) workspaceViews(reg workspaceRegistry) ([]workspaceView, string) {
	known := s.knownWorkspacePaths()
	activePath := ""
	if cwd, err := os.Getwd(); err == nil {
		activePath = workspacePathKey(cwd)
	}
	activeID := workspaceIDForPath(activePath)

	records := make(map[string]workspaceRecord, len(reg.Workspaces))
	removed := make(map[string]bool, len(reg.Workspaces))
	maxOrder := -1
	for _, record := range reg.Workspaces {
		records[record.ID] = record
		removed[record.ID] = record.Removed
		if record.Order > maxOrder {
			maxOrder = record.Order
		}
	}
	if activePath != "" && !removed[activeID] {
		known[activeID] = activePath
	}

	views := make([]workspaceView, 0, len(known)+len(records))
	seen := make(map[string]struct{}, len(known)+len(records))
	for _, record := range reg.Workspaces {
		if record.Removed {
			continue
		}
		views = append(views, workspaceView{
			ID: record.ID, Path: record.Path, Name: workspaceDisplayName(record.Path, record.Name),
			Order: record.Order, Registered: true, Available: workspaceAvailable(record.Path), Active: record.ID == activeID,
		})
		seen[record.ID] = struct{}{}
	}
	ids := make([]string, 0, len(known))
	for id := range known {
		if removed[id] {
			continue
		}
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return known[ids[i]] < known[ids[j]] })
	for _, id := range ids {
		path := known[id]
		maxOrder++
		views = append(views, workspaceView{
			ID: id, Path: path, Name: workspaceDisplayName(path, ""), Order: maxOrder,
			Registered: id == activeID, Available: workspaceAvailable(path), Active: id == activeID,
		})
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Order != views[j].Order {
			return views[i].Order < views[j].Order
		}
		return views[i].Name < views[j].Name
	})
	return views, activeID
}

func removedWorkspaceIDs(reg workspaceRegistry) []string {
	ids := make([]string, 0)
	for _, record := range reg.Workspaces {
		if record.Removed {
			ids = append(ids, record.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func maxWorkspaceOrder(reg workspaceRegistry) int {
	maxOrder := -1
	for _, record := range reg.Workspaces {
		if record.Order > maxOrder {
			maxOrder = record.Order
		}
	}
	return maxOrder
}

func upsertWorkspaceRecord(reg *workspaceRegistry, path, name string, order int) *workspaceRecord {
	id := workspaceIDForPath(path)
	now := time.Now().UTC()
	for i := range reg.Workspaces {
		if reg.Workspaces[i].ID != id {
			continue
		}
		reg.Workspaces[i].Path = path
		if name != "" {
			reg.Workspaces[i].Name = name
		}
		if order >= 0 {
			reg.Workspaces[i].Order = order
		}
		reg.Workspaces[i].Removed = false
		reg.Workspaces[i].UpdatedAt = now
		return &reg.Workspaces[i]
	}
	if order < 0 {
		order = maxWorkspaceOrder(*reg) + 1
	}
	reg.Workspaces = append(reg.Workspaces, workspaceRecord{
		ID: id, Path: path, Name: name, Order: order, AddedAt: now, UpdatedAt: now,
	})
	return &reg.Workspaces[len(reg.Workspaces)-1]
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	s.workspacesMu.Lock()
	defer s.workspacesMu.Unlock()
	reg, err := loadWorkspaceRegistry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace registry unreadable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		views, activeID := s.workspaceViews(reg)
		writeJSON(w, http.StatusOK, map[string]any{
			"workspaces": views,
			"activeId":   activeID,
			"removedIds": removedWorkspaceIDs(reg),
		})
	case http.MethodPost:
		var body struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		path, err := validateWorkspacePath(body.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		name := ""
		if strings.TrimSpace(body.Name) != "" {
			name, err = validateWorkspaceName(body.Name)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		record := upsertWorkspaceRecord(&reg, path, name, -1)
		if err := saveWorkspaceRegistry(reg); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save workspace")
			return
		}
		writeJSON(w, http.StatusCreated, workspaceView{
			ID: record.ID, Path: record.Path, Name: workspaceDisplayName(record.Path, record.Name), Order: record.Order,
			Registered: true, Available: true, Active: record.ID == workspaceIDForPath(mustCurrentWorkspace()),
		})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func mustCurrentWorkspace() string {
	cwd, _ := os.Getwd()
	return cwd
}

func (s *Server) findWorkspacePath(reg workspaceRegistry, id string) string {
	for _, record := range reg.Workspaces {
		if record.ID == id {
			return record.Path
		}
	}
	return s.knownWorkspacePaths()[id]
}

func (s *Server) handleWorkspaceRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.workspacesMu.Lock()
	defer s.workspacesMu.Unlock()
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	name, err := validateWorkspaceName(body.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	reg, err := loadWorkspaceRegistry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace registry unreadable")
		return
	}
	path := s.findWorkspacePath(reg, body.ID)
	if path == "" {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	record := upsertWorkspaceRecord(&reg, path, name, -1)
	if err := saveWorkspaceRegistry(reg); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to rename workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": record.ID, "name": record.Name})
}

func (s *Server) handleWorkspaceRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.workspacesMu.Lock()
	defer s.workspacesMu.Unlock()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "workspace id is required")
		return
	}
	reg, err := loadWorkspaceRegistry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace registry unreadable")
		return
	}
	path := s.findWorkspacePath(reg, body.ID)
	if path == "" {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	record := upsertWorkspaceRecord(&reg, path, "", -1)
	record.Removed = true
	record.UpdatedAt = time.Now().UTC()
	if err := saveWorkspaceRegistry(reg); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "id": body.ID})
}

func (s *Server) handleWorkspaceReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.workspacesMu.Lock()
	defer s.workspacesMu.Unlock()
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&body); err != nil || len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "workspace ids are required")
		return
	}
	reg, err := loadWorkspaceRegistry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace registry unreadable")
		return
	}
	known := s.knownWorkspacePaths()
	for _, record := range reg.Workspaces {
		known[record.ID] = record.Path
	}
	seen := make(map[string]struct{}, len(body.IDs))
	for order, id := range body.IDs {
		if _, duplicate := seen[id]; duplicate {
			writeError(w, http.StatusBadRequest, "duplicate workspace id")
			return
		}
		seen[id] = struct{}{}
		path := known[id]
		if path == "" {
			writeError(w, http.StatusBadRequest, "unknown workspace id")
			return
		}
		upsertWorkspaceRecord(&reg, path, "", order)
	}
	if err := saveWorkspaceRegistry(reg); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reorder workspaces")
		return
	}
	views, _ := s.workspaceViews(reg)
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": views})
}

func (s *Server) handleWorkspaceOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "workspace id is required")
		return
	}
	s.workspacesMu.Lock()
	reg, err := loadWorkspaceRegistry()
	path := ""
	if err == nil {
		path = s.findWorkspacePath(reg, body.ID)
	}
	s.workspacesMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "workspace registry unreadable")
		return
	}
	path, err = validateWorkspacePath(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.openWorkspace == nil {
		writeError(w, http.StatusServiceUnavailable, "native workspace launcher unavailable")
		return
	}
	if err := s.openWorkspace(path); err != nil {
		writeError(w, http.StatusBadGateway, "failed to open workspace: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"opened": true, "id": body.ID})
}
