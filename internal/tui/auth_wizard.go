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
// `id` is what we persist into auth.json and pass to cfg.ResolveAPIKey().
// `label` is what the user sees. `placeholder` is the example shown above
// the API-key field — purely cosmetic but it doubles as a "this is what
// the key looks like" sanity hint.
type authProvider struct {
	id          string
	label       string
	keyHelp     string
	placeholder string
}

// Built-in entries are limited to providers whose endpoint and model defaults
// Metis can configure without more input. Third-party compatible services use
// the complete custom-provider flow so a credential is never paired with the
// wrong vendor endpoint.
var builtInAuthProviders = []authProvider{
	{
		id: "anthropic", label: "Anthropic",
		keyHelp:     "API key from https://console.anthropic.com",
		placeholder: "sk-ant-...",
	},
	{
		id: "openai", label: "OpenAI",
		keyHelp:     "API key from https://platform.openai.com",
		placeholder: "sk-...",
	},
	{
		id: "custom", label: "Other / Custom provider…",
		keyHelp:     "Configure an API-compatible custom provider",
		placeholder: "",
	},
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
	stepCustomID              // only reached when user selects "custom"
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
	step      authStep
	providers []authProvider
	cursor    int

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
	resultProvider string
	resultKey      string
	err            error
	cancelled      bool

	width int
}

func newAuthModel() authModel {
	ki := textinput.New()
	ki.EchoMode = textinput.EchoNormal
	ki.Prompt = "› "
	ki.CharLimit = 512
	ki.Focus()

	cid := textinput.New()
	cid.Prompt = "› "
	cid.CharLimit = 64
	cid.Placeholder = "e.g. groq, together, ollama"

	baseURL := textinput.New()
	baseURL.Prompt = "› "
	baseURL.CharLimit = 2048
	baseURL.Placeholder = "https://api.example.com/v1"

	model := textinput.New()
	model.Prompt = "› "
	model.CharLimit = 256
	model.Placeholder = "e.g. model-name-latest"

	return authModel{
		step:          stepPickProvider,
		providers:     builtInAuthProviders,
		keyInput:      ki,
		customID:      cid,
		customBaseURL: baseURL,
		customModel:   model,
		width:         80,
	}
}

