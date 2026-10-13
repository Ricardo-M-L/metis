package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func seedCatalogCache(t *testing.T, home, body string, modified time.Time) {
	t.Helper()
	path := filepath.Join(home, "cache", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientMakesStaleDiskContextAvailableBeforeNetwork(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("METIS_MODELS_URL", srv.URL)
	home := t.TempDir()
	seedCatalogCache(t, home, fixture, time.Now().Add(-24*time.Hour))

	c := NewClient(home)
	if window, ok := c.LookupContextWindow("minimax", "MiniMax-M2.7"); !ok || window != 192000 {
		t.Fatalf("cold start lost disk context: window=%d found=%v", window, ok)
	}
	if requests.Load() != 0 {
		t.Fatalf("constructor contacted network: %d requests", requests.Load())
	}
	if stat := c.Stat(); !stat.Stale || stat.Source != "disk_cache" || stat.SnapshotAt.After(time.Now().Add(-23*time.Hour)) {
		t.Fatalf("old disk snapshot lost its age/source: %+v", stat)
	}
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || !c.Stat().Stale {
		t.Fatalf("offline refresh must retain stale snapshot: requests=%d stat=%+v", requests.Load(), c.Stat())
	}
}

func TestRefreshDoesNotBlockCachedCapabilityLookups(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	c := NewStaticClient(Catalog{"route": {Models: map[string]Model{
		"known": {Limit: Limit{Context: 1_050_000}},
	}}})
	c.URL, c.HTTP, c.CachePath = srv.URL, srv.Client(), filepath.Join(t.TempDir(), "models.json")
	refresh := make(chan error, 1)
	go func() { refresh <- c.Refresh(context.Background()) }()
	<-started
	lookup := make(chan int, 1)
	go func() { window, _ := c.LookupContextWindow("route", "known"); lookup <- window }()
	select {
	case window := <-lookup:
		close(release)
		if window != 1_050_000 {
			t.Fatalf("lookup=%d, want cached 1050000", window)
		}
	case <-time.After(time.Second):
		close(release)
		<-refresh
		t.Fatal("capability lookup waited for network refresh")
	}
	if err := <-refresh; err != nil {
		t.Fatal(err)
	}
}

func TestNewClientIgnoresMalformedDiskCatalog(t *testing.T) {
	home := t.TempDir()
	seedCatalogCache(t, home, `{"unfinished":`, time.Now())
	c := NewClient(home)
	if _, ok := c.LookupContextWindow("unfinished", "model"); ok {
		t.Fatal("malformed cache must not supply a model context")
	}
	if c.Stat().InMemory {
		t.Fatal("malformed cache must leave the client cold")
	}
}

func TestNewClientFreshDiskAvoidsNetworkUntilTTLExpires(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	t.Setenv("METIS_MODELS_URL", srv.URL)
	home := t.TempDir()
	seedCatalogCache(t, home, fixture, time.Now())
	c := NewClient(home)
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || c.Stat().Stale {
		t.Fatalf("fresh disk should avoid network: requests=%d stat=%+v", requests.Load(), c.Stat())
	}
}

func TestGetSharesFetchAndWaiterCancellationDoesNotCancelOwner(t *testing.T) {
	var requests atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	owner := make(chan error, 1)
	go func() { _, err := c.Get(context.Background()); owner <- err }()
	<-started
	waitContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, waitErr := c.Get(waitContext)
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		close(release)
		<-owner
		t.Fatalf("cancelled waiter = %v, want deadline exceeded", waitErr)
	}
	if requests.Load() != 1 {
		close(release)
		<-owner
		t.Fatalf("concurrent callers issued %d requests", requests.Load())
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatalf("waiter cancelled owner's fetch: %v", err)
	}
	if window, found := c.LookupContextWindow("minimax", "MiniMax-M2.7"); !found || window != 192000 {
		t.Fatalf("owner did not publish metadata: (%d, %v)", window, found)
	}
}

func TestCancelledFetchReleasesFlightForNextAttempt(t *testing.T) {
	var requests atomic.Int64
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() { _, err := c.Get(ctx); owner <- err }()
	<-started
	cancel()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled owner = %v", err)
	}
	if _, err := c.Get(context.Background()); err != nil {
		t.Fatalf("next fetch remained blocked by cancelled flight: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("fetches=%d, want two independent attempts", requests.Load())
	}
}

func TestLookupRouteOrUnambiguousContext(t *testing.T) {
	c := NewStaticClient(Catalog{
		"smaller-route": {Models: map[string]Model{
			"shared":    {Limit: Limit{Context: 272000}},
			"unanimous": {Limit: Limit{Context: 1_050_000}},
			"no-limit":  {Limit: Limit{}},
		}},
		"larger-route": {Models: map[string]Model{
			"shared":    {Limit: Limit{Context: 872000}},
			"unanimous": {Limit: Limit{Context: 1_050_000}},
			"no-limit":  {Limit: Limit{Context: 1_050_000}},
			"elsewhere": {Limit: Limit{Context: 65536}},
		}},
	})
	tests := []struct {
		name, provider, model, source string
		strict                        bool
		window                        int
	}{
		{name: "route capacity wins over larger sibling", provider: "smaller-route", model: "shared", source: "catalog_route", window: 272000},
		{name: "known route missing model cannot borrow", provider: "smaller-route", model: "elsewhere"},
		{name: "known route missing limit cannot borrow", provider: "smaller-route", model: "no-limit"},
		{name: "friendly alias with unanimous exact model", provider: "easycliproxyapi", model: "unanimous", source: "catalog_model", window: 1_050_000},
		{name: "explicit catalog provider typo stays unknown", provider: "typo", model: "unanimous", strict: true},
		{name: "conflicting routes cannot be guessed", provider: "easycliproxyapi", model: "shared"},
		{name: "unknown route capacity is not unanimous", provider: "easycliproxyapi", model: "no-limit"},
		{name: "empty model is not wildcard", provider: "easycliproxyapi"},
		{name: "model prefixes are not exact", provider: "easycliproxyapi", model: "unanimous-latest"},
		{name: "model case is not normalized", provider: "easycliproxyapi", model: "Unanimous"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				match, found := c.LookupRouteOrUnambiguousContext(tt.provider, tt.model, tt.strict)
				if found != (tt.window > 0) || match.Window != tt.window || match.Source != tt.source {
					t.Fatalf("resolution = (%+v, %v), want window=%d source=%q", match, found, tt.window, tt.source)
				}
				if found && match.Stale {
					t.Fatal("static metadata must not be marked stale")
				}
				if found && match.Source == "catalog_route" && match.ProviderID != tt.provider {
					t.Fatalf("exact route identity = %q", match.ProviderID)
				}
				if match.Source == "catalog_model" && match.ProviderID != "" {
					t.Fatal("unanimous model-only metadata must not invent a route identity")
				}
			}
		})
	}
}

