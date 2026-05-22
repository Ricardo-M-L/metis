package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/tui"
)

// cmdAuth is the entry point for `metis auth <subcommand>`.
//
// Subcommands intentionally mirror opencode's surface so muscle memory carries
// over: `login`, `logout`, `list`. We don't implement opencode's `oauth` flow
// — every provider Metis supports today (Anthropic / OpenAI / MiniMax /
// Gemini-OAI / custom) takes a flat API key, so the simpler password prompt
// is enough.
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
		return cmdAuthOAuth(rest)
	case "keys":
		return cmdAuthKeys(rest)
	case "help", "-h", "--help":
		printAuthUsage()
		return nil
	}
	return fmt.Errorf("auth: unknown subcommand %q (use: login | logout | list | oauth | keys)", sub)
}

// cmdAuthKeys handles `metis auth keys <put|list|rm> ...` for
// non-LLM credentials — currently just WebSearch backend keys
// (Tavily / Brave / Serper). Distinct subcommand on purpose:
// `metis auth login` is for LLM providers and runs the bubbletea
// wizard; search-backend keys are flat strings written
// non-interactively so a `keys put` one-liner is the right shape.
func cmdAuthKeys(args []string) error {
	if len(args) == 0 {
		printAuthKeysUsage()
		return nil
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "put", "set":
		return cmdAuthKeysPut(rest)
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
  metis auth keys put <backend> <key>   Store a WebSearch backend key
  metis auth keys list                   List stored search-backend keys
  metis auth keys rm <backend>           Remove a stored search-backend key

Known backends: tavily, brave, serper

Notes:
  Stored under "search:<backend>" in ~/.metis/auth.json (0o600). Env vars
  (TAVILY_API_KEY / BRAVE_SEARCH_API_KEY / SERPER_API_KEY) still take
  precedence — useful for CI overrides without touching the file.

  Free tiers:
    tavily  — 1k searches/mo, tavily.com
    brave   — 2k queries/mo, api.search.brave.com
    serper  — paid Google SERP, serper.dev`)
}

func cmdAuthKeysPut(args []string) error {
	if len(args) < 2 {
		return errors.New("auth keys put: usage `metis auth keys put <backend> <key>`")
	}
	backend, key := args[0], args[1]
	if err := auth.SetSearchKey(backend, key); err != nil {
		return fmt.Errorf("auth keys put %s: %w", backend, err)
	}
	fmt.Fprintf(os.Stderr, "✓ saved search:%s\n", backend)
	return nil
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
		// Show only the prefix so a screen-share doesn't leak the
		// full key. Same shape as `metis diag` uses for provider
		// keys (safePrefix(v, 6) + "…").
		v, _ := auth.GetSearchKey(k)
		head := v
		if len(head) > 6 {
			head = head[:6] + "…"
		}
		fmt.Printf("- %s (%s)\n", k, head)
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

// cmdAuthOAuth runs the browser-based OAuth flow for the named
// provider. Spawns a localhost callback server, opens browser to
// the provider's auth URL, exchanges the code for a token, and
// persists to auth.json — same store the rest of metis already
// reads from.
//
// Pass --manual to switch to paste-the-code mode for non-browser
// environments (SSH, headless, locked-down corp). The auth URL is
// printed to stderr and the user pastes the resulting code back
// to stdin. Mirrors claude-code-sourcemap's manual flow path.
func cmdAuthOAuth(args []string) error {
	if len(args) == 0 {
		fmt.Println("metis auth oauth — browser-based OAuth login")
		fmt.Println()
		fmt.Println("Usage: metis auth oauth [--manual] <provider>")
		fmt.Println()
		fmt.Print("Known providers: ")
		first := true
		for name := range auth.KnownProviders {
			if !first {
				fmt.Print(", ")
			}
			fmt.Print(name)
			first = false
		}
		fmt.Println()
		fmt.Println()
		fmt.Println("Flags:")
		fmt.Println("  --manual    Paste-the-code flow for SSH / no-browser envs.")
		fmt.Println("              Auth URL printed to stderr; user pastes the")
		fmt.Println("              displayed code back to stdin. Required when the")
		fmt.Println("              browser runs on a different host than metis.")
		return nil
	}
	manual := false
	provider := ""
	for _, a := range args {
		switch a {
		case "--manual", "-m":
			manual = true
		default:
			if provider == "" {
				provider = a
			}
		}
	}
	if provider == "" {
		return fmt.Errorf("oauth: provider name required")
	}
	if manual {
		fmt.Printf("Starting OAuth flow for %s — manual paste mode...\n", provider)
	} else {
		fmt.Printf("Starting OAuth flow for %s — browser will open shortly...\n", provider)
	}
	tok, err := auth.OAuthLoginOpts(provider, auth.OAuthOptions{Manual: manual})
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	suffix := ""
	if !tok.ExpiresAt.IsZero() {
		suffix = fmt.Sprintf(", expires_in=%s", tok.ExpiresAt.Sub(timeNow()).Round(time.Second))
	}
	fmt.Printf("✓ %s authorized — token saved to auth.json (length=%d%s)\n",
		provider, len(tok.AccessToken), suffix)
	return nil
}

// timeNow is a var to let tests freeze the clock in expires_in math.
var timeNow = time.Now

func printAuthUsage() {
	fmt.Println(`metis auth — manage provider credentials in ~/.metis/auth.json

Usage:
  metis auth login            Interactive wizard: pick provider + enter key
  metis auth logout <prov>    Remove a stored credential
  metis auth list             Show which providers have stored credentials
  metis auth oauth <prov>     Browser-based OAuth login (github, etc.)
  metis auth keys <sub>       Manage WebSearch backend keys
                              (try ` + "`metis auth keys help`" + ` for the sub-commands)

Notes:
  Stored as 0o600. Env vars (e.g. ANTHROPIC_API_KEY) still take precedence
  over auth.json so existing CI flows keep working unchanged.`)
}

// cmdAuthLogin runs the bubbletea wizard and prints a confirmation line.
//
// It deliberately does NOT call setupRuntime — the user might be running this
// before any provider is configured at all, which would fail setupRuntime.
func cmdAuthLogin(ctx context.Context, args []string) error {
	// Bail early if stderr isn't a tty: the wizard requires interactive input.
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return errors.New("auth login: requires an interactive terminal (stderr is not a tty)")
	}
	res, err := tui.RunAuthWizard()
	if err != nil {
		if errors.Is(err, tui.ErrAuthCancelled) {
			fmt.Fprintln(os.Stderr, "auth login: cancelled")
			return nil
		}
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ saved credentials for %s\n", res.Provider)
	_ = ctx
	return nil
}

func cmdAuthLogout(args []string) error {
	if len(args) == 0 {
		return errors.New("auth logout: provider id required")
	}
	for _, p := range args {
		if err := auth.Remove(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		fmt.Fprintf(os.Stderr, "✓ removed %s\n", p)
	}
	return nil
}

func cmdAuthList() error {
	ids, err := auth.List()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("(no credentials stored — try `metis auth login`)")
		return nil
	}
	fmt.Printf("# stored in %s\n", auth.Path())
	for _, id := range ids {
		fmt.Println("- " + id)
	}
	return nil
}
