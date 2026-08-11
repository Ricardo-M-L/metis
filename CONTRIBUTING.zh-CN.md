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
internal/agent/skills/     SKILL.md 加载器 + 内置 skill
internal/agent/transcript/ 单次 run 的 transcript 持久化
internal/tools/            Tool 接口 + 注册表
internal/tools/builtin/    第一方工具（Read/Write/Edit/Glob/
                           Grep/LS/Git/WebFetch/WebSearch/WebBrowse/
                           NotebookEdit/Todo/Ask/LSP/Agent/Fork/Task*/
                           plan-mode/Skill/Memory/MetisInfo/Monitor/
                           ScheduleWakeup/MessageTeammate/SendMessage）
internal/tools/builtin/bash/  Bash 工具家族（Bash + List/Output/Kill
                              job 工具 + classifier + 安全规则）
internal/llm/              Provider 客户端（Anthropic / OpenAI / Gemini /
                           Azure / Bedrock / Vertex / Cloud / 自定义）
internal/llm/transport/    共享 HTTP 客户端 + retry/dump/log/overflow
internal/memory/           多层记忆（Core / Archival / Recall + Daily）
internal/runtime/          装配胶水：构建 provider、注册工具、装配 loop
internal/runtime/mcp/      MCP 注册表 + 缓存 + prompts 收集器
internal/tui/              bubbletea 聊天界面（按职责拆分，单 Model）
internal/tui/screen/       全屏 overlay（help/history/…）
internal/slash/            slash 命令注册表 + handler
internal/permission/       5 模式级联权限网关（default/acceptEdits/plan/dontAsk/bypassPermissions）
internal/exitcode/         typed 错误 → shell 退出码
internal/jobs/             Bash 家族的后台进程池
internal/channels/         聊天平台 adapter（Slack/DingTalk/…）
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

使用 `go.mod` 声明的 Go 工具链；CI 直接读取该文件，本文不重复写死版本号。

根模块检查（每次代码改动都必须执行）：

```sh
gofmt -l .                              # 必须无输出
go vet ./...
go build ./...                          # 编译根模块的所有 package
go test -count=1 -timeout 90s ./...
```

常用的本地构建和安装 target：

```sh
make test                               # 可选：根模块 race + coverage
make build                              # 带版本信息的 CLI 输出到 ./bin/metis
make install                            # 安装到 ~/.local/bin/metis 和当前 Go bin 目录
```

仓库还包含多个嵌套模块，根目录的 `./...` 不会跨越它们。改到对应
区域时执行以下检查；CI 会全部执行：

```sh
(cd vendor-patches/bubbletea-v2 && go vet ./... && go test -race -count=1 ./...)
(cd vendor-patches/ultraviolet && go vet ./... && go test -race -count=1 ./...)

(cd metis-desktop/frontend && npm ci && npm run check && npm run build)
(cd metis-desktop && go vet ./... && go test -race -count=1 ./... && go build -tags production ./...)
```

GitHub Actions 会在 Ubuntu、macOS 和 Windows 上重复根模块的格式、vet、构建和
测试检查；desktop job 在 macOS 上运行。

端到端（TUI 行为）测试使用 tmux 驱动真实的 `metis chat` 会话，并通过
`capture-pane` 断言屏幕内容：

```sh
scripts/e2e/tmux_drive.sh --list        # 列出可用 case
scripts/e2e/tmux_drive.sh slash_help    # 运行单个 case
scripts/e2e/tmux_drive.sh               # 运行全部 case
```

截图输出到 `${METIS_E2E_OUT:-/tmp/metis-e2e-tmux}/`。较旧的
`scripts/e2e/macos_drive.sh`（osascript / Terminal.app）仍可在 macOS 上使用；
tmux driver 支持无界面运行，也支持 Linux。

与 Claude Code 做对等性比较时，`scripts/e2e/cmp_drive.sh` 会并排启动两个
binary，发送相同输入，并为每个 case 记录两份 pane 内容和一行 Markdown
triage：

```sh
scripts/e2e/cmp_drive.sh --list         # 列出对比 case
scripts/e2e/cmp_drive.sh slash__help    # 运行单个 case
scripts/e2e/cmp_drive.sh                # 运行全部 case（约 3 分钟）
```

输出位于 `/tmp/metis-cmp-captures/*.txt`（完整 pane 内容）和
`/tmp/metis-cmp-issues.md`（triage）。失败应直接暴露；修复后重新运行。

提交前清单：

1. 上述根模块的格式、vet、构建和测试命令通过。
2. 所有被修改的嵌套模块或前端区域通过对应检查。
3. TUI 改动通过相关 tmux case（适用时还要运行 parity case），否则在 PR 中说明手测原因。
4. 新行为配有测试，或明确注明「手测原因：<理由>」。
5. 行为变更在同一 PR 中更新相关文档（README 中的 flag / slash / keybind 表、
   `CHANGELOG.md` 的 Unreleased 条目，以及架构或 package 边界变化时的
   `docs/ARCHITECTURE.md`）。

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

安全问题请勿在公开 issue 反馈。参见 [SECURITY.zh-CN.md](SECURITY.zh-CN.md)。

## 行为准则

本项目采用 [Contributor Covenant 2.1](CODE_OF_CONDUCT.zh-CN.md)。
参与即视为同意遵守。
