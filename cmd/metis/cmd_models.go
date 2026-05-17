package main

// `metis models` — list LLM providers + models from the public
// catalog at https://models.dev. opencode and crush use the same
// upstream catalog, so model availability + capability metadata stays
// in sync with the broader ecosystem without metis maintaining its
// own database.
//
// Subcommands:
//   metis models                 list providers (id, name, transport-hint, model count)
//   metis models <provider>      list that provider's models with context window + cost
//   metis models <provider> <id> deep-dive on one model (capabilities + cost breakdown)
//   metis models status          cache freshness + coverage; debug "is catalog warm?"
//   metis models status <id>     same, plus every catalog row matching <id>
//   metis models --refresh       force a network refresh of the cache

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
)

func cmdModels(ctx context.Context, args []string) error {
	refresh := false
	rest := args[:0:0]
	for _, a := range args {
		if a == "--refresh" || a == "-r" {
			refresh = true
			continue
		}
		rest = append(rest, a)
	}

	home := config.Home()
	client := catalog.NewClient(home)
	if refresh {
		// Bounded refresh — don't hang the CLI on a slow network.
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := client.Refresh(ctx2); err != nil {
			fmt.Fprintf(os.Stderr, "metis: models refresh: %v\n", err)
			// Fall through and try Get — disk fallback may still serve.
		}
	}

	// `status` subcommand handles its own catalog load — it must work
	// when the catalog itself is broken (network down + empty cache),
	// which is exactly the case `status` exists to surface.
	if len(rest) >= 1 && rest[0] == "status" {
		return printCatalogStatus(ctx, client, rest[1:])
	}

	cat, err := client.Get(ctx)
	if err != nil {
		return fmt.Errorf("models: %w", err)
	}

	switch len(rest) {
	case 0:
		return printProviderList(cat)
	case 1:
		return printProviderDetail(cat, rest[0])
	case 2:
		return printModelDetail(cat, rest[0], rest[1])
	default:
		return fmt.Errorf("usage: metis models [<provider> [<model>]] [--refresh] | metis models status [<model-id>]")
	}
}

// printCatalogStatus dumps cache freshness + coverage, optionally
// followed by every catalog row matching a model id. Built so the
// user can answer "is my GLM-5.1 reading coming from catalog or a
// hardcoded fallback?" without grepping ~/.metis/cache/models.json
// or reading code.
//
// Uses Stat() (no I/O) rather than Get() so it works even when the
// network is down + cache is empty — which is exactly when the user
// needs to know.
func printCatalogStatus(ctx context.Context, client *catalog.Client, args []string) error {
	// Best-effort load so Stat sees in-memory state. Ignore errors —
	// Stat will surface empty/loaded=false if the load failed.
	loadCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = client.Get(loadCtx)

	st := client.Stat()
	fmt.Printf("Cache path     : %s\n", st.CachePath)
	if st.CacheBytes > 0 {
		fmt.Printf("Cache size     : %d bytes\n", st.CacheBytes)
	}
	if !st.CacheModTime.IsZero() {
		age := time.Since(st.CacheModTime)
		fmt.Printf("Cache mtime    : %s (%s ago)\n",
			st.CacheModTime.Format(time.RFC3339),
			age.Round(time.Minute))
	} else {
		fmt.Println("Cache mtime    : (no on-disk cache yet — try `metis models --refresh`)")
	}
	fmt.Printf("In-memory      : loaded=%v providers=%d models=%d\n",
		st.InMemory, st.ProviderCount, st.ModelCount)
	fmt.Printf("Source URL     : %s\n", catalog.DefaultURL)
	fmt.Printf("Default TTL    : %s\n", catalog.DefaultTTL)

	if len(args) == 0 {
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: metis models status [<model-id>]")
	}
	modelID := args[0]
	hits := client.LookupModel(modelID)
	fmt.Println()
	if len(hits) == 0 {
		fmt.Printf("Lookup %q: no matches. metis will fall back to suffix/prefix tables.\n", modelID)
		fmt.Println("  (Try `metis models --refresh` if you expect this model to be in models.dev.)")
		return nil
	}
	fmt.Printf("Lookup %q: %d catalog row(s)\n", modelID, len(hits))
	for _, h := range hits {
		fmt.Printf("  %s/%s — context=%d output=%d tool=%v reasoning=%v\n",
			h.ProviderID, h.Model.ID,
			h.Model.Limit.Context, h.Model.Limit.Output,
			h.Model.ToolCall, h.Model.Reasoning)
	}
	return nil
}

