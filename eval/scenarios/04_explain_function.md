---
id: 04_explain_function
description: Agent reads source and explains a function
tags: regression, read, explain
timeout_seconds: 90
---

# Setup
internal/eval/reward.go defines the `ComputeReward` function.

# Prompt
Read the ComputeReward function in internal/eval/reward.go. In 1-2 sentences, tell me what it does and the range of its return value.

# Reward
- contains_any: ["score", "reward", "Score", "Reward"] weight=1
- contains_any: ["0", "1", "[0,1]", "0.0", "1.0", "0~1", "0 to 1", "range"] weight=1
- used_tool: Read weight=2
- length: 20..600 weight=0.5
