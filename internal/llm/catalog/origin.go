package catalog

import (
	"net/url"
	"strconv"
	"strings"
)

type issuerOrigin struct {
	providerIDs []string
	apiDomains  []string
	docDomains  []string
}

// This registry describes verified issuer identities, not model capacity.
// Limits still come from exact model records in the cached public catalog.
var moonshotOrigin = issuerOrigin{
	providerIDs: []string{"moonshotai", "moonshotai-cn"},
	apiDomains:  []string{"moonshot.ai", "moonshot.cn"},
	docDomains:  []string{"moonshot.ai", "moonshot.cn", "kimi.ai"},
}

var zhipuOrigin = issuerOrigin{
	providerIDs: []string{"zhipuai", "zai", "zai-coding-plan"},
	apiDomains:  []string{"bigmodel.cn", "z.ai"},
	docDomains:  []string{"bigmodel.cn", "z.ai"},
}

var verifiedIssuerOrigins = map[string]issuerOrigin{
	"kimi":       moonshotOrigin,
	"moonshot":   moonshotOrigin,
	"moonshotai": moonshotOrigin,
	"zhipu":      zhipuOrigin,
	"zhipuai":    zhipuOrigin,
	"zai":        zhipuOrigin,
	"z.ai":       zhipuOrigin,
}

// LookupIssuerLimits is a compatibility lookup for an unknown custom route
// whose endpoint explicitly identifies its model's issuer. Explicit catalog
// mappings and known routes retain authority even when their model is absent.
// Only verified issuer catalog IDs with trusted API and documentation origins
// may contribute an exact model record. This reads memory and performs no I/O.
func (c *Client) LookupIssuerLimits(providerID, modelID, issuer string, strictRoute bool) (LimitsMatch, bool) {
	if c == nil || strictRoute || modelID == "" {
		return LimitsMatch{}, false
	}
	origin, verified := verifiedIssuerOrigins[strings.ToLower(strings.TrimSpace(issuer))]
	if !verified {
		return LimitsMatch{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil {
		return LimitsMatch{}, false
	}
	if _, knownRoute := c.cached[providerID]; knownRoute {
		return LimitsMatch{}, false
	}
	limit := Limit{}
	canonicalProvider := ""
	for _, id := range origin.providerIDs {
		p, exists := c.cached[id]
		if !exists || (p.ID != "" && p.ID != id) || !trustedIssuerOrigin(p.API, origin.apiDomains) || !trustedIssuerOrigin(p.Doc, origin.docDomains) {
			continue
		}
		m, found := p.Models[modelID]
		if !found || (m.ID != "" && m.ID != modelID) {
			continue
		}
		if m.Limit.Context <= 0 || (limit.Context > 0 && limit.Context != m.Limit.Context) {
			return LimitsMatch{}, false
		}
		if canonicalProvider == "" {
			canonicalProvider = id
			limit = m.Limit
			if limit.Input <= 0 {
				limit.Input = 0
			}
			if limit.Output <= 0 {
				limit.Output = 0
			}
			continue
		}
		// Missing or conflicting independent limits remain unknown; do not
		// infer input from context minus output, or total from input plus output.
		if m.Limit.Input <= 0 || m.Limit.Input != limit.Input {
			limit.Input = 0
		}
		if m.Limit.Output <= 0 || m.Limit.Output != limit.Output {
			limit.Output = 0
		}
	}
	if canonicalProvider == "" {
		return LimitsMatch{}, false
	}
	return LimitsMatch{Limit: limit, Source: "catalog_issuer", ProviderID: canonicalProvider, Stale: !c.freshLocked()}, true
}

func trustedIssuerOrigin(raw string, domains []string) bool {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				return false
			}
		}
	}
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
