package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
)

// ContextResolver retains the exact route used for generation. The credential
// is only passed to authenticated discovery; cache records never contain it.
// It does not wrap the provider, so optional transport capabilities survive.
type ContextResolver struct {
	client                         *modelinfo.Client
	binding                        modelinfo.Binding
	model, apiKey, catalogProvider string
	budgetSource                   string
	strictRoute                    bool
	catalog                        *catalog.Client
	catalogMu                      sync.Mutex
	catalogFlight                  *contextCatalogFlight
	catalogRetryAt                 time.Time
	catalogNow                     func() time.Time
	catalogRetryDelay              time.Duration
	catalogRefreshTimeout          time.Duration
}

const (
	defaultContextCatalogRetryDelay = 30 * time.Second
	defaultContextCatalogTimeout    = 5 * time.Second
)

type contextCatalogFlight struct {
	done chan struct{}
}

type ContextResolverOptions struct {
	Client          *modelinfo.Client
	Binding         modelinfo.Binding
	Model           string
	APIKey          string
	CatalogProvider string
	BudgetSource    string
	StrictRoute     bool
	Catalog         *catalog.Client
}

func NewContextResolver(opts ContextResolverOptions) (*ContextResolver, error) {
	binding, err := modelinfo.CanonicalBinding(opts.Binding)
	if err != nil {
		return nil, err
	}
	if opts.Catalog == nil {
		opts.Catalog = newDiskOnlyContextCatalog()
	}
	return &ContextResolver{client: opts.Client, binding: binding, model: opts.Model,
		apiKey: opts.APIKey, catalogProvider: opts.CatalogProvider, budgetSource: opts.BudgetSource, strictRoute: opts.StrictRoute, catalog: opts.Catalog,
		catalogNow: time.Now, catalogRetryDelay: defaultContextCatalogRetryDelay, catalogRefreshTimeout: defaultContextCatalogTimeout}, nil
}

// Capture the configured disk snapshot during construction without starting
// catalog.Default's background warmup. Status reads use this captured client;
// only a request boundary may refresh it from the network.
func newDiskOnlyContextCatalog() *catalog.Client {
	if os.Getenv("METIS_CATALOG_DISABLE") == "1" {
		return nil
	}
	home := os.Getenv("METIS_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(userHome, ".metis")
	}
	return catalog.NewClient(home)
}

// Refresh runs only at a request boundary, with no agent locks held. An
// unavailable listing is not a generation failure: retain the last validated
// metadata (or catalog/fallback) and allow endpoints without discovery APIs.
func (r *ContextResolver) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	var metadata modelinfo.Model
	var found bool
	if r.client != nil {
		_, err := r.client.RefreshIfStale(ctx, r.binding, r.apiKey)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return ctx.Err()
		}
		metadata, found = r.client.Get(r.binding, r.model)
	}
	// ID-only, partial, unavailable discovery and explicitly profile-shaped
	// responses all rely on independently cached total model capacity. Their
	// known stale records need the same refresh policy. Ordinary served totals
	// remain authoritative and do not trigger unrelated catalog lookups.
	if !found || metadata.ContextWindow <= 0 || metadata.WindowKind == modelinfo.WindowKindClientProfile {
		if err := r.refreshProfileCatalog(ctx, metadata); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (r *ContextResolver) refreshProfileCatalog(ctx context.Context, metadata modelinfo.Model) error {
	if r.catalog == nil {
		return nil
	}
	if match, known := r.lookupProfileLimits(metadata); known && !match.Stale {
		return nil
	}
	r.catalogMu.Lock()
	if err := ctx.Err(); err != nil {
		r.catalogMu.Unlock()
		return err
	}
	flight := r.catalogFlight
	var fetchCtx context.Context
	var cancel context.CancelFunc
	if flight == nil {
		now := time.Now()
		if r.catalogNow != nil {
			now = r.catalogNow()
		}
		if now.Before(r.catalogRetryAt) {
			r.catalogMu.Unlock()
			return nil
		}
		flight = &contextCatalogFlight{done: make(chan struct{})}
		r.catalogFlight = flight
		timeout := r.catalogRefreshTimeout
		if timeout <= 0 {
			timeout = defaultContextCatalogTimeout
		}
		// Individual callers can cancel their wait. A shared bounded request
		// still completes for other callers and cannot inherit their cancellation.
		fetchCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), timeout)
	}
	r.catalogMu.Unlock()
	if cancel != nil {
		go r.runProfileCatalogRefresh(fetchCtx, cancel, flight, metadata)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-flight.done:
		return ctx.Err()
	}
}

