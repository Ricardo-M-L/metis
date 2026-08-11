# Metis Design Notes

> v0.4.18 设计摘要。早期逐轮记录已经合并、删繁；当前实现边界以
> [`ARCHITECTURE.md`](ARCHITECTURE.md) 和代码为准。

## 设计原则

- **本地优先，但不是“只有一个进程”**：交互式 CLI 默认在本地单进程运行；
  `daemon`、`cron start`、coordinator worker、MCP 子进程和独立 Wails Desktop
  都是按需启动的边界。
- **公开接口稳定、装配留在内部**：provider、tool、hook、channel、skill、
  memory、session、plugin 和 client 面向 `pkg/`；配置解析及运行时组合位于
  `internal/runtime/`。
- **协议与厂商解耦**：上层只依赖统一 Provider 接口；内置 profile 快捷方式
  与底层 wire transport 分开，custom profile 可以接兼容网关。
- **并发是工具契约的一部分**：每次调用按 `Safe`、`Queue`、`Background`、
  `Exclusive` 分类；并行执行不能改变 tool result 的源顺序。
- **权限按来源权威而非简单覆盖**：policy 高于 CLI、会话交互、配置和持久规则；
  同一权威级别才用较新的规则打破平局。
- **TUI 保持一个 Bubble Tea Model**：渲染、按键和 overlay 按文件族拆分，状态仍由
  单一 `Model.Update` 串行收敛。
- **长上下文分层治理**：工具输出截断、落盘、microcompact、collapse 和完整摘要各有
  独立触发条件；持久 memory/recall 与 token 驱动的会话 compaction 是两套机制。
- **多 Agent 显式可控**：后台 Agent、named teammate mailbox、fork、roster 和停止/取回
  输出都有独立工具；`/batch` 是生成并行工作提示词的入口，不是另一个调度器。
- **默认不导出遥测**：仅在配置 OTLP endpoint 时导出；provider、MCP、channel、更新、
  网络工具和插件等显式能力仍会访问网络。

## 决策脉络

- 2026-04：项目从 Talon/Delphi 定名 Metis，建立 Go CLI、公开 SDK、memory、session、
  provider、工具和单 Model TUI 的基本边界。
- 2026-05：补齐 agent 安全网、权限分层、后台任务、sub-agent/teammate、cron 与
  四层工具并发。
- 2026-06 至 v0.4.18：继续演进 compaction、MCP/ACP、Web/Desktop、可选 daemon、
  公共发行、自更新和 Windows 安装链路；这些能力的“已接线/仅声明”边界记录在
  [`ARCHITECTURE.md`](ARCHITECTURE.md)。
