---
id: 05_simple_math
description: Pure reasoning, no tools — a simple arithmetic question
tags: smoke, no_tools, basic
timeout_seconds: 30
---

# Setup
(none)

# Prompt
What is 17 × 23? Answer with just the number, no explanation.

# Reward
- contains_all: ["391"] weight=2
- not_contains: ["I", "wait"] weight=0.5
- length: 1..50 weight=1
