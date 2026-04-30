package main

import (
	"context"
	"errors"
	"fmt"
	"os"

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
	case "help", "-h", "--help":
		printAuthUsage()
		return nil
	}
	return fmt.Errorf("auth: unknown subcommand %q (use: login | logout | list | oauth)", sub)
}

// cmdAuthOAuth runs the browser-based OAuth flow for the named
// provider. Spawns a localhost callback server, opens browser to
// the provider's auth URL, exchanges the code for a token, and
// persists to auth.json — same store the rest of metis already
// reads from.
func cmdAuthOAuth(args []string) error {
	if len(args) == 0 {
		fmt.Println("metis auth oauth — browser-based OAuth login")
		fmt.Println()
		fmt.Println("Usage: metis auth oauth <provider>")
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
		return nil
	}
	provider := args[0]
	fmt.Printf("Starting OAuth flow for %s — browser will open shortly...\n", provider)
	token, err := auth.OAuthLogin(provider)
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	fmt.Printf("✓ %s authorized — token saved to auth.json (length=%d)\n", provider, len(token))
	return nil
}

func printAuthUsage() {
	fmt.Println(`metis auth — manage provider credentials in ~/.metis/auth.json

Usage:
  metis auth login            Interactive wizard: pick provider + enter key
  metis auth logout <prov>    Remove a stored credential
  metis auth list             Show which providers have stored credentials
  metis auth oauth <prov>     Browser-based OAuth login (github, etc.)

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
