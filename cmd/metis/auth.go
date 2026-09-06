package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/tui"
)

// cmdAuth is the entry point for `metis auth <subcommand>`.
//
// `metis login` is the canonical sign-in command. `metis auth login` and bare
// `metis auth` remain compatibility aliases so scripts and older docs keep
// working while credential management stays grouped under `metis auth`.
func cmdAuth(ctx context.Context, args []string) error {
	sub := "login"
	rest := []string{}
	if len(args) > 0 {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "login":
		return cmdAuthLogin(ctx, rest)
	case "logout":
		return cmdAuthLogout(rest)
	case "list", "ls":
		return cmdAuthList()
	case "oauth":
		return cmdAuthOAuth(ctx, rest)
	case "keys":
		return cmdAuthKeys(ctx, rest)
	case "help", "-h", "--help":
		printAuthUsage()
		return nil
	}
	return fmt.Errorf("auth: unknown subcommand %q (use: login | logout | list | oauth | keys)", sub)
}

// cmdAuthKeys handles `metis auth keys <put|list|rm> ...` for
// non-LLM credentials — currently just WebSearch backend keys
// (Tavily / Brave / Serper). Distinct subcommand on purpose:
// `metis login` is for LLM providers and runs the bubbletea
// wizard; search-backend keys can be read from a hidden terminal prompt or
// stdin so they do not need to appear in process arguments or shell history.
func cmdAuthKeys(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printAuthKeysUsage()
		return nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "put", "set":
		return cmdAuthKeysPut(ctx, rest)
	case "list", "ls":
		return cmdAuthKeysList()
	case "rm", "remove", "delete":
		return cmdAuthKeysRemove(rest)
	case "help", "-h", "--help":
		printAuthKeysUsage()
		return nil
	}
	return fmt.Errorf("auth keys: unknown subcommand %q (use: put | list | rm)", sub)
}

func printAuthKeysUsage() {
	fmt.Println(`metis auth keys — manage non-LLM API keys (WebSearch backends)

Usage:
  metis auth keys put <backend>         Store a key (hidden TTY prompt or stdin)
  metis auth keys put <backend> <key>   Deprecated compatibility form
  metis auth keys list                   List stored search-backend keys
  metis auth keys rm <backend>           Remove a stored search-backend key

Known backends: tavily, brave, serper

Notes:
  Stored under "search:<backend>" in ~/.metis/.credentials/auth.json (0o600). Env vars
  (TAVILY_API_KEY / BRAVE_SEARCH_API_KEY / SERPER_API_KEY) still take
  precedence — useful for CI overrides without touching the file.

  Free tiers:
    tavily  — 1k searches/mo, tavily.com
    brave   — 2k queries/mo, api.search.brave.com
    serper  — paid Google SERP, serper.dev`)
}

const maxSearchKeyInputBytes = 16 * 1024

var (
	acquireSearchBackendKey = readSearchBackendKey
	authKeysStderr          = func() io.Writer { return os.Stderr }
)

func cmdAuthKeysPut(ctx context.Context, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) < 1 || len(args) > 2 {
		return errors.New("auth keys put: usage `metis auth keys put <backend> [key]`")
	}
	backend := args[0]
	var key string
	if len(args) == 2 {
		var err error
		key, err = normalizeSearchKeyInput([]byte(args[1]))
		if err != nil {
			return fmt.Errorf("auth keys put %s: %w", backend, err)
		}
		fmt.Fprintln(authKeysStderr(), "warning: passing a key as an argument can expose it in shell history or process listings; omit [key] to use a hidden prompt or piped stdin")
	} else {
		var err error
		key, err = acquireSearchBackendKey(ctx)
		if err != nil {
			return fmt.Errorf("auth keys put %s: %w", backend, err)
		}
	}
	// Cancellation can race with the final Enter or key acquisition. Never
	// commit input returned after the command has been interrupted.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := auth.SetSearchKey(backend, key); err != nil {
		return fmt.Errorf("auth keys put %s: %w", backend, err)
	}
	fmt.Fprintf(authKeysStderr(), "✓ saved search:%s\n", backend)
	return nil
}

