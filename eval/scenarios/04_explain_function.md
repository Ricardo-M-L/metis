---
id: 04_explain_function
description: agent 读源码并解释一个函数的作用
tags: regression, read, explain
timeout_seconds: 90
---

# Setup
internal/eval/reward.go 里的 ComputeReward 函数。

# Prompt
读 internal/eval/reward.go 的 ComputeReward 函数，用 1-2 句话告诉我它的作用以及返回值的范围。

# Reward
- contains_any: ["score", "reward", "评分", "分数", "Score", "Reward", "得分"] weight=1
- contains_any: ["0", "1", "[0,1]", "0.0", "1.0", "0~1", "0 到 1", "范围"] weight=1
- used_tool: Read weight=2
- length: 20..600 weight=0.5
