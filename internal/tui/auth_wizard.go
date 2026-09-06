package tui

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

// ErrAuthCancelled is returned by RunAuthWizard when the user aborts (Ctrl-C / Esc)
// before selecting a provider AND entering a key. setupRuntime treats it as a fatal
// "user said no" — we don't want to silently start chat with no key.
var ErrAuthCancelled = errors.New("auth wizard cancelled")

// authProvider is one entry in the provider picker.
//
// `id` is the stable provider id used by the credential/runtime layers.
// `label` is what the user sees. `placeholder` is the example shown above
// the API-key field — purely cosmetic but it doubles as a "this is what
// the key looks like" sanity hint.
type authProvider struct {
	id          string
	label       string
	keyHelp     string
	placeholder string
	methods     []authMethod
	custom      *CustomProviderResult
}

// authMethod describes one way a provider can authenticate. Keep the values
// aligned with the CLI's --method flag; they are deliberately transport-agnostic
// so adding another OAuth-backed provider does not grow another wizard.
type authMethod struct {
	id    string
	label string
	note  string
}

type oauthFlowOption struct {
	id    string
	label string
	note  string
}

const (
	AuthMethodAPIKey = "api-key"
	AuthMethodOAuth  = "oauth"
	OAuthFlowBrowser = "browser"
	OAuthFlowDevice  = "device-code"
)

var (
	apiKeyAuthMethod      = authMethod{id: AuthMethodAPIKey, label: "API key", note: "Use a provider API key"}
	oauthAuthMethod       = authMethod{id: AuthMethodOAuth, label: "Subscription / OAuth", note: "Sign in in your browser; the token refreshes automatically"}
	openAIAccountMethod   = authMethod{id: AuthMethodOAuth, label: "Sign in with ChatGPT", note: "Sign in with your ChatGPT account in a browser; no API key required"}
	anthropicOAuthMethod  = authMethod{id: AuthMethodOAuth, label: "Subscription / OAuth (experimental)", note: "Anthropic does not publish a third-party CLI compatibility contract"}
	openAICodexOAuthFlows = []oauthFlowOption{
		{id: OAuthFlowBrowser, label: "Sign in with browser", note: "Open the ChatGPT authorization page on this computer"},
		{id: OAuthFlowDevice, label: "Sign in with device code", note: "Use a verification code; best for SSH or headless environments"},
	}
)

// Built-in entries are limited to providers whose endpoint and model defaults
// Metis can configure without more input. Third-party compatible services use
// the complete custom-provider flow so a credential is never paired with the
// wrong vendor endpoint.
var builtInAuthProviders = []authProvider{
	{
		id: "anthropic", label: "Anthropic",
		keyHelp:     "API key from https://console.anthropic.com",
		placeholder: "sk-ant-...",
		methods:     []authMethod{apiKeyAuthMethod, anthropicOAuthMethod},
	},
	{
		id: "openai", label: "OpenAI",
		keyHelp:     "API key from https://platform.openai.com",
		placeholder: "sk-...",
		methods:     []authMethod{openAIAccountMethod, apiKeyAuthMethod},
	},
	{
		id: "openai-codex", label: "OpenAI Codex (ChatGPT Plus / Pro)",
		methods: []authMethod{oauthAuthMethod},
	},
	{
		id: "gemini", label: "Google Gemini",
		keyHelp:     "API key from https://aistudio.google.com/apikey",
		placeholder: "AIza...",
		methods:     []authMethod{apiKeyAuthMethod},
	},
	{
		id: "custom", label: "Other / Custom provider…",
		keyHelp:     "Configure an API-compatible custom provider",
		placeholder: "",
		methods:     []authMethod{apiKeyAuthMethod},
	},
}

// LoginOptions preselects part of the explicit `metis login` flow. Empty
// fields retain the corresponding picker. Provider matching is exact but
// case-insensitive; Method accepts "api-key" or "oauth".
type LoginOptions struct {
	Provider string
	Method   string
	// OAuthFlow is interpreted by the command layer after the provider/method
	// picker. The wizard carries it so non-interactive preselection and the
	// interactive UI share one options value.
	OAuthFlow string
	// ConfiguredProviders lets the command layer project already configured
	// custom profiles into the same picker as built-ins. The TUI stays free of
	// config-loading side effects and can still be tested deterministically.
	ConfiguredProviders []ConfiguredLoginProvider
}

// ConfiguredLoginProvider is the non-secret portion of an existing custom
// provider profile. Its API key is collected by the wizard and remains in the
// dedicated credential store.
type ConfiguredLoginProvider struct {
	ID        string
	Transport string
	BaseURL   string
	Model     string
}

type authTransportOption struct {
	id    string
	label string
	note  string
}

// customAuthTransports are the API wire formats the first-run wizard can
// configure without cloud-specific credentials. OpenAI-compatible chat is the
// default because it is the most common shape for third-party gateways.
var customAuthTransports = []authTransportOption{
	{
		id:    "openai_chat",
		label: "OpenAI-compatible Chat Completions",
		note:  "Metis appends /chat/completions to the base URL",
	},
	{
		id:    "openai_responses",
		label: "OpenAI-compatible Responses",
		note:  "Metis appends /responses to the base URL",
	},
	{
		id:    "anthropic_messages",
		label: "Anthropic-compatible Messages",
		note:  "Metis sends Anthropic Messages API requests",
	},
	{
		id:    "gemini_native",
		label: "Google Gemini native",
		note:  "Metis sends Gemini generateContent requests",
	},
}

