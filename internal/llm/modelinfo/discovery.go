package modelinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes    = 2 << 20
	maxModels           = 1000
	maxCapabilityTokens = 100_000_000
)

type options struct {
	timeout    time.Duration
	ttl        time.Duration
	httpClient *http.Client
}

type Option func(*options)

func WithTimeout(timeout time.Duration) Option { return func(o *options) { o.timeout = timeout } }
func WithTTL(ttl time.Duration) Option         { return func(o *options) { o.ttl = ttl } }

// WithHTTPClient is intended for custom TLS transports and tests. Redirects and
// the overall discovery deadline are still enforced by this package.
func WithHTTPClient(client *http.Client) Option { return func(o *options) { o.httpClient = client } }

func capabilityHTTPClient(custom *http.Client, timeout time.Duration) *http.Client {
	c := &http.Client{}
	if custom != nil {
		*c = *custom
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = func(req *http.Request) (*url.URL, error) {
			host := req.URL.Hostname()
			if strings.EqualFold(host, "localhost") {
				return nil, nil
			}
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return nil, nil
			}
			return http.ProxyFromEnvironment(req)
		}
		t.ResponseHeaderTimeout = timeout
		c.Transport = t
	}
	c.Timeout = timeout
	c.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// Discover asks the selected endpoint for capabilities. It does not update
// state or persist credentials. Missing default windows in a valid rich catalog
// are enriched from the ordinary models route under the same total deadline.
func (c *Client) Discover(ctx context.Context, binding Binding, key string) ([]Model, error) {
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.discover(bounded, ctx, binding, key)
}

func (c *Client) discover(ctx, callerCtx context.Context, binding Binding, key string) ([]Model, error) {
	b, _, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(key, "\r\n") {
		return nil, ErrInvalidBinding
	}
	models, unsupported, err := c.fetch(ctx, b, key, true)
	if err != nil {
		return nil, err
	}
	if !unsupported && allHaveContextWindow(models) && !needsProfileDiscovery(models) {
		return models, nil
	}
	plain, _, err := c.fetch(ctx, b, key, false)
	if err != nil {
		if callerCtx.Err() != nil {
			return nil, callerCtx.Err()
		}
		if hasCapabilities(models) {
			return models, nil
		}
		return nil, err
	}
	merged, err := mergeCapabilities(models, plain)
	if err != nil && hasCapabilities(models) {
		return models, nil
	}
	return merged, err
}

func needsProfileDiscovery(models []Model) bool {
	for _, m := range models {
		if m.WindowKind == WindowKindClientProfile {
			return true
		}
	}
	return false
}

func allHaveContextWindow(models []Model) bool {
	if len(models) == 0 {
		return false
	}
	for _, m := range models {
		if m.ContextWindow <= 0 {
			return false
		}
	}
	return true
}

