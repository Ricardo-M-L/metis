package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

type presetView struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Source         string   `json:"source"` // standard | bundled | user | project
	Custom         bool     `json:"custom"`
	Default        bool     `json:"default"`
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permissionMode,omitempty"`
	Effort         string   `json:"effort,omitempty"`
	MaxTurns       int      `json:"maxTurns,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	PromptPreview  string   `json:"promptPreview,omitempty"`
}

func titlePreset(name string) string {
	if name == "standard" {
		return "Standard"
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i := range parts {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, " ")
}

func compactPreview(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

func listPresetViews() ([]presetView, error) {
	prefs, err := loadDesktopPreferences()
	if err != nil {
		return nil, err
	}
	source := map[string]string{}
	for _, name := range rtpkg.BuiltinProfileNames() {
		source[name] = "bundled"
	}
	scan := func(dir, label string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".md")
			if desktopPresetName.MatchString(name) {
				source[name] = label
			}
		}
		return nil
	}
	if err := scan(filepath.Join(config.Home(), "agents"), "user"); err != nil {
		return nil, err
	}
	// Project profiles win over user and bundled profiles, matching the runtime.
	if err := scan(filepath.Join(".metis", "agents"), "project"); err != nil {
		return nil, err
	}
	out := []presetView{{
		ID: "standard", Name: "Standard", Description: "Use the normal Metis system prompt and complete tool set.",
		Source: "standard", Default: prefs.DefaultPreset == "standard",
	}}
	names := make([]string, 0, len(source))
	for name := range source {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		profile, err := rtpkg.LoadAgentProfile(name)
		if err != nil || profile == nil {
			continue
		}
		display := titlePreset(profile.Name)
		if display == "" {
			display = titlePreset(name)
		}
		out = append(out, presetView{
			ID: name, Name: display, Description: profile.Description, Source: source[name],
			Custom: source[name] == "user", Default: prefs.DefaultPreset == name,
			Model: profile.Model, PermissionMode: profile.PermissionMode, Effort: profile.Effort,
			MaxTurns: profile.MaxTurns, Tools: append([]string(nil), profile.Tools...),
			PromptPreview: compactPreview(profile.SystemPrompt, 280),
		})
	}
	return out, nil
}

func singleLineFrontmatter(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "\r", " "), "\n", " ")
}

func serializePresetCopy(profile *rtpkg.AgentProfile, name string) []byte {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(name)
	b.WriteByte('\n')
	if profile.Description != "" {
		b.WriteString("description: ")
		b.WriteString(singleLineFrontmatter(profile.Description))
		b.WriteByte('\n')
	}
	if profile.Model != "" {
		fmt.Fprintf(&b, "model: %s\n", singleLineFrontmatter(profile.Model))
	}
	if len(profile.Tools) > 0 {
		fmt.Fprintf(&b, "tools: %s\n", strings.Join(profile.Tools, ", "))
	}
	if len(profile.DisallowedTools) > 0 {
		fmt.Fprintf(&b, "disallowed_tools: %s\n", strings.Join(profile.DisallowedTools, ", "))
	}
	if profile.PermissionMode != "" {
		fmt.Fprintf(&b, "permission_mode: %s\n", profile.PermissionMode)
	}
	if profile.Effort != "" {
		fmt.Fprintf(&b, "effort: %s\n", profile.Effort)
	}
	if profile.MaxTurns > 0 {
		fmt.Fprintf(&b, "max_turns: %d\n", profile.MaxTurns)
	}
	if len(profile.Skills) > 0 {
		fmt.Fprintf(&b, "skills: %s\n", strings.Join(profile.Skills, ", "))
	}
	if profile.OmitClaudeMd {
		b.WriteString("omit_claude_md: true\n")
	}
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(profile.SystemPrompt))
	b.WriteByte('\n')
	return []byte(b.String())
}

func writeNewPreset(path string, data []byte) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		views, err := listPresetViews()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "presets unreadable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"presets": views, "directory": filepath.Join(config.Home(), "agents")})
	case http.MethodPost:
		var body struct {
			Source string `json:"source"`
			Target string `json:"target"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		body.Source = strings.TrimSpace(body.Source)
		body.Target = strings.TrimSpace(body.Target)
		if body.Source == "standard" || !desktopPresetName.MatchString(body.Target) {
			writeError(w, http.StatusBadRequest, "choose a profile source and a target name using letters, numbers, dot, dash, or underscore")
			return
		}
		profile, err := rtpkg.LoadAgentProfile(body.Source)
		if err != nil || profile == nil {
			writeError(w, http.StatusNotFound, "source preset not found")
			return
		}
		path := filepath.Join(config.Home(), "agents", body.Target+".md")
		if err := writeNewPreset(path, serializePresetCopy(profile, body.Target)); err != nil {
			if errors.Is(err, os.ErrExist) {
				writeError(w, http.StatusConflict, "target preset already exists")
			} else {
				writeError(w, http.StatusInternalServerError, "preset copy failed")
			}
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": body.Target, "path": path, "restartRequired": true})
	case http.MethodDelete:
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || !desktopPresetName.MatchString(body.ID) {
			writeError(w, http.StatusBadRequest, "valid preset id is required")
			return
		}
		prefs, err := loadDesktopPreferences()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "preferences unreadable")
			return
		}
		if prefs.DefaultPreset == body.ID {
			writeError(w, http.StatusConflict, "select another default preset first")
			return
		}
		path := filepath.Join(config.Home(), "agents", body.ID+".md")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			writeError(w, http.StatusNotFound, "custom user preset not found")
			return
		}
		trash := filepath.Join(config.Home(), "agents", ".trash")
		if err := os.MkdirAll(trash, 0o700); err != nil {
			writeError(w, http.StatusInternalServerError, "preset trash unavailable")
			return
		}
		dest := filepath.Join(trash, body.ID+"-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".md")
		if err := os.Rename(path, dest); err != nil {
			writeError(w, http.StatusInternalServerError, "preset removal failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "recoverableAt": dest, "restartRequired": true})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handlePresetDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil || !desktopPresetName.MatchString(body.ID) {
		writeError(w, http.StatusBadRequest, "valid preset id is required")
		return
	}
	if body.ID != "standard" {
		if profile, err := rtpkg.LoadAgentProfile(body.ID); err != nil || profile == nil {
			writeError(w, http.StatusNotFound, "preset not found")
			return
		}
	}
	prefs, err := loadDesktopPreferences()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "preferences unreadable")
		return
	}
	prefs.DefaultPreset = body.ID
	if err := saveDesktopPreferences(prefs); err != nil {
		writeError(w, http.StatusInternalServerError, "default preset save failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": body.ID, "restartRequired": true})
}

func (s *Server) handlePresetDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dir := filepath.Join(config.Home(), "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "preset directory unavailable")
		return
	}
	if s.openPath == nil {
		writeError(w, http.StatusServiceUnavailable, "file manager integration unavailable")
		return
	}
	if err := s.openPath(dir); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": true, "path": dir})
}
