package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopPresentationModeDefaultsAndMigratesOldPreferences(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	s, _ := testServer(t)

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("default GET: %d %s", rr.Code, rr.Body.String())
	}
	var prefs desktopPreferences
	if err := json.Unmarshal(rr.Body.Bytes(), &prefs); err != nil {
		t.Fatal(err)
	}
	if prefs.PresentationMode != "standard" {
		t.Fatalf("default presentation mode = %q, want standard", prefs.PresentationMode)
	}

	// An older Desktop file has no presentationMode but keeps its other choices.
	legacy := `{"busyEnter":"send","sidebarView":"flat","sidebarSort":"recent","defaultPreset":"standard","language":"en","rootTurnParallelism":8}`
	if err := os.WriteFile(filepath.Join(home, "desktop-preferences.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/preferences", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy GET: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &prefs); err != nil {
		t.Fatal(err)
	}
	if prefs.PresentationMode != "standard" || prefs.Language != "en" || prefs.BusyEnter != "send" {
		t.Fatalf("legacy migration changed preferences: %+v", prefs)
	}
}

func TestDesktopPresentationModePersistsAndRejectsInvalidValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	s, _ := testServer(t)

	for _, mode := range []string{"compact", "standard", "detailed", "verbose"} {
		rr := httptest.NewRecorder()
		body, err := json.Marshal(map[string]string{"presentationMode": mode})
		if err != nil {
			t.Fatal(err)
		}
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences", bytes.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("save %q: %d %s", mode, rr.Code, rr.Body.String())
		}
		got, err := loadDesktopPreferences()
		if err != nil {
			t.Fatal(err)
		}
		if got.PresentationMode != mode {
			t.Fatalf("persisted presentation mode = %q, want %q", got.PresentationMode, mode)
		}
	}

	before, err := os.ReadFile(filepath.Join(home, "desktop-preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "expanded", "VERBOSE"} {
		rr := httptest.NewRecorder()
		body, err := json.Marshal(map[string]string{"presentationMode": invalid})
		if err != nil {
			t.Fatal(err)
		}
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/preferences", bytes.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid mode %q: %d %s", invalid, rr.Code, rr.Body.String())
		}
		after, err := os.ReadFile(filepath.Join(home, "desktop-preferences.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("invalid mode %q overwrote saved preferences", invalid)
		}
	}
}
