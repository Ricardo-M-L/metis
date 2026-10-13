package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/azure"
	"github.com/Ricardo-M-L/metis/internal/llm/bedrock"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/llm/gemini"
	"github.com/Ricardo-M-L/metis/internal/llm/modelinfo"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/vertex"
)

// ProviderContextBudgetRoute derives the same generation identity used by
// construction. Settings callers supply only a provider id; endpoints and
// account scopes always come from the current local configuration.
func ProviderContextBudgetRoute(cfg *config.Config, id string) (config.ContextBudgetRoute, error) {
	if cfg == nil {
		return config.ContextBudgetRoute{}, errors.New("context budget: missing provider configuration")
	}
	if id == "google" {
		id = "gemini"
	}
	route := config.ContextBudgetRoute{ProviderID: id}
	switch id {
	case "anthropic":
		route.Transport, route.BaseURL = "anthropic_messages", endpointOrDefault(cfg.Provider.Anthropic.BaseURL, "https://api.anthropic.com")
		if _, err := cfg.ResolveAPIKey(id); errors.Is(err, config.ErrMissingAPIKey) {
			credential, err := auth.GetOAuth(id)
			if err != nil {
				return route, err
			}
			if credential != nil {
				route.AccountScope = credential.AccountID
			}
		}
	case "openai-codex":
		route.Transport, route.BaseURL = "openai_codex_responses", "https://chatgpt.com/backend-api/codex"
		credential, err := auth.GetOAuth(id)
		if err != nil {
			return route, err
		}
		if credential == nil || strings.TrimSpace(credential.AccountID) == "" {
			return route, errors.New("context budget: Codex account is not configured")
		}
		route.AccountScope = credential.AccountID
	case "openai":
		route.Transport, route.BaseURL = "openai_chat", endpointOrDefault(cfg.Provider.OpenAI.BaseURL, "https://api.openai.com/v1")
		if strings.EqualFold(strings.TrimSpace(cfg.Provider.OpenAI.WireProtocol), "responses") {
			route.Transport = "openai_responses"
		}
	case "gemini":
		route.Transport, route.BaseURL = "gemini_native", endpointOrDefault(cfg.Provider.Gemini.BaseURL, "https://generativelanguage.googleapis.com")
	default:
		raw, ok := cfg.Provider.Custom[id]
		if !ok {
			return route, errors.New("context budget: provider is not configured")
		}
		route.Transport = normalizedCustomTransport(raw)
		switch route.Transport {
		case "anthropic_messages", "anthropic":
			route.BaseURL = endpointOrDefault(raw.BaseURL, "https://api.anthropic.com")
		case "openai_chat", "openai", "openai_responses":
			route.BaseURL = endpointOrDefault(raw.BaseURL, "https://api.openai.com/v1")
		case "gemini_native", "gemini":
			route.BaseURL = endpointOrDefault(raw.BaseURL, "https://generativelanguage.googleapis.com")
		case "azure_openai", "azure":
			route.Transport = "azure_openai"
			route.BaseURL = strings.TrimSpace(raw.BaseURL)
			if !strings.HasPrefix(route.BaseURL, "https://") && !strings.HasPrefix(route.BaseURL, "http://") {
				route.BaseURL = "https://" + route.BaseURL + ".openai.azure.com"
			}
		case "vertex_anthropic", "vertex":
			region := endpointOrDefault(raw.Region, endpointOrDefault(raw.BaseURL, "us-central1"))
			if raw.Project == "" {
				return route, errors.New("context budget: Vertex project is missing")
			}
			route.BaseURL = vertexContextRouteURL(region, raw.Project)
		case "bedrock_anthropic", "bedrock":
			region := endpointOrDefault(raw.Region, endpointOrDefault(raw.BaseURL, endpointOrDefault(os.Getenv("AWS_REGION"), "us-east-1")))
			route.BaseURL = bedrockContextRouteURL(region)
		default:
			return route, errors.New("context budget: unsupported provider transport")
		}
	}
	return config.CanonicalContextBudgetRoute(route)
}

func endpointOrDefault(endpoint, fallback string) string {
	if endpoint == "" {
		return fallback
	}
	return endpoint
}

func vertexContextRouteURL(region, project string) string {
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic", region, project, region)
}