func (r *ContextResolver) runProfileCatalogRefresh(ctx context.Context, cancel context.CancelFunc, flight *contextCatalogFlight, metadata modelinfo.Model) {
	defer cancel()
	_, err := r.catalog.Get(ctx)
	match, found := r.lookupProfileLimits(metadata)
	// Get can return an offline disk snapshot with nil error. Only a fresh
	// independently matched record counts as a successful capability refresh.
	fresh := err == nil && !r.catalog.Stat().Stale && found && !match.Stale
	r.catalogMu.Lock()
	if fresh {
		r.catalogRetryAt = time.Time{}
	} else {
		now := time.Now()
		if r.catalogNow != nil {
			now = r.catalogNow()
		}
		delay := r.catalogRetryDelay
		if delay <= 0 {
			delay = defaultContextCatalogRetryDelay
		}
		// A brief cooldown prevents an unavailable catalog from adding a
		// five-second network wait to every request while retaining old limits.
		r.catalogRetryAt = now.Add(delay)
	}
	r.catalogFlight = nil
	close(flight.done)
	r.catalogMu.Unlock()
}

// Resolve is memory-only. Served-model limits remain authoritative, but a
// recognized Codex client profile is a recommendation, not a capacity limit.
// Its default cannot replace independently known full-model catalog limits.
func (r *ContextResolver) Resolve(configuredWindow int, fallback func() int) (resolved ContextCapabilities) {
	if r == nil {
		return ResolveContextCapabilities(configuredWindow, "", "", false, fallback)
	}
	defer func() { resolved = ApplyContextBudgetSource(resolved, r.budgetSource) }()
	capabilities := resolveCachedCatalogCapabilities(r.catalog, r.catalogProvider, r.model, r.strictRoute, fallback)
	if r.client == nil {
		return configuredCapabilities(capabilities, configuredWindow)
	}
	metadata, ok := r.client.Get(r.binding, r.model)
	if !ok {
		return configuredCapabilities(capabilities, configuredWindow)
	}
	capabilities.DefaultWindow = metadata.ContextWindow
	if metadata.WindowKind == modelinfo.WindowKindClientProfile {
		capabilities.DefaultWindow = 0
		if match, found := r.lookupProfileLimits(metadata); found {
			capabilities = catalogContextCapabilities(match)
		}
		capabilities.ProfileDefaultWindow = metadata.ContextWindow
		capabilities.ProfileMaxWindow = metadata.MaxContextWindow
		if capabilities.Source == "catalog_route" || capabilities.Source == "catalog_model" || capabilities.Source == "catalog_issuer" {
			if capabilities.Source == "catalog_issuer" {
				capabilities.Source = "catalog_issuer_profile"
			} else {
				capabilities.Source = "catalog_profile"
			}
			capabilities.MaxInputTokens = constrainedLimit(capabilities.MaxInputTokens, metadata.MaxInputTokens)
			capabilities.MaxOutputTokens = constrainedLimit(capabilities.MaxOutputTokens, metadata.MaxOutputTokens)
		} else {
			// Unknown models keep the recommendation as a conservative budget;
			// the profile maximum must not be invented as a measured capacity.
			if metadata.ContextWindow > 0 {
				capabilities.Window = metadata.ContextWindow
			}
			capabilities.MaxWindow = 0
			capabilities.LimitSource = ""
			capabilities.MaxInputTokens = metadata.MaxInputTokens
			capabilities.MaxOutputTokens = metadata.MaxOutputTokens
			capabilities.Source = metadata.Source
		}
		return associatedProfileCapabilities(capabilities, configuredWindow, metadata.AssocLimits)
	}
	if metadata.ContextWindow > 0 {
		ceiling := metadata.MaxContextWindow
		if ceiling <= 0 {
			ceiling = metadata.ContextWindow
		}
		capabilities.MaxWindow = constrainedLimit(capabilities.MaxWindow, ceiling)
		capabilities.WindowKind = ContextWindowTotal
		capabilities.MaxInputTokens = constrainedLimit(capabilities.MaxInputTokens, metadata.MaxInputTokens)
		capabilities.MaxOutputTokens = constrainedLimit(capabilities.MaxOutputTokens, metadata.MaxOutputTokens)
		if capabilities.ModelWindow > 0 {
			// A known full model may use the route's genuine expanded capacity.
			// The endpoint default remains visible, but is not a smaller auto
			// policy when an explicit serving maximum proves expansion is valid.
			capabilities.Window = min(capabilities.ModelWindow, ceiling)
			if ceiling < capabilities.ModelWindow {
				capabilities.Source = metadata.Source
				capabilities.LimitSource = metadata.Source
			}
		} else {
			// A route maximum is a serving constraint, not independent evidence
			// of the model's complete physical capacity. Without such evidence,
			// retain the endpoint's conservative working default.
			capabilities.Window = metadata.ContextWindow
			capabilities.Source = metadata.Source
			capabilities.LimitSource = metadata.Source
		}
	} else {
		if metadata.MaxContextWindow > 0 {
			capabilities.MaxWindow = constrainedLimit(capabilities.MaxWindow, metadata.MaxContextWindow)
			capabilities.LimitSource = metadata.Source
			if capabilities.Window > metadata.MaxContextWindow {
				capabilities.Window = metadata.MaxContextWindow
				capabilities.Source = "endpoint_max"
			}
		}
		if metadata.MaxInputTokens > 0 {
			capabilities.MaxInputTokens = constrainedLimit(capabilities.MaxInputTokens, metadata.MaxInputTokens)
		}
		if metadata.MaxOutputTokens > 0 {
			capabilities.MaxOutputTokens = constrainedLimit(capabilities.MaxOutputTokens, metadata.MaxOutputTokens)
		}
		// Input-only metadata is an input budget, not a total context window.
		// Its independent output ceiling remains separate and its total model
		// capacity stays unknown until another source actually reports one.
		if metadata.MaxInputTokens > 0 && metadata.MaxContextWindow <= 0 && capabilities.ModelWindow <= 0 {
			capabilities.Window = metadata.MaxInputTokens
			capabilities.WindowKind = ContextWindowInput
			capabilities.Source = metadata.Source
			capabilities.LimitSource = metadata.Source
		}
	}
	return configuredCapabilities(capabilities, configuredWindow)
}

