package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// WizardResult is what a successful auth wizard returns. We don't depend
// on the tui package directly so this layer stays UI-agnostic — the
// caller wires up tui.RunAuthWizard via the WizardFn injection point.
type WizardResult struct {
	Provider string
	Key      string
}

// WizardFn is the contract a caller passes in to drive the first-run
// wizard. main.go injects tui.RunAuthWizard wrapped to this signature;
// tests inject a fake.
//
// The error return distinguishes "user cancelled" from a programming /
// IO error — sentinel ErrWizardCancelled signals the former so the gate
// returns a clear "auth setup cancelled" message instead of bubbling up
// a generic failure.
type WizardFn func() (*WizardResult, error)

// ErrWizardCancelled is the sentinel a WizardFn returns when the user
// pressed Esc / Ctrl-C during the wizard. EnsureAPIKey turns it into a
// caller-friendly error suggesting `metis auth login`.
var ErrWizardCancelled = errors.New("wizard cancelled")

// AuthGateOptions tunes EnsureAPIKey behavior. Any function field left
// nil falls back to a sensible default.
type AuthGateOptions struct {
	// NoWizard skips the wizard even when stderr is a tty (--no-auth-wizard).
	NoWizard bool
	// IsTTY reports whether the wizard's UI surface (typically stderr)
	// is interactive. Tests inject `func() bool { return false }` to
	// disable the wizard cleanly without touching the global FD.
	IsTTY func() bool
	// RunWizard launches the interactive flow. Required when the gate
	// would otherwise want to invoke it.
	RunWizard WizardFn
	// Stderr receives the "no API key found — launching first-run setup"
	// banner. Defaults to os.Stderr when nil.
	Stderr io.Writer
}

// EnsureAPIKey verifies cfg has the credentials required by `provName`.
// API-key transports use ResolveAPIKey; Vertex service-account and Bedrock AWS
// credentials use the same transport-aware preflight as the model picker. When
// credentials are missing AND the wizard is allowed AND stderr is a tty, the
// wizard runs to completion and the caller gets the (possibly different)
// chosen provider plus a fresh cfg with auth.json picked up.
//
// Returns:
//   - the cfg the caller should use going forward (may be a fresh load
//     after the wizard wrote auth.json)
//   - the resolved provider name (the wizard might have picked a different
//     provider than the cfg default)
//   - error: missing key + can't run wizard, OR wizard cancelled, OR
//     wizard returned a non-cancellation error
func EnsureAPIKey(cfg *config.Config, provName string, opts AuthGateOptions) (*config.Config, string, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	// Happy path: the active transport's credentials already exist, no wizard
	// needed. In particular, do not send a correctly configured Vertex or
	// Bedrock profile through an API-key wizard.
	if ProviderHasCredentials(cfg, provName) {
		return cfg, provName, nil
	}

	// Decide whether to launch the wizard. Three conditions must be true:
	// caller hasn't explicitly disabled, stderr is interactive, and we
	// have a wizard fn to call.
	canWizard := !opts.NoWizard && opts.RunWizard != nil
	if canWizard && opts.IsTTY != nil && opts.IsTTY() {
		fmt.Fprintln(stderr, "metis: no API key found — launching first-run setup")
		res, werr := opts.RunWizard()
		if werr != nil {
			if errors.Is(werr, ErrWizardCancelled) {
				return nil, "", errors.New("auth setup cancelled — re-run `metis auth login` or set the api_key_env env var")
			}
			return nil, "", werr
		}
		if res != nil && res.Provider != "" && res.Provider != provName && IsKnownProvider(cfg, res.Provider) {
			provName = res.Provider
		}
		// Reload config so any state the wizard wrote (auth.json) is
		// reflected for the rest of bootstrap.
		if newCfg, _, lerr := config.Load(); lerr == nil {
			cfg = newCfg
		}
	}

	// After the wizard run (or skipped), final check. If still missing,
	// surface a clear error pointing the user at remediation.
	if !ProviderHasCredentials(cfg, provName) {
		return nil, "", missingProviderCredentialsError(cfg, provName)
	}
	return cfg, provName, nil
}

func missingProviderCredentialsError(cfg *config.Config, provName string) error {
	if cfg != nil {
		if raw, ok := cfg.Provider.Custom[provName]; ok {
			switch normalizedCustomTransport(raw) {
			case "vertex_anthropic", "vertex":
				return fmt.Errorf("missing credentials for provider %q: set service_account_file to a readable GCP service-account JSON file", provName)
			case "bedrock_anthropic", "bedrock":
				return fmt.Errorf("missing credentials for provider %q: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (or api_key_env and secret_key_env)", provName)
			}
		}
		if _, err := cfg.ResolveAPIKey(provName); err != nil {
			return fmt.Errorf("%w: try `metis auth login` or export the api_key_env", err)
		}
	}
	return fmt.Errorf("missing credentials for provider %q", provName)
}

// IsKnownProvider reports whether the given provider id is one setupRuntime
// can wire up: anthropic, openai, or any id under [provider.custom].
//
// Exposed so the wizard's "user picked a different provider" branch can
// gate before changing provName. Returns false for unknown ids; the caller
// keeps its original choice.
func IsKnownProvider(cfg *config.Config, id string) bool {
	switch id {
	case "anthropic", "openai":
		return true
	}
	if cfg == nil {
		return false
	}
	if _, ok := cfg.Provider.Custom[id]; ok {
		return true
	}
	return false
}
