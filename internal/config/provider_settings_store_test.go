package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/auth"
)

func providerSettingsTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Chdir(t.TempDir())
	return home
}
func settingsSnapshotForTest(t *testing.T) ProviderSettingsSnapshot {
	t.Helper()
	s, e := ReadProviderSettings()
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func settingsProviderForTest() ProviderSettingsProvider {
	return ProviderSettingsProvider{ID: "fixture", DisplayName: "Fixture gateway", CatalogProvider: "openai", Transport: "openai_chat", BaseURL: "https://example.invalid/v1", Model: "fixture-a", Models: []ProviderModelSettings{{ID: "fixture-a", Name: "First", ContextWindow: 1000000, MaxOutputTokens: 128000, OutputBudget: 1024}, {ID: "fixture-b", Name: "Second"}}}
}
func saveSettingsForTest(t *testing.T, p ProviderSettingsProvider, key string) ProviderSettingsSnapshot {
	t.Helper()
	s := settingsSnapshotForTest(t)
	next, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p, APIKey: key})
	if e != nil {
		t.Fatal(e)
	}
	return next
}
func findSettingsProvider(t *testing.T, s ProviderSettingsSnapshot, id string) ProviderSettingsProvider {
	t.Helper()
	for _, p := range s.Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("provider %s missing", id)
	return ProviderSettingsProvider{}
}

func TestProviderSettingsCustomCRUDPreservesConfigAndDefault(t *testing.T) {
	home := providerSettingsTestHome(t)
	original := "# preserve comment\n[provider]\ndefault = \"openai-codex\" # chosen default\n\n[future]\nflag = \"unknown\" # keep exact\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	p := settingsProviderForTest()
	s := saveSettingsForTest(t, p, "fixture-private-key")
	if s.DefaultProvider != "openai-codex" {
		t.Fatalf("editing changed default: %+v", s)
	}
	read := settingsSnapshotForTest(t)
	if read.Revision != s.Revision {
		t.Fatal("returned revision does not represent committed credential state")
	}
	got := findSettingsProvider(t, read, p.ID)
	if got.Kind != "custom" || got.CredentialSource != "managed" || !got.HasCredential || !reflect.DeepEqual(got.Models, p.Models) {
		t.Fatalf("saved settings differ: %+v", got)
	}
	raw, e := os.ReadFile(filepath.Join(home, "config.toml"))
	if e != nil {
		t.Fatal(e)
	}
	for _, literal := range []string{"# preserve comment", `default = "openai-codex" # chosen default`, `flag = "unknown" # keep exact`} {
		if !bytes.Contains(raw, []byte(literal)) {
			t.Fatalf("lost user TOML %s", literal)
		}
	}
	encoded, _ := json.Marshal(read)
	sidecar, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
	for _, data := range [][]byte{encoded, raw, sidecar} {
		if bytes.Contains(data, []byte("fixture-private-key")) {
			t.Fatal("API key escaped its private credential store")
		}
	}
	selected, e := SaveProviderSettings(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: read.Revision, ID: p.ID, Model: "fixture-b"})
	if e != nil {
		t.Fatal(e)
	}
	if selected.DefaultProvider != p.ID || selected.DefaultModel != "fixture-b" {
		t.Fatalf("explicit default selection failed: %+v", selected)
	}
	if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: selected.Revision, ID: p.ID}); !errors.Is(e, ErrProviderSettingsReadOnly) {
		t.Fatalf("deleted current default: %v", e)
	}
	openAI := findSettingsProvider(t, selected, "openai")
	selected, e = SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: selected.Revision, Provider: &openAI})
	if e != nil {
		t.Fatal(e)
	}
	selected, e = SaveProviderSettings(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: selected.Revision, ID: "openai"})
	if e != nil {
		t.Fatal(e)
	}
	deleted, e := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: selected.Revision, ID: p.ID})
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range deleted.Providers {
		if entry.ID == p.ID {
			t.Fatal("provider remains after deletion")
		}
	}
	if key, err := auth.Get(p.ID); err != nil || key != "" {
		t.Fatal("deleted provider retained managed API key")
	}
}

func TestProviderSettingsBuiltinsAndCodexBoundaries(t *testing.T) {
	providerSettingsTestHome(t)
	s := settingsSnapshotForTest(t)
	for _, id := range []string{"anthropic", "openai", "gemini"} {
		p := findSettingsProvider(t, s, id)
		p.DisplayName = "Renamed " + id
		p.Models = append(p.Models, ProviderModelSettings{ID: "new-model"})
		p.Model = "new-model"
		var e error
		s, e = SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p, APIKey: "fixture-" + id})
		if e != nil {
			t.Fatalf("%s: %v", id, e)
		}
		p = findSettingsProvider(t, s, id)
		if !p.HasCredential || p.Model != "new-model" || p.DisplayName != "Renamed "+id {
			t.Fatalf("builtin save: %+v", p)
		}
	}
	codex := findSettingsProvider(t, s, "openai-codex")
	codex.Models = append(codex.Models, ProviderModelSettings{ID: "another-codex"})
	codex.Model = "another-codex"
	next, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &codex})
	if e != nil {
		t.Fatal(e)
	}
	if next.DefaultProvider != s.DefaultProvider {
		t.Fatal("Codex editing changed global default")
	}
	for _, mut := range []ProviderSettingsMutation{{Action: "upsert", ExpectedRevision: next.Revision, Provider: &codex, APIKey: "not-oauth"}, {Action: "upsert", ExpectedRevision: next.Revision, Provider: &codex, ClearCredential: true}} {
		if _, e := SaveProviderSettings(mut); !errors.Is(e, ErrProviderSettingsReadOnly) {
			t.Fatalf("Codex OAuth key mutation accepted: %v", e)
		}
	}
	codex.Models[0].OutputBudget = 100
	if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: next.Revision, Provider: &codex}); e == nil {
		t.Fatal("Codex request output budget accepted")
	}
}