// Associated ordinary records constrain a client recommendation only when
// discovery retained explicit capacity evidence. Profile numbers remain
// unchanged. An explicit operating budget may expand to a reported maximum,
// but cannot enlarge a proven physical ceiling.
func associatedProfileCapabilities(capabilities ContextCapabilities, configuredWindow int, limits modelinfo.AssociatedLimits) ContextCapabilities {
	if limits.Source != modelinfo.SourceEndpointModels ||
		(limits.Evidence != modelinfo.AssocEvidenceCapacityWindow && limits.Evidence != modelinfo.AssocEvidenceDeclaredServed) {
		return configuredCapabilities(capabilities, configuredWindow)
	}
	beforeWindow, beforeMaximum := capabilities.Window, capabilities.MaxWindow
	if limits.ContextWindow > 0 {
		capabilities.DefaultWindow = limits.ContextWindow
		capabilities.Window = constrainedLimit(capabilities.Window, limits.ContextWindow)
	}
	ceiling := limits.MaxContextWindow
	if ceiling <= 0 {
		ceiling = limits.ContextWindow
	}
	if ceiling > 0 {
		capabilities.MaxWindow = constrainedLimit(capabilities.MaxWindow, ceiling)
		capabilities.LimitSource = "endpoint_associated_capacity"
		// A known output ceiling cannot exceed the served total window. Keep
		// unknown input/output fields unknown instead of manufacturing limits.
		if capabilities.MaxOutputTokens > ceiling {
			capabilities.MaxOutputTokens = ceiling
		}
	}
	if limits.Evidence == modelinfo.AssocEvidenceDeclaredServed {
		capabilities.MaxInputTokens = constrainedLimit(capabilities.MaxInputTokens, limits.MaxInputTokens)
		capabilities.MaxOutputTokens = constrainedLimit(capabilities.MaxOutputTokens, limits.MaxOutputTokens)
	}
	if capabilities.Window != beforeWindow || capabilities.MaxWindow != beforeMaximum {
		capabilities.Source = "endpoint_associated_capacity"
	}
	capabilities = configuredCapabilities(capabilities, configuredWindow)
	if capabilities.Source == "configured_clamped" && ceiling > 0 {
		// Retain the established associated-evidence provenance while the
		// requested budget and limiting source make the clamp explicit.
		capabilities.Source = "endpoint_associated_capacity"
	}
	return capabilities
}

