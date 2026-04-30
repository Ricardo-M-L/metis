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
- 2026-04-30（这一轮）— Gemini provider native 接入、MCP HTTP/SSE、Plugin 系统、11 channel adapter、auto-compaction 三层修复（estimateTokens 全字段计数 / context_window 配置覆盖 / tryRecoverOverflow 错误恢复）、token tracker 改回 claude-code 一致的 sum-sum 语义（之前误改成 max-of-latest）、错误格式化（提取 message + 恢复提示 + 折叠 ×N）、Gemini finish_reason chunk 加 InputTokens 转发、Anthropic message_delta 加 InputTokens 转发（修 MiniMax）、TUI 拆 50 文件保持单 Model、版本号显示

详细 round-by-round 时间线见 `~/.metis/iteration-log.md`。
