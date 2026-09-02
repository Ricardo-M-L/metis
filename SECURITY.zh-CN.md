# 安全策略

English version: [SECURITY.md](SECURITY.md).

## 支持版本

Metis 处于 `0.x` 阶段，仅最新发布版本接收安全修复。当前发布线为
`0.4.x`；该发布线的用户应使用其最新可用版本。

| 版本 | 是否支持 |
|---|:---:|
| 最新发布的 `0.4.x` 版本 | ✅ |
| 更早版本 | ❌ |

## 漏洞反馈

**安全问题请勿在 GitHub 公开 issue 中提出。**

请使用 GitHub 的
[私密漏洞报告表单](https://github.com/Ricardo-M-L/metis/security/advisories/new)，
并附上：

- 问题描述
- 复现步骤（PoC 代码 / 命令最佳）
- 受影响版本
- 已知规避方案（如有）

你可以期待：

- **5 个工作日**内收到确认
- 已确认的问题在 **30 天**内给出修复或进度
- 协同披露：在任何细节公开前先约定发布日期，并署名报告者（除非你希望匿名）

## 威胁模型 —— 我们关心的范围

**接收**（请反馈）：

- 沙箱 / 权限网关绕过：工具调用越过了 permission mode 声明的范围
- Read / Edit / Glob / LS 中的路径穿越
- Bash policy 命令注入
- Plugin 或 MCP 服务的提权，能让第三方读取凭证、系统文件、其他插件状态
- 多用户共享主机时记忆库串号泄密

**不在范围**（请勿提交）：

- 用户主动设置 `--mode fullAccess`（或
  `--dangerously-bypass-approvals-and-sandbox`），并允许命令使用宿主账号权限运行
  —— 该模式明确关闭审批和进程沙箱
- 上游 LLM provider API 的问题
- 巨大输入导致的拒绝服务（Metis 是本地 CLI，请用 ulimit）
- 用户自己以明文写进 config 文件的敏感数据

## 加固建议

- 永远不要在 config.toml 用 `api_key = "..."` 直填真实 API key。
  改用 `api_key_env = "ANTHROPIC_API_KEY"`，把密钥放在环境变量。
- 在审过哪些工具可信任自动运行之前，默认 `mode = "default"`。
- 把 `~/.metis/` 当 `~/.ssh/` 同等保护 —— 同样的文件权限，不要 checkin 到共享 dotfiles 仓库。
