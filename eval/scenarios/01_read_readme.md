---
id: 01_read_readme
description: Agent reads README.md and summarises it in one sentence
tags: smoke, basic, read
timeout_seconds: 60
---

# Setup
The metis repo root contains a README.md.

# Prompt
Read README.md in the current directory and tell me in one sentence what this project is.

# Reward
- contains_any: ["metis", "Metis", "MetIs", "METIS"] weight=1
- contains_any: ["agent", "Agent", "CLI", "Go", "go"] weight=1
- used_tool: Read weight=2
- length: 10..400 weight=0.5
