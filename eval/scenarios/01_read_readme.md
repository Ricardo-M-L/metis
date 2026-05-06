---
id: 01_read_readme
description: agent 读 README.md 并用一句话总结
tags: smoke, basic, read
timeout_seconds: 60
---

# Setup
metis 仓库根目录下有 README.md。

# Prompt
读一下当前目录的 README.md，用一句话告诉我这是什么项目。

# Reward
- contains_any: ["metis", "Metis", "MetIs", "METIS"] weight=1
- contains_any: ["agent", "Agent", "CLI", "Go", "go", "工具", "命令行"] weight=1
- used_tool: Read weight=2
- length: 10..400 weight=0.5
