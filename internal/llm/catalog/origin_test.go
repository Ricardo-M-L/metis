package catalog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func issuerTestProvider(id, model string, limit Limit) Provider {
	domain := "moonshot.ai"
	if id == "moonshotai-cn" {
		domain = "moonshot.cn"
	}
	return Provider{ID: id, API: "https://api." + domain + "/v1", Doc: "https://platform." + domain + "/docs",
		Models: map[string]Model{model: {ID: model, Limit: limit}}}
}

func TestIssuerLookupResolvesOriginalLimitsDespiteAggregatorConflict(t *testing.T) {
	want := Limit{Context: 1_000_000, Input: 800000, Output: 64000}
	c := NewStaticClient(Catalog{
		"moonshotai": issuerTestProvider("moonshotai", "kimi-k3", want),
		"aggregator": {Models: map[string]Model{"kimi-k3": {Limit: Limit{Context: 262144}}}},
	})
	if got, found := c.LookupRouteOrUnambiguousLimits("private-proxy", "kimi-k3", false); found || got != (LimitsMatch{}) {
		t.Fatalf("conflicting provider-blind lookup must stay unknown: (%+v,%v)", got, found)
	}
	got, found := c.LookupIssuerLimits("private-proxy", "kimi-k3", "kimi", false)
	if !found || got.Limit != want || got.Source != "catalog_issuer" || got.ProviderID != "moonshotai" || got.Stale {
		t.Fatalf("explicit issuer did not resolve original metadata: (%+v,%v)", got, found)
	}
}

func TestIssuerLookupIsIdentityDrivenAndExactModelOnly(t *testing.T) {
	want := Limit{Context: 512000, Input: 400000, Output: 16000}
	c := NewStaticClient(Catalog{"moonshotai": issuerTestProvider("moonshotai", "future-unpublished-wire-id", want)})
	for _, issuer := range []string{"kimi", "moonshot", "moonshotai", "  KiMi \t"} {
		got, found := c.LookupIssuerLimits("private-proxy", "future-unpublished-wire-id", issuer, false)
		if !found || got.Limit != want || got.ProviderID != "moonshotai" {
			t.Fatalf("issuer %q failed without a model-name rule: (%+v,%v)", issuer, got, found)
		}
	}
	for _, issuer := range []string{"", "unknown", "kimi-proxy", "moonshotai-cn", "moonshotai/anything", "moonshot.ai"} {
		if got, found := c.LookupIssuerLimits("private-proxy", "future-unpublished-wire-id", issuer, false); found || got != (LimitsMatch{}) {
			t.Fatalf("unknown issuer %q was guessed: (%+v,%v)", issuer, got, found)
		}
	}
	for _, model := range []string{"", "future-unpublished-wire-id-latest", "Future-unpublished-wire-id", " future-unpublished-wire-id", "kimi-k3"} {
		if got, found := c.LookupIssuerLimits("private-proxy", model, "kimi", false); found || got != (LimitsMatch{}) {
			t.Fatalf("model %q was not an exact match: (%+v,%v)", model, got, found)
		}
	}
}

func TestIssuerLookupDoesNotOverrideKnownOrExplicitRoutes(t *testing.T) {
	c := NewStaticClient(Catalog{
		"moonshotai":  issuerTestProvider("moonshotai", "shared", Limit{Context: 1_000_000}),
		"small-route": {Models: map[string]Model{"shared": {Limit: Limit{Context: 128000}}}},
		"known-miss":  {},
	})
	if got, found := c.LookupRouteOrUnambiguousLimits("small-route", "shared", false); !found || got.Limit.Context != 128000 {
		t.Fatalf("known smaller route lost authority: (%+v,%v)", got, found)
	}
	for _, route := range []string{"small-route", "known-miss", "moonshotai"} {
		if got, found := c.LookupIssuerLimits(route, "shared", "kimi", false); found || got != (LimitsMatch{}) {
			t.Fatalf("known route %q borrowed issuer metadata: (%+v,%v)", route, got, found)
		}
	}
	if got, found := c.LookupIssuerLimits("explicit-typo", "shared", "kimi", true); found || got != (LimitsMatch{}) {
		t.Fatalf("strict route mapping borrowed issuer metadata: (%+v,%v)", got, found)
	}
}