func readSearchBackendKey(ctx context.Context) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		secret, err := readHiddenTerminalInputContext(ctx, "API key (input hidden): ", maxSearchKeyInputBytes)
		if err != nil {
			return "", fmt.Errorf("read hidden API key: %w", err)
		}
		return normalizeSearchKeyInput([]byte(secret))
	}

	// Non-interactive callers can pipe a key on stdin. The strict bound avoids
	// accidentally reading an unbounded stream while still accommodating every
	// practical provider credential format.
	secret, err := io.ReadAll(io.LimitReader(os.Stdin, maxSearchKeyInputBytes+1))
	defer clear(secret)
	if err != nil {
		return "", fmt.Errorf("read API key from stdin: %w", err)
	}
	return normalizeSearchKeyInput(secret)
}

func normalizeSearchKeyInput(secret []byte) (string, error) {
	if len(secret) > maxSearchKeyInputBytes {
		return "", fmt.Errorf("API key exceeds %d-byte limit", maxSearchKeyInputBytes)
	}
	key := strings.TrimSpace(string(secret))
	if key == "" {
		return "", errors.New("API key is empty")
	}
	if strings.ContainsAny(key, "\x00\r\n") {
		return "", errors.New("API key must be a single line without NUL bytes")
	}
	return key, nil
}

func cmdAuthKeysList() error {
	keys, err := auth.ListSearchKeys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("(no search keys stored — try `metis auth keys put tavily tvly-...`)")
		return nil
	}
	fmt.Printf("# stored in %s\n", auth.Path())
	for _, k := range keys {
		// Never print even a prefix: command output is commonly copied into
		// bug reports and screen shares. Presence and length are sufficient.
		v, _ := auth.GetSearchKey(k)
		fmt.Printf("- %s (configured, %d chars)\n", k, len(v))
	}
	return nil
}

func cmdAuthKeysRemove(args []string) error {
	if len(args) == 0 {
		return errors.New("auth keys rm: backend name required")
	}
	for _, b := range args {
		if err := auth.RemoveSearchKey(b); err != nil {
			return fmt.Errorf("remove search:%s: %w", b, err)
		}
		fmt.Fprintf(os.Stderr, "✓ removed search:%s\n", b)
	}
	return nil
}

var (
	richOAuthLogin             = auth.AcquireOAuthCredential
	openAICodexDeviceCodeLogin = auth.AcquireOpenAICodexDeviceCode
)

// cmdAuthOAuth keeps the historical command spelling while routing supported
// LLM subscription providers through the refreshable credential store. Older
// releases offered GitHub OAuth even though no METIS subsystem consumed that
// token; refuse that misleading path instead of requesting broad, unused
// repository permissions and printing a false success.
func cmdAuthOAuth(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printLegacyOAuthUsage()
		return nil
	}
	opts, manual, help, err := parseLoginArgs(args)
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	if help {
		printLegacyOAuthUsage()
		return nil
	}
	if opts.Method != "" && opts.Method != tui.AuthMethodOAuth {
		return errors.New("oauth: --method must be oauth")
	}
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))
	if provider == "" {
		return errors.New("oauth: provider name required")
	}
	// Preserve the old spelling, but always route it through the rich
	// Anthropic credential path. Sending it through the generic legacy helper
	// would persist only the access token in auth.json and lose refresh data.
	if provider == "anthropic-claudeai" {
		provider = "anthropic"
	}
	if provider == "openai" {
		provider = "openai-codex"
	}
	if provider != "anthropic" && provider != "openai-codex" {
		return fmt.Errorf("oauth: provider %q is not connected to a METIS runtime; use `metis login` for an API-key provider", provider)
	}

	compatArgs := []string{provider, "--method", tui.AuthMethodOAuth}
	if manual {
		compatArgs = append(compatArgs, "--manual")
	}
	if opts.OAuthFlow == tui.OAuthFlowDevice {
		compatArgs = append(compatArgs, "--device-code")
	}
	return cmdAuthLogin(ctx, compatArgs)
}

