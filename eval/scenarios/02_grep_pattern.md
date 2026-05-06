---
id: 02_grep_pattern
description: agent 用 Grep 找代码里的某个符号
tags: smoke, search, grep
timeout_seconds: 60
---

# Setup
metis 仓库 internal/agent/error_classifier.go 里有 ErrContextOverflow 这个常量。

# Prompt
找一下 internal/ 下哪些文件提到了 "ErrContextOverflow"，简短列出文件路径。

# Reward
- contains_all: ["error_classifier"] weight=2
- used_tool: Grep weight=1
- not_contains: ["I cannot", "I don't know", "无法"] weight=1
- length: 10..600 weight=0.3