func TestProviderSettingsCASIncludesCredentialAndSidecarChanges(t *testing.T) {
	home := providerSettingsTestHome(t)
	p := settingsProviderForTest()
	s := saveSettingsForTest(t, p, "first-key")
	before, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if e := auth.ActivateAPIKeyBound(p.ID, "rotated-key", p.Transport, p.BaseURL); e != nil {
		t.Fatal(e)
	}
	p.DisplayName = "stale change"
	if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p}); !errors.Is(e, ErrProviderSettingsConflict) {
		t.Fatalf("stale credential revision accepted: %v", e)
	}
	after, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if !bytes.Equal(before, after) {
		t.Fatal("CAS conflict changed config")
	}
	fresh := settingsSnapshotForTest(t)
	raw, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
	raw = append(raw, '\n')
	if e := os.WriteFile(filepath.Join(home, providerSettingsFilename), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: fresh.Revision, Provider: &p}); !errors.Is(e, ErrProviderSettingsConflict) {
		t.Fatalf("stale sidecar revision accepted: %v", e)
	}
}

func TestProviderSettingsKeepClearAndEndpointTrust(t *testing.T) {
	providerSettingsTestHome(t)
	p := settingsProviderForTest()
	s := saveSettingsForTest(t, p, "private-key")
	p.DisplayName = "edited"
	next, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p})
	if e != nil {
		t.Fatal(e)
	}
	if key, err := auth.Get(p.ID); err != nil || key != "private-key" {
		t.Fatal("blank key erased credential")
	}
	p.BaseURL = "https://other.invalid/v1"
	if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: next.Revision, Provider: &p}); !errors.Is(e, ErrProviderSettingsReadOnly) {
		t.Fatalf("endpoint changed with old key: %v", e)
	}
	cleared, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: next.Revision, Provider: &p, ClearCredential: true})
	if e != nil {
		t.Fatal(e)
	}
	if key, err := auth.Get(p.ID); err != nil || findSettingsProvider(t, cleared, p.ID).HasCredential || key != "" {
		t.Fatal("explicit clear failed")
	}
}

func TestProviderSettingsRejectsFalseEnvironmentOrInlineClear(t *testing.T) {
	for _, source := range []string{"env", "inline"} {
		t.Run(source, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			extra := "api_key_env = \"FIXTURE_SETTINGS_KEY\"\n"
			if source == "env" {
				t.Setenv("FIXTURE_SETTINGS_KEY", "environment-private")
			} else {
				extra = "api_key = \"inline-private\"\n"
			}
			original := "[provider.openai]\n" + extra
			if e := os.WriteFile(filepath.Join(home, "config.toml"), []byte(original), 0600); e != nil {
				t.Fatal(e)
			}
			s := settingsSnapshotForTest(t)
			p := findSettingsProvider(t, s, "openai")
			if p.CredentialSource != source {
				t.Fatalf("wrong source: %+v", p)
			}
			if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p, ClearCredential: true}); !errors.Is(e, ErrProviderSettingsCredentialSource) {
				t.Fatalf("false clear accepted: %v", e)
			}
			got, _ := os.ReadFile(filepath.Join(home, "config.toml"))
			if string(got) != original {
				t.Fatal("rejected clear changed config")
			}
		})
	}
}

func TestProviderSettingsRollbackEveryPublicationFailure(t *testing.T) {
	for _, failure := range []string{"config-before", "config-after", "sidecar-before", "sidecar-after", "credential-stage"} {
		t.Run(failure, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			p := settingsProviderForTest()
			s := saveSettingsForTest(t, p, "original-key")
			configBefore, _ := os.ReadFile(filepath.Join(home, "config.toml"))
			sidecarBefore, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
			previousWriter := providerSettingsWriteFile
			previousCredentials := providerSettingsCredentialTransaction
			t.Cleanup(func() {
				providerSettingsWriteFile = previousWriter
				providerSettingsCredentialTransaction = previousCredentials
			})
			injected := errors.New("injected publication failure")
			providerSettingsWriteFile = func(path string, data []byte) error {
				kind := "config"
				if filepath.Base(path) == providerSettingsFilename {
					kind = "sidecar"
				}
				if failure == kind+"-before" {
					return injected
				}
				if e := atomicWritePrivateFile(path, data); e != nil {
					return e
				}
				if failure == kind+"-after" {
					return injected
				}
				return nil
			}
			if failure == "credential-stage" {
				providerSettingsCredentialTransaction = func(fn func(*auth.ProviderSettingsCredentials) error) error {
					return auth.WithProviderSettingsCredentials(func(c *auth.ProviderSettingsCredentials) error {
						if e := fn(c); e != nil {
							return e
						}
						return injected
					})
				}
			}
			p.DisplayName = "failed edit"
			p.Model = "fixture-b" // This attempt changes config as well as sidecar/auth.
			_, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p, APIKey: "failed-rotation"})
			if !errors.Is(e, injected) {
				t.Fatalf("failure not returned: %v", e)
			}
			configAfter, _ := os.ReadFile(filepath.Join(home, "config.toml"))
			sidecarAfter, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
			if key, err := auth.Get(p.ID); err != nil || !bytes.Equal(configBefore, configAfter) || !bytes.Equal(sidecarBefore, sidecarAfter) || key != "original-key" {
				t.Fatal("failed save left partially changed provider/config/credential state")
			}
		})
	}
}

func TestProviderSettingsModelRouteAliasesAndReasoningRoundtrip(t *testing.T) {
	providerSettingsTestHome(t)
	p := settingsProviderForTest()
	yes := true
	p.Models[0].SupportsVision = &yes
	p.Models[0].SupportsReasoning = &yes
	p.Models[0].ReasoningEfforts = map[string]string{"low": "low", "medium": "high", "high": "xhigh"}
	saveSettingsForTest(t, p, "")
	got, ok, e := LookupProviderModelSettings(p.ID, "openai", "https://EXAMPLE.INVALID:443/v1/", p.Model)
	if e != nil || !ok || !reflect.DeepEqual(got, p.Models[0]) {
		t.Fatalf("saved exact-route metadata=%+v %t %v", got, ok, e)
	}
	got.ReasoningEfforts["high"] = "mutated"
	again, _, _ := LookupProviderModelSettings(p.ID, p.Transport, p.BaseURL, p.Model)
	if again.ReasoningEfforts["high"] != "xhigh" {
		t.Fatal("lookup returned mutable shared state")
	}
	for _, route := range [][3]string{{"other", p.Transport, p.BaseURL}, {p.ID, "openai_responses", p.BaseURL}, {p.ID, p.Transport, "https://other.invalid/v1"}} {
		if _, found, e := LookupProviderModelSettings(route[0], route[1], route[2], p.Model); e != nil || found {
			t.Fatalf("model settings crossed exact route: %v %v", found, e)
		}
	}
}

