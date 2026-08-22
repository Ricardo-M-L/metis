package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// desktopPreferences contains UI-only choices that do not belong in the
// agent's config.toml. They live below METIS_HOME so a Wails launch on a new
// random loopback port keeps the same behavior (localStorage is origin/port
// scoped and therefore cannot provide that guarantee).
type desktopPreferences struct {
	BusyEnter     string   `json:"busyEnter"`     // queue | send
	SidebarView   string   `json:"sidebarView"`   // grouped | flat
	SidebarSort   string   `json:"sidebarSort"`   // recent | name | manual
	SessionOrder  []string `json:"sessionOrder"`  // stable ids, manual mode
	DefaultPreset string   `json:"defaultPreset"` // standard | agent profile name
	Language      string   `json:"language"`      // auto | en | zh-CN
}

func defaultDesktopPreferences() desktopPreferences {
	return desktopPreferences{BusyEnter: "queue", SidebarView: "grouped", SidebarSort: "recent", DefaultPreset: "standard", Language: "zh-CN"}
}

var desktopPresetName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func desktopPreferencesPath() string {
	return filepath.Join(config.Home(), "desktop-preferences.json")
}

func loadDesktopPreferences() (desktopPreferences, error) {
	prefs := defaultDesktopPreferences()
	data, err := os.ReadFile(desktopPreferencesPath())
	if errors.Is(err, os.ErrNotExist) {
		return prefs, nil
	}
	if err != nil {
		return prefs, err
	}
	if err := json.Unmarshal(data, &prefs); err != nil {
		return defaultDesktopPreferences(), err
	}
	// Migrate preference files written before presets/manual ordering existed.
	if prefs.DefaultPreset == "" {
		prefs.DefaultPreset = "standard"
	}
	if prefs.Language == "" {
		prefs.Language = "zh-CN"
	}
	if !validDesktopPreferences(prefs) {
		return defaultDesktopPreferences(), errors.New("invalid desktop preferences")
	}
	return prefs, nil
}

func validDesktopPreferences(p desktopPreferences) bool {
	if p.BusyEnter != "queue" && p.BusyEnter != "send" {
		return false
	}
	if p.SidebarView != "grouped" && p.SidebarView != "flat" {
		return false
	}
	if p.SidebarSort != "recent" && p.SidebarSort != "name" && p.SidebarSort != "manual" {
		return false
	}
	if p.DefaultPreset == "" || !desktopPresetName.MatchString(p.DefaultPreset) || len(p.SessionOrder) > 10000 {
		return false
	}
	if p.Language != "auto" && p.Language != "en" && p.Language != "zh-CN" {
		return false
	}
	seen := make(map[string]struct{}, len(p.SessionOrder))
	for _, id := range p.SessionOrder {
		if id == "" || len(id) > 128 {
			return false
		}
		if _, exists := seen[id]; exists {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// DesktopLaunchPreferences exposes only process-start choices. The command
// layer uses this before setupRuntime so an Agent preset can shape the system
// prompt and tool registry atomically rather than being half-applied live.
type DesktopLaunchPreferences struct {
	DefaultPreset string
}

func LoadDesktopLaunchPreferences() (DesktopLaunchPreferences, error) {
	prefs, err := loadDesktopPreferences()
	return DesktopLaunchPreferences{DefaultPreset: prefs.DefaultPreset}, err
}

func saveDesktopPreferences(prefs desktopPreferences) (err error) {
	if !validDesktopPreferences(prefs) {
		return errors.New("invalid desktop preferences")
	}
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	dir := config.Home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".desktop-preferences-*.tmp")
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
	return os.Rename(tmpPath, desktopPreferencesPath())
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	s.prefsMu.Lock()
	defer s.prefsMu.Unlock()

	prefs, err := loadDesktopPreferences()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "desktop preferences unreadable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, prefs)
	case http.MethodPost:
		var body struct {
			BusyEnter     *string   `json:"busyEnter"`
			SidebarView   *string   `json:"sidebarView"`
			SidebarSort   *string   `json:"sidebarSort"`
			SessionOrder  *[]string `json:"sessionOrder"`
			DefaultPreset *string   `json:"defaultPreset"`
			Language      *string   `json:"language"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if body.BusyEnter == nil && body.SidebarView == nil && body.SidebarSort == nil && body.SessionOrder == nil && body.DefaultPreset == nil && body.Language == nil {
			writeError(w, http.StatusBadRequest, "no changes")
			return
		}
		if body.BusyEnter != nil {
			prefs.BusyEnter = *body.BusyEnter
		}
		if body.SidebarView != nil {
			prefs.SidebarView = *body.SidebarView
		}
		if body.SidebarSort != nil {
			prefs.SidebarSort = *body.SidebarSort
		}
		if body.SessionOrder != nil {
			prefs.SessionOrder = append([]string(nil), (*body.SessionOrder)...)
		}
		if body.DefaultPreset != nil {
			prefs.DefaultPreset = *body.DefaultPreset
		}
		if body.Language != nil {
			prefs.Language = *body.Language
		}
		if err := saveDesktopPreferences(prefs); err != nil {
			if !validDesktopPreferences(prefs) {
				writeError(w, http.StatusBadRequest, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, prefs)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
