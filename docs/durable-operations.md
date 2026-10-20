# 长期岗位运行与管理

Desktop 的“岗位”入口与 `metis operations snapshot --cwd /absolute/project --json`
读取同一份宿主状态。页面跟随已注册的所选工作区，分别展示长期岗位、交接回执、
待处理运行和学习版本。它与 cron 定时任务相互独立。

岗位暂停/恢复，以及学习回滚/恢复，都由固定管理动作执行；页面不能传入 shell 命令、
模型路由或新权限。学习动作绑定刚查看的版本与激活 epoch，并要求复核说明。
切换工作区后旧确认失效。未知外部操作只显示处理提示，不能点击按钮假定它没有执行。

## 操作和岗位历史归档

同一 run 的已完成操作达到 512 条或结果达到 8 MiB 时自动写入不可变归档段。
热状态保留未决操作和归档链头；原 1024 条、16 MiB 是热记录保护边界，已完成历史
不再占用这份配额。操作身份、调用次数及完整成功结果保留，恢复后的 worker 仍重放
原结果；未决操作不会因归档变成可以重试。

归档文件及目录先 fsync，再原子更新 run。提交前崩溃最多留下未引用的孤段；
已经引用的段缺失、摘要不符或损坏时拒绝继续执行，不能把“查不到”当成新操作。
归档目录必须与 run 一起保存。操作查询会扫描分段索引，但不会一次加载全部历史结果。

岗位保留最近 64 条已结算 run 和所有活动、阻塞或未结算 run；更老历史保存在
内容寻址索引中。终身累计预算保留，迟到账单仍可纠正旧 run。每次付费请求只刷新需要
继续追踪的 run，不扫描所有已经结算的历史。学习版本、评估和控制器回执也保留冷历史
以及去重证据，详见 `learning-quality-feedback.md`。

当前单个 run 的任务图节点和物理请求账本仍完整保存；本次分段的是操作结果和岗位
已结算 run 历史。磁盘总量、长历史索引查询和多日增长仍应监测，不能直接删除 archive
文件来释放空间。Desktop 的 storage 区域展示热记录、归档数量、字节数和告警。

状态版本为 graph 5、principal 2、handoff 3、learning 2、controller 2。
升级前停止旧 writer；读取兼容旧状态，新状态禁止旧程序覆盖。

## 原生用量和账单来源

现有回执目录、私有适配器命令仍可使用。新增原生来源通过宿主私有 JSON 配置启用：

```sh
metis coordinator usage reconcile RUN --cwd /absolute/project \
  --billing-config /private/provider-billing.json --json
metis coordinator serve RUN --cwd /absolute/project --agent worker \
  --token-file /private/event-token \
  --usage-billing-config /private/provider-billing.json
```

`coordinator service install` 也接收 `--usage-billing-config`。配置文件和凭据文件必须
为宿主拥有、0600、绝对路径且在工作区外；前端和模型不接触它们。安装后的配置和凭据
必须保留在稳定宿主路径。未配置时不自动选择别的供应商，也不启用网关统计。

当前 CLIProxyGUI 来源使用只读 SQLite，不消费网关事件队列、不修改数据库：

```json
{
  "provider": "cliproxy_gui",
  "ledger_provider": "YOUR_SELECTED_PROVIDER",
  "inference_endpoint": "http://127.0.0.1:8317/v1/chat/completions",
  "usage_db": "/absolute/path/to/usage.db"
}
```

端点必须与物理请求完全一致。受协调器观察的每次物理请求都会在 User-Agent 中加入
独立的 `metis-usage/<128-bit-random-id>` 标记，保留原 User-Agent，不包含账号或提示词。
数据库精确匹配这个标记，验证唯一性、模型与已有网关请求 ID 后才可补账。
旧请求没有标记、记录尚未到达或存在冲突时保持未知，不按时间或模型猜测。
该数据库没有实际金额，`actual_usd` 始终未知，不能用目录估价代替。

OpenRouter 的原生配置如下：

```json
{
  "provider": "openrouter",
  "ledger_provider": "YOUR_SELECTED_PROVIDER",
  "inference_endpoint": "https://openrouter.ai/api/v1/chat/completions",
  "api_base_url": "https://openrouter.ai/api/v1",
  "token_file": "/private/openrouter-key",
  "correction_window": "168h"
}
```

只采用与原请求精确绑定、已终结 generation 的供应商 `total_cost`；BYOK 不把路由平台
费用冒充上游账单。延迟、未终结、暂缺记录和缺少金额保持未知。原始证据私有保存并
记录 SHA256，重复导入不重复计费，账单修订保留前后证据。目录估价、实际金额和预算
保守下限独立展示。启用此来源及真实最终金额验收需要相应供应商账号。

## 上线质量与代码任务评估

使用 `coordinator-host-checks` 质量策略后，实际使用的 Skill/Workflow 版本在执行前
绑定，独立 verifier 的宿主检查完成后生成反馈。模型自报、未完成调用、旧 claim、
旧尝试和检查基础设施失败不能充当新版本的业务失败样本。详情与阈值配置见
`learning-quality-feedback.md`。

可选 Workflow fixture 支持 Python 3 标准库代码任务：候选仅编辑声明的 `.py` 文件，
最后调用 `fixture-test`；固定宿主测试保持不可写，宿主随后独立重跑并验收 JSON 产物。

```sh
metis learning configure --kind workflow --name code-repair \
  --suite /private/suite.json --fixture /private/fixture.json \
  --max-evaluations 4 --max-total-tokens 400000 --auto-promote
metis learning watch --kind workflow --name code-repair --once
```

配置冻结 fixture 和 checker，评估在隔离工作区运行；缺少严格隔离能力时拒绝执行。
这不是允许候选访问真实项目凭据、联网安装依赖或执行任意工程构建。

## 验收

持久化 Go 回归覆盖跨热记录上限、归档后重放、损坏拒写、并发领取、迟到账单和质量
幂等；这些不等于同等数量的模型请求。`durable_operations_live_check.py` 使用新编译
CLI、真实模型和固定宿主断言串联接单、回执、质量回滚、代码 fixture 与网关精确对账。
每轮结束先销毁测试 CLI、私有认证与拥有的进程，失败轮证据仍保留。