func TestProviderSettingsRejectsInvalidModelCapabilities(t *testing.T) {
	providerSettingsTestHome(t)
	s := settingsSnapshotForTest(t)
	for _, tc := range []struct {
		name   string
		mutate func(*ProviderSettingsProvider)
	}{
		{"saved default missing", func(p *ProviderSettingsProvider) { p.Models = p.Models[1:] }},
		{"duplicate id", func(p *ProviderSettingsProvider) { p.Models = append(p.Models, p.Models[0]) }},
		{"negative capability", func(p *ProviderSettingsProvider) { p.Models[0].ContextWindow = -1 }},
		{"output cap beyond full context", func(p *ProviderSettingsProvider) { p.Models[0].MaxOutputTokens = 2000000 }},
		{"request budget beyond output cap", func(p *ProviderSettingsProvider) { p.Models[0].OutputBudget = 200000 }},
		{"reasoning map without true", func(p *ProviderSettingsProvider) { p.Models[0].ReasoningEfforts = map[string]string{"high": "xhigh"} }},
		{"unknown canonical effort", func(p *ProviderSettingsProvider) {
			v := true
			p.Models[0].SupportsReasoning = &v
			p.Models[0].ReasoningEfforts = map[string]string{"max": "xhigh"}
		}},
		{"invalid wire token", func(p *ProviderSettingsProvider) {
			v := true
			p.Models[0].SupportsReasoning = &v
			p.Models[0].ReasoningEfforts = map[string]string{"high": "not a token"}
		}},
		{"Gemini unsupported true", func(p *ProviderSettingsProvider) {
			p.Transport = "gemini_native"
			v := true
			p.Models[0].SupportsReasoning = &v
		}},
		{"Anthropic unsupported map", func(p *ProviderSettingsProvider) {
			p.Transport = "anthropic_messages"
			v := true
			p.Models[0].SupportsReasoning = &v
			p.Models[0].ReasoningEfforts = map[string]string{"high": "high"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := settingsProviderForTest()
			tc.mutate(&p)
			if _, e := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p}); e == nil {
				t.Fatal("invalid model setting accepted")
			}
			if current := settingsSnapshotForTest(t); current.Revision != s.Revision {
				t.Fatal("validation failure changed stored state")
			}
		})
	}
}

func TestProviderSettingsGeminiGoogleEnvironmentFallback(t *testing.T) {
	providerSettingsTestHome(t)
	t.Setenv("GOOGLE_API_KEY", "fixture-google-private")
	before := settingsSnapshotForTest(t)
	p := findSettingsProvider(t, before, "gemini")
	if p.CredentialSource != "env" || !p.HasCredential {
		t.Fatalf("Google fallback not reflected: %+v", p)
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: before.Revision, Provider: &p, ClearCredential: true}); !errors.Is(err, ErrProviderSettingsCredentialSource) {
		t.Fatalf("Google environment falsely cleared: %v", err)
	}
	t.Setenv("GOOGLE_API_KEY", "fixture-google-rotated")
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: before.Revision, Provider: &p}); !errors.Is(err, ErrProviderSettingsConflict) {
		t.Fatalf("environment rotation not included in CAS: %v", err)
	}
}

func TestProviderSettingsProjectOverrideCannotBeSilentlyShadowed(t *testing.T) {
	home := providerSettingsTestHome(t)
	project := filepath.Join(".metis", "config.toml")
	if err := os.MkdirAll(filepath.Dir(project), 0700); err != nil {
		t.Fatal(err)
	}
	content := "[provider.openai]\nmodel = \"project-only\"\n\n[provider.custom.fixture]\ntransport = \"openai_chat\"\nbase_url = \"https://project.invalid/v1\"\nmodel = \"project-model\"\n"
	if err := os.WriteFile(project, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	s := settingsSnapshotForTest(t)
	builtin := findSettingsProvider(t, s, "openai")
	if !builtin.ReadOnly {
		t.Fatal("project controlled builtin presented as editable")
	}
	for _, p := range []ProviderSettingsProvider{builtin, settingsProviderForTest()} {
		if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p}); !errors.Is(err, ErrProviderSettingsReadOnly) {
			t.Fatalf("project-controlled save accepted: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected project save created user config")
	}
}

func TestProviderSettingsFailureRemovesNewConfigAndSidecar(t *testing.T) {
	home := providerSettingsTestHome(t)
	s := settingsSnapshotForTest(t)
	p := settingsProviderForTest()
	previous := providerSettingsWriteFile
	t.Cleanup(func() { providerSettingsWriteFile = previous })
	injected := errors.New("injected first sidecar failure")
	providerSettingsWriteFile = func(path string, data []byte) error {
		if filepath.Base(path) == providerSettingsFilename {
			return injected
		}
		return atomicWritePrivateFile(path, data)
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: s.Revision, Provider: &p, APIKey: "staged-key"}); !errors.Is(err, injected) {
		t.Fatal("first-save failure not reported")
	}
	for _, filename := range []string{"config.toml", providerSettingsFilename} {
		if _, err := os.Stat(filepath.Join(home, filename)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("first-save failure retained %s", filename)
		}
	}
	if key, err := auth.Get(p.ID); err != nil || key != "" {
		t.Fatal("failed first save retained credential")
	}
}

