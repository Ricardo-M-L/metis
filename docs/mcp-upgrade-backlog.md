# MCP 协议兼容升级待办

记录日期：2026-09-10。状态：**待办，暂不实施**（用户明确要求）。

- 当前 CLI 客户端及 `mcp-serve` 声明协议版本 `2024-11-05`。
- 官方现行规范为 `2026-07-28`；SDK v2、JSON-RPC 2.0 与 MCP 日期版本不是同一概念。
- 后续优先评估新旧协议并存、能力探测与回退，保留现有 stdio/HTTP 服务兼容性；不能仅修改日期常量。
- 核对新请求元信息、无状态调用、鉴权、通知/交互、断线后的重复执行保护，并补兼容性测试。
- Tasks 扩展单独评估，须客户端和服务端配合；不等同于 Metis 自主任务持续运行六小时的能力。
- 未授权开始升级；本次任务状态、断流和缓存问题修复不包含 MCP 实现变更。

参考：[官方版本规则](https://modelcontextprotocol.io/specification/versioning)、[兼容性规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)。