func TestRefreshPublishesNewModelForSubsequentLookups(t *testing.T) {
	newCatalog := Catalog{"route": {Models: map[string]Model{
		"new-model": {Limit: Limit{Context: 872000}},
	}}}
	body, err := json.Marshal(newCatalog)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, string(body))
	defer srv.Close()
	c := newClientFor(t, srv.URL)
	if _, found := c.LookupContextWindow("route", "new-model"); found {
		t.Fatal("new model unexpectedly present before discovery")
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, found := c.LookupContextWindow("route", "new-model"); !found || got != 872000 {
		t.Fatalf("new model after refresh = (%d, %v)", got, found)
	}
}

func TestLookupRouteOrUnambiguousLimitsKeepsIndependentFields(t *testing.T) {
	c := NewStaticClient(Catalog{
		"smaller-route": {Models: map[string]Model{
			"shared":         {Limit: Limit{Context: 128000, Input: 96000, Output: 8000}},
			"coherent":       {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"input-conflict": {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"out-conflict":   {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"missing-input":  {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"context-only":   {Limit: Limit{Context: 1024000}},
		}},
		"larger-route": {Models: map[string]Model{
			"shared":         {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"coherent":       {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"input-conflict": {Limit: Limit{Context: 1024000, Input: 750000, Output: 32000}},
			"out-conflict":   {Limit: Limit{Context: 1024000, Input: 800000, Output: 64000}},
			"missing-input":  {Limit: Limit{Context: 1024000, Output: 32000}},
			"context-only":   {Limit: Limit{Context: 1024000}},
			"other-model":    {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
		}},
		"third-route": {Models: map[string]Model{
			"coherent":       {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"input-conflict": {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"out-conflict":   {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"missing-input":  {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
		}},
	})
	tests := []struct {
		name, route, model, source string
		strict                     bool
		limit                      Limit
	}{
		{"exact lower route", "smaller-route", "shared", "catalog_route", false, Limit{Context: 128000, Input: 96000, Output: 8000}},
		{"known route authoritative under strict matching", "smaller-route", "shared", "catalog_route", true, Limit{Context: 128000, Input: 96000, Output: 8000}},
		{"unknown profile coherent exact model", "friendly-profile", "coherent", "catalog_model", false, Limit{Context: 1024000, Input: 800000, Output: 32000}},
		{"input conflict keeps agreed output", "friendly-profile", "input-conflict", "catalog_model", false, Limit{Context: 1024000, Output: 32000}},
		{"output conflict keeps agreed input", "friendly-profile", "out-conflict", "catalog_model", false, Limit{Context: 1024000, Input: 800000}},
		{"missing input does not infer context minus output", "friendly-profile", "missing-input", "catalog_model", false, Limit{Context: 1024000, Output: 32000}},
		{"context only does not invent input or output", "friendly-profile", "context-only", "catalog_model", false, Limit{Context: 1024000}},
		{"context conflict rejects entire match", "friendly-profile", "shared", "", false, Limit{}},
		{"explicit typo does not borrow", "friendly-profile", "coherent", "", true, Limit{}},
		{"known route model miss does not borrow", "smaller-route", "other-model", "", false, Limit{}},
		{"exact ID required", "friendly-profile", "coherent-latest", "", false, Limit{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 30; i++ {
				got, found := c.LookupRouteOrUnambiguousLimits(tt.route, tt.model, tt.strict)
				if found != (tt.limit.Context > 0) || got.Limit != tt.limit || got.Source != tt.source {
					t.Fatalf("limits=(%+v, %v), want limit=%+v source=%s", got, found, tt.limit, tt.source)
				}
				if got.Stale || (got.Source == "catalog_route" && got.ProviderID != tt.route) || (got.Source == "catalog_model" && got.ProviderID != "") {
					t.Fatalf("incorrect match provenance: %+v", got)
				}
				context, contextFound := c.LookupRouteOrUnambiguousContext(tt.route, tt.model, tt.strict)
				if contextFound != found || context.Window != got.Limit.Context || context.Source != got.Source || context.ProviderID != got.ProviderID {
					t.Fatalf("legacy context result diverged: context=%+v limits=%+v", context, got)
				}
			}
		})
	}
}

func TestLookupLimitsNonpositiveOptionalFieldsRemainUnknown(t *testing.T) {
	c := NewStaticClient(Catalog{
		"route-a": {Models: map[string]Model{
			"negative-input":  {Limit: Limit{Context: 1024000, Input: -1, Output: 32000}},
			"negative-output": {Limit: Limit{Context: 1024000, Input: 800000, Output: -1}},
			"missing-context": {Limit: Limit{Input: 800000, Output: 32000}},
		}},
		"route-b": {Models: map[string]Model{
			"negative-input":  {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"negative-output": {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
			"missing-context": {Limit: Limit{Context: 1024000, Input: 800000, Output: 32000}},
		}},
	})
	for _, route := range []string{"route-a", "unknown-profile"} {
		if got, found := c.LookupRouteOrUnambiguousLimits(route, "negative-input", false); !found || got.Limit != (Limit{Context: 1024000, Output: 32000}) {
			t.Fatalf("negative input from %s became a usable or inferred cap: (%+v, %v)", route, got, found)
		}
		if got, found := c.LookupRouteOrUnambiguousLimits(route, "negative-output", false); !found || got.Limit != (Limit{Context: 1024000, Input: 800000}) {
			t.Fatalf("negative output from %s became a usable cap: (%+v, %v)", route, got, found)
		}
		if got, found := c.LookupRouteOrUnambiguousLimits(route, "missing-context", false); found || got != (LimitsMatch{}) {
			t.Fatalf("missing context from %s borrowed or inferred a total: (%+v, %v)", route, got, found)
		}
	}
}

func TestLimitsColdDiskSnapshotAndStatDoNotRequestNetwork(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "must not be requested", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("METIS_MODELS_URL", srv.URL)
	home := t.TempDir()
	seedCatalogCache(t, home, `{"route":{"models":{"novel-wire-model":{"limit":{"context":1024000,"input":800000,"output":32000}}}}}`, time.Now().Add(-24*time.Hour))
	c := NewClient(home)
	for i := 0; i < 30; i++ {
		got, found := c.LookupRouteOrUnambiguousLimits("friendly-profile", "novel-wire-model", false)
		if !found || got.Limit != (Limit{Context: 1024000, Input: 800000, Output: 32000}) || !got.Stale || got.Source != "catalog_model" {
			t.Fatalf("cold cached limits = (%+v, %v)", got, found)
		}
		if stat := c.Stat(); !stat.Stale || stat.Source != "disk_cache" {
			t.Fatalf("stale disk identity lost: %+v", stat)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("cache-only lookup/stat performed %d HTTP requests", requests.Load())
	}
}

func TestCatalogDefaultUsesConfiguredHomeInFreshProcess(t *testing.T) {
	userHome, metisHome := t.TempDir(), filepath.Join(t.TempDir(), "custom metis home")
	seedCatalogCache(t, filepath.Join(userHome, ".metis"), `{"route":{"models":{"same-wire-model":{"limit":{"context":128000,"input":96000,"output":8000}}}}}`, time.Now())
	seedCatalogCache(t, metisHome, `{"route":{"models":{"same-wire-model":{"limit":{"context":1050000,"input":922000,"output":128000}}}}}`, time.Now())
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "network is not needed for fresh snapshots", http.StatusInternalServerError)
	}))
	defer srv.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, userHome, metisHome, expectedHome, mode string
		disabled                                      bool
	}{
		{"METIS_HOME wins over HOME", userHome, metisHome, metisHome, "full", false},
		{"unset METIS_HOME uses HOME default", userHome, "", filepath.Join(userHome, ".metis"), "home", false},
		{"METIS_HOME works without HOME", "", metisHome, metisHome, "full", false},
		{"disable skips configured home and network", userHome, metisHome, metisHome, "disabled", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestCatalogDefaultHomeHelper$", "-test.v")
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "METIS_HOME=") && !strings.HasPrefix(value, "METIS_CATALOG_DISABLE=") && !strings.HasPrefix(value, "METIS_MODELS_URL=") && !strings.HasPrefix(value, "METIS_CATALOG_DEFAULT_HELPER_") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+tt.userHome, "METIS_MODELS_URL="+srv.URL,
				"METIS_CATALOG_DEFAULT_HELPER_MODE="+tt.mode, "METIS_CATALOG_DEFAULT_HELPER_HOME="+tt.expectedHome)
			if tt.metisHome != "" {
				cmd.Env = append(cmd.Env, "METIS_HOME="+tt.metisHome)
			}
			if tt.disabled {
				cmd.Env = append(cmd.Env, "METIS_CATALOG_DISABLE=1")
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fresh-process catalog home checks failed: %v\n%s", err, output)
			}
			if requests.Load() != 0 {
				t.Fatalf("synchronous cached initialization or disable mode contacted network: %d", requests.Load())
			}
		})
	}
}

func TestCatalogDefaultHomeHelper(t *testing.T) {
	mode := os.Getenv("METIS_CATALOG_DEFAULT_HELPER_MODE")
	if mode == "" {
		t.Skip("fresh-process helper")
	}
	c := Default()
	if mode == "disabled" {
		if c != nil {
			t.Fatal("METIS_CATALOG_DISABLE=1 must leave the singleton disabled")
		}
		return
	}
	if c == nil {
		t.Fatal("Default ignored configured cache home")
	}
	want := Limit{Context: 128000, Input: 96000, Output: 8000}
	if mode == "full" {
		want = Limit{Context: 1050000, Input: 922000, Output: 128000}
	}
	got, found := c.LookupRouteOrUnambiguousLimits("route", "same-wire-model", true)
	if !found || got.Limit != want || got.Source != "catalog_route" || got.Stale {
		t.Fatalf("synchronous Default used wrong cache: got=(%+v,%v) want=%+v", got, found, want)
	}
	wantPath := filepath.Join(os.Getenv("METIS_CATALOG_DEFAULT_HELPER_HOME"), "cache", "models.json")
	if stat := c.Stat(); stat.CachePath != wantPath || !stat.InMemory || stat.Source != "disk_cache" {
		t.Fatalf("Default cache scope mismatch: %+v, want %s", stat, wantPath)
	}
}
