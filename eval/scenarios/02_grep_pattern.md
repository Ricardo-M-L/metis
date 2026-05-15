---
id: 02_grep_pattern
description: Agent uses Grep to find a symbol in the codebase
tags: smoke, search, grep
timeout_seconds: 60
---

# Setup
The metis repo defines the `ErrContextOverflow` constant in
internal/agent/error_classifier.go.

# Prompt
Find which files under internal/ mention `ErrContextOverflow`. List the file paths briefly.

# Reward
- contains_all: ["error_classifier"] weight=2
- used_tool: Grep weight=1
- not_contains: ["I cannot", "I don't know", "I'm unable"] weight=1
- length: 10..600 weight=0.3
