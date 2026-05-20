# Metis Design Notes

> 2026-04-28 版本（讨论 OpenClaw/MemGPT/Hermes/Claude Code 设计借鉴的早期 design doc）已合并到 [`ARCHITECTURE.md`](ARCHITECTURE.md)。
>
> 实际现状以 [`ARCHITECTURE.md`](ARCHITECTURE.md) 为准；跨项目对比与设计哲学对照见 [`../../COMPARISON.md`](../../COMPARISON.md)。

## 设计原则（不再展开，仅 list）

- 单二进制，无 daemon，无容器
- Go 单进程
- Memory 借 MemGPT / Hermes 的多层架构
- TUI 借 bubbletea idiom（单 Model + 文件拆分）
- 持续执行借三家组合：Claude Code 的 `ScheduleWakeup`（LLM 自决唤醒）+ OpenClaw 的 3 种 SessionMode（isolated/persistent/main）+ Hermes 的 per-job 工具黑名单。Cron 表达式用 robfig/cron/v3 真正解析（之前 hand-rolled fallback 是个 bug）。
- Provider 适配层用接口而不是 SDK 直连
- Concurrency 三层（Safe / Queue / Exclusive）借 Claude Code

## 关键决策记录（time-line）

- 2026-04-26 — 立项，名字 `talon`
- 2026-04-26 → 2026-04-29 — 改名 `Delphi`（被占用）→ `Metis`
- 2026-04-29 — 自驱迭代 19 rounds 后 v1 完成（30+ slash 命令、memory 三层、loop detection、cron、SDK pkg/ 抽出）
- 2026-04-30 — Gemini provider native 接入、MCP HTTP/SSE、Plugin 系统、11 channel adapter、auto-compaction 三层修复（estimateTokens 全字段计数 / context_window 配置覆盖 / tryRecoverOverflow 错误恢复）、token tracker 改回 claude-code 一致的 sum-sum 语义、错误格式化、Gemini/Anthropic input-token chunk 转发（修 MiniMax）、TUI 拆 50 文件保持单 Model
- 2026-05-18/20 — **Agent 安全网 6 层**落地：
  - Phase A：诚实退出 (`*exitcode.IncompleteError` → exit 11)
  - Phase B：解析 verify subagent 的 `VERDICT: PASS/FAIL/PARTIAL` 并 gate end_turn
  - Stuck detector v5：sig×4 OR noGreen×8 + 2-reset budget + deny-不重置 counter
  - Progress detector：成功 Bash 算 full credit（避免短输出 false-positive abort）
  - Loop detector（signature-hash 早已存）
  - Contract dispatch gate：5 writes / 2 agents 后强制 spawn verify
  
  Bench 验证：bench6 mini-interpreter 任务在 minimax-m2.7 上 5M tokens / abort → 1.9M tokens / clean exit；bench5 HTTP server stuck-loop 13 min → 21 min 通过。`go test -race ./...` 首次 67 包全绿——修了 TUI runTurnAsync race、TUI exitFunc 杀 test binary、jobs.Registry 暴露 *Job 指针被并发写、bash_jobs_test 同样问题。
  
  **结构调整**：拆出 `internal/tools/builtin/bash/` + `internal/runtime/mcp/` 子包；7 个 cluttered 包加 navigation README；cmd/metis 19 个文件命名统一去 `cmd_` 前缀。bash-security rule #23 heredoc carve-out + rule #29 regex 收紧 + deny-reason 在 dispatch 完整暴露。

详细 round-by-round 时间线见 `~/.metis/iteration-log.md`；2026-05 期间 metis 自身迭代的详细 bench 报告见 `~/Documents/公司学习文件/我自己的agent的cli/测试报告/`。