func mergeCapabilities(rich, plain []Model) ([]Model, error) {
	models := append([]Model(nil), rich...)
	byID := make(map[string]int, len(models))
	richIDs := make(map[string]bool, len(models))
	ownerConflicts := make(map[string]bool)
	for i, m := range models {
		byID[m.ID] = i
		richIDs[m.ID] = true
	}
	for _, fallback := range plain {
		i, exists := byID[fallback.ID]
		if !exists {
			byID[fallback.ID] = len(models)
			models = append(models, fallback)
			continue
		}
		m := models[i]
		if m.OwnedBy != "" && fallback.OwnedBy != "" && m.OwnedBy != fallback.OwnedBy {
			m.OwnedBy, m.OwnerSource, m.OwnerModelID = "", "", ""
			ownerConflicts[m.ID] = true
		} else if m.OwnedBy == "" && fallback.OwnedBy != "" {
			m.OwnedBy, m.OwnerSource, m.OwnerModelID = fallback.OwnedBy, fallback.OwnerSource, fallback.OwnerModelID
		}
		if m.WindowKind == WindowKindClientProfile {
			m.AssocLimits = associatedLimits(fallback)
		}
		if m.LimitEvidence == "" {
			m.LimitEvidence = fallback.LimitEvidence
		}
		hadRich := hasCapabilities([]Model{m})
		contributed := false
		// The richer client-specific catalog is authoritative when both routes
		// advertise the same field. Ordinary discovery fills missing values;
		// ID-only data cannot erase existing endpoint capabilities.
		fill := func(dst *int, src int) {
			if *dst == 0 && src > 0 {
				*dst = src
				contributed = true
			}
		}
		// Window policies and capacities have different semantics. Do not turn
		// an ordinary route capacity into a full capacity by mixing in profile
		// windows. If no window was reported, adopt the donor's semantics.
		hadWindow := m.ContextWindow > 0 || m.MaxContextWindow > 0
		windowKindsMatch := (m.WindowKind == WindowKindClientProfile) == (fallback.WindowKind == WindowKindClientProfile)
		if !hadWindow || windowKindsMatch {
			fill(&m.ContextWindow, fallback.ContextWindow)
			fill(&m.MaxContextWindow, fallback.MaxContextWindow)
			if !hadWindow && (fallback.ContextWindow > 0 || fallback.MaxContextWindow > 0) {
				m.WindowKind = fallback.WindowKind
			}
		}
		fill(&m.MaxInputTokens, fallback.MaxInputTokens)
		fill(&m.MaxOutputTokens, fallback.MaxOutputTokens)
		if m.Name == "" {
			m.Name = fallback.Name
		}
		if m.DisplayName == "" {
			m.DisplayName = fallback.DisplayName
		}
		m.aliasAmbiguous = m.aliasAmbiguous || fallback.aliasAmbiguous
		if contributed {
			if hadRich {
				m.Source = SourceEndpointMerged
			} else {
				m.Source = SourceEndpointModels
			}
		}
		models[i] = m
	}
	// A display label by itself is not model identity. Only a recognized
	// client-profile ID and one unique original display_name on this same
	// endpoint can associate an issuer or evidence-bearing serving constraints.
	// Bare token fields never cross alias IDs.
	for i, m := range models {
		if !richIDs[m.ID] || m.WindowKind != WindowKindClientProfile || ownerConflicts[m.ID] || m.aliasAmbiguous {
			continue
		}
		candidate := Model{}
		matches := 0
		for _, p := range plain {
			if p.DisplayName == m.ID {
				matches++
				candidate = p
			}
		}
		if matches != 1 || candidate.OwnedBy == "" || candidate.aliasAmbiguous || richIDs[candidate.ID] {
			continue
		}
		if m.OwnedBy != "" && m.OwnedBy != candidate.OwnedBy {
			continue
		}
		if m.OwnedBy == "" {
			m.OwnedBy, m.OwnerSource, m.OwnerModelID = candidate.OwnedBy, candidate.OwnerSource, candidate.ID
		}
		if m.AssocLimits == (AssociatedLimits{}) {
			m.AssocLimits = associatedLimits(candidate)
		}
		models[i] = m
	}
	// Individually valid responses can still conflict when a fallback default
	// exceeds the rich catalog's explicit maximum. Never publish that mixture.
	if len(models) > maxModels {
		return nil, ErrTooManyModels
	}
	if !validCachedModels(models) {
		return nil, ErrInvalidResponse
	}
	sortModels(models)
	return models, nil
}

func associatedLimits(peer Model) AssociatedLimits {
	if peer.WindowKind == WindowKindClientProfile || peer.Source != SourceEndpointModels {
		return AssociatedLimits{}
	}
	limits := AssociatedLimits{ContextWindow: peer.ContextWindow, MaxContextWindow: peer.MaxContextWindow, Source: SourceEndpointModels, ModelID: peer.ID}
	if peer.LimitEvidence == LimitEvidenceServedCapacity {
		limits.Evidence = AssocEvidenceDeclaredServed
		limits.MaxInputTokens, limits.MaxOutputTokens = peer.MaxInputTokens, peer.MaxOutputTokens
	} else if peer.ContextWindow > 0 || peer.MaxContextWindow > 0 {
		limits.Evidence = AssocEvidenceCapacityWindow
	} else {
		return AssociatedLimits{}
	}
	if !validAssociatedLimits(limits) {
		return AssociatedLimits{}
	}
	return limits
}

func hasCapabilities(models []Model) bool {
	for _, m := range models {
		if m.ContextWindow > 0 || m.MaxContextWindow > 0 || m.MaxInputTokens > 0 || m.MaxOutputTokens > 0 {
			return true
		}
	}
	return false
}

