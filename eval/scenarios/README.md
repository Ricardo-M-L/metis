# metis eval scenarios

Markdown-driven end-to-end evaluation pack for metis.

## Running

```
metis eval                              # run every scenario, print summary
metis eval --dir eval/scenarios         # explicit scenario directory
metis eval --out results.jsonl          # write per-scenario JSONL
metis eval --tag smoke                  # run only scenarios tagged smoke
metis eval --binary ~/go/bin/metis      # point at a specific metis binary
```

## Adding a scenario

Each scenario is a markdown file with two parts: a YAML-style front-matter and body sections.

```markdown
---
id: my_scenario          # unique slug; defaults to the filename
description: one-line description
tags: smoke, regression  # comma-separated
timeout_seconds: 60      # integer seconds; default 60
---

# Setup
Free-form notes for the scenario author — the runner does NOT execute this.

# Prompt
The exact prompt string `metis run` will receive. Multi-line is fine.

# Reward
- contains_all: ["foo", "bar"] weight=1
- contains_any: [alpha, beta] weight=0.5
- not_contains: [error, unable] weight=1
- used_tool: Read weight=2
- regex: ^answer\s*=\s*\d+ weight=1
- length: 10..400 weight=0.3
```

## Reward rules

Each assertion is binary (pass=1 / fail=0). `weight` is a relative weight (default 1). The final score is `Σ(earned×weight) / Σ(weight)`, in the range [0, 1]. A score ≥ 0.7 counts as a pass.

| key            | meaning                                       |
|----------------|-----------------------------------------------|
| `contains_all` | response must contain **every** token         |
| `contains_any` | response must contain **at least one** token  |
| `not_contains` | response must contain **none** of the tokens  |
| `used_tool`    | the named tool was invoked during the turn    |
| `regex`        | response matches a Go regexp                  |
| `length`       | response length in `N..M` characters (inclusive) |

## Design references

- markdown scenario pack — borrowed from openclaw `qa/scenarios/`
- compute_reward shape — borrowed from hermes `environments/hermes_base_env.py`
- terminal-bench task layout — borrowed from kimi-cli `tests_ai/accuracy_smoke/`

## CI integration

The simplest way to gate a PR on eval pass-rate:

```bash
metis eval --dir eval/scenarios --out /tmp/eval.jsonl
jq -s 'map(select(.type == "summary"))[0].pass_rate' /tmp/eval.jsonl
```

Fail the CI when the rate falls below your threshold (e.g. 0.8).
