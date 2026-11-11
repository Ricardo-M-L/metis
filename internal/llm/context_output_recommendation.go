package llm

// CatalogReasoningOutputLimit reads an exact issuer/model record from the
// resolver's captured local catalog. It is a recommendation source, not an
// override of the independently resolved serving-route limits. In particular,
// conflicting reseller windows do not manufacture a larger context capacity.
// This method performs no discovery, refresh or other network I/O.
func (r *ContextResolver) CatalogReasoningOutputLimit(issuer string) (int, bool) {
	if r == nil || r.catalog == nil || r.model == "" || issuer == "" {
		return 0, false
	}
	for _, hit := range r.catalog.LookupModel(r.model) {
		if hit.ProviderID == issuer && hit.Model.Reasoning && hit.Model.Limit.Output > 0 {
			return hit.Model.Limit.Output, true
		}
	}
	return 0, false
}
