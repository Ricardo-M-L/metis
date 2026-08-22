package webui

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The settings panel is backed by real config: GET lists editable entries
// with values, POST persists through SaveUserSettingsAndLoad (scoped to a
// temp METIS_HOME so the dev machine's config.toml is never touched).
func TestSettingsListAndSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	s, _ := testServer(t)
	h := s.handler()

	// GET: entries are present with sane defaults.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/settings", nil))
	if rr.Code != 200 {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var listed struct {
		Settings []struct {
			Key     string   `json:"key"`
			Value   string   `json:"value"`
			Type    string   `json:"type"`
			Options []string `json:"options"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, st := range listed.Settings {
		byKey[st.Key] = st.Value
	}
	for _, key := range []string{"permission.mode", "ui.theme", "session.max_iterations", "loop_detection.disabled"} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("settings list missing %q: %+v", key, byKey)
		}
	}

	// POST: a valid change persists.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings",
		bytes.NewBufferString(`{"changes":[{"key":"ui.theme","value":"nord"}]}`)))
	if rr.Code != 200 {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	var saved struct {
		LiveApplied []string `json:"liveApplied"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.LiveApplied) != 1 || saved.LiveApplied[0] != "ui.theme" {
		t.Fatalf("ui.theme should apply live: %+v", saved.LiveApplied)
	}

	// GET again: the value round-tripped.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/settings", nil))
	json.Unmarshal(rr.Body.Bytes(), &listed)
	for _, st := range listed.Settings {
		if st.Key == "ui.theme" && st.Value != "nord" {
			t.Fatalf("theme not persisted: %q", st.Value)
		}
	}

	// Rejects: unknown key, bad value, empty body.
	for _, body := range []string{
		`{"changes":[{"key":"provider.anthropic.api_key","value":"x"}]}`,
		`{"changes":[{"key":"ui.theme","value":"hotdog-stand"}]}`,
		`{"changes":[{"key":"session.max_iterations","value":"-5"}]}`,
		`{}`,
	} {
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/settings", bytes.NewBufferString(body)))
		if rr.Code == 200 {
			t.Fatalf("body %s should be rejected", body)
		}
	}
}
