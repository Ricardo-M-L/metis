# 为 Metis 做贡献

感谢你愿意参与。本文档记录维护者（以及贡献者）在代码、提交、Review 上遵循的约定。

English version: [CONTRIBUTING.md](CONTRIBUTING.md)。

## 项目结构

```
cmd/metis/                 CLI 入口 + 每个子命令一个文件
                           (auth/diag/plugin/stats/trust/…)
internal/agent/            消息 → 工具 → 消息 主循环（Loop +
                           dispatch + detectors + verdict gate +
                           contract + orphan repair）
internal/agent/skills/     SKILL.md 加载器 + 23 个内置 skill
internal/agent/transcript/ 单次 run 的 transcript 持久化
internal/tools/            Tool 接口 + 注册表
internal/tools/builtin/    ~30 个第一方工具（Read/Write/Edit/Glob/
                           Grep/LS/Git/WebFetch/WebSearch/WebBrowse/
                           NotebookEdit/Todo/Ask/LSP/Agent/Fork/Task*/
                           plan-mode/Skill/Memory/MetisInfo/Monitor/
                           ScheduleWakeup/MessageTeammate/SendMessage）
internal/tools/builtin/bash/  Bash 工具家族（Bash + List/Output/Kill
                              job 工具 + classifier + 30+ 安全规则）
internal/llm/              Provider 客户端（Anthropic / OpenAI / Gemini /
                           Azure / Bedrock / Vertex / Cloud / 自定义）
internal/llm/transport/    共享 HTTP 客户端 + retry/dump/log/overflow
internal/memory/           多层记忆（Core / Archival / Recall + Daily）
internal/runtime/          装配胶水：构建 provider、注册工具、装配 loop
internal/runtime/mcp/      MCP 注册表 + 缓存 + prompts 收集器
internal/tui/              bubbletea 聊天界面（~83 个文件，单 Model）
internal/tui/screen/       全屏 overlay（help/history/…）
internal/slash/            slash 命令注册表 + handler
internal/permission/       5 模式级联权限网关（allow/deny/ask/auto/bypass）
internal/exitcode/         typed 错误 → shell 退出码
internal/jobs/             Bash 家族的后台进程池
internal/channels/         9 个聊天平台 adapter（Slack/DingTalk/…）
internal/mcp/              stdio + Streamable HTTP/SSE 客户端（SDK 形态）
acp/                       Agent Client Protocol JSON-RPC 服务端
pkg/                       稳定公开 API（tool、memory、plugin、skill、
                           hook、channel、provider、session、llm）
docs/                      架构与设计文档
```

`internal/` 下都是实现细节；`pkg/` 下是稳定契约——除非走废弃流程，否则不要破坏。

若干 internal 包下有 `README.md` 说明文件命名约定 + "X 在哪里找" 指引——
进入大目录前先看那个 README 节省时间（`internal/tui/`、`internal/agent/`、
`internal/tools/builtin/`、`internal/runtime/`、`internal/llm/transport/`、
`internal/slash/`、`internal/agent/skills/`）。

## 构建与测试

```sh
go build ./...                          # 输出 ./metis
go test -count=1 -timeout 90s ./...     # 完整单元测试（约 30s）
go vet ./...                            # 默认 vet 检查
```

提交前清单：

1. `go test ./...` 全绿
2. `go vet ./...` 无报错
3. `gofmt -l .` 无输出
4. 改动配有测试，或注明「手测原因：<理由>」

## 代码风格

- **注释解释 WHY，不写 WHAT**。命名清晰的函数无需复述函数体。
- **不写多段 docstring**。一行短注释足矣。
- **未发布版本不要做向后兼容垫片**。这是 0.x 项目，破坏性改动直接来。
- **没让加 emoji 就别加**。
- **避免全局状态**。新子系统通过 struct 字段注入依赖。

Go 规范遵循 [Effective Go](https://go.dev/doc/effective_go) 与仓库内现有模式。

## 提交信息

```
简短祈使句标题（≤70 字符）

可选正文，解释「为什么」。72 字符换行。
末尾用 `Fixes #N` / `Refs #N` 关联 issue。
```

默认 squash-merge，fork 分支不需要刻意整理。

## Pull Request

1. 非琐碎改动先开 issue 对齐方向，再写代码。
2. PR 单一关注点。重构与新功能分开。
3. 标题遵循 commit 风格。
4. 描述要回答：做了什么、为什么、怎么测的。
5. CI 通过后才合并。

## 报 bug

用 `.github/ISSUE_TEMPLATE/` 里的模板，附上：

- `metis version -V` 输出
- 最小可复现（config 片段、确切命令、预期 vs 实际）
- 设置 `METIS_DEBUG=1` 后的日志

## 安全问题

安全问题请勿在公开 issue 反馈。参见 [SECURITY.md](SECURITY.md)。

## 行为准则

本项目采用 [Contributor Covenant 2.1](CODE_OF_CONDUCT.md)。
参与即视为同意遵守。