func printLegacyOAuthUsage() {
	fmt.Println("metis auth oauth — compatibility OAuth login\n\nUsage:\n  metis auth oauth [--manual] <provider>\n\nSupported providers: anthropic, openai (ChatGPT), openai-codex")
}

func printAuthUsage() {
	fmt.Println(`metis auth — manage provider credentials

Usage:
  metis login [provider]      Canonical interactive provider sign-in
  metis auth login [provider] Compatibility alias for metis login
  metis auth logout <prov>    Remove a stored credential
  metis auth list             Show which providers have stored credentials
  metis auth oauth <prov>     Compatibility alias for metis login --method oauth
  metis auth keys <sub>       Manage WebSearch backend keys
                              (try ` + "`metis auth keys help`" + ` for the sub-commands)

Notes:
  API keys and refreshable OAuth credentials are stored locally with mode 0o600.
  Env vars (e.g. ANTHROPIC_API_KEY) still take precedence over stored API keys.`)
}

// cmdAuthLogin runs the bubbletea wizard and prints a confirmation line.
//
// It deliberately does NOT call setupRuntime — the user might be running this
// before any provider is configured at all, which would fail setupRuntime.
func cmdAuthLogin(ctx context.Context, args []string) error {
	opts, manual, help, err := parseLoginArgs(args)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if help {
		printLoginUsage()
		return nil
	}
	opts.Provider = auth.CanonicalProviderID(opts.Provider)
	// Device-code login is fully specified by the flag and is specifically for
	// headless/SSH environments. It must not instantiate Bubble Tea or require
	// a TTY; the user code and verification URL are ordinary stderr output.
	if opts.OAuthFlow == tui.OAuthFlowDevice {
		return completeOAuthLogin(ctx, opts.Provider, opts.OAuthFlow, false)
	}
	if err := validateExplicitLoginProvider(opts.Provider); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	// Bubble Tea reads keys from stdin and renders to stderr. Requiring both
	// prevents a piped/EOF stdin from entering an apparently interactive wizard
	// merely because stderr still points at a terminal.
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return errors.New("login: requires an interactive terminal (stdin and stderr must both be TTYs)")
	}
	configured, err := configuredLoginProviders()
	if err != nil {
		return fmt.Errorf("login: load configured providers: %w", err)
	}
	opts.ConfiguredProviders = configured
	res, err := tui.RunLoginWizard(opts)
	if err != nil {
		if errors.Is(err, tui.ErrAuthCancelled) {
			fmt.Fprintln(os.Stderr, "login: cancelled")
			return nil
		}
		return fmt.Errorf("login: %w", err)
	}
	if res.Method == tui.AuthMethodOAuth {
		flow := res.OAuthFlow
		if flow == "" {
			flow = opts.OAuthFlow
		}
		return completeOAuthLogin(ctx, res.Provider, flow, manual)
	}
	return completeAPIKeyLogin(res)
}

func completeAPIKeyLogin(res tui.AuthResult) error {
	if err := preflightSelectLoggedInProvider(res.Provider); err != nil {
		return fmt.Errorf("login %s with api-key: %w", res.Provider, err)
	}
	if err := tui.PrepareLoginResultForWorkspace(res, currentWorkspaceTrusted()); err != nil {
		return fmt.Errorf("login %s with api-key: %w", res.Provider, err)
	}
	// The explicit wizard only collected the key. Activate it once under the
	// shared cross-store transaction lock so a concurrent OAuth/API-key switch
	// cannot leave both methods installed.
	var activateErr error
	if res.Custom != nil {
		activateErr = auth.ActivateAPIKeyBound(res.Provider, res.Key, res.Custom.Transport, res.Custom.BaseURL)
	} else {
		activateErr = auth.ActivateAPIKey(res.Provider, res.Key)
	}
	if activateErr != nil {
		return fmt.Errorf("login %s with api-key: %w", res.Provider, activateErr)
	}
	if err := selectLoggedInProvider(res.Provider); err != nil {
		return fmt.Errorf("login %s with api-key: credential was saved but selecting it as the default provider failed: %w", res.Provider, err)
	}
	fmt.Fprintf(os.Stderr, "✓ signed in to %s with api-key\n", res.Provider)
	return nil
}