func bedrockContextRouteURL(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

func providerBudgetSelection(route config.ContextBudgetRoute, model string, legacy int) (int, string, error) {
	window, found, err := config.LookupContextBudget(route, model)
	if err != nil {
		return 0, "", err
	}
	if found {
		if window == 0 {
			return 0, llm.ContextBudgetAutomatic, nil
		}
		return window, llm.ContextBudgetPerModel, nil
	}
	if legacy > 0 {
		return legacy, llm.ContextBudgetLegacyProvider, nil
	}
	return 0, llm.ContextBudgetAutomatic, nil
}

func validateOAuthContextBudgetAccount(authProvider string, expectedRoute config.ContextBudgetRoute) error {
	credential, err := auth.GetOAuth(authProvider)
	if err != nil || credential == nil {
		return fmt.Errorf("%w: OAuth account unavailable; rebuild the provider", llm.ErrContextBudgetPolicy)
	}
	currentRoute := expectedRoute
	currentRoute.AccountScope = credential.AccountID
	currentRoute, err = config.CanonicalContextBudgetRoute(currentRoute)
	if err != nil || currentRoute.AccountScope != expectedRoute.AccountScope {
		return fmt.Errorf("%w: OAuth account changed; rebuild the provider", llm.ErrContextBudgetPolicy)
	}
	return nil
}

// bindProviderContext installs cached metadata without making a network call.
// The first agent request refreshes discovery before tool sizing/compaction.
// Subscription and cloud transports keep their own capability/auth contracts.
func bindProviderContext(provider llm.Provider, id, transportName string, strictRoute bool) error {
	var baseURL, key, model, route, authKind string
	var oauthProvider string
	var legacyWindow int
	budgetRoute := config.ContextBudgetRoute{ProviderID: id, Transport: strings.ToLower(strings.TrimSpace(transportName))}
	switch p := provider.(type) {
	case *anthropic.Anthropic:
		p.CatalogProviderStrict = strictRoute
		baseURL, key, model, route, authKind = p.BaseURL, p.APIKey, p.Model, p.CatalogProvider, modelinfo.AuthAPIKey
		legacyWindow = p.ContextWindow
		if p.OAuthTokenSource != nil {
			oauthProvider = "anthropic"
			credential, err := auth.GetOAuth("anthropic")
			if err != nil {
				return err
			}
			if credential != nil {
				budgetRoute.AccountScope = credential.AccountID
			}
		}
		if id != "anthropic" {
			authKind = modelinfo.AuthAPIKeyBearer
		}
	case *openai.OpenAI:
		p.CatalogProviderStrict = strictRoute
		baseURL, key, model, route, authKind = p.BaseURL, p.APIKey, p.Model, p.CatalogProvider, modelinfo.AuthBearer
		legacyWindow = p.ContextWindow
	case *openai.Responses:
		p.CatalogProviderStrict = strictRoute
		baseURL, key, model, route, authKind = p.BaseURL, p.APIKey, p.Model, p.CatalogProvider, modelinfo.AuthBearer
		legacyWindow = p.ContextWindow
		if p.OAuthTokenSource != nil || p.Name() == "openai-codex" {
			oauthProvider = "openai-codex"
			budgetRoute.Transport = "openai_codex_responses"
			credential, err := auth.GetOAuth("openai-codex")
			if err != nil {
				return err
			}
			if credential != nil {
				budgetRoute.AccountScope = credential.AccountID
			}
		}
	case *gemini.Gemini:
		baseURL, key, model, legacyWindow = p.BaseURL, p.APIKey, p.Model, p.ContextWindow
	case *azure.Azure:
		baseURL, model, legacyWindow = p.BaseURL, p.Model, p.ContextWindow
		budgetRoute.Transport = "azure_openai"
	case *vertex.Vertex:
		baseURL, model, legacyWindow = vertexContextRouteURL(p.Region, p.Project), p.Model, p.ContextWindow
	case *bedrock.Bedrock:
		baseURL, model, legacyWindow = bedrockContextRouteURL(p.Region), p.Model, p.ContextWindow
	default:
		return nil
	}
	budgetRoute.BaseURL = baseURL
	window, budgetSource, err := providerBudgetSelection(budgetRoute, model, legacyWindow)
	if err != nil {
		return fmt.Errorf("bind context budget for provider %q: %w", id, err)
	}
	canonicalRoute, err := config.CanonicalContextBudgetRoute(budgetRoute)
	if err != nil {
		return err
	}
	routeKey, err := config.ContextBudgetRouteKey(canonicalRoute, model)
	if err != nil {
		return err
	}
	budgetPolicy := llm.NewContextBudgetPolicy(llm.ContextBudgetPolicyOptions{
		Owner:        llm.ContextBudgetOwner{ProviderID: canonicalRoute.ProviderID, Model: model, RouteKey: routeKey},
		LegacyWindow: legacyWindow, InitialWindow: window, InitialSource: budgetSource,
		Lookup: func(ctx context.Context) (int, bool, error) {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
			if oauthProvider != "" {
				if err := validateOAuthContextBudgetAccount(oauthProvider, canonicalRoute); err != nil {
					return 0, false, err
				}
			}
			return config.LookupContextBudget(canonicalRoute, model)
		},
	})
	switch p := provider.(type) {
	case *anthropic.Anthropic:
		p.ContextWindow, p.ContextBudgetSource = window, budgetSource
		p.ContextBudgetPolicy = budgetPolicy
	case *openai.OpenAI:
		p.ContextWindow, p.ContextBudgetSource = window, budgetSource
		p.ContextBudgetPolicy = budgetPolicy
	case *openai.Responses:
		p.ContextWindow, p.ContextBudgetSource = window, budgetSource
		p.ContextBudgetPolicy = budgetPolicy
	case *gemini.Gemini:
		p.ContextWindow, p.ContextBudgetSource = window, budgetSource
		p.ContextBudgetPolicy = budgetPolicy
	case *azure.Azure:
		p.ContextWindow = window
		p.ContextBudgetPolicy = budgetPolicy
		return nil
	case *vertex.Vertex:
		p.ContextWindow = window
		p.ContextBudgetPolicy = budgetPolicy
		return nil
	case *bedrock.Bedrock:
		p.ContextWindow = window
		p.ContextBudgetPolicy = budgetPolicy
		return nil
	}
	var client *modelinfo.Client
	if oauthProvider == "" {
		client, err = modelinfo.New(filepath.Join(config.Home(), "cache", "provider-models"))
		if err != nil {
			client, _ = modelinfo.New("")
		}
	}
	modelCatalog := catalog.NewClient(config.Home())
	if os.Getenv("METIS_CATALOG_DISABLE") == "1" {
		modelCatalog = catalog.NewStaticClient(catalog.Catalog{})
	}
	if p, ok := provider.(*openai.Responses); ok && (p.OAuthTokenSource != nil || p.Name() == "openai-codex") {
		p.CodexContextResolver = openai.NewCodexContextResolver(openai.CodexContextResolverOptions{
			TokenSource: p.OAuthTokenSource, AccountScope: budgetRoute.AccountScope,
			CacheDir: filepath.Join(config.Home(), "cache", "codex-context"), Catalog: modelCatalog,
		})
		return nil
	}
	if p, ok := provider.(*gemini.Gemini); ok {
		p.ContextDiscovery = gemini.NewContextDiscovery(gemini.ContextDiscoveryOptions{
			BaseURL: p.BaseURL, Model: p.Model, APIKey: p.APIKey, CacheDir: filepath.Join(config.Home(), "cache", "gemini-context"),
		})
		return nil
	}
	resolver, err := llm.NewContextResolver(llm.ContextResolverOptions{
		Client: client, Catalog: modelCatalog, Binding: modelinfo.Binding{ProviderID: id, Transport: transportName, BaseURL: baseURL, AuthKind: authKind},
		Model: model, APIKey: key, CatalogProvider: route, StrictRoute: strictRoute,
		BudgetSource: budgetSource,
	})
	if err != nil {
		return nil
	}
	switch p := provider.(type) {
	case *anthropic.Anthropic:
		p.ContextResolver = resolver
	case *openai.OpenAI:
		p.ContextResolver = resolver
	case *openai.Responses:
		p.ContextResolver = resolver
	}
	return nil
}

// Metadata wrappers are selected only when the wrapped provider implements the
// optional interface. Vision overrides must not erase discovery or fabricate
// history-policy support for transports that do not have it.
type visionOverrideContextProvider struct {
	visionOverrideProvider
	contextProvider llm.ContextCapabilitiesProvider
}

func (p visionOverrideContextProvider) ContextCapabilities() llm.ContextCapabilities {
	return p.contextProvider.ContextCapabilities()
}
func (p visionOverrideContextProvider) RefreshContextCapabilities(ctx context.Context) error {
	return p.contextProvider.RefreshContextCapabilities(ctx)
}

type visionOverrideContextHistoryProvider struct {
	visionOverrideContextProvider
	historyPolicy llm.ContextHistoryPolicy
}

func (p visionOverrideContextHistoryProvider) ContextIncludesAssistantBlock(block llm.ContentBlock) bool {
	return p.historyPolicy.ContextIncludesAssistantBlock(block)
}