func TestProviderSettingsRejectsConfigurationGrowingBeyondReadLimit(t *testing.T) {
	home := providerSettingsTestHome(t)
	original := []byte("#" + strings.Repeat("x", providerSettingsMaxBytes-2) + "\n")
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	before := settingsSnapshotForTest(t)
	p := settingsProviderForTest()
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: before.Revision, Provider: &p, APIKey: "must-not-be-saved"}); err == nil {
		t.Fatal("provider configuration was allowed to exceed the subsequent reader limit")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("oversize validation changed original configuration")
	}
	if _, err := os.Stat(filepath.Join(home, providerSettingsFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversize validation wrote provider sidecar")
	}
	if key, err := auth.Get(p.ID); err != nil || key != "" {
		t.Fatal("oversize validation wrote staged credential")
	}
	if current := settingsSnapshotForTest(t); current.Revision != before.Revision {
		t.Fatal("oversize validation changed settings revision")
	}
}

func TestProviderSettingsCredentialStatusUsesRuntimeEndpointNormalization(t *testing.T) {
	providerSettingsTestHome(t)
	p := settingsProviderForTest()
	saveSettingsForTest(t, p, "")
	for _, tc := range []struct {
		name    string
		binding auth.EndpointBinding
		want    bool
	}{
		{"equivalent spellings", auth.EndpointBinding{Provider: p.ID, Transport: "openai", BaseURL: "https://EXAMPLE.INVALID:443/v1/"}, true},
		{"other host", auth.EndpointBinding{Provider: p.ID, Transport: p.Transport, BaseURL: "https://other.invalid/v1"}, false},
		{"other path", auth.EndpointBinding{Provider: p.ID, Transport: p.Transport, BaseURL: "https://example.invalid/v2"}, false},
		{"other protocol", auth.EndpointBinding{Provider: p.ID, Transport: "openai_responses", BaseURL: p.BaseURL}, false},
		{"other provider", auth.EndpointBinding{Provider: "different", Transport: p.Transport, BaseURL: p.BaseURL}, false},
		{"invalid endpoint", auth.EndpointBinding{Provider: p.ID, Transport: p.Transport, BaseURL: "not-a-url"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := auth.Save(auth.File{p.ID: auth.Entry{Type: "api", Key: "normalization-fixture-key", Endpoint: &tc.binding}}); err != nil {
				t.Fatal(err)
			}
			got := findSettingsProvider(t, settingsSnapshotForTest(t), p.ID)
			runtimeKey, runtimeErr := auth.GetAPIKeyForEndpoint(p.ID, p.Transport, p.BaseURL, false)
			if (runtimeErr == nil && runtimeKey != "") != tc.want {
				t.Fatal("runtime fixture credential expectation differs")
			}
			if got.HasCredential != tc.want || got.CredentialSource != "managed" {
				t.Fatalf("credential endpoint truth differs: got credential=%t source=%s want=%t", got.HasCredential, got.CredentialSource, tc.want)
			}
		})
	}
}

func TestProviderSettingsQuotedTablesPreserveDefaultsCommentsAndKeys(t *testing.T) {
	home := providerSettingsTestHome(t)
	original := `# preserved root comment
["provider"] # preserved quoted provider header
"default" = 'origin' # saved selection

["provider"."custom"."origin"] # preserved quoted custom header
"transport" = "openai_chat"
"base_url" = "https://origin.invalid/v1"
"model" = "origin-model"
"api_key" = "origin-inline-fixture"
"future" = "unknown provider field" # retained

[future]
"flag" = "unknown root field" # retained
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("origin", "origin-managed-fixture", "openai_chat", "https://origin.invalid/v1"); err != nil {
		t.Fatal(err)
	}
	initial := settingsSnapshotForTest(t)
	p := settingsProviderForTest()
	p.ID = "alias"
	saved, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: initial.Revision, Provider: &p, APIKey: "alias-managed-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.DefaultProvider != "origin" {
		t.Fatal("quoted-table upsert silently changed default")
	}
	selected, err := SaveProviderSettings(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: saved.Revision, ID: p.ID, Model: "fixture-b"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.DefaultProvider != "alias" || selected.DefaultModel != "fixture-b" {
		t.Fatalf("quoted default selection failed: provider=%s model=%s", selected.DefaultProvider, selected.DefaultModel)
	}
	origin := findSettingsProvider(t, selected, "origin")
	origin.DisplayName = "Origin renamed"
	origin.Models = append(origin.Models, ProviderModelSettings{ID: "origin-second"})
	updated, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: selected.Revision, Provider: &origin})
	if err != nil {
		t.Fatal(err)
	}
	reread := settingsSnapshotForTest(t)
	if reread.Revision != updated.Revision {
		t.Fatal("quoted-table revision did not describe committed state")
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{`# preserved root comment`, `["provider"] # preserved quoted provider header`, `["provider"."custom"."origin"] # preserved quoted custom header`, `# saved selection`, `"api_key" = "origin-inline-fixture"`, `"future" = "unknown provider field" # retained`, `"flag" = "unknown root field" # retained`} {
		if !bytes.Contains(raw, []byte(literal)) {
			t.Fatalf("lost quoted-table surrounding content %s", literal)
		}
	}
	for _, id := range []string{"origin", "alias"} {
		key, e := auth.Get(id)
		if e != nil || key == "" {
			t.Fatal("quoted-table edit lost a managed credential")
		}
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: initial.Revision, ID: "origin"}); !errors.Is(err, ErrProviderSettingsConflict) {
		t.Fatalf("quoted tables bypassed CAS: %v", err)
	}
}