func loadAuthProviderConfig() (*config.Config, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := config.ApplyProviderPolicyForWorkspace(cfg, currentWorkspaceTrusted()); err != nil {
		return nil, err
	}
	return cfg, nil
}

func configuredLoginProviders() ([]tui.ConfiguredLoginProvider, error) {
	cfg, err := loadAuthProviderConfig()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cfg.Provider.Custom))
	for id := range cfg.Provider.Custom {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	providers := make([]tui.ConfiguredLoginProvider, 0, len(ids))
	for _, id := range ids {
		raw := cfg.Provider.Custom[id]
		if !config.CustomProviderSupportsManagedAPIKey(raw) {
			continue
		}
		providers = append(providers, tui.ConfiguredLoginProvider{
			ID: id, Transport: raw.Transport, BaseURL: raw.BaseURL, Model: raw.Model,
		})
	}
	return providers, nil
}

func validateExplicitLoginProvider(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || provider == "anthropic" || provider == "anthropic-claudeai" ||
		provider == "openai" || provider == "openai-codex" || provider == "gemini" ||
		provider == "google" || provider == "custom" {
		return nil
	}
	cfg, err := loadAuthProviderConfig()
	if err != nil {
		return err
	}
	raw, ok := cfg.Provider.Custom[provider]
	if !ok || config.CustomProviderSupportsManagedAPIKey(raw) {
		return nil
	}
	transport := strings.ToLower(strings.TrimSpace(raw.Transport))
	switch transport {
	case "vertex", "vertex_anthropic":
		return fmt.Errorf("provider %q uses Vertex service-account authentication; configure service_account_file, project, and region instead of an API key", provider)
	case "bedrock", "bedrock_anthropic":
		return fmt.Errorf("provider %q uses AWS credentials; configure AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY instead of a single API key", provider)
	default:
		return fmt.Errorf("provider %q uses transport %q, which cannot be configured by the single-key login wizard", provider, transport)
	}
}

func completeOAuthLogin(ctx context.Context, provider, flow string, manual bool) error {
	if err := validateOAuthLoginTarget(provider); err != nil {
		return fmt.Errorf("login %s with oauth: %w", provider, err)
	}
	if err := preflightSelectLoggedInProvider(provider); err != nil {
		return fmt.Errorf("login %s with oauth: %w", provider, err)
	}
	if provider == "anthropic" {
		fmt.Fprintln(os.Stderr, "warning: Anthropic subscription OAuth is experimental for third-party clients; Metis does not impersonate Claude Code")
		fmt.Fprintln(os.Stderr, "warning: Pi reports third-party harness usage as per-token extra usage, not Claude plan allowance; verify billing at https://claude.ai/settings/usage")
	}
	if source, err := higherPriorityAPIKeySource(provider); err != nil {
		return fmt.Errorf("login %s with oauth: %w", provider, err)
	} else if source != "" {
		return fmt.Errorf("login %s with oauth: %s currently takes precedence; remove or unset it before switching authentication methods", provider, source)
	}
	credential, err := loginOAuth(ctx, provider, flow, manual)
	if err != nil {
		return fmt.Errorf("login %s with oauth: %w", provider, err)
	}
	if credential == nil {
		return fmt.Errorf("login %s with oauth: provider returned no credential", provider)
	}
	// Browser/device acquisition can complete concurrently with cancellation.
	// Re-check immediately before the first persistent mutation so a cancelled
	// command never installs a credential that the user meant to abandon.
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("login %s with oauth: %w", provider, err)
		}
	}
	if err := auth.ActivateOAuthContext(ctx, provider, *credential); err != nil {
		return fmt.Errorf("login %s with oauth: %w", provider, err)
	}
	if err := selectLoggedInProvider(provider); err != nil {
		return fmt.Errorf("login %s with oauth: OAuth was saved but selecting it as the default provider failed: %w", provider, err)
	}
	fmt.Fprintf(os.Stderr, "✓ signed in to %s with oauth\n", provider)
	return nil
}

