package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func writePluginValidationFixture(t *testing.T, home, declaration string) string {
	t.Helper()
	root := filepath.Join(home, "plugins", "validation-fixture")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "manifest_version = 1\nname = \"validation-fixture\"\nversion = \"0.1.0\"\ndescription = \"Has a description\"\n" + declaration
	if err := os.WriteFile(filepath.Join(root, "plugin.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: fixture\ndescription: fixture\n---\nRead only.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func getPluginValidationViews(t *testing.T, s *Server) []pluginView {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/plugins", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("plugins: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Plugins []pluginView `json:"plugins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Plugins
}

func TestPluginValidationAPIRejectsUnsupportedDeclarations(t *testing.T) {
	for _, declaration := range []string{
		"schedules = []\n",
		"themes = []\n",
		"[[hooks]]\nevents = [\"SessionStart\"]\ncommand = \"not-executed\"\n",
		"",
		"skills = [\"missing.md\"]\n",
	} {
		t.Run(declaration, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			writePluginValidationFixture(t, home, declaration)
			s, _ := testServer(t)
			views := getPluginValidationViews(t, s)
			if len(views) != 1 {
				t.Fatalf("views = %+v", views)
			}
			p := views[0]
			if p.Error == "" || p.Loaded || p.RestartRequired || len(p.Capabilities) != 0 {
				t.Fatalf("invalid plugin must show its error, not advertise capabilities/restart: %+v", p)
			}
		})
	}
}

func TestPluginValidationAPIReportsValidAndStaleLoadedState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	root := writePluginValidationFixture(t, home, "skills = [\"SKILL.md\"]\n")
	s, _ := testServer(t)
	views := getPluginValidationViews(t, s)
	if len(views) != 1 || views[0].Error != "" || views[0].Loaded || !views[0].RestartRequired {
		t.Fatalf("valid pending plugin = %+v", views)
	}
	reg, errs := rtpkg.LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	defer reg.Close()
	s.plugins = reg
	views = getPluginValidationViews(t, s)
	if len(views) != 1 || !views[0].Loaded || views[0].Error != "" {
		t.Fatalf("valid loaded plugin = %+v", views)
	}
	file := filepath.Join(root, "plugin.toml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "Has a description", "Changed description", 1)
	if err := os.WriteFile(file, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	views = getPluginValidationViews(t, s)
	if len(views) != 1 || views[0].Loaded || !views[0].RestartRequired || views[0].Error != "" {
		t.Fatalf("changed manifest incorrectly matches the loaded runtime: %+v", views)
	}
	if err := os.WriteFile(file, append(data, []byte("themes = []\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	views = getPluginValidationViews(t, s)
	if len(views) != 1 || views[0].Loaded || views[0].Error == "" {
		t.Fatalf("stale loaded state hid invalid manifest: %+v", views)
	}
}

func TestPluginValidationUIShowsErrorsAlongsideDescriptions(t *testing.T) {
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/chat.js", nil))
	script := rr.Body.String()
	if !strings.Contains(script, `class="plugin-validation-error"`) || strings.Contains(script, "plugins may add tools, skills, hooks") {
		t.Fatal("plugin UI hides validation errors or advertises unimplemented Hooks")
	}
}

func TestPluginValidationInstallAPIExplainsInvalidCapabilities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	writeMarketplaceFixture(t, home)
	path := filepath.Join(home, "plugins", "marketplaces", "fixture-market", "plugins", "docs", "plugin.toml")
	if err := os.WriteFile(path, []byte("manifest_version = 1\nname = \"document-tools\"\nversion = \"0.1.0\"\nschedules = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/plugins/install", bytes.NewBufferString(`{"name":"document-tools","marketplace":"fixture-market"}`)))
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "schedules") {
		t.Fatalf("invalid install should explain the unsupported declaration: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, "plugins", "document-tools")); !os.IsNotExist(err) {
		t.Fatalf("invalid plugin was installed: %v", err)
	}
}

func TestPluginValidationCardDOMBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", pluginValidationCardHarness)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plugin card behavior: %v\n%s", err, output)
	}
}

const pluginValidationCardHarness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const start = source.indexOf('function paintPlugins() {');
const end = source.indexOf('\nfunction switchPluginView(', start);
assert(start >= 0 && end > start);
const list = { innerHTML: '', setAttribute() {} }, count = { textContent: '' };
const escape = value => String(value).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const c = {document: {getElementById: id => id === 'pluginList' ? list : count},
  escHtml:escape,escAttr:escape,escOnclick:escape,uiText:en=>en,pluginsCache:[]};
vm.createContext(c);vm.runInContext(source.slice(start,end),c);
c.pluginsCache = [{id:'invalid',name:'Fixture',description:'Useful description',loaded:true,
  error:'unsupported <img onerror=bad()>',capabilities:[]}];
c.paintPlugins();
assert.match(list.innerHTML,/Useful description/);
assert.match(list.innerHTML,/plugin-validation-error/);
assert.match(list.innerHTML,/unsupported &lt;img/);
assert.doesNotMatch(list.innerHTML,/<img|>Loaded<|Restart required/);
assert.match(list.innerHTML,/>Invalid</);
c.pluginsCache = [{id:'valid',name:'Valid',loaded:false,capabilities:['Skills']}];
c.paintPlugins();assert.match(list.innerHTML,/>Restart required</);assert.doesNotMatch(list.innerHTML,/role="alert"/);
c.pluginsCache[0].loaded=true;c.paintPlugins();assert.match(list.innerHTML,/>Loaded</);
assert.equal(count.textContent,'1');
`