func (c *Client) fetch(ctx context.Context, b Binding, key string, rich bool) ([]Model, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL(b, rich), nil)
	if err != nil {
		return nil, false, ErrInvalidBinding
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		if b.AuthKind == AuthAPIKey || b.AuthKind == AuthAPIKeyBearer {
			req.Header.Set("X-Api-Key", key)
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		if b.AuthKind != AuthAPIKey {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if boundedTimeout(err) {
			return nil, false, context.DeadlineExceeded
		}
		// net/http errors may contain the URL. Do not forward endpoint details.
		return nil, false, errors.New("model discovery: endpoint request failed")
	}
	defer resp.Body.Close()
	if rich && unsupportedStatus(resp.StatusCode) {
		return nil, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, errors.New("model discovery: endpoint HTTP status " + strconv.Itoa(resp.StatusCode))
	}
	if resp.ContentLength > maxResponseBytes {
		return nil, false, ErrResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if boundedTimeout(err) {
			return nil, false, context.DeadlineExceeded
		}
		return nil, false, errors.New("model discovery: endpoint body read failed")
	}
	if len(body) > maxResponseBytes {
		return nil, false, ErrResponseTooLarge
	}
	source := SourceEndpointModels
	if rich {
		source = SourceEndpointPI
	}
	models, err := parseModels(body, source)
	return models, false, err
}

func boundedTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout())
}

func unsupportedStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented || status == http.StatusUnprocessableEntity
}

// Parse validates an already-fetched capability response using the same rules
// as Discover. It accepts only endpoint sources and does not retain the body.
func Parse(data []byte, source string) ([]Model, error) {
	if len(data) > maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	if source != SourceEndpointPI && source != SourceEndpointModels {
		return nil, ErrInvalidResponse
	}
	return parseModels(data, source)
}

func parseModels(body []byte, source string) ([]Model, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var data any
	if err := dec.Decode(&data); err != nil {
		return nil, ErrInvalidResponse
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, ErrInvalidResponse
	}
	var records []any
	var profileBeforeAliases map[string]bool
	switch value := data.(type) {
	case []any:
		records = value
	case map[string]any:
		var list any
		var exists bool
		var listField string
		for _, k := range []string{"models", "data"} {
			if list, exists = value[k]; exists {
				listField = k
				break
			}
		}
		if !exists {
			return nil, ErrInvalidResponse
		}
		switch list := list.(type) {
		case []any:
			records = list
		case map[string]any:
			// OpenAI/Anthropic data is an array. A keyed models object is a
			// separate endpoint schema whose keys are callable route aliases.
			if listField != "models" {
				return nil, ErrInvalidResponse
			}
			if len(list) > maxModels {
				return nil, ErrTooManyModels
			}
			profileBeforeAliases = make(map[string]bool, len(list))
			for id, value := range list {
				record, ok := value.(map[string]any)
				if !ok {
					return nil, ErrInvalidResponse
				}
				// A synthetic slug used to preserve a callable alias is not
				// evidence that the original record was a Codex client profile.
				profileBeforeAliases[id] = codexClientProfile(record)
				// Preserve the map key as the selected route ID even when the
				// value names a different upstream model behind that alias.
				record["slug"] = id
				records = append(records, record)
			}
		default:
			return nil, ErrInvalidResponse
		}
	default:
		return nil, ErrInvalidResponse
	}
	if len(records) > maxModels {
		return nil, ErrTooManyModels
	}
	seen := map[string]Model{}
	models := make([]Model, 0, len(records))
	for _, item := range records {
		record, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalidResponse
		}
		m, err := parseModel(record, source)
		if err != nil {
			return nil, err
		}
		if profileBeforeAliases != nil && !profileBeforeAliases[m.ID] {
			m.WindowKind = ""
		}
		if previous, found := seen[m.ID]; found {
			compare := previous
			compare.aliasAmbiguous = false
			if compare != m {
				return nil, ErrInvalidResponse
			}
			previous.aliasAmbiguous = true
			seen[m.ID] = previous
			for i := range models {
				if models[i].ID == m.ID {
					models[i].aliasAmbiguous = true
					break
				}
			}
			continue
		}
		seen[m.ID] = m
		models = append(models, m)
	}
	return models, nil
}

