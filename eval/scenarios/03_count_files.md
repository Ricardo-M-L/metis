---
id: 03_count_files
description: agent 用 Glob 数 .go 文件数量
tags: smoke, glob, count
timeout_seconds: 60
---

# Setup
metis 仓库 internal/eval/ 下有 4 个 .go 源文件 + 测试文件。

# Prompt
internal/eval/ 目录下有几个 .go 文件？回答时给出总数（包括测试文件）。

# Reward
- regex: \b\d+\b weight=2
- used_tool: Glob weight=1
- not_contains: ["error", "无法"] weight=0.5
- length: 5..400 weight=0.3
