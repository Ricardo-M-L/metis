---
id: 05_simple_math
description: 不用工具的纯推理 — 算个简单数学题
tags: smoke, no_tools, basic
timeout_seconds: 30
---

# Setup
（无）

# Prompt
17 × 23 等于多少？只回答数字，不要解释。

# Reward
- contains_all: ["391"] weight=2
- not_contains: ["I", "wait"] weight=0.5
- length: 1..50 weight=1
