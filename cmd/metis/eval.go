package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/eval"
)

// cmdEval implements `metis eval` — runs the markdown scenario pack
// against a metis binary and prints a summary + optional JSONL.
//
// The default binary is the currently-running metis (os.Args[0]);
// override with --binary to test a different build.
func cmdEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "eval/scenarios", "directory containing scenario *.md files")
	out := fs.String("out", "", "write JSONL results to this file (in addition to console summary)")
	tag := fs.String("tag", "", "only run scenarios with this tag (smoke, regression, ...)")
	binary := fs.String("binary", "", "path to metis binary (default: this process)")
	provider := fs.String("provider", "", "override provider for scenarios (forwarded as -p to metis run)")
	model := fs.String("model", "", "override model for scenarios (forwarded as -m to metis run)")
	verbose := fs.Bool("verbose", false, "stream subprocess stderr live")
	grace := fs.Duration("grace", 5*time.Second, "extra time on top of each scenario's timeout")
	if err := fs.Parse(args); err != nil {
		evalUsage()
		return err
	}

	scenarios, err := eval.LoadScenarios(*dir)
	if err != nil {
		return fmt.Errorf("load scenarios from %s: %w", *dir, err)
	}
	if len(scenarios) == 0 {
		return fmt.Errorf("no scenarios found under %s", *dir)
	}
	if *tag != "" {
		scenarios = filterByTag(scenarios, *tag)
		if len(scenarios) == 0 {
			return fmt.Errorf("no scenarios with tag %q under %s", *tag, *dir)
		}
	}

	bin := *binary
	if bin == "" {
		// Resolve self (os.Args[0]) to an absolute path so the
		// subprocess doesn't depend on $PATH order.
		self, err := os.Executable()
		if err != nil {
			bin = "metis"
		} else {
			bin = self
		}
	}

	rn := &eval.Runner{
		MetisBinary: bin,
		GlobalGrace: *grace,
	}
	// --provider / --model forward as `-p X` / `-m Y` to each
	// `metis run` subprocess. Use case: default provider has a known
	// quirk for tool-calling scenarios (e.g. minimax empty-args bug
	// in project_minimax_function_calling_bug.md), so the user runs
	// `metis eval --provider deepseek` to swap to a known-good route
	// for the eval batch without changing global config.
	if *provider != "" {
		rn.ExtraArgs = append(rn.ExtraArgs, "-p", *provider)
	}
	if *model != "" {
		rn.ExtraArgs = append(rn.ExtraArgs, "-m", *model)
	}
	if *verbose {
		rn.StreamLogs = os.Stderr
	}

	fmt.Printf("metis eval: %d scenarios via %s\n", len(scenarios), bin)
	fmt.Printf("threshold: %.2f (per-scenario score), pass = score ≥ threshold\n\n",
		eval.PassThreshold)

	rep := rn.RunAll(ctx, scenarios)

	for _, sc := range rep.Scores {
		mark := "✗"
		if sc.Passed {
			mark = "✓"
		}
		fmt.Printf("%s  %-30s  score=%.2f  dur=%5dms",
			mark, sc.ScenarioID, sc.Total, sc.Duration.Milliseconds())
		if sc.Err != nil {
			fmt.Printf("  err=%v", sc.Err)
		}
		fmt.Println()
		if *verbose {
			for _, b := range sc.Breakdown {
				gate := "·"
				if b.Earned > 0 {
					gate = "+"
				}
				fmt.Printf("       %s %-14s w=%.1f  %s\n",
					gate, b.Type, b.Weight, b.Note)
			}
		}
	}

	fmt.Println()
	fmt.Printf("pass rate : %d / %d  (%.1f%%)\n",
		countPassed(rep), len(rep.Scores), rep.PassRate()*100)
	fmt.Printf("avg score : %.3f\n", rep.AvgScore())
	fmt.Printf("total time: %s\n", rep.Finished.Sub(rep.Started).Round(time.Millisecond))

	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return fmt.Errorf("create %s: %w", *out, err)
		}
		defer f.Close()
		if err := eval.WriteJSONL(f, rep); err != nil {
			return fmt.Errorf("write jsonl: %w", err)
		}
		abs, _ := filepath.Abs(*out)
		fmt.Printf("jsonl     : %s\n", abs)
	}

	if rep.PassRate() < 1.0 {
		// Non-zero exit so CI can gate on this.
		return fmt.Errorf("%d/%d scenarios failed",
			len(rep.Scores)-countPassed(rep), len(rep.Scores))
	}
	return nil
}

func filterByTag(scenarios []eval.Scenario, tag string) []eval.Scenario {
	out := make([]eval.Scenario, 0, len(scenarios))
	for _, s := range scenarios {
		for _, t := range s.Tags {
			if strings.EqualFold(strings.TrimSpace(t), tag) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func countPassed(rep eval.Report) int {
	n := 0
	for _, s := range rep.Scores {
		if s.Passed {
			n++
		}
	}
	return n
}

func evalUsage() {
	fmt.Println(`metis eval — run the markdown scenario pack

Usage:
  metis eval [--dir DIR] [--tag TAG] [--out PATH] [--binary BIN] [--verbose]

Flags:
  --dir DIR       directory containing *.md scenarios (default: eval/scenarios)
  --tag TAG       only scenarios with this tag (smoke, regression, ...)
  --out PATH      write JSONL results to PATH (in addition to console summary)
  --binary BIN    metis binary to test (default: this process's executable)
  --provider P    forward as -p P to each metis run (override provider)
  --model M       forward as -m M to each metis run (override model)
  --verbose       stream subprocess stderr live + show assertion breakdown
  --grace D       extra time on top of each scenario's timeout (default: 5s)

Exit codes:
  0   all scenarios passed
  1   one or more failed (use --out to inspect details)`)
}