func TestIssuerLookupRegionAgreementAndCanonicalProvenance(t *testing.T) {
	want := Limit{Context: 512000, Input: 400000, Output: 16000}
	tests := []struct {
		name     string
		catalog  Catalog
		limit    Limit
		provider string
	}{
		{"both agree", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", want)}, want, "moonshotai"},
		{"only cn publishes model", Catalog{"moonshotai": issuerTestProvider("moonshotai", "only-k2", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", want)}, want, "moonshotai-cn"},
		{"region context conflict", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", Limit{Context: 262144})}, Limit{}, ""},
		{"region context missing", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", Limit{Input: 400000, Output: 16000})}, Limit{}, ""},
		{"region input conflict", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", Limit{Context: 512000, Input: 300000, Output: 16000})}, Limit{Context: 512000, Output: 16000}, "moonshotai"},
		{"region output conflict", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", Limit{Context: 512000, Input: 400000, Output: 32000})}, Limit{Context: 512000, Input: 400000}, "moonshotai"},
		{"region optional limits missing", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", want), "moonshotai-cn": issuerTestProvider("moonshotai-cn", "shared", Limit{Context: 512000})}, Limit{Context: 512000}, "moonshotai"},
		{"only another model", Catalog{"moonshotai": issuerTestProvider("moonshotai", "only-k2", want)}, Limit{}, ""},
		{"only aggregator", Catalog{"aggregator": issuerTestProvider("aggregator", "shared", want)}, Limit{}, ""},
		{"zero context", Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", Limit{Input: 400000, Output: 16000})}, Limit{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewStaticClient(tt.catalog)
			for i := 0; i < 30; i++ {
				got, found := c.LookupIssuerLimits("private-proxy", "shared", "moonshot", false)
				if found != (tt.limit.Context > 0) || got.Limit != tt.limit || got.ProviderID != tt.provider {
					t.Fatalf("issuer limits = (%+v,%v), want limit=%+v provider=%s", got, found, tt.limit, tt.provider)
				}
			}
		})
	}
}

func TestIssuerLookupRejectsUntrustedProviderIdentityAndOrigins(t *testing.T) {
	want := Limit{Context: 512000, Input: 400000, Output: 16000}
	tests := []struct {
		name   string
		modify func(*Provider)
	}{
		{"map ID mismatch", func(p *Provider) { p.ID = "aggregator" }},
		{"empty API", func(p *Provider) { p.API = "" }},
		{"empty docs", func(p *Provider) { p.Doc = "" }},
		{"non HTTPS API", func(p *Provider) { p.API = "http://api.moonshot.ai/v1" }},
		{"non HTTPS docs", func(p *Provider) { p.Doc = "http://platform.moonshot.cn/docs" }},
		{"fake API suffix", func(p *Provider) { p.API = "https://api.moonshot.ai.evil.example/v1" }},
		{"fake docs suffix", func(p *Provider) { p.Doc = "https://moonshot.cn.evil.example/docs" }},
		{"domain without boundary", func(p *Provider) { p.API = "https://notmoonshot.ai/v1" }},
		{"API userinfo", func(p *Provider) { p.API = "https://evil.example@api.moonshot.ai/v1" }},
		{"docs userinfo", func(p *Provider) { p.Doc = "https://user:secret@platform.moonshot.ai/docs" }},
		{"relative docs", func(p *Provider) { p.Doc = "/docs" }},
		{"malformed API", func(p *Provider) { p.API = "https://api.moonshot.ai:invalid/v1" }},
		{"bracketed DNS host", func(p *Provider) { p.API = "https://[api.moonshot.ai]/v1" }},
		{"invalid numeric API port", func(p *Provider) { p.API = "https://api.moonshot.ai:65536/v1" }},
		{"empty subdomain label", func(p *Provider) { p.API = "https://api..moonshot.ai/v1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := issuerTestProvider("moonshotai", "shared", want)
			tt.modify(&p)
			c := NewStaticClient(Catalog{"moonshotai": p})
			if got, found := c.LookupIssuerLimits("private-proxy", "shared", "kimi", false); found || got != (LimitsMatch{}) {
				t.Fatalf("untrusted issuer row accepted: (%+v,%v)", got, found)
			}
		})
	}
	for _, uri := range []string{"https://moonshot.ai", "https://api.moonshot.cn/v1", "https://nested.api.moonshot.ai:443/v1"} {
		p := issuerTestProvider("moonshotai", "shared", want)
		p.ID, p.API, p.Doc = "", uri, "https://platform.moonshot.cn/docs#guide"
		c := NewStaticClient(Catalog{"moonshotai": p})
		if got, found := c.LookupIssuerLimits("private-proxy", "shared", "kimi", false); !found || got.Limit != want {
			t.Fatalf("valid origin %q rejected: (%+v,%v)", uri, got, found)
		}
	}
}

func TestIssuerLookupReadsOnlyMemoryAndPreservesStaleness(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "lookup must not request network", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewStaticClient(Catalog{"moonshotai": issuerTestProvider("moonshotai", "shared", Limit{Context: 512000})})
	c.URL, c.HTTP, c.CachePath = srv.URL, srv.Client(), filepath.Join(t.TempDir(), "must-not-be-created", "models.json")
	c.source, c.TTL, c.snapshotT = "disk_cache", time.Minute, time.Now().Add(-time.Hour)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for j := 0; j < 50; j++ {
				got, found := c.LookupIssuerLimits("private-proxy", "shared", "kimi", false)
				if !found || got.Limit.Context != 512000 || !got.Stale {
					t.Errorf("stale memory metadata lost: (%+v,%v)", got, found)
					return
				}
			}
		})
	}
	workers.Wait()
	if requests.Load() != 0 {
		t.Fatalf("memory lookup made %d HTTP requests", requests.Load())
	}
	if _, err := os.Stat(filepath.Dir(c.CachePath)); !os.IsNotExist(err) {
		t.Fatalf("memory lookup touched disk: %v", err)
	}
}

func TestIssuerDocumentationDomainMigrationDoesNotExpandAPITrust(t *testing.T) {
	limit := Limit{Context: 512000, Input: 400000, Output: 16000}
	p := issuerTestProvider("moonshotai", "shared", limit)
	p.Doc = "https://platform.kimi.ai/docs/api/chat"
	c := NewStaticClient(Catalog{"moonshotai": p})
	if got, found := c.LookupIssuerLimits("private-proxy", "shared", "kimi", false); !found || got.Limit != limit {
		t.Fatalf("verified documentation migration rejected original catalog: (%+v,%v)", got, found)
	}
	for _, api := range []string{"https://api.kimi.ai/v1", "https://api.unknown.example/v1"} {
		p.API = api
		c = NewStaticClient(Catalog{"moonshotai": p})
		if got, found := c.LookupIssuerLimits("private-proxy", "shared", "kimi", false); found || got != (LimitsMatch{}) {
			t.Fatalf("documentation domain broadened API trust to %s: (%+v,%v)", api, got, found)
		}
	}
	for _, doc := range []string{"https://platform.kimi.ai.evil.example/docs", "http://platform.kimi.ai/docs", "https://user:secret@platform.kimi.ai/docs"} {
		p.API, p.Doc = "https://api.moonshot.ai/v1", doc
		c = NewStaticClient(Catalog{"moonshotai": p})
		if got, found := c.LookupIssuerLimits("private-proxy", "shared", "kimi", false); found || got != (LimitsMatch{}) {
			t.Fatalf("untrusted migrated documentation origin accepted: (%+v,%v)", got, found)
		}
	}
}

func zhipuIssuerTestProvider(id, model string, limit Limit) Provider {
	api := "https://api.z.ai/api/paas/v4"
	if id == "zhipuai" {
		api = "https://open.bigmodel.cn/api/paas/v4"
	} else if id == "zai-coding-plan" {
		api = "https://api.z.ai/api/coding/paas/v4"
	}
	return Provider{ID: id, API: api, Doc: "https://docs.z.ai/guides/llm/glm-5.3", Models: map[string]Model{model: {ID: model, Limit: limit}}}
}

func TestZhipuIssuerLookupUsesExactVerifiedOriginals(t *testing.T) {
	want := Limit{Context: 1_000_000, Output: 131_072}
	c := NewStaticClient(Catalog{
		"zhipuai":         zhipuIssuerTestProvider("zhipuai", "glm-5.3", want),
		"zai":             zhipuIssuerTestProvider("zai", "glm-5.3", want),
		"zai-coding-plan": zhipuIssuerTestProvider("zai-coding-plan", "glm-5.3", want),
		"reseller":        {Models: map[string]Model{"glm-5.3": {Limit: Limit{Context: 272_000}}}},
	})
	if got, found := c.LookupRouteOrUnambiguousLimits("private-proxy", "glm-5.3", false); found || got != (LimitsMatch{}) {
		t.Fatalf("reseller conflict should prevent provider-blind lookup: (%+v,%v)", got, found)
	}
	for _, issuer := range []string{"zhipu", "zhipuai", "zai", "Z.AI", "  ZhIpU \t"} {
		got, found := c.LookupIssuerLimits("private-proxy", "glm-5.3", issuer, false)
		if !found || got.Limit != want || got.ProviderID != "zhipuai" || got.Source != "catalog_issuer" || got.Stale {
			t.Fatalf("verified issuer %q did not resolve exact original: (%+v,%v)", issuer, got, found)
		}
	}
	for _, issuer := range []string{"", "glm", "zhipu-proxy", "zai-coding-plan", "api.z.ai", "unknown"} {
		if got, found := c.LookupIssuerLimits("private-proxy", "glm-5.3", issuer, false); found || got != (LimitsMatch{}) {
			t.Fatalf("unverified issuer %q was accepted: (%+v,%v)", issuer, got, found)
		}
	}
	for _, model := range []string{"GLM-5.3", "glm-5.3-high", "glm-5.3-latest", "glm-5.3 ", "glm-5.2"} {
		if got, found := c.LookupIssuerLimits("private-proxy", model, "zhipu", false); found || got != (LimitsMatch{}) {
			t.Fatalf("non-exact model %q imported original limits: (%+v,%v)", model, got, found)
		}
	}
	// Issuer trust is independent of model family and does not encode capacity.
	c = NewStaticClient(Catalog{"zai": zhipuIssuerTestProvider("zai", "future-wire-id", Limit{Context: 512_000, Input: 400_000, Output: 16_000})})
	if got, found := c.LookupIssuerLimits("private-proxy", "future-wire-id", "zhipu", false); !found || got.Limit != (Limit{Context: 512_000, Input: 400_000, Output: 16_000}) || got.ProviderID != "zai" {
		t.Fatalf("issuer limits were inferred from model name or fixed capacity: (%+v,%v)", got, found)
	}
}

func TestZhipuIssuerLookupRejectsUntrustedOriginsAndCapacityConflicts(t *testing.T) {
	want := Limit{Context: 1_000_000, Input: 800_000, Output: 131_072}
	for _, test := range []struct {
		name   string
		modify func(*Provider)
	}{
		{"wrong provider identity", func(p *Provider) { p.ID = "reseller" }},
		{"unverified API", func(p *Provider) { p.API = "https://api.proxy.example/v1" }},
		{"fake API suffix", func(p *Provider) { p.API = "https://open.bigmodel.cn.evil.example/v1" }},
		{"fake docs suffix", func(p *Provider) { p.Doc = "https://docs.z.ai.evil.example/guide" }},
		{"non HTTPS docs", func(p *Provider) { p.Doc = "http://docs.z.ai/guide" }},
		{"API userinfo", func(p *Provider) { p.API = "https://evil.example@api.z.ai/v1" }},
		{"model identity mismatch", func(p *Provider) { m := p.Models["glm-5.3"]; m.ID = "glm-5.2"; p.Models["glm-5.3"] = m }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := zhipuIssuerTestProvider("zhipuai", "glm-5.3", want)
			test.modify(&p)
			c := NewStaticClient(Catalog{"zhipuai": p})
			if got, found := c.LookupIssuerLimits("private-proxy", "glm-5.3", "zhipu", false); found || got != (LimitsMatch{}) {
				t.Fatalf("untrusted original row accepted: (%+v,%v)", got, found)
			}
		})
	}
	c := NewStaticClient(Catalog{
		"zhipuai": zhipuIssuerTestProvider("zhipuai", "glm-5.3", want),
		"zai":     zhipuIssuerTestProvider("zai", "glm-5.3", Limit{Context: 128_000, Input: 100_000, Output: 16_000}),
	})
	if got, found := c.LookupIssuerLimits("private-proxy", "glm-5.3", "zhipu", false); found || got != (LimitsMatch{}) {
		t.Fatalf("conflicting original context capacities were guessed: (%+v,%v)", got, found)
	}
	// A consistent total does not make conflicting or absent I/O limits known.
	c = NewStaticClient(Catalog{
		"zhipuai": zhipuIssuerTestProvider("zhipuai", "glm-5.3", want),
		"zai":     zhipuIssuerTestProvider("zai", "glm-5.3", Limit{Context: 1_000_000, Input: 900_000}),
	})
	if got, found := c.LookupIssuerLimits("private-proxy", "glm-5.3", "zhipu", false); !found || got.Limit != (Limit{Context: 1_000_000}) {
		t.Fatalf("independent input/output constraints were invented: (%+v,%v)", got, found)
	}
}

func TestZhipuIssuerLookupPreservesKnownAndExplicitRouteAuthority(t *testing.T) {
	c := NewStaticClient(Catalog{
		"zhipuai":     zhipuIssuerTestProvider("zhipuai", "glm-5.3", Limit{Context: 1_000_000}),
		"small-route": {Models: map[string]Model{"glm-5.3": {Limit: Limit{Context: 128_000}}}},
		"known-miss":  {},
	})
	for _, route := range []string{"small-route", "known-miss", "zhipuai"} {
		if got, found := c.LookupIssuerLimits(route, "glm-5.3", "zhipu", false); found || got != (LimitsMatch{}) {
			t.Fatalf("known route %q imported larger issuer capacity: (%+v,%v)", route, got, found)
		}
	}
	if got, found := c.LookupIssuerLimits("explicit-miss", "glm-5.3", "zhipu", true); found || got != (LimitsMatch{}) {
		t.Fatalf("explicit catalog mapping imported issuer capacity: (%+v,%v)", got, found)
	}
}