// authStep tracks where the wizard is in its little state machine.
// Clearer than a pile of bools and survives future steps (e.g., model picker).
type authStep int

const (
	stepPickProvider authStep = iota
	stepPickMethod
	stepPickOAuthFlow
	stepCustomID // only reached when user selects "custom"
	stepCustomTransport
	stepCustomBaseURL
	stepCustomModel
	stepEnterKey
	stepDone
)

// authModel is the bubbletea model behind RunAuthWizard.
//
// Why bubbletea here instead of just prompting on stdin: the flow uses
// arrow-key navigation plus editable paste-aware inputs, which a plain shell
// prompt cannot provide without ANSI hackery. We already pull bubbletea/lipgloss for the chat TUI;
// reusing them keeps the install closure small.
type authModel struct {
	step               authStep
	providers          []authProvider
	cursor             int
	methods            []authMethod
	methodCursor       int
	requestedMethod    string
	oauthFlows         []oauthFlowOption
	oauthFlowCursor    int
	requestedOAuthFlow string

	// custom-provider path
	customID          textinput.Model
	customBaseURL     textinput.Model
	customModel       textinput.Model
	transportCursor   int
	customTransport   string
	normalizedBaseURL string
	isCustom          bool
	validationErr     string

	// chosen provider state (filled in once cursor commits)
	chosen authProvider

	// key input
	keyInput textinput.Model

	// output
	resultProvider  string
	resultMethod    string
	resultOAuthFlow string
	resultKey       string
	err             error
	cancelled       bool
	persistOnSubmit bool

	width int
}

func newAuthModel() authModel {
	// The startup authentication gate must never open a browser implicitly.
	// It therefore offers API-key methods only. The explicit `metis login`
	// command uses newLoginAuthModel below and can offer OAuth.
	m, err := newLoginAuthModel(LoginOptions{Method: AuthMethodAPIKey}, true)
	if err != nil {
		panic(err) // static provider registry invariant
	}
	return m
}

func newLoginAuthModel(options LoginOptions, startupAPIKeyOnly bool) (authModel, error) {
	ki := textinput.New()
	ki.EchoMode = textinput.EchoPassword
	ki.EchoCharacter = '•'
	ki.Prompt = "› "
	ki.CharLimit = 512
	ki.SetWidth(64)
	ki.Focus()

	cid := textinput.New()
	cid.Prompt = "› "
	cid.CharLimit = 64
	cid.SetWidth(64)
	cid.Placeholder = "e.g. groq, together, ollama"

	baseURL := textinput.New()
	baseURL.Prompt = "› "
	baseURL.CharLimit = 2048
	baseURL.SetWidth(72)
	baseURL.Placeholder = "https://api.example.com/v1"

	model := textinput.New()
	model.Prompt = "› "
	model.CharLimit = 256
	model.SetWidth(64)
	model.Placeholder = "e.g. model-name-latest"

	m := authModel{
		step:            stepPickProvider,
		keyInput:        ki,
		customID:        cid,
		customBaseURL:   baseURL,
		customModel:     model,
		persistOnSubmit: startupAPIKeyOnly,
		width:           80,
	}

	method, err := normalizeLoginMethod(options.Method)
	if err != nil {
		return authModel{}, err
	}
	if startupAPIKeyOnly {
		method = AuthMethodAPIKey
	}
	m.requestedMethod = method
	oauthFlow, err := normalizeOAuthFlow(options.OAuthFlow)
	if err != nil {
		return authModel{}, err
	}
	m.requestedOAuthFlow = oauthFlow
	availableProviders := loginProviders(options.ConfiguredProviders)

	providerName := strings.TrimSpace(options.Provider)
	if providerName != "" {
		provider, ok := findAuthProvider(availableProviders, providerName)
		if !ok && strings.EqualFold(providerName, "openai-codex") {
			// Retain the explicit legacy provider and its OAuth flow picker.
			// The regular picker groups both OpenAI methods under OpenAI.
			provider, ok = findAuthProvider(builtInAuthProviders, providerName)
		}
		if !ok {
			return authModel{}, fmt.Errorf("unknown provider %q; choose one of: %s", providerName, strings.Join(authProviderIDs(availableProviders), ", "))
		}
		if method != "" && !providerSupportsMethod(provider, method) {
			return authModel{}, fmt.Errorf("provider %q does not support %s login; supported methods: %s", provider.id, method, strings.Join(providerMethodIDs(provider), ", "))
		}
		m.providers = []authProvider{provider}
		m.chosen = provider
		m.advanceAfterProvider()
		return m, nil
	}

	for _, provider := range availableProviders {
		if startupAPIKeyOnly && !providerSupportsMethod(provider, AuthMethodAPIKey) {
			continue
		}
		if method != "" && !providerSupportsMethod(provider, method) {
			continue
		}
		m.providers = append(m.providers, provider)
	}
	if len(m.providers) == 0 {
		return authModel{}, fmt.Errorf("no providers support login method %q", method)
	}
	if len(m.providers) == 1 {
		m.chosen = m.providers[0]
		m.advanceAfterProvider()
	}
	return m, nil
}

func normalizeLoginMethod(raw string) (string, error) {
	method := strings.ToLower(strings.TrimSpace(raw))
	switch method {
	case "", AuthMethodAPIKey, AuthMethodOAuth:
		return method, nil
	default:
		return "", fmt.Errorf("unknown login method %q; use api-key or oauth", raw)
	}
}