func printProviderList(cat catalog.Catalog) error {
	ids := make([]string, 0, len(cat))
	for id := range cat {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tNAME\tTRANSPORT\tMODELS\tENV")
	for _, id := range ids {
		p := cat[id]
		t := catalog.TransportHint(p.NPM)
		envs := strings.Join(p.Env, ",")
		if len(envs) > 30 {
			envs = envs[:27] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", id, p.Name, t, len(p.Models), envs)
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "\n%d providers; transport=unsupported means metis cannot drive that provider yet (use opencode/crush for those).\n", len(ids))
	return nil
}

func printProviderDetail(cat catalog.Catalog, id string) error {
	p, ok := cat[id]
	if !ok {
		// Suggest near matches — typo tolerance without bringing in
		// fuzzysort. Just substring scan over provider ids.
		var hints []string
		needle := strings.ToLower(id)
		for k := range cat {
			if strings.Contains(strings.ToLower(k), needle) {
				hints = append(hints, k)
			}
		}
		if len(hints) > 0 {
			sort.Strings(hints)
			return fmt.Errorf("provider %q not found in catalog. Did you mean: %s", id, strings.Join(hints, ", "))
		}
		return fmt.Errorf("provider %q not found in catalog (try `metis models` to list all)", id)
	}

	fmt.Printf("Provider: %s (%s)\n", id, p.Name)
	if p.API != "" {
		fmt.Printf("  default api : %s\n", p.API)
	}
	fmt.Printf("  transport   : %s\n", catalog.TransportHint(p.NPM))
	fmt.Printf("  npm package : %s\n", p.NPM)
	if len(p.Env) > 0 {
		fmt.Printf("  env vars    : %s\n", strings.Join(p.Env, ", "))
	}
	if p.Doc != "" {
		fmt.Printf("  docs        : %s\n", p.Doc)
	}
	fmt.Println()

	ids := make([]string, 0, len(p.Models))
	for k := range p.Models {
		ids = append(ids, k)
	}
	sort.Strings(ids)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCONTEXT\tOUTPUT\tTOOL\tCOST IN\tCOST OUT\tSTATUS")
	for _, k := range ids {
		m := p.Models[k]
		tool := "-"
		if m.ToolCall {
			tool = "✓"
		}
		status := m.Status
		if status == "" {
			status = "stable"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%.2f\t%.2f\t%s\n",
			k, m.Limit.Context, m.Limit.Output, tool, m.Cost.Input, m.Cost.Output, status)
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "\nCost is per million tokens (USD). %d models.\n", len(ids))
	return nil
}

func printModelDetail(cat catalog.Catalog, providerID, modelID string) error {
	p, ok := cat[providerID]
	if !ok {
		return fmt.Errorf("provider %q not found", providerID)
	}
	m, ok := p.Models[modelID]
	if !ok {
		// Try near-match on the model id only (within the right
		// provider) — most common typo is case or version suffix.
		var hints []string
		needle := strings.ToLower(modelID)
		for k := range p.Models {
			if strings.Contains(strings.ToLower(k), needle) {
				hints = append(hints, k)
			}
		}
		if len(hints) > 0 {
			sort.Strings(hints)
			return fmt.Errorf("model %q not found under %s. Did you mean: %s",
				modelID, providerID, strings.Join(hints, ", "))
		}
		return fmt.Errorf("model %q not found under provider %q", modelID, providerID)
	}

	fmt.Printf("Model: %s (under %s)\n", modelID, providerID)
	if m.Name != "" {
		fmt.Printf("  display name : %s\n", m.Name)
	}
	if m.Family != "" {
		fmt.Printf("  family       : %s\n", m.Family)
	}
	if m.ReleaseDate != "" {
		fmt.Printf("  released     : %s\n", m.ReleaseDate)
	}
	if m.Status != "" {
		fmt.Printf("  status       : %s\n", m.Status)
	}
	fmt.Printf("  context win  : %d tokens\n", m.Limit.Context)
	fmt.Printf("  output max   : %d tokens\n", m.Limit.Output)
	fmt.Printf("  reasoning    : %v\n", m.Reasoning)
	fmt.Printf("  tool call    : %v\n", m.ToolCall)
	fmt.Printf("  attachments  : %v\n", m.Attachment)
	if m.Cost.Input > 0 || m.Cost.Output > 0 {
		fmt.Printf("  cost in/out  : $%.4f / $%.4f per million tokens\n", m.Cost.Input, m.Cost.Output)
	}
	if m.Cost.CacheRead > 0 {
		fmt.Printf("  cache read   : $%.4f per million tokens\n", m.Cost.CacheRead)
	}

	// Show how to wire this exact provider/model into config.toml so
	// the user can copy-paste rather than re-typing the base_url.
	if t := catalog.TransportHint(p.NPM); t != "unsupported" {
		fmt.Println()
		fmt.Println("To use this in metis, add to ~/.metis/config.toml:")
		fmt.Println()
		fmt.Printf("  [provider.custom.%s]\n", providerID)
		fmt.Printf("  transport   = %q\n", t)
		fmt.Printf("  api_key_env = %q\n", firstEnv(p.Env))
		if p.API != "" {
			fmt.Printf("  base_url    = %q\n", p.API)
		}
		fmt.Printf("  model       = %q\n", modelID)
		if m.Limit.Context > 0 {
			fmt.Printf("  context_window = %d\n", m.Limit.Context)
		}
		fmt.Println()
		fmt.Printf("Then run: metis -p %s chat\n", providerID)
	} else {
		fmt.Println()
		fmt.Printf("Note: %s uses npm=%q which metis doesn't have a transport for. opencode/crush support it.\n", providerID, p.NPM)
	}
	return nil
}

func firstEnv(envs []string) string {
	if len(envs) == 0 {
		return ""
	}
	return envs[0]
}