func parseModel(record map[string]any, source string) (Model, error) {
	m := Model{Source: source}
	for _, field := range []string{"slug", "id"} {
		if value, present := record[field]; present {
			id, ok := value.(string)
			if !ok || !validLabel(id, 512) {
				return Model{}, ErrInvalidResponse
			}
			// Pi sometimes reports a separate wire ID. Its slug is the selection
			// identifier, so prefer slug when both are supplied.
			if m.ID == "" {
				m.ID = id
			}
		}
	}
	if m.ID == "" {
		return Model{}, ErrInvalidResponse
	}
	for _, field := range []string{"display_name", "name"} {
		if value, present := record[field]; present && value != nil {
			name, ok := value.(string)
			if !ok || !validLabel(name, 512) {
				return Model{}, ErrInvalidResponse
			}
			m.Name = name
			if field == "display_name" {
				m.DisplayName = name
			}
			break
		}
	}
	if value, exists := record["owned_by"]; exists && value != nil {
		owner, ok := value.(string)
		if !ok {
			return Model{}, ErrInvalidResponse
		}
		if owner != "" {
			if !validLabel(owner, 256) {
				return Model{}, ErrInvalidResponse
			}
			m.OwnedBy, m.OwnerSource, m.OwnerModelID = owner, source, m.ID
		}
	}
	if value, ok := record["limit_evidence"].(string); ok && value == LimitEvidenceServedCapacity {
		m.LimitEvidence = value
	}
	var err error
	m.ContextWindow, err = capability(record,
		"context_window", "contextWindow", "context_length", "contextLength",
		"limit.context", "limits.context", "limits.context_window", "limits.context_length",
		"context.window", "context.context_window", "context.length", "context")
	if err != nil {
		return Model{}, err
	}
	m.MaxContextWindow, err = capability(record,
		"max_context_window", "maxContextWindow", "max_context_length", "limits.max_context_window", "limit.max_context", "context.max_context_window")
	if err != nil {
		return Model{}, err
	}
	// Input ceilings constrain the prompt independently of the total/default
	// window. They are not aliases for total context, and cannot establish a
	// total window by adding a separately advertised output limit.
	m.MaxInputTokens, err = capability(record,
		"max_input_tokens", "maxInputTokens", "input_token_limit", "inputTokenLimit", "max_input",
		"limit.input", "limit.max_input_tokens", "limits.input", "limits.input_tokens", "limits.max_input_tokens",
		"context.input_token_limit", "context.max_input_tokens", "top_provider.max_input_tokens")
	if err != nil {
		return Model{}, err
	}
	m.MaxOutputTokens, err = capability(record,
		"max_output_tokens", "maxOutputTokens", "max_tokens", "maxTokens", "max_completion_tokens", "output_token_limit",
		"limit.output", "limit.max_tokens", "limits.output", "limits.max_output_tokens", "limits.output_tokens",
		"context.output_token_limit", "top_provider.max_completion_tokens")
	if err != nil {
		return Model{}, err
	}
	if m.ContextWindow > 0 && m.MaxContextWindow > 0 && m.ContextWindow > m.MaxContextWindow {
		return Model{}, ErrInvalidResponse
	}
	if codexClientProfile(record) {
		m.WindowKind = WindowKindClientProfile
	}
	return m, nil
}

// codexClientProfile requires the complete original client-profile schema.
// A route name, Pi source, slug alone, or a particular window number does not
// prove that a small context limit is a client policy rather than capacity.
func codexClientProfile(record map[string]any) bool {
	slug, ok := record["slug"].(string)
	if !ok || !validLabel(slug, 512) {
		return false
	}
	if _, ok := record["supported_in_api"].(bool); !ok {
		return false
	}
	if _, ok := record["supports_experimental_context"].(bool); !ok {
		return false
	}
	minimum, ok := record["minimal_client_version"].(string)
	if !ok || !validLabel(minimum, 256) {
		return false
	}
	// Codex's native schema reports JSON numbers, not stringified limits.
	if _, ok := record["context_window"].(json.Number); !ok {
		return false
	}
	if _, ok := record["max_context_window"].(json.Number); !ok {
		return false
	}
	window, err := capability(record, "context_window")
	if err != nil || window <= 0 {
		return false
	}
	maximum, err := capability(record, "max_context_window")
	if err != nil || maximum < window {
		return false
	}
	return true
}

func capability(record map[string]any, fields ...string) (int, error) {
	result := 0
	for _, path := range fields {
		var value any = record
		present := true
		for _, field := range strings.Split(path, ".") {
			object, ok := value.(map[string]any)
			if !ok {
				present = false
				break
			}
			value, present = object[field]
			if !present {
				break
			}
		}
		if !present || value == nil {
			continue
		}
		if path == "context" {
			if _, nested := value.(map[string]any); nested {
				continue
			}
		}
		var text string
		switch n := value.(type) {
		case json.Number:
			text = string(n)
		case string:
			text = n
		default:
			return 0, ErrInvalidResponse
		}
		if text == "" || strings.Trim(text, "0123456789") != "" {
			return 0, ErrInvalidResponse
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n <= 0 || n > maxCapabilityTokens {
			return 0, ErrInvalidResponse
		}
		if result != 0 && result != int(n) {
			return 0, ErrInvalidResponse
		}
		result = int(n)
	}
	return result, nil
}