func normalizeOAuthFlow(raw string) (string, error) {
	flow := strings.ToLower(strings.TrimSpace(raw))
	switch flow {
	case "", OAuthFlowBrowser, OAuthFlowDevice:
		return flow, nil
	default:
		return "", fmt.Errorf("unknown OAuth flow %q; use browser or device-code", raw)
	}
}

func loginProviders(configured []ConfiguredLoginProvider) []authProvider {
	providers := make([]authProvider, 0, len(builtInAuthProviders)+len(configured))
	// Keep the generic "Other / Custom" entry last, after named profiles.
	for _, provider := range builtInAuthProviders {
		if provider.id != "custom" && provider.id != "openai-codex" {
			providers = append(providers, provider)
		}
	}
	for _, profile := range configured {
		id, err := validateCustomProviderID(profile.ID)
		if err != nil {
			continue
		}
		if !config.CustomProviderSupportsManagedAPIKey(config.ProviderRaw{Transport: profile.Transport}) {
			continue
		}
		custom := &CustomProviderResult{
			Transport: profile.Transport,
			BaseURL:   profile.BaseURL,
			Model:     profile.Model,
			Existing:  true,
		}
		providers = append(providers, authProvider{
			id: id, label: id + " (configured)", keyHelp: "API key for " + id,
			methods: []authMethod{apiKeyAuthMethod}, custom: custom,
		})
	}
	for _, provider := range builtInAuthProviders {
		if provider.id == "custom" {
			providers = append(providers, provider)
		}
	}
	return providers
}

func findAuthProvider(providers []authProvider, raw string) (authProvider, bool) {
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(raw), provider.id) {
			return provider, true
		}
	}
	return authProvider{}, false
}

func authProviderIDs(providers []authProvider) []string {
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.id)
	}
	return ids
}

func providerSupportsMethod(provider authProvider, method string) bool {
	for _, candidate := range provider.methods {
		if candidate.id == method {
			return true
		}
	}
	return false
}

func providerMethodIDs(provider authProvider) []string {
	methods := make([]string, 0, len(provider.methods))
	for _, method := range provider.methods {
		methods = append(methods, method.id)
	}
	return methods
}

func (m *authModel) advanceAfterProvider() {
	m.methods = m.methods[:0]
	for _, method := range m.chosen.methods {
		if m.requestedMethod == "" || method.id == m.requestedMethod {
			m.methods = append(m.methods, method)
		}
	}
	if len(m.methods) == 1 {
		m.chooseMethod(m.methods[0])
		return
	}
	m.step = stepPickMethod
}

func (m *authModel) chooseMethod(method authMethod) {
	m.resultMethod = method.id
	if method.id == AuthMethodOAuth {
		m.resultProvider = m.chosen.id
		if m.chosen.id == "openai" {
			// ChatGPT subscriptions use the Codex runtime and credential store.
			m.resultProvider = "openai-codex"
			m.resultOAuthFlow = OAuthFlowBrowser
			if m.requestedOAuthFlow != "" {
				m.resultOAuthFlow = m.requestedOAuthFlow
			}
			m.step = stepDone
			return
		}
		if m.chosen.id == "openai-codex" {
			if m.requestedOAuthFlow != "" {
				m.resultOAuthFlow = m.requestedOAuthFlow
				m.step = stepDone
				return
			}
			m.oauthFlows = append(m.oauthFlows[:0], openAICodexOAuthFlows...)
			m.step = stepPickOAuthFlow
			return
		}
		m.resultOAuthFlow = OAuthFlowBrowser
		m.step = stepDone
		return
	}
	if m.chosen.id == "custom" {
		m.isCustom = true
		m.validationErr = ""
		m.customID.Focus()
		m.step = stepCustomID
		return
	}
	if m.chosen.custom != nil {
		m.isCustom = true
		m.customTransport = m.chosen.custom.Transport
		m.normalizedBaseURL = m.chosen.custom.BaseURL
		m.customModel.SetValue(m.chosen.custom.Model)
	}
	m.keyInput.Placeholder = m.chosen.placeholder
	m.keyInput.Focus()
	m.step = stepEnterKey
}

func (m authModel) Init() tea.Cmd {
	if m.step == stepDone {
		return tea.Quit
	}
	return textinput.Blink
}

func (m authModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		inputWidth := msg.Width - 4
		if inputWidth < 20 {
			inputWidth = 20
		}
		if inputWidth > 72 {
			inputWidth = 72
		}
		m.keyInput.SetWidth(inputWidth)
		m.customID.SetWidth(inputWidth)
		m.customBaseURL.SetWidth(inputWidth)
		m.customModel.SetWidth(inputWidth)

	case tea.KeyPressMsg:
		// v2: KeyPressMsg replaces KeyMsg's struct/.Type pattern; match
		// by .String() — handles named keys ("esc", "enter") + ASCII.
		// Ctrl-C / Esc bail out of the wizard from any step.
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}

		switch m.step {
		case stepPickProvider:
			return m.updatePickProvider(msg)
		case stepPickMethod:
			return m.updatePickMethod(msg)
		case stepPickOAuthFlow:
			return m.updatePickOAuthFlow(msg)
		case stepCustomID:
			return m.updateCustomID(msg)
		case stepCustomTransport:
			return m.updateCustomTransport(msg)
		case stepCustomBaseURL:
			return m.updateCustomBaseURL(msg)
		case stepCustomModel:
			return m.updateCustomModel(msg)
		case stepEnterKey:
			return m.updateKey(msg)
		}

	case tea.PasteMsg:
		// Bubble Tea v2 emits bracketed paste as PasteMsg, not as a run of
		// KeyPressMsg values. The bubbles textinput already understands the
		// message; route it to whichever field currently owns input.
		m.validationErr = ""
		switch m.step {
		case stepCustomID:
			return m.updateCustomID(msg)
		case stepCustomBaseURL:
			return m.updateCustomBaseURL(msg)
		case stepCustomModel:
			return m.updateCustomModel(msg)
		case stepEnterKey:
			return m.updateKey(msg)
		}
	}
	return m, nil
}