func TestProviderSettingsSimpleQuotedSyntaxUsesSemanticKeys(t *testing.T) {
	providerSettingsTestHome(t)
	for _, tc := range []struct{ name, root, child, defaultKey, modelKey string }{
		{"double quoted", `["provider"]`, `["provider"."custom"."origin"]`, `"default"`, `"model"`},
		{"literal quoted", `['provider']`, `['provider'.'custom'.'origin']`, `'default'`, `'model'`},
		{"TOML escapes and whitespace", `[ "pro\u0076ider" ]`, `[ "provider" . 'custom' . "ori\u0067in" ]`, `"def\u0061ult"`, `"mo\u0064el"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.root + " # root header\r\n" + tc.defaultKey + " = 'origin' # default comment\r\n\r\n" + tc.child + " # child header\r\n\"transport\" = \"openai_chat\"\r\n\"base_url\" = \"https://origin.invalid/v1\"\r\n" + tc.modelKey + " = 'origin-model' # model comment\r\n")
			updated, err := renderSettingsDefault(original, "openai")
			if err != nil {
				t.Fatal(err)
			}
			var cfg Config
			if _, err := toml.Decode(string(updated), &cfg); err != nil || cfg.Provider.Default != "openai" {
				t.Fatal("quoted default update failed semantic verification")
			}
			p, _, _, ok := providerSettingsRaw(&cfg, "origin")
			if !ok {
				t.Fatal("quoted custom profile missing")
			}
			p.Model = "new-model"
			updated, err = renderSettingsProvider(updated, p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := toml.Decode(string(updated), &cfg); err != nil || cfg.Provider.Custom["origin"].Model != "new-model" || cfg.Provider.Default != "openai" {
				t.Fatal("quoted model update changed the wrong semantic field")
			}
			for _, literal := range []string{tc.root + " # root header", tc.child + " # child header", tc.defaultKey + " = \"openai\" # default comment", tc.modelKey + " = \"new-model\" # model comment"} {
				if !bytes.Contains(updated, []byte(literal)) {
					t.Fatalf("quoted key/header/comment not preserved: %s", literal)
				}
			}
			if bytes.Contains(bytes.ReplaceAll(updated, []byte("\r\n"), nil), []byte("\n")) {
				t.Fatal("quoted-table update changed CRLF style")
			}
			deleted, err := renderSettingsDelete(updated, "origin")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := toml.Decode(string(deleted), &cfg); err != nil {
				t.Fatal(err)
			}
			var verify Config
			if _, err := toml.Decode(string(deleted), &verify); err != nil {
				t.Fatal(err)
			}
			if _, exists := verify.Provider.Custom["origin"]; exists {
				t.Fatal("quoted target table not deleted")
			}
		})
	}
	for _, header := range []string{`["provider.custom.origin"]`, `["provider"."custom.origin"]`, `[["provider"."custom"."origin"]]`} {
		if _, _, found := findSettingsTable(splitLines(header+"\n"), "provider.custom.origin"); found {
			t.Fatalf("literal dotted key or array table mistaken for target: %s", header)
		}
	}
}

func TestProviderSettingsQuotedTablesIgnoreMultilineExamples(t *testing.T) {
	providerSettingsTestHome(t)
	documentation := `notes = """
["provider"]
"default" = "example-only"
["provider"."custom"."origin"]
"model" = "example-only"
"""

`
	original := []byte(documentation + `["provider"]
"default" = "origin"

["provider"."custom"."origin"]
"transport" = "openai_chat"
"base_url" = "https://origin.invalid/v1"
"model" = "origin-model"
`)
	updated, err := renderSettingsDefault(original, "openai")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if _, err := toml.Decode(string(updated), &cfg); err != nil {
		t.Fatal(err)
	}
	p, _, _, _ := providerSettingsRaw(&cfg, "origin")
	p.Model = "new-model"
	updated, err = renderSettingsProvider(updated, p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(updated, []byte(documentation)) {
		t.Fatal("settings writer changed multiline documentation examples")
	}
	var verify Config
	if _, err := toml.Decode(string(updated), &verify); err != nil || verify.Provider.Default != "openai" || verify.Provider.Custom["origin"].Model != "new-model" {
		t.Fatal("settings writer returned success without updating actual fields")
	}
}

func TestProviderSettingsRejectsUnsafeInlineAndDottedAssignments(t *testing.T) {
	providerSettingsTestHome(t)
	for _, original := range []string{
		`provider = { default = "origin" }` + "\n",
		`provider.default = "origin"` + "\n",
		`"provider".default = "origin"` + "\n",
	} {
		if _, err := renderSettingsDefault([]byte(original), "openai"); err == nil {
			t.Fatal("unsafe inline or dotted assignment was accepted")
		}
	}
	// A default defined as a multiline string remains readable, but preserving
	// its arbitrary span safely is outside this text writer's supported subset.
	multiline := []byte("[\"provider\"]\n\"default\" = \"\"\"origin\"\"\"\n")
	if _, err := renderSettingsDefault(multiline, "openai"); err == nil {
		t.Fatal("multiline managed assignment accepted")
	}
	child := []byte("[\"provider\".\"custom\".\"origin\"]\nmodel=\"origin\"\n[\"provider\".\"custom\".\"origin\".\"future\"]\nflag=true\n")
	if _, err := renderSettingsDelete(child, "origin"); err == nil {
		t.Fatal("delete abandoned a provider child table")
	}
}

func TestProviderSettingsMemoryDefaultsAndStarterAreNotRegistrations(t *testing.T) {
	home := providerSettingsTestHome(t)
	assertEmpty := func() {
		t.Helper()
		snapshot, err := ReadProviderSettingsForWorkspace(false)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Providers) != 4 {
			t.Fatal("internal compatibility presets changed")
		}
		for _, p := range snapshot.Providers {
			if p.Registered || p.Removable {
				t.Fatalf("implicit/starter provider advertised as added: %s", p.ID)
			}
		}
	}
	assertEmpty()
	starter := "[provider]\ndefault = \"anthropic\"\n\n[provider.anthropic]\napi_key_env = \"ANTHROPIC_API_KEY\"\nmodel = \"claude-opus-4-7\"\nmax_tokens = 64000\ntimeout_seconds = 120\ntemperature = 1.0\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(starter), 0600); err != nil {
		t.Fatal(err)
	}
	assertEmpty()
	numeric := starter + "\n[provider.openai]\nmax_tokens = 123\n[provider.gemini]\ntimeout_seconds = 12\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(numeric), 0600); err != nil {
		t.Fatal(err)
	}
	assertEmpty()
	before := settingsSnapshotForTest(t)
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: before.Revision, ID: "openai"}); err == nil {
		t.Fatal("unregistered compatibility preset could be activated as an added provider")
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: before.Revision, ID: "openai"}); err == nil {
		t.Fatal("unregistered compatibility preset accepted deletion")
	}
}

func TestProviderSettingsMeaningfulUserConnectionsRegisterBuiltins(t *testing.T) {
	for _, tc := range []struct{ name, table, assignment string }{
		{"model", "openai", `model = "user-model"`},
		{"endpoint", "openai", `base_url = "https://user.invalid/v1"`},
		{"protocol", "openai", `wire_protocol = "responses"`},
		{"catalog", "gemini", `catalog_provider = "user-catalog"`},
		{"credential variable", "anthropic", `api_key_env = "UNSET_PROVIDER_MEMBERSHIP_FIXTURE"`},
		{"inline credential", "openai", `api_key = "private-inline-membership-fixture"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			t.Setenv("UNSET_PROVIDER_MEMBERSHIP_FIXTURE", "")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[\"provider\".\""+tc.table+"\"]\n"+tc.assignment+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p := findSettingsProvider(t, settingsSnapshotForTest(t), tc.table)
			if !p.Registered || !p.Removable {
				t.Fatalf("user connection was not registered/removable: %s reason=%s", p.ID, p.RemoveReason)
			}
		})
	}
}