func (m authModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m authModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width

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
		if m.chosen.id == "custom" {
			m.isCustom = true
			m.validationErr = ""
			m.customID.Focus()
			m.step = stepCustomID
			return m, textinput.Blink
		}
		m.keyInput.Placeholder = m.chosen.placeholder
		m.keyInput.Focus()
		m.step = stepEnterKey
		return m, textinput.Blink
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
		if err := persistAuthWizardResult(m.authResult()); err != nil {
			m.err = err
			return m, tea.Quit
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
	if id == "anthropic" || id == "openai" || id == "gemini" || id == "google" || id == "custom" {
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
	b.WriteString(authMuted.Render("Stored at "+auth.Path()+" (mode 0600). Esc to cancel.") + "\n\n")

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

	case stepCustomID:
		b.WriteString(authHelp.Render("Provider id (lowercase, used as key in auth.json):") + "\n\n")
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
		if m.chosen.keyHelp != "" {
			b.WriteString(authMuted.Render(m.chosen.keyHelp) + "\n")
		}
		b.WriteString("\n" + m.keyInput.View() + "\n\n")
		m.writeValidationError(&b)
		b.WriteString(authMuted.Render("Input is visible and editable. Enter to save, Esc to cancel."))

	case stepDone:
		if m.err != nil {
			b.WriteString(authError.Render("Failed: "+m.err.Error()) + "\n")
		} else {
			b.WriteString(authActive.Render("✓ Saved "+m.chosen.label+" credentials") + "\n")
			b.WriteString(authMuted.Render("Continuing to chat…") + "\n")
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
}

// AuthResult is what RunAuthWizard returns on success.
//
// Provider is the id used in auth.json / cfg.ResolveAPIKey.
// Key is included so callers can plug it straight into a provider client without
// re-reading auth.json (this matters because, in the startup-detection path,
// setupRuntime has already loaded cfg before the wizard runs).
type AuthResult struct {
	Provider string
	Key      string
	Custom   *CustomProviderResult
}

func (m authModel) authResult() AuthResult {
	result := AuthResult{
		Provider: m.resultProvider,
		Key:      m.resultKey,
	}
	if m.isCustom {
		result.Custom = &CustomProviderResult{
			Transport: m.customTransport,
			BaseURL:   m.normalizedBaseURL,
			Model:     strings.TrimSpace(m.customModel.Value()),
		}
	}
	return result
}

// persistAuthWizardResult writes non-secret provider settings first, then the
// API key. If credential persistence fails the user is left with a recoverable
// key-less profile rather than an orphaned secret that no configured provider
// can resolve. The config writer only touches the user-level config.toml.
func persistAuthWizardResult(result AuthResult) error {
	current, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("validate provider %q before auth setup: %w", result.Provider, err)
	}
	if result.Custom != nil {
		spec := config.CustomProviderSpec{
			ID:        result.Provider,
			Transport: result.Custom.Transport,
			BaseURL:   result.Custom.BaseURL,
			Model:     result.Custom.Model,
		}
		raw, profileExists := current.Provider.Custom[result.Provider]
		if profileExists {
			if raw.Transport != spec.Transport || raw.BaseURL != spec.BaseURL {
				return fmt.Errorf("custom provider %q already resolves to a different endpoint; remove or rename the old profile and credential before changing its transport or base_url", result.Provider)
			}
			if customProviderHasHigherPriorityCredential(raw) {
				return fmt.Errorf("custom provider %q already has an active api_key_env or inline api_key; remove or unset it before saving a wizard-managed key", result.Provider)
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
		got, ok := merged.Provider.Custom[result.Provider]
		if !ok || got.Transport != spec.Transport || got.BaseURL != spec.BaseURL || got.Model != spec.Model {
			return fmt.Errorf("custom provider %q is overridden by a higher-precedence project config; refusing to store its API key", result.Provider)
		}
	} else {
		if !builtInProviderUsesOfficialEndpoint(current, result.Provider) {
			return fmt.Errorf("provider %q has a non-official base_url from higher-precedence config; use Other / Custom provider so the endpoint is shown before entering a key", result.Provider)
		}
		if builtInProviderHasHigherPriorityCredential(current, result.Provider) {
			return fmt.Errorf("provider %q already has an active api_key_env or inline api_key; remove or unset it before saving a wizard-managed key", result.Provider)
		}
		if err := config.SaveUserProviderDefault(result.Provider); err != nil {
			return fmt.Errorf("select provider %q: %w", result.Provider, err)
		}
		merged, _, err := config.Load()
		if err != nil {
			return fmt.Errorf("verify provider %q: %w", result.Provider, err)
		}
		if !builtInProviderUsesOfficialEndpoint(merged, result.Provider) {
			return fmt.Errorf("provider %q has a non-official base_url from higher-precedence config; use Other / Custom provider so the endpoint is shown before entering a key", result.Provider)
		}
	}
	return auth.Set(result.Provider, result.Key)
}

func customProviderHasHigherPriorityCredential(raw config.ProviderRaw) bool {
	return strings.TrimSpace(raw.APIKey) != "" || (strings.TrimSpace(raw.APIKeyEnv) != "" && os.Getenv(raw.APIKeyEnv) != "")
}

func builtInProviderHasHigherPriorityCredential(cfg *config.Config, provider string) bool {
	if cfg == nil {
		return false
	}
	var key, keyEnv string
	switch provider {
	case "anthropic":
		key, keyEnv = cfg.Provider.Anthropic.APIKey, cfg.Provider.Anthropic.APIKeyEnv
	case "openai":
		key, keyEnv = cfg.Provider.OpenAI.APIKey, cfg.Provider.OpenAI.APIKeyEnv
	default:
		return false
	}
	return strings.TrimSpace(key) != "" || (strings.TrimSpace(keyEnv) != "" && os.Getenv(keyEnv) != "")
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
	p := tea.NewProgram(newAuthModel(), tea.WithOutput(os.Stderr))
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
	if final.resultProvider == "" || final.resultKey == "" {
		return AuthResult{}, ErrAuthCancelled
	}
	return final.authResult(), nil
}
