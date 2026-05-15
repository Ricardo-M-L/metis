---
id: 03_count_files
description: Agent uses Glob to count .go files in a directory
tags: smoke, glob, count
timeout_seconds: 60
---

# Setup
internal/eval/ currently contains 4 source files (reward.go, runner.go,
scenario.go, types.go) plus 3 test files, for 7 .go files total.

# Prompt
How many .go files are in the internal/eval/ directory? Give the total count (including test files).

# Reward
- regex: \b\d+\b weight=2
- used_tool: Glob weight=1
- not_contains: ["error", "I cannot"] weight=0.5
- length: 5..400 weight=0.3