func (m authModel) updatePickProvider(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down":
		if m.cursor < len(m.providers)-1 {
			m.cursor++
		}
	case "enter":
		m.chosen = m.providers[m.cursor]
		m.advanceAfterProvider()
		if m.step == stepDone {
			return m, tea.Quit
		}
		if m.step == stepEnterKey || m.step == stepCustomID {
			return m, textinput.Blink
		}
	}
	return m, nil
}

func (m authModel) updatePickMethod(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.methodCursor > 0 {
			m.methodCursor--
		}
	case "down":
		if m.methodCursor < len(m.methods)-1 {
			m.methodCursor++
		}
	case "enter":
		if len(m.methods) == 0 {
			m.validationErr = "no login methods are available for this provider"
			return m, nil
		}
		m.chooseMethod(m.methods[m.methodCursor])
		if m.step == stepDone {
			return m, tea.Quit
		}
		return m, textinput.Blink
	}
	return m, nil
}

func (m authModel) updatePickOAuthFlow(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.oauthFlowCursor > 0 {
			m.oauthFlowCursor--
		}
	case "down":
		if m.oauthFlowCursor < len(m.oauthFlows)-1 {
			m.oauthFlowCursor++
		}
	case "enter":
		if len(m.oauthFlows) == 0 {
			m.validationErr = "no OAuth flows are available for this provider"
			return m, nil
		}
		m.resultOAuthFlow = m.oauthFlows[m.oauthFlowCursor].id
		m.validationErr = ""
		m.step = stepDone
		return m, tea.Quit
	}
	return m, nil
}

func (m authModel) updateCustomID(msg tea.Msg) (tea.Model, tea.Cmd) {
	if isAuthEnter(msg) {
		id, err := validateCustomProviderID(m.customID.Value())
		if err != nil {
			m.validationErr = err.Error()
			return m, nil
		}
		m.validationErr = ""
		m.customID.SetValue(id)
		m.chosen = authProvider{
			id:          id,
			label:       id,
			keyHelp:     "API key for " + id,
			placeholder: "",
		}
		m.customID.Blur()
		m.step = stepCustomTransport
		return m, nil
	}
	var cmd tea.Cmd
	m.customID, cmd = m.customID.Update(msg)
	return m, cmd
}

func (m authModel) updateCustomTransport(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "left":
		if m.transportCursor > 0 {
			m.transportCursor--
		}
	case "down", "right":
		if m.transportCursor < len(customAuthTransports)-1 {
			m.transportCursor++
		}
	case "enter":
		if len(customAuthTransports) == 0 {
			m.validationErr = "no custom transports are available"
			return m, nil
		}
		m.customTransport = customAuthTransports[m.transportCursor].id
		m.validationErr = ""
		switch m.customTransport {
		case "openai_chat":
			m.customBaseURL.Placeholder = "https://api.example.com/v1"
		case "openai_responses":
			m.customBaseURL.Placeholder = "https://api.example.com/v1"
		case "anthropic_messages":
			m.customBaseURL.Placeholder = "https://api.example.com"
		case "gemini_native":
			m.customBaseURL.Placeholder = "https://generativelanguage.googleapis.com"
		}
		m.customBaseURL.Focus()
		m.step = stepCustomBaseURL
		return m, textinput.Blink
	}
	return m, nil
}

func (m authModel) updateCustomBaseURL(msg tea.Msg) (tea.Model, tea.Cmd) {
	if isAuthEnter(msg) {
		baseURL, err := validateAndNormalizeCustomBaseURL(m.customTransport, m.customBaseURL.Value())
		if err != nil {
			m.validationErr = err.Error()
			return m, nil
		}
		m.validationErr = ""
		m.normalizedBaseURL = baseURL
		m.customBaseURL.SetValue(baseURL)
		m.customBaseURL.Blur()
		m.customModel.Focus()
		m.step = stepCustomModel
		return m, textinput.Blink
	}
	var cmd tea.Cmd
	m.customBaseURL, cmd = m.customBaseURL.Update(msg)
	return m, cmd
}

func (m authModel) updateCustomModel(msg tea.Msg) (tea.Model, tea.Cmd) {
	if isAuthEnter(msg) {
		model, err := validateCustomModel(m.customModel.Value())
		if err != nil {
			m.validationErr = err.Error()
			return m, nil
		}
		m.validationErr = ""
		m.customModel.SetValue(model)
		m.customModel.Blur()
		m.keyInput.Placeholder = m.chosen.placeholder
		m.keyInput.Focus()
		m.step = stepEnterKey
		return m, textinput.Blink
	}
	var cmd tea.Cmd
	m.customModel, cmd = m.customModel.Update(msg)
	return m, cmd
}