func loginOAuth(ctx context.Context, provider, flow string, manual bool) (*auth.OAuthCredential, error) {
	if flow == tui.OAuthFlowDevice {
		if provider != "openai-codex" {
			return nil, errors.New("device-code login is supported only for openai-codex")
		}
		return openAICodexDeviceCodeLogin(ctx, auth.OpenAICodexDeviceOptions{
			Notify: func(info auth.OpenAICodexDeviceCode) error {
				fmt.Fprintf(os.Stderr, "Open %s and enter code %s\n", info.VerificationURI, info.UserCode)
				return nil
			},
		})
	}
	oauthOptions := auth.OAuthOptions{Manual: manual}
	if manual {
		oauthOptions.PasteCodeContext = readManualOAuthCode
	} else {
		oauthOptions.PasteCodeContext = automaticOAuthPasteCode()
		oauthOptions.FallbackPasteCodeContext = readManualOAuthCode
	}
	return richOAuthLogin(ctx, provider, oauthOptions)
}

func selectLoggedInProvider(provider string) error {
	if err := config.SaveUserProviderDefault(provider); err != nil {
		return err
	}
	cfg, err := loadAuthProviderConfig()
	if err != nil {
		return err
	}
	if cfg.Provider.Default != provider {
		// A project config can appear or change after the login preflight. The
		// credential and user default are still valid; do not report a failed
		// login after both durable writes have already succeeded.
		fmt.Fprintf(os.Stderr, "warning: signed in to %s, but this project overrides provider.default as %q\n", provider, cfg.Provider.Default)
	}
	return nil
}

// preflightSelectLoggedInProvider rejects a stable project-level default
// override before OAuth acquisition or any credential/config mutation. This
// avoids reporting a failed login only after secrets have already been saved.
func preflightSelectLoggedInProvider(provider string) error {
	if !currentWorkspaceTrusted() {
		// Provider routing from an unseen checkout is ignored by the runtime,
		// so it must not block an explicit user-level login either.
		return nil
	}
	source, err := config.ProviderDefaultOverrideSource()
	if err != nil {
		return err
	}
	if source == "" {
		return nil
	}
	cfg, err := loadAuthProviderConfig()
	if err != nil {
		return err
	}
	if cfg.Provider.Default != provider {
		return fmt.Errorf("provider.default is controlled by %s as %q; edit that file before selecting %s", source, cfg.Provider.Default, provider)
	}
	return nil
}

func validateOAuthLoginTarget(provider string) error {
	if provider != "anthropic" {
		return nil
	}
	cfg, err := loadAuthProviderConfig()
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(cfg.Provider.Anthropic.BaseURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("refusing to send Anthropic OAuth credentials to a non-Anthropic base_url; use an API key for custom gateways")
	}
	host := strings.ToLower(u.Hostname())
	if host != "api.anthropic.com" && !strings.HasSuffix(host, ".anthropic.com") {
		return errors.New("refusing to send Anthropic OAuth credentials to a non-Anthropic base_url; use an API key for custom gateways")
	}
	return nil
}

func clearSupersededCredential(provider, activeMethod string) error {
	switch activeMethod {
	case tui.AuthMethodOAuth:
		return auth.Remove(provider)
	case tui.AuthMethodAPIKey:
		return auth.RemoveOAuth(provider)
	default:
		return fmt.Errorf("unknown active credential method %q", activeMethod)
	}
}

// higherPriorityAPIKeySource reports API-key sources that cannot be removed by
// the login command. Stored auth.json entries are intentionally excluded: an
// OAuth login replaces those only after the browser exchange succeeds.
func higherPriorityAPIKeySource(provider string) (string, error) {
	cfg, err := loadAuthProviderConfig()
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		if env := strings.TrimSpace(cfg.Provider.Anthropic.APIKeyEnv); env != "" && os.Getenv(env) != "" {
			return env, nil
		}
		if strings.TrimSpace(cfg.Provider.Anthropic.APIKey) != "" {
			return "provider.anthropic.api_key", nil
		}
	}
	return "", nil
}