// A model issuer can disambiguate an unknown proxy's client recommendation.
// It never substitutes for a known route, an explicit mapping, or an ordinary
// endpoint capacity. The catalog verifies issuer identities and exact models;
// selection IDs and capabilities from other wire aliases remain independent.
func (r *ContextResolver) lookupProfileLimits(metadata modelinfo.Model) (catalog.LimitsMatch, bool) {
	client := r.catalog
	if client == nil {
		return catalog.LimitsMatch{}, false
	}
	if match, found := client.LookupRouteOrUnambiguousLimits(r.catalogProvider, r.model, r.strictRoute); found {
		return match, true
	}
	if metadata.WindowKind != modelinfo.WindowKindClientProfile {
		return catalog.LimitsMatch{}, false
	}
	return client.LookupIssuerLimits(r.catalogProvider, r.model, metadata.OwnedBy, r.strictRoute)
}

// ResolveContextCapabilities is the cached catalog compatibility path for
// transports instantiated without endpoint discovery. Explicit catalog routes
// remain strict; unknown profile names may use only unanimous exact model IDs.
func ResolveContextCapabilities(configuredWindow int, catalogProvider, model string, strictRoute bool, fallback func() int) ContextCapabilities {
	return configuredCapabilities(resolveCatalogCapabilities(nil, catalogProvider, model, strictRoute, fallback), configuredWindow)
}

func configuredCapabilities(capabilities ContextCapabilities, window int) ContextCapabilities {
	if capabilities.WindowKind == "" {
		capabilities.WindowKind = ContextWindowTotal
	}
	capabilities.BudgetSource = ContextBudgetAutomatic
	if window > 0 {
		capabilities.RequestedWindow = window
		capabilities.BudgetSource = ContextBudgetLegacyProvider
		limit := constrainedLimit(capabilities.MaxWindow, capabilities.ModelWindow)
		if capabilities.WindowKind == ContextWindowInput {
			limit = constrainedLimit(limit, capabilities.MaxInputTokens)
		}
		if limit > 0 && window > limit {
			capabilities.Window, capabilities.Source = limit, "configured_clamped"
		} else {
			capabilities.Window, capabilities.Source = window, "configured"
		}
	}
	return capabilities
}

func constrainedLimit(catalogLimit, endpointLimit int) int {
	if endpointLimit <= 0 {
		return catalogLimit
	}
	if catalogLimit <= 0 {
		return endpointLimit
	}
	return min(catalogLimit, endpointLimit)
}

func resolveCatalogCapabilities(client *catalog.Client, catalogProvider, model string, strictRoute bool, fallback func() int) ContextCapabilities {
	if client == nil {
		client = catalog.Default()
	}
	return resolveCachedCatalogCapabilities(client, catalogProvider, model, strictRoute, fallback)
}

func resolveCachedCatalogCapabilities(client *catalog.Client, catalogProvider, model string, strictRoute bool, fallback func() int) ContextCapabilities {
	if client != nil {
		if match, ok := client.LookupRouteOrUnambiguousLimits(catalogProvider, model, strictRoute); ok {
			return catalogContextCapabilities(match)
		}
	}
	window := 0
	if fallback != nil {
		window = fallback()
	}
	return ContextCapabilities{Window: window, WindowKind: ContextWindowTotal, Source: "fallback"}
}

func catalogContextCapabilities(match catalog.LimitsMatch) ContextCapabilities {
	return ContextCapabilities{Window: match.Limit.Context, WindowKind: ContextWindowTotal,
		MaxWindow: match.Limit.Context, MaxInputTokens: match.Limit.Input,
		MaxOutputTokens: match.Limit.Output, Source: match.Source, LimitSource: match.Source,
		ModelWindow: match.Limit.Context, ModelMaxInputTokens: match.Limit.Input,
		ModelMaxOutputTokens: match.Limit.Output, ModelSource: match.Source}
}