func (m authModel) updateKey(msg tea.Msg) (tea.Model, tea.Cmd) {
	if isAuthEnter(msg) {
		key := strings.TrimSpace(m.keyInput.Value())
		if key == "" {
			m.validationErr = "API key is required"
			return m, nil
		}
		m.validationErr = ""
		m.resultProvider = m.chosen.id
		m.resultKey = key
		if m.persistOnSubmit {
			if err := persistAuthWizardResult(m.authResult()); err != nil {
				m.err = err
				return m, tea.Quit
			}
		}
		m.step = stepDone
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.keyInput, cmd = m.keyInput.Update(msg)
	return m, cmd
}

func isAuthEnter(msg tea.Msg) bool {
	key, ok := msg.(tea.KeyPressMsg)
	return ok && key.String() == "enter"
}

func validateCustomProviderID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", errors.New("provider id is required")
	}
	if id == "anthropic" || id == "anthropic-claudeai" || id == "openai" || id == "openai-codex" || id == "gemini" || id == "google" || id == "custom" {
		return "", fmt.Errorf("provider id %q is reserved", id)
	}
	for i, r := range id {
		alphaNumeric := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		separator := r == '-' || r == '_'
		if !alphaNumeric && !(i > 0 && separator) {
			return "", errors.New("provider id must use lowercase letters, digits, '_' or '-' and start with a letter or digit")
		}
	}
	return id, nil
}

func validateAndNormalizeCustomBaseURL(transport, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("base URL is required")
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", errors.New("base URL must be an absolute HTTP(S) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("base URL must use http or https")
	}
	if err := config.ValidateProviderEndpointTransport(value); err != nil {
		return "", err
	}
	if u.User != nil {
		return "", errors.New("base URL must not contain credentials")
	}
	if u.Fragment != "" {
		return "", errors.New("base URL must not contain a query string or fragment")
	}

	u.Path = strings.TrimRight(u.Path, "/")
	switch transport {
	case "openai_chat":
		if u.RawQuery != "" || u.ForceQuery {
			return "", errors.New("base URL must not contain a query string or fragment")
		}
		if strings.HasSuffix(u.Path, "/chat/completions") {
			u.Path = strings.TrimSuffix(u.Path, "/chat/completions")
		}
	case "openai_responses":
		if u.RawQuery != "" || u.ForceQuery {
			return "", errors.New("base URL must not contain a query string or fragment")
		}
		if strings.HasSuffix(u.Path, "/responses") {
			u.Path = strings.TrimSuffix(u.Path, "/responses")
		}
	case "anthropic_messages":
		if u.RawQuery != "" || u.ForceQuery {
			return "", errors.New("base URL must not contain a query string or fragment")
		}
		if strings.HasSuffix(u.Path, "/v1/messages") {
			u.Path = strings.TrimSuffix(u.Path, "/v1/messages")
		} else if strings.HasSuffix(u.Path, "/v1") {
			u.Path = strings.TrimSuffix(u.Path, "/v1")
		}
	case "gemini_native":
		fullEndpoint := false
		if marker := strings.LastIndex(u.Path, "/v1beta/models/"); marker >= 0 {
			tail := u.Path[marker+len("/v1beta/models/"):]
			if strings.HasSuffix(tail, ":generateContent") || strings.HasSuffix(tail, ":streamGenerateContent") {
				u.Path = u.Path[:marker]
				fullEndpoint = true
			}
		} else if strings.HasSuffix(u.Path, "/v1beta") {
			u.Path = strings.TrimSuffix(u.Path, "/v1beta")
		}
		if u.RawQuery != "" || u.ForceQuery {
			if !fullEndpoint || u.Query().Get("alt") != "sse" || len(u.Query()) != 1 {
				return "", errors.New("base URL must not contain a query string or fragment")
			}
			u.RawQuery = ""
			u.ForceQuery = false
		}
	default:
		return "", fmt.Errorf("unsupported API transport %q", transport)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	// RawPath must agree with Path. Clearing it makes url.String derive the
	// escaped path from the normalized value above.
	u.RawPath = ""
	return u.String(), nil
}

func validateCustomModel(raw string) (string, error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "", errors.New("model id is required")
	}
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("model id must not contain whitespace")
		}
	}
	return model, nil
}

// Style helpers — kept local rather than reaching into the chat-room palette,
// since the wizard runs *before* a config is even loaded and shouldn't depend
// on user theming.
var (
	authTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#64b5f6"))
	authHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("#a0a0a0"))
	authActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb74d"))
	authInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("#e8e8e8"))
	authMuted    = lipgloss.NewStyle().Foreground(lipgloss.Color("#606060"))
	authError    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e57373")).Bold(true)
)