func TestProviderSettingsExplicitBuiltinRegistrationAndRemovalSurvivesReload(t *testing.T) {
	home := providerSettingsTestHome(t)
	initial := settingsSnapshotForTest(t)
	p := findSettingsProvider(t, initial, "openai")
	saved, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: initial.Revision, Provider: &p})
	if err != nil {
		t.Fatal(err)
	}
	p = findSettingsProvider(t, saved, "openai")
	if !p.Registered || !p.Removable || p.HasCredential {
		t.Fatalf("explicit no-key add lost membership: added=%t removable=%t credential=%t", p.Registered, p.Removable, p.HasCredential)
	}
	deleted, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: saved.Revision, ID: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if p := findSettingsProvider(t, deleted, "openai"); p.Registered || p.Removable {
		t.Fatal("deleted builtin remained added")
	}
	reloaded := settingsSnapshotForTest(t)
	if reloaded.Revision != deleted.Revision {
		t.Fatal("post-delete revision does not describe persisted state")
	}
	for _, p := range reloaded.Providers {
		if p.Registered {
			t.Fatalf("provider resurrected from in-memory defaults: %s", p.ID)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	md, err := toml.Decode(string(raw), &cfg)
	if err != nil || md.IsDefined("provider", "openai") {
		t.Fatal("builtin user table remains after delete")
	}
	if _, ok, err := LookupProviderModelSettings("openai", p.Transport, p.BaseURL, p.Model); err != nil || ok {
		t.Fatal("deleted builtin retained saved model metadata")
	}
}

func TestProviderSettingsStoredCredentialsRegisterWithoutExplicitConfig(t *testing.T) {
	for _, kind := range []string{"managed", "invalid API entry", "expired OAuth"} {
		t.Run(kind, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			id := "openai"
			switch kind {
			case "managed":
				if err := auth.ActivateAPIKeyBound(id, "stored-membership-fixture", "openai_chat", "https://api.openai.com/v1"); err != nil {
					t.Fatal(err)
				}
			case "invalid API entry":
				if err := auth.Save(auth.File{id: {Type: "unusable", Key: ""}}); err != nil {
					t.Fatal(err)
				}
			case "expired OAuth":
				id = "openai-codex"
				if err := auth.PutOAuth(id, auth.OAuthCredential{AccessToken: "expired-membership-fixture", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := settingsSnapshotForTest(t)
			p := findSettingsProvider(t, snapshot, id)
			if !p.Registered || !p.Removable {
				t.Fatalf("stored credential membership lost: %s", id)
			}
			deleted, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: snapshot.Revision, ID: id})
			if err != nil {
				t.Fatal(err)
			}
			if findSettingsProvider(t, deleted, id).Registered {
				t.Fatal("credentials-only provider remained registered")
			}
			// No explicit user table existed; deletion must not manufacture config.
			if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("credentials-only delete created a user config")
			}
			if key, err := auth.Get(id); err != nil || key != "" {
				t.Fatal("stored API key survived provider deletion")
			}
			if exists, err := auth.HasOAuth(id); err != nil || exists {
				t.Fatal("stored OAuth survived provider deletion")
			}
			if findSettingsProvider(t, settingsSnapshotForTest(t), id).Registered {
				t.Fatal("public credential reload resurrected provider")
			}
		})
	}
}

func TestProviderSettingsSidecarOnlyRegistrationIsRouteBoundAndRemovable(t *testing.T) {
	home := providerSettingsTestHome(t)
	initial := settingsSnapshotForTest(t)
	p := findSettingsProvider(t, initial, "openai")
	saved, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: initial.Revision, Provider: &p})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "config.toml")); err != nil {
		t.Fatal(err)
	}
	sidecarOnly := settingsSnapshotForTest(t)
	if !findSettingsProvider(t, sidecarOnly, "openai").Registered {
		t.Fatal("matching explicit sidecar registration disappeared")
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: sidecarOnly.Revision, ID: "openai"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sidecar-only delete created user config")
	}
	// A stale record for another route must not become a fallback registration.
	p.Transport = "openai_responses"
	saved, err = SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: settingsSnapshotForTest(t).Revision, Provider: &p})
	if err != nil {
		t.Fatal(err)
	}
	if !findSettingsProvider(t, saved, "openai").Registered {
		t.Fatal("explicit Responses route not added")
	}
	if err := os.Remove(filepath.Join(home, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if findSettingsProvider(t, settingsSnapshotForTest(t), "openai").Registered {
		t.Fatal("stale Responses sidecar borrowed the default Chat route")
	}
}

func TestProviderSettingsEnvironmentAndWorkspaceRemovalTruth(t *testing.T) {
	home := providerSettingsTestHome(t)
	t.Setenv("OPENAI_API_KEY", "active-environment-membership")
	snapshot := settingsSnapshotForTest(t)
	p := findSettingsProvider(t, snapshot, "openai")
	if !p.Registered || p.Removable || p.RemoveReason == "" {
		t.Fatal("environment provider removal status is misleading")
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: snapshot.Revision, ID: "openai"}); !errors.Is(err, ErrProviderSettingsReadOnly) {
		t.Fatalf("environment provider falsely deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected environment delete changed config")
	}
	t.Setenv("OPENAI_API_KEY", "")
	if err := os.MkdirAll(".metis", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".metis", "config.toml"), []byte("[provider.openai]\nmodel=\"workspace-model\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	trusted, err := ReadProviderSettingsForWorkspace(true)
	if err != nil {
		t.Fatal(err)
	}
	untrusted, err := ReadProviderSettingsForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	p = findSettingsProvider(t, trusted, "openai")
	if !p.Registered || !p.ReadOnly || p.Removable {
		t.Fatal("workspace provider not shown as read-only registration")
	}
	if findSettingsProvider(t, untrusted, "openai").Registered {
		t.Fatal("untrusted workspace created a provider registration")
	}
	if trusted.Revision != untrusted.Revision {
		t.Fatal("trust projection changed file CAS revision")
	}
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: trusted.Revision, ID: "openai"}); !errors.Is(err, ErrProviderSettingsReadOnly) {
		t.Fatalf("workspace-controlled provider deleted: %v", err)
	}
}

