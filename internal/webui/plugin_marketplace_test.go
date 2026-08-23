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

func writeMarketplaceFixture(t *testing.T, home string) {
	t.Helper()
	registry := map[string]any{
		"fixture-market": map[string]any{
			"source": map[string]any{"source": "github", "repo": "example/plugins"},
		},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "marketplaces.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "plugins", "marketplaces", "fixture-market")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "plugins", "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "plugins", "docs", ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "plugins", "docs", "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name": "Fixture marketplace",
  "plugins": [
    {
      "name": "document-tools",
      "description": "Read and create documents",
      "source": "./plugins/docs",
      "skills": ["./plugins/docs/SKILL.md"],
      "homepage": "https://example.com/document-tools"
    },
    {
      "name": "external-tool",
      "description": "Pinned external source not supported yet",
      "source": {"source": "url", "url": "https://example.com/external.git"}
    },
    {
      "name": "escape-tool",
      "description": "Traversal fixture",
      "source": "../../outside"
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: document-tools\ndescription: document fixture\n---\nfixture\n"
	if err := os.WriteFile(filepath.Join(root, "plugins", "docs", "SKILL.md"), []byte(skill), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginManifest := `{"name":"document-tools","interface":{"displayName":"Document Tools","logo":"./assets/icon.svg","brandColor":"#4668ff"}}`
	if err := os.WriteFile(filepath.Join(root, "plugins", "docs", ".claude-plugin", "plugin.json"), []byte(pluginManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	icon := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" fill="#4668ff"/></svg>`
	if err := os.WriteFile(filepath.Join(root, "plugins", "docs", "assets", "icon.svg"), []byte(icon), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPluginMarketplaceCatalogInstallAndRecoverableRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	writeMarketplaceFixture(t, home)

	s, _ := testServer(t)
	h := s.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plugins/catalog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"name":"document-tools"`)) ||
		!bytes.Contains(rr.Body.Bytes(), []byte(`"marketplace":"fixture-market"`)) ||
		!bytes.Contains(rr.Body.Bytes(), []byte(`"installable":true`)) ||
		!bytes.Contains(rr.Body.Bytes(), []byte(`"displayName":"Document Tools"`)) ||
		!bytes.Contains(rr.Body.Bytes(), []byte(`"icon":"/api/plugins/icon?marketplace=fixture-market\u0026plugin=document-tools"`)) {
		t.Fatalf("catalog missing installable fixture: %s", rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"name":"external-tool"`)) ||
		!bytes.Contains(rr.Body.Bytes(), []byte(`"installable":false`)) {
		t.Fatalf("catalog missing unsupported fixture: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plugins/icon?marketplace=fixture-market&plugin=document-tools", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/svg+xml" || !bytes.Contains(rr.Body.Bytes(), []byte("<svg")) {
		t.Fatalf("icon: %d %s %s", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/plugins/install",
		bytes.NewBufferString(`{"name":"document-tools","marketplace":"fixture-market"}`)))
	if rr.Code != http.StatusCreated || !bytes.Contains(rr.Body.Bytes(), []byte(`"restartRequired":true`)) {
		t.Fatalf("install: %d %s", rr.Code, rr.Body.String())
	}
	installed := filepath.Join(home, "plugins", "document-tools")
	if _, err := os.Stat(filepath.Join(installed, "plugin.toml")); err != nil {
		t.Fatalf("synthesized manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installed, "SKILL.md")); err != nil {
		t.Fatalf("installed skill: %v", err)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/plugins/remove",
		bytes.NewBufferString(`{"id":"document-tools"}`)))
	if rr.Code != http.StatusOK || !bytes.Contains(rr.Body.Bytes(), []byte(`"recoverableAt"`)) {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatalf("installed plugin survived remove: %v", err)
	}
	trash, err := filepath.Glob(filepath.Join(home, "trash", "plugins", "document-tools-*"))
	if err != nil || len(trash) != 1 {
		t.Fatalf("recoverable plugin copy = %v err=%v", trash, err)
	}
}

func TestPluginMarketplaceRejectsUnsafeAndUnsupportedInstalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	writeMarketplaceFixture(t, home)
	s, _ := testServer(t)
	h := s.handler()

	for _, body := range []string{
		`{"name":"../escape","marketplace":"fixture-market"}`,
		`{"name":"escape-tool","marketplace":"fixture-market"}`,
		`{"name":"external-tool","marketplace":"fixture-market"}`,
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/plugins/install", bytes.NewBufferString(body)))
		if rr.Code < 400 || rr.Code >= 500 {
			t.Fatalf("unsafe install %s = %d: %s", body, rr.Code, rr.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, "plugins", "escape")); !os.IsNotExist(err) {
		t.Fatalf("unsafe plugin path created: %v", err)
	}
}

func TestPluginMarketplaceMutationMethodsAreGuarded(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _ := testServer(t)
	h := s.handler()
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/plugins/catalog"},
		{http.MethodGet, "/api/plugins/catalog/refresh"},
		{http.MethodGet, "/api/plugins/install"},
		{http.MethodPost, "/api/plugins/remove"},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rr.Code)
		}
	}
}