// readManualOAuthCode keeps a pasted one-time authorization response out of
// terminal scrollback. Unix implementations poll the raw terminal with the
// caller context so Ctrl-C restores terminal state without waiting for Enter.
func readManualOAuthCode(ctx context.Context, _ string) (string, error) {
	return readHiddenOAuthCodeContext(ctx, "Paste authorization code or redirect URL (input hidden): ")
}

func parseLoginArgs(args []string) (tui.LoginOptions, bool, bool, error) {
	var opts tui.LoginOptions
	manual := false
	deviceCode := false
	help := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			help = true
		case arg == "--manual" || arg == "-m":
			manual = true
		case arg == "--device-code" || arg == "--device":
			deviceCode = true
		case arg == "--method":
			i++
			if i >= len(args) {
				return opts, false, false, errors.New("--method requires api-key or oauth")
			}
			opts.Method = args[i]
		case strings.HasPrefix(arg, "--method="):
			opts.Method = strings.TrimPrefix(arg, "--method=")
		case strings.HasPrefix(arg, "-"):
			return opts, false, false, fmt.Errorf("unknown flag %q", arg)
		default:
			if opts.Provider != "" {
				return opts, false, false, fmt.Errorf("unexpected argument %q; only one provider may be selected", arg)
			}
			opts.Provider = arg
		}
	}
	method := strings.ToLower(strings.TrimSpace(opts.Method))
	if method != "" && method != tui.AuthMethodAPIKey && method != tui.AuthMethodOAuth {
		return opts, false, false, fmt.Errorf("unknown method %q; use api-key or oauth", opts.Method)
	}
	opts.Method = method
	if manual {
		if opts.Method == tui.AuthMethodAPIKey {
			return opts, false, false, errors.New("--manual is only valid with --method oauth")
		}
		opts.Method = tui.AuthMethodOAuth
		opts.OAuthFlow = tui.OAuthFlowBrowser
	}
	if deviceCode {
		if manual {
			return opts, false, false, errors.New("--device-code and --manual cannot be used together")
		}
		if opts.Method == tui.AuthMethodAPIKey {
			return opts, false, false, errors.New("--device-code is only valid with --method oauth")
		}
		if strings.TrimSpace(opts.Provider) == "" || strings.EqualFold(strings.TrimSpace(opts.Provider), "openai") {
			opts.Provider = "openai-codex"
		}
		if !strings.EqualFold(strings.TrimSpace(opts.Provider), "openai-codex") {
			return opts, false, false, errors.New("--device-code is supported only for openai-codex")
		}
		opts.Method = tui.AuthMethodOAuth
		opts.Provider = "openai-codex"
		opts.OAuthFlow = tui.OAuthFlowDevice
	}
	return opts, manual, help, nil
}

// loginArgsWithLeadingGlobals preserves the global-before-subcommand syntax
// without allowing flag meanings to change when dispatch hoists `login`.
// Provider is the only global setting that has a useful login equivalent.
func loginArgsWithLeadingGlobals(prefix, loginArgs []string) ([]string, error) {
	return providerArgsWithLeadingGlobals("login", prefix, loginArgs)
}

func logoutArgsWithLeadingGlobals(prefix, logoutArgs []string) ([]string, error) {
	return providerArgsWithLeadingGlobals("logout", prefix, logoutArgs)
}

func providerArgsWithLeadingGlobals(command string, prefix, commandArgs []string) ([]string, error) {
	provider := ""
	for i := 0; i < len(prefix); i++ {
		arg := prefix[i]
		switch {
		case arg == "-p" || arg == "--provider":
			i++
			if i >= len(prefix) || strings.TrimSpace(prefix[i]) == "" {
				return nil, fmt.Errorf("%s: %s requires a provider id", command, arg)
			}
			provider = prefix[i]
		case strings.HasPrefix(arg, "--provider="):
			provider = strings.TrimSpace(strings.TrimPrefix(arg, "--provider="))
			if provider == "" {
				return nil, fmt.Errorf("%s: --provider requires a provider id", command)
			}
		default:
			return nil, fmt.Errorf("%s: leading global flag %q is not applicable; put `%s` first and use its flags", command, arg, command)
		}
	}
	out := append([]string(nil), commandArgs...)
	if provider != "" {
		out = append([]string{provider}, out...)
	}
	return out, nil
}