func TestProviderSettingsBuiltinRemovalPreservesUnrelatedConfigAndClearsBothLoginMethods(t *testing.T) {
	home := providerSettingsTestHome(t)
	prefix := "# keep first\n[provider]\ndefault = \"anthropic\" # keep default\n\n"
	target := "[\"provider\".\"openai\"] # owned table\n\"model\" = \"owned-model\"\n\"api_key\" = \"owned-inline-key\"\nfuture=\"owned unknown field\"\n\n"
	suffix := "[future]\nflag=\"unrelated unknown\" # keep tail\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(prefix+target+suffix), 0600); err != nil {
		t.Fatal(err)
	}
	if err := auth.ActivateAPIKeyBound("openai", "owned-managed-key", "openai_chat", "https://api.openai.com/v1"); err != nil {
		t.Fatal(err)
	}
	if err := auth.PutOAuth("openai", auth.OAuthCredential{AccessToken: "owned-oauth-key"}); err != nil {
		t.Fatal(err)
	}
	snapshot := settingsSnapshotForTest(t)
	deleted, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: snapshot.Revision, ID: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || string(raw) != prefix+suffix {
		t.Fatal("builtin removal changed unrelated user bytes or retained owned fields")
	}
	if findSettingsProvider(t, deleted, "openai").Registered {
		t.Fatal("removed builtin retained membership")
	}
	if key, err := auth.Get("openai"); err != nil || key != "" {
		t.Fatal("managed API key survived builtin removal")
	}
	if exists, err := auth.HasOAuth("openai"); err != nil || exists {
		t.Fatal("OAuth survived builtin removal")
	}
}

func TestProviderSettingsBuiltinRemovalRejectsDefaultAndComplexOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		defaultCase   bool
	}{
		{"active default", "[provider]\ndefault=\"openai\"\n[provider.openai]\nmodel=\"owned-model\"\n", true},
		{"dotted assignment", "provider.openai.model=\"owned-model\"\n", false},
		{"separate child", "[provider.openai]\nmodel=\"owned-model\"\n[provider.openai.future]\nflag=true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			path := filepath.Join(home, "config.toml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			before := settingsSnapshotForTest(t)
			p := findSettingsProvider(t, before, "openai")
			if !p.Registered {
				t.Fatal("owned connection lost registration")
			}
			if p.Removable != tc.defaultCase {
				t.Fatal("default and syntax ownership removal flags conflated")
			}
			if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: before.Revision, ID: "openai"}); !errors.Is(err, ErrProviderSettingsReadOnly) {
				t.Fatalf("unsafe delete accepted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != tc.content {
				t.Fatal("rejected delete changed user config")
			}
		})
	}
}

func TestProviderSettingsBuiltinDeleteRollbackRestoresRegistrationAndCredentials(t *testing.T) {
	for _, failure := range []string{"config-after", "sidecar-after", "credential-stage"} {
		t.Run(failure, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			initial := settingsSnapshotForTest(t)
			p := findSettingsProvider(t, initial, "openai")
			saved, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: initial.Revision, Provider: &p, APIKey: "delete-rollback-api"})
			if err != nil {
				t.Fatal(err)
			}
			if err := auth.PutOAuth("openai", auth.OAuthCredential{AccessToken: "delete-rollback-oauth"}); err != nil {
				t.Fatal(err)
			}
			saved = settingsSnapshotForTest(t)
			originalConfig, _ := os.ReadFile(filepath.Join(home, "config.toml"))
			originalSidecar, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
			oldWriter, oldCredentials := providerSettingsWriteFile, providerSettingsCredentialTransaction
			t.Cleanup(func() { providerSettingsWriteFile = oldWriter; providerSettingsCredentialTransaction = oldCredentials })
			injected := errors.New("injected provider deletion failure")
			providerSettingsWriteFile = func(path string, data []byte) error {
				if err := atomicWritePrivateFile(path, data); err != nil {
					return err
				}
				kind := "config-after"
				if filepath.Base(path) == providerSettingsFilename {
					kind = "sidecar-after"
				}
				if failure == kind {
					return injected
				}
				return nil
			}
			if failure == "credential-stage" {
				providerSettingsCredentialTransaction = func(fn func(*auth.ProviderSettingsCredentials) error) error {
					return auth.WithProviderSettingsCredentials(func(c *auth.ProviderSettingsCredentials) error {
						if err := fn(c); err != nil {
							return err
						}
						return injected
					})
				}
			}
			if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: saved.Revision, ID: "openai"}); !errors.Is(err, injected) {
				t.Fatalf("delete failure not propagated: %v", err)
			}
			configAfter, _ := os.ReadFile(filepath.Join(home, "config.toml"))
			sidecarAfter, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
			if !bytes.Equal(originalConfig, configAfter) || !bytes.Equal(originalSidecar, sidecarAfter) {
				t.Fatal("failed builtin delete left partial metadata state")
			}
			if key, err := auth.Get("openai"); err != nil || key != "delete-rollback-api" {
				t.Fatal("failed builtin delete lost managed API credential")
			}
			if exists, err := auth.HasOAuth("openai"); err != nil || !exists {
				t.Fatal("failed builtin delete lost OAuth credential")
			}
			providerSettingsWriteFile, providerSettingsCredentialTransaction = oldWriter, oldCredentials
			restored := settingsSnapshotForTest(t)
			if restored.Revision != saved.Revision || !findSettingsProvider(t, restored, "openai").Registered {
				t.Fatal("failed delete changed registration or CAS revision")
			}
		})
	}
}