func (m authModel) View() tea.View {
	// v2: View returns a tea.View struct, not a string. NewView wraps
	// the rendered content; AltScreen / MouseMode etc are declarative
	// fields on the View — auth wizard runs in inline mode (no alt
	// screen) so we don't set those.
	var b strings.Builder

	b.WriteString(authTitle.Render("Metis · sign in") + "\n")
	b.WriteString(authMuted.Render("Credentials stay local (mode 0600). Esc to cancel.") + "\n\n")

	switch m.step {
	case stepPickProvider:
		b.WriteString(authHelp.Render("Select a provider:") + "\n\n")
		for i, p := range m.providers {
			marker := "  "
			line := p.label
			if i == m.cursor {
				marker = authActive.Render("› ")
				line = authActive.Render(line)
			} else {
				line = authInactive.Render(line)
			}
			b.WriteString(marker + line + "\n")
		}
		b.WriteString("\n" + authMuted.Render("↑/↓ to move, Enter to select, Esc to cancel"))

	case stepPickMethod:
		b.WriteString(authHelp.Render("Provider: ") + authActive.Render(m.chosen.label) + "\n")
		b.WriteString(authHelp.Render("Choose how to sign in:") + "\n\n")
		for i, method := range m.methods {
			marker := "  "
			line := method.label
			if i == m.methodCursor {
				marker = authActive.Render("› ")
				line = authActive.Render(line)
			} else {
				line = authInactive.Render(line)
			}
			b.WriteString(marker + line + "\n")
			if i == m.methodCursor && method.note != "" {
				b.WriteString("    " + authMuted.Render(method.note) + "\n")
			}
		}
		b.WriteString("\n" + authMuted.Render("↑/↓ to move, Enter to select, Esc to cancel"))

	case stepPickOAuthFlow:
		b.WriteString(authHelp.Render("Provider: ") + authActive.Render(m.chosen.label) + "\n")
		b.WriteString(authHelp.Render("Choose an OAuth flow:") + "\n\n")
		for i, flow := range m.oauthFlows {
			marker := "  "
			line := flow.label
			if i == m.oauthFlowCursor {
				marker = authActive.Render("› ")
				line = authActive.Render(line)
			} else {
				line = authInactive.Render(line)
			}
			b.WriteString(marker + line + "\n")
			if i == m.oauthFlowCursor && flow.note != "" {
				b.WriteString("    " + authMuted.Render(flow.note) + "\n")
			}
		}
		b.WriteString("\n" + authMuted.Render("↑/↓ to move, Enter to select, Esc to cancel"))

	case stepCustomID:
		b.WriteString(authHelp.Render("Provider id (lowercase, used in the private credential store):") + "\n\n")
		b.WriteString(m.customID.View() + "\n\n")
		m.writeValidationError(&b)
		b.WriteString(authMuted.Render("Enter to continue, Esc to cancel"))

	case stepCustomTransport:
		b.WriteString(authHelp.Render("API transport:") + "\n\n")
		for i, transport := range customAuthTransports {
			marker := "  "
			line := transport.label
			if i == m.transportCursor {
				marker = authActive.Render("› ")
				line = authActive.Render(line)
			} else {
				line = authInactive.Render(line)
			}
			b.WriteString(marker + line + "\n")
			if i == m.transportCursor && transport.note != "" {
				b.WriteString("    " + authMuted.Render(transport.note) + "\n")
			}
		}
		b.WriteString("\n" + authMuted.Render("↑/↓ to move, Enter to select, Esc to cancel"))

	case stepCustomBaseURL:
		b.WriteString(authHelp.Render("Base URL:") + "\n")
		switch m.customTransport {
		case "openai_chat":
			b.WriteString(authMuted.Render("You may paste either .../v1 or the full .../v1/chat/completions endpoint.") + "\n")
		case "openai_responses":
			b.WriteString(authMuted.Render("You may paste either .../v1 or the full .../v1/responses endpoint.") + "\n")
		case "anthropic_messages":
			b.WriteString(authMuted.Render("You may paste the API origin or a full .../v1/messages endpoint.") + "\n")
		case "gemini_native":
			b.WriteString(authMuted.Render("You may paste the API origin, .../v1beta, or a full generateContent endpoint.") + "\n")
		}
		b.WriteString("\n" + m.customBaseURL.View() + "\n\n")
		m.writeValidationError(&b)
		b.WriteString(authMuted.Render("Enter to continue, Esc to cancel"))

	case stepCustomModel:
		b.WriteString(authHelp.Render("Model id:") + "\n\n")
		b.WriteString(m.customModel.View() + "\n\n")
		m.writeValidationError(&b)
		b.WriteString(authMuted.Render("Enter to continue, Esc to cancel"))

	case stepEnterKey:
		b.WriteString(authHelp.Render("Provider: ") +
			authActive.Render(m.chosen.label) + "\n")
		if m.chosen.custom != nil {
			b.WriteString(authMuted.Render("Transport: "+m.chosen.custom.Transport) + "\n")
			b.WriteString(authMuted.Render("Endpoint: "+m.chosen.custom.BaseURL) + "\n")
			if strings.TrimSpace(m.chosen.custom.Model) != "" {
				b.WriteString(authMuted.Render("Model: "+m.chosen.custom.Model) + "\n")
			}
		}
		if m.chosen.keyHelp != "" {
			b.WriteString(authMuted.Render(m.chosen.keyHelp) + "\n")
		}
		b.WriteString("\n" + m.keyInput.View() + "\n\n")
		m.writeValidationError(&b)
		b.WriteString(authMuted.Render("Input is masked. Enter to save, Esc to cancel."))

	case stepDone:
		if m.err != nil {
			b.WriteString(authError.Render("Failed: "+m.err.Error()) + "\n")
		} else if m.resultMethod == AuthMethodOAuth {
			b.WriteString(authActive.Render("✓ Ready to authorize "+m.chosen.label) + "\n")
		} else if m.persistOnSubmit {
			b.WriteString(authActive.Render("✓ Saved "+m.chosen.label+" credentials") + "\n")
			b.WriteString(authMuted.Render("Continuing to chat…") + "\n")
		} else {
			b.WriteString(authActive.Render("✓ Ready to save "+m.chosen.label+" credentials") + "\n")
		}
	}
	return tea.NewView(b.String())
}

