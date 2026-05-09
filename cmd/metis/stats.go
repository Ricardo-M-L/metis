package main

// stats.go — `metis stats` command. Reads ~/.metis/sessions/*.jsonl,
// aggregates into a per-day / per-hour / per-model breakdown via
// internal/stats, renders a self-contained HTML page to
// ~/.metis/stats/index.html, and (unless --no-open) opens the user's
// browser. Mirrors crush's `cmd/stats.go` UX.
//
// The HTML file is everything-inline (CSS, no external JS) so it can
// be emailed or copied to another machine and still render correctly
// — same offline-friendly principle as the rest of metis.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	gort "runtime"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/stats"
)

// cmdStats parses --no-open / --output flags, runs the aggregator,
// writes the HTML, and offers to open it in a browser.
func cmdStats(ctx context.Context, args []string) error {
	_ = ctx
	noOpen := false
	outputPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-open":
			noOpen = true
		case "--output", "-o":
			if i+1 >= len(args) {
				return errors.New("stats: --output requires a path")
			}
			outputPath = args[i+1]
			i++
		case "--help", "-h":
			fmt.Println("metis stats — write an HTML usage dashboard")
			fmt.Println()
			fmt.Println("Usage: metis stats [flags]")
			fmt.Println()
			fmt.Println("Reads ~/.metis/sessions/*.jsonl and writes a self-contained HTML")
			fmt.Println("page summarising session count, models used, and a heatmap of")
			fmt.Println("activity by day-of-week × hour. Output is offline-friendly — no")
			fmt.Println("external JS or CSS dependencies.")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --output PATH   Write to PATH instead of ~/.metis/stats/index.html")
			fmt.Println("  --no-open       Don't open the browser after writing")
			return nil
		default:
			return fmt.Errorf("stats: unknown flag %q (try --help)", args[i])
		}
	}

	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("stats: load config: %w", err)
	}
	sessionsDir := cfg.Session.Dir
	if sessionsDir == "" {
		sessionsDir = filepath.Join(config.Home(), "sessions")
	}

	s, err := stats.Aggregate(sessionsDir)
	if err != nil {
		return fmt.Errorf("stats: aggregate: %w", err)
	}

	html, err := stats.Render(s)
	if err != nil {
		return fmt.Errorf("stats: render: %w", err)
	}

	if outputPath == "" {
		outDir := filepath.Join(config.Home(), "stats")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("stats: mkdir output: %w", err)
		}
		outputPath = filepath.Join(outDir, "index.html")
	}
	if err := os.WriteFile(outputPath, []byte(html), 0o644); err != nil {
		return fmt.Errorf("stats: write output: %w", err)
	}

	fmt.Fprintf(os.Stderr, "stats · %d sessions · %d messages · written to %s\n",
		s.Total.Sessions, s.Total.Messages, outputPath)

	if !noOpen {
		_ = openInBrowser(outputPath)
	}
	return nil
}

// openInBrowser launches the user's default browser to view the HTML
// file. Best-effort — falls back to nothing if the OS-specific
// command isn't available; the caller's printed path is the fallback
// affordance.
func openInBrowser(path string) error {
	switch gort.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "linux":
		return exec.Command("xdg-open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	}
	return fmt.Errorf("unsupported platform: %s", gort.GOOS)
}