func TestProviderSettingsBuiltinDeleteCannotExposeShadowedDefaultEnvironment(t *testing.T) {
	for _, tc := range []struct{ id, variable string }{{"openai", "OPENAI_API_KEY"}, {"anthropic", "ANTHROPIC_API_KEY"}, {"gemini", "GEMINI_API_KEY"}} {
		t.Run(tc.id, func(t *testing.T) {
			home := providerSettingsTestHome(t)
			t.Setenv(tc.variable, "shadowed-default-environment-fixture")
			t.Setenv("UNSET_REMOVAL_KEY", "")
			original := []byte("[provider." + tc.id + "]\napi_key_env=\"UNSET_REMOVAL_KEY\"\n")
			if err := os.WriteFile(filepath.Join(home, "config.toml"), original, 0600); err != nil {
				t.Fatal(err)
			}
			before := settingsSnapshotForTest(t)
			p := findSettingsProvider(t, before, tc.id)
			if !p.Registered || p.HasCredential || p.Removable || p.RemoveReason == "" {
				t.Fatal("deletion would expose a shadowed default environment credential")
			}
			if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "delete", ExpectedRevision: before.Revision, ID: tc.id}); !errors.Is(err, ErrProviderSettingsReadOnly) {
				t.Fatalf("shadowed default env could be reactivated by delete: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(home, "config.toml"))
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("blocked environment deletion changed user configuration")
			}
		})
	}
}

func TestProviderSettingsUntrustedWorkspaceReadWritePolicyMatches(t *testing.T) {
	home := providerSettingsTestHome(t)
	if err := os.MkdirAll(".metis", 0700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(".metis", "config.toml")
	originalProject := []byte("# never modify project\n[provider]\ndefault=\"gemini\"\n[provider.openai]\nmodel=\"untrusted-model\"\n")
	if err := os.WriteFile(project, originalProject, 0600); err != nil {
		t.Fatal(err)
	}
	untrusted, err := ReadProviderSettingsForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := ReadProviderSettingsForWorkspace(true)
	if err != nil {
		t.Fatal(err)
	}
	if untrusted.Revision != trusted.Revision {
		t.Fatal("trust changed shared CAS fingerprint")
	}
	if findSettingsProvider(t, untrusted, "openai").Registered {
		t.Fatal("untrusted project appeared as a user registration")
	}
	p := findSettingsProvider(t, untrusted, "openai")
	p.Models = append(p.Models, ProviderModelSettings{ID: "user-model"})
	p.Model = "user-model"
	saved, err := SaveProviderSettingsForWorkspace(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: untrusted.Revision, Provider: &p, APIKey: "user-owned-fixture-key", MakeDefault: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	owned := findSettingsProvider(t, saved, "openai")
	if !owned.Registered || owned.ReadOnly || !owned.HasCredential || owned.Model != "user-model" || saved.DefaultProvider != "openai" {
		t.Fatal("untrusted project prevented an owned provider save/default")
	}
	reread, err := ReadProviderSettingsForWorkspace(false)
	if err != nil {
		t.Fatal(err)
	}
	if reread.Revision != saved.Revision {
		t.Fatal("untrusted save returned an inconsistent revision")
	}
	beforeConfig, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	beforeSidecar, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
	if _, err := SaveProviderSettingsForWorkspace(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: saved.Revision, Provider: &owned}, true); !errors.Is(err, ErrProviderSettingsReadOnly) {
		t.Fatalf("trusted provider overlay was silently overridden: %v", err)
	}
	afterConfig, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	afterSidecar, _ := os.ReadFile(filepath.Join(home, providerSettingsFilename))
	if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforeSidecar, afterSidecar) {
		t.Fatal("trusted overlay rejection changed owned settings")
	}
	selected, err := SaveProviderSettingsForWorkspace(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: saved.Revision, ID: "openai", Model: p.Models[0].ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if selected.DefaultModel != p.Models[0].ID {
		t.Fatal("untrusted project prevented selecting a saved user model")
	}
	custom := settingsProviderForTest()
	customSaved, err := SaveProviderSettingsForWorkspace(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: selected.Revision, Provider: &custom, MakeDefault: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveProviderSettingsForWorkspace(ProviderSettingsMutation{Action: "make_default", ExpectedRevision: customSaved.Revision, ID: custom.ID}, true); !errors.Is(err, ErrProviderSettingsReadOnly) {
		t.Fatalf("trusted default overlay was silently overridden: %v", err)
	}
	deleted, err := SaveProviderSettingsForWorkspace(ProviderSettingsMutation{Action: "delete", ExpectedRevision: customSaved.Revision, ID: "openai"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if findSettingsProvider(t, deleted, "openai").Registered {
		t.Fatal("untrusted project resurrected the deleted user provider")
	}
	afterProject, err := os.ReadFile(project)
	if err != nil || !bytes.Equal(afterProject, originalProject) {
		t.Fatal("user settings mutation modified project configuration")
	}
	// The legacy public writer remains conservative/trusted by default.
	if _, err := SaveProviderSettings(ProviderSettingsMutation{Action: "upsert", ExpectedRevision: deleted.Revision, Provider: &p}); !errors.Is(err, ErrProviderSettingsReadOnly) {
		t.Fatalf("legacy trusted Save policy changed: %v", err)
	}
}