func (m authModel) writeValidationError(b *strings.Builder) {
	if m.validationErr != "" {
		b.WriteString(authError.Render(m.validationErr) + "\n")
	}
}

// CustomProviderResult is the non-secret profile collected for an entry under
// [provider.custom]. It is nil for built-in providers. Persistence of this
// profile is deliberately kept behind persistAuthWizardResult so the config
// writer can be connected without teaching the TUI about config file layout.
type CustomProviderResult struct {
	Transport string
	BaseURL   string
	Model     string
	Existing  bool
}

// AuthResult is what RunAuthWizard returns on success.
//
// Provider is the stable provider id and Method identifies API-key versus
// OAuth login. Key is populated only for API-key results so startup callers can
// construct a provider without re-reading auth.json (their cfg was loaded before
// the startup wizard ran). OAuth results never contain a token; the command
// layer performs and persists that context-aware flow after the wizard exits.
type AuthResult struct {
	Provider  string
	Method    string
	OAuthFlow string
	Key       string
	Custom    *CustomProviderResult
}

func (m authModel) authResult() AuthResult {
	result := AuthResult{
		Provider:  m.resultProvider,
		Method:    m.resultMethod,
		OAuthFlow: m.resultOAuthFlow,
		Key:       m.resultKey,
	}
	if m.isCustom {
		if m.chosen.custom != nil {
			custom := *m.chosen.custom
			result.Custom = &custom
		} else {
			result.Custom = &CustomProviderResult{
				Transport: m.customTransport,
				BaseURL:   m.normalizedBaseURL,
				Model:     strings.TrimSpace(m.customModel.Value()),
			}
		}
	}
	return result
}

// persistAuthWizardResult writes non-secret provider settings first, then the
// API key. If credential persistence fails the user is left with a recoverable
// key-less profile rather than an orphaned secret that no configured provider
// can resolve. The config writer only touches the user-level config.toml.
func persistAuthWizardResult(result AuthResult) error {
	if err := PrepareLoginResultForWorkspace(result, true); err != nil {
		return err
	}
	var activateErr error
	if result.Custom != nil {
		activateErr = auth.ActivateAPIKeyBound(result.Provider, result.Key, result.Custom.Transport, result.Custom.BaseURL)
	} else {
		activateErr = auth.ActivateAPIKey(result.Provider, result.Key)
	}
	if activateErr != nil {
		return activateErr
	}
	if result.Custom == nil {
		return config.SaveUserProviderDefault(result.Provider)
	}
	return nil
}

// PrepareLoginResult validates the selected endpoint and persists only the
// non-secret provider profile. The explicit `metis login` command calls this
// before atomically activating exactly one credential method.
func PrepareLoginResult(result AuthResult) error {
	return PrepareLoginResultForWorkspace(result, true)
}

// PrepareLoginResultForWorkspace is the source-aware form used by the
// top-level `metis login` command. A standalone login does not run the chat
// trust prompt, so project provider routing must be excluded unless this
// workspace was already trusted. Startup's embedded wizard runs only after
// the interactive trust gate and therefore passes true.
func PrepareLoginResultForWorkspace(result AuthResult, projectTrusted bool) error {
	current, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("validate provider %q before auth setup: %w", result.Provider, err)
	}
	if err := config.ApplyProviderPolicyForWorkspace(current, projectTrusted); err != nil {
		return fmt.Errorf("apply provider trust policy for %q: %w", result.Provider, err)
	}
	if result.Custom != nil {
		spec := config.CustomProviderSpec{
			ID:        result.Provider,
			Transport: result.Custom.Transport,
			BaseURL:   result.Custom.BaseURL,
			Model:     result.Custom.Model,
		}
		raw, profileExists := current.Provider.Custom[result.Provider]
		if result.Custom.Existing {
			if !profileExists {
				return fmt.Errorf("configured custom provider %q no longer exists; reopen the login picker", result.Provider)
			}
			if raw.Transport != spec.Transport || raw.BaseURL != spec.BaseURL || raw.Model != spec.Model {
				return fmt.Errorf("configured custom provider %q changed while signing in; reopen the login picker", result.Provider)
			}
			if customProviderHasHigherPriorityCredential(raw) {
				return fmt.Errorf("custom provider %q has an active api_key_env override; unset it before saving a wizard-managed key", result.Provider)
			}
			return nil
		}
		if profileExists {
			if raw.Transport != spec.Transport || raw.BaseURL != spec.BaseURL {
				return fmt.Errorf("custom provider %q already resolves to a different endpoint; remove or rename the old profile and credential before changing its transport or base_url", result.Provider)
			}
			if customProviderHasHigherPriorityCredential(raw) {
				return fmt.Errorf("custom provider %q has an active api_key_env override; unset it before saving a wizard-managed key", result.Provider)
			}
		}
		if existingKey, getErr := auth.Get(result.Provider); getErr != nil {
			return fmt.Errorf("inspect existing credential for %q: %w", result.Provider, getErr)
		} else if !profileExists && existingKey != "" {
			return fmt.Errorf("custom provider %q has an existing credential but no configured profile; remove it with `metis auth logout %s` or choose a new provider id before creating an endpoint", result.Provider, result.Provider)
		}
		if err := config.SaveUserCustomProvider(spec); err != nil {
			return fmt.Errorf("save custom provider %q: %w", result.Provider, err)
		}
		merged, _, err := config.Load()
		if err != nil {
			return fmt.Errorf("verify custom provider %q: %w", result.Provider, err)
		}
		if err := config.ApplyProviderPolicyForWorkspace(merged, projectTrusted); err != nil {
			return fmt.Errorf("apply provider trust policy while verifying %q: %w", result.Provider, err)
		}
		got, ok := merged.Provider.Custom[result.Provider]
		if !ok || got.Transport != spec.Transport || got.BaseURL != spec.BaseURL || got.Model != spec.Model {
			return fmt.Errorf("custom provider %q is overridden by a higher-precedence project config; refusing to store its API key", result.Provider)
		}
	} else {
		if !builtInProviderUsesOfficialEndpoint(current, result.Provider) {
			return fmt.Errorf("provider %q has a non-official base_url from higher-precedence config; use Other / Custom provider so the endpoint is shown before entering a key", result.Provider)
		}
		if builtInProviderHasHigherPriorityCredential(current, result.Provider) {
			return fmt.Errorf("provider %q has an active API-key environment override; unset it before saving a wizard-managed key", result.Provider)
		}
	}
	return nil
}