func printLoginUsage() {
	fmt.Println(`metis login — sign in to an LLM provider

Usage:
  metis login [provider] [--method api-key|oauth] [--manual|--device-code]

Providers:
  anthropic       API key or experimental Claude subscription OAuth
  openai          Sign in with ChatGPT in your browser, or use an API key
  openai-codex    Compatibility entry for ChatGPT account sign-in
  gemini          Google AI Studio API key
  custom          Configure an API-compatible provider and API key

Flags:
  --method <kind>  Preselect api-key or oauth
  --manual, -m     OAuth code-paste flow for SSH or headless environments
  --device-code    OpenAI Codex device-code flow for headless environments
  --help, -h       Show this help

Examples:
  metis login openai                     Choose ChatGPT account or API key
  metis login openai --method oauth      Open the ChatGPT browser sign-in
  metis login openai --device-code       Sign in from SSH or headless devices
  metis login openai --method api-key    Use OpenAI Platform API billing

ChatGPT sign-in selects the openai-codex provider for subsequent chats.
Model access and usage follow your ChatGPT account's available entitlements.

Compatibility:
  metis auth login ... and metis auth oauth ... remain supported.`)
}

func cmdAuthLogout(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printLogoutUsage()
		return nil
	}
	if len(args) == 0 {
		return errors.New("auth logout: provider id required")
	}
	apiProviders, err := auth.List()
	if err != nil {
		return fmt.Errorf("list API-key credentials: %w", err)
	}
	oauthProviders, err := auth.ListOAuth()
	if err != nil {
		return fmt.Errorf("list OAuth credentials: %w", err)
	}
	stored := make(map[string]bool, len(apiProviders)+len(oauthProviders))
	for _, provider := range append(apiProviders, oauthProviders...) {
		stored[auth.CanonicalProviderID(provider)] = true
	}
	removedAny := false
	for _, p := range args {
		provider := auth.CanonicalProviderID(p)
		if provider == "" {
			return errors.New("auth logout: provider id required")
		}
		if !stored[provider] {
			fmt.Fprintf(os.Stderr, "- no stored credential for %s\n", provider)
			continue
		}
		if err := auth.RemoveProviderCredentials(provider); err != nil {
			return fmt.Errorf("remove %s: %w", provider, err)
		}
		delete(stored, provider)
		removedAny = true
		fmt.Fprintf(os.Stderr, "✓ removed %s\n", provider)
	}
	if removedAny {
		fmt.Fprintln(os.Stderr, "note: environment variables and inline config are unchanged and may still authenticate after restart")
	}
	return nil
}

func printLogoutUsage() {
	fmt.Println(`metis logout — remove stored provider credentials

Usage:
  metis logout <provider> [provider...]

Compatibility:
  metis auth logout <provider> remains supported.

Note:
  Environment variables and inline config are not removed.`)
}

func cmdAuthList() error {
	apiIDs, err := auth.List()
	if err != nil {
		return err
	}
	oauthIDs, err := auth.ListOAuth()
	if err != nil {
		return err
	}
	typesByProvider := make(map[string][]string, len(apiIDs)+len(oauthIDs))
	for _, id := range apiIDs {
		typesByProvider[id] = append(typesByProvider[id], tui.AuthMethodAPIKey)
	}
	for _, id := range oauthIDs {
		typesByProvider[id] = append(typesByProvider[id], tui.AuthMethodOAuth)
	}
	if len(typesByProvider) == 0 {
		fmt.Println("(no credentials stored — try `metis login`)")
		return nil
	}
	ids := make([]string, 0, len(typesByProvider))
	for id := range typesByProvider {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Println("# configured provider credentials")
	for _, id := range ids {
		fmt.Printf("- %s (%s)\n", id, strings.Join(typesByProvider[id], ", "))
	}
	return nil
}
