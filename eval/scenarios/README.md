# metis eval scenarios

Markdown 驱动的 metis 端到端评测包。

## 跑评测

```
metis eval                              # 跑所有 scenarios，输出汇总
metis eval --dir eval/scenarios         # 显式指定目录
metis eval --out results.jsonl          # 保存 JSONL 结果
metis eval --tag smoke                  # 只跑带 smoke tag 的
metis eval --binary ~/go/bin/metis      # 指定 metis 路径
```

## 加新 scenario

每个 scenario 是一个 markdown 文件，由两部分组成：YAML 风格的 front-matter + body sections。

```markdown
---
id: my_scenario          # 唯一 slug，缺省取文件名
description: 一句话描述
tags: smoke, regression  # 逗号分隔
timeout_seconds: 60      # 整数秒，默认 60
---

# Setup
随便写说明，runner 不会执行这里的内容（这是给作者读的）。

# Prompt
metis run 收到的 prompt 字符串。multi-line 也行。

# Reward
- contains_all: ["foo", "bar"] weight=1
- contains_any: [alpha, beta] weight=0.5
- not_contains: [error, 无法] weight=1
- used_tool: Read weight=2
- regex: ^answer\s*=\s*\d+ weight=1
- length: 10..400 weight=0.3
```

## Reward 规则

每条 assertion 是 binary（pass=1 / fail=0）。`weight` 是相对权重（默认 1）。最终 score = `Σ(earned×weight) / Σ(weight)`，范围 [0, 1]。score ≥ 0.7 算 pass。

| key            | 含义                                       |
|----------------|--------------------------------------------|
| `contains_all` | response 必须包含**全部** token            |
| `contains_any` | response 包含**任一** token                |
| `not_contains` | response 必须**不**包含任一 token          |
| `used_tool`    | 整个 turn 中调用过指定工具                 |
| `regex`        | response 匹配正则                          |
| `length`       | response 字符数在 `N..M` 区间内（含端点）  |

## 设计参考

- markdown scenario pack — 借自 openclaw `qa/scenarios/`
- compute_reward 模式 — 借自 hermes `environments/hermes_base_env.py`
- terminal-bench 任务集 — 借自 kimi-cli `tests_ai/accuracy_smoke/`

## CI 集成

PR diff 评测最简单的姿势：

```bash
metis eval --dir eval/scenarios --out /tmp/eval.jsonl
jq -s 'map(select(.type == "summary"))[0].pass_rate' /tmp/eval.jsonl
```

通过率低于阈值（比如 0.8）就让 CI 失败。