func customProviderHasHigherPriorityCredential(raw config.ProviderRaw) bool {
	return config.CustomProviderCredentialOverrideSource(raw) != ""
}

func builtInProviderHasHigherPriorityCredential(cfg *config.Config, provider string) bool {
	if cfg == nil {
		return false
	}
	var keyEnv string
	switch provider {
	case "anthropic":
		keyEnv = cfg.Provider.Anthropic.APIKeyEnv
	case "openai":
		keyEnv = cfg.Provider.OpenAI.APIKeyEnv
	case "gemini":
		keyEnv = cfg.Provider.Gemini.APIKeyEnv
		// Gemini accepts GOOGLE_API_KEY as a legacy runtime fallback in
		// addition to GEMINI_API_KEY. Treat it as higher precedence too, or
		// the wizard could report success while requests continue using the
		// older environment credential.
		if strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")) != "" {
			return true
		}
	default:
		return false
	}
	// Inline api_key is lower priority than the managed credential store and
	// therefore must not prevent migration to `metis login`.
	return strings.TrimSpace(keyEnv) != "" && os.Getenv(keyEnv) != ""
}

func builtInProviderUsesOfficialEndpoint(cfg *config.Config, provider string) bool {
	if cfg == nil {
		return false
	}
	var raw, wantHost, wantPath string
	switch provider {
	case "anthropic":
		raw, wantHost, wantPath = cfg.Provider.Anthropic.BaseURL, "api.anthropic.com", ""
	case "openai":
		raw, wantHost, wantPath = cfg.Provider.OpenAI.BaseURL, "api.openai.com", "/v1"
	case "gemini":
		raw, wantHost, wantPath = cfg.Provider.Gemini.BaseURL, "generativelanguage.googleapis.com", ""
	default:
		return false
	}
	if strings.TrimSpace(raw) == "" {
		return true // provider constructors use the official default for empty URLs
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), wantHost) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return strings.TrimRight(u.EscapedPath(), "/") == wantPath
}

// RunAuthWizard launches the bubbletea picker + key prompt and returns the
// result. The wizard writes auth.json itself before returning, so even callers
// that ignore AuthResult will see the side effect on the next cfg reload.
//
// Returns ErrAuthCancelled if the user pressed Ctrl-C / Esc.
//
// Output goes to stderr so a `metis run "..." | jq` style pipeline doesn't
// interleave wizard ANSI with the assistant's stdout payload. (The wizard
// only fires when stderr is a tty anyway — see setupRuntime gating.)
func RunAuthWizard() (AuthResult, error) {
	return runAuthModel(newAuthModel())
}

// RunLoginWizard launches the explicit login flow. Unlike RunAuthWizard it
// only collects a result: it never persists a key or token. The command layer
// owns the single atomic credential activation after the wizard exits.
func RunLoginWizard(options LoginOptions) (AuthResult, error) {
	m, err := newLoginAuthModel(options, false)
	if err != nil {
		return AuthResult{}, err
	}
	return runAuthModel(m)
}

func runAuthModel(initial authModel) (AuthResult, error) {
	p := tea.NewProgram(initial, tea.WithInput(os.Stdin), tea.WithOutput(os.Stderr))
	finalRaw, err := p.Run()
	if err != nil {
		return AuthResult{}, fmt.Errorf("auth wizard: %w", err)
	}
	final, ok := finalRaw.(authModel)
	if !ok {
		return AuthResult{}, fmt.Errorf("auth wizard: unexpected final model type")
	}
	if final.cancelled {
		return AuthResult{}, ErrAuthCancelled
	}
	if final.err != nil {
		return AuthResult{}, final.err
	}
	if final.resultProvider == "" || final.resultMethod == "" {
		return AuthResult{}, ErrAuthCancelled
	}
	if final.resultMethod == AuthMethodAPIKey && final.resultKey == "" {
		return AuthResult{}, ErrAuthCancelled
	}
	return final.authResult(), nil
}
