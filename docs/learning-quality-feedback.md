# 学习版本上线后的业务质量反馈

学习状态和控制器热状态使用 schema 2。新 CLI 可只读检查旧 schema 1，首次实际写入才迁移为 2；旧 CLI 会拒绝读取 2，以免写回时丢弃质量门禁、冻结 fixture 或已归档预算。不要让旧 CLI 写入迁移后的目录。

离线 `learning evaluate` 验证候选能否上线；上线后的质量策略由宿主单独配置。业务结果必须同时具有实际启用版本、激活 epoch、执行凭据和宿主独立验收结果。模型文字里的成功或失败不计入质量样本。

```sh
metis learning quality-configure --kind workflow --name invoice-summary \
  --window 10 --minimum-samples 5 --max-failure-rate 0.2 \
  --action rollback --verifier coordinator-host-checks
metis learning quality --kind workflow --name invoice-summary
```

`--max-failure-rate` 是允许的失败比例上限。达到最小样本数后，最近窗口的失败比例**超过**上限才触发；例如 5 个样本、上限 0.2，需要至少 2 个失败。策略只统计相同版本、相同激活 epoch、相同策略 revision 的结果，窗口按宿主结果提交顺序排列。

协调器实际调用已管理的 Skill/Workflow 时，宿主在工具执行前捕获精确版本和 epoch，并通过 CAS 保存业务执行绑定。工具实际执行的结果摘要构成 invocation 证据；下游宿主 checker 验收依赖产物时，以其独立结果构成反馈。没有实际调用证据的模型回答、模型自报完成、预算不足、无法结算用量及一般运行时错误都不能冒充业务质量结论。

同一业务执行 ID 只能绑定一次；重复提交完全相同的反馈返回原决策，冲突结果拒绝。延迟到达的旧版本/旧 epoch/旧策略结果仍留存，标记 `ignored_stale`，不能回滚后来的版本。绑定和反馈都跨进程重启保存。协调器调用凭据使用 schema 2，记录尝试次数与 claim 摘要，工具完成时再次核对活跃租约；只有当前已完成尝试的实际调用参与质量归因。旧调用凭据、重试时只重放旧操作而无新调用证据的任务保留 unknown。宿主 checker 取消、超时或无法启动产生的负退出码不计为业务失败。

`rollback` 只恢复该激活版本的直接前任；没有前任时暂停。`pause` 隐藏受管理的 Skill、阻止 Workflow 加载，原来的平面文件不能绕过暂停。触发后同时暂停自动学习晋升；回滚后的前任仍可使用，后续反馈留证但不会继续连锁回滚。宿主复核后明确恢复，并传入刚查看的版本与 epoch：

```sh
metis learning quality-resume --kind workflow --name invoice-summary \
  --version REVIEWED_SHA256 --epoch REVIEWED_EPOCH --note '已复核恢复版本和失败原因'
```

状态在复核后发生变化时拒绝恢复；恢复会增加 epoch 并开始新窗口。重新配置策略本身不解除暂停。

宿主集成或人工验收也可使用 `feedback-bind --outcome BUSINESS_EXECUTION_ID`，再通过 `feedback --file RECEIPT.json` 导入 `{ "binding": <原绑定>, "outcome": <宿主结果> }`。宿主结果含 `passed`、固定 `verifier`、`invocation_sha256` 和 `evidence_sha256`，可附短 `reason_code`。这些是可信本机管理入口，摘要用于证据绑定，不是供应商签名或模型自报可信证明；不要把策略配置和反馈写权限提供给候选评估环境。

## 冷历史归档

```sh
metis learning archive --kind workflow --name invoice-summary
metis learning controller-archive
```

学习历史保留冻结 baseline、当前版本、直接回滚目标和当前 epoch 的候选；当前候选的评估只保留最近一份在热状态，较早评估仍可按原 ID 查询和晋升校验；其他版本、过期评估与旧 transition 段写入 `.learning/NAME/archive/`。版本到 128 条时自动尝试归档冷历史；全部都是当前未处理候选时仍拒绝增长，需要先处理候选。归档后的旧候选 ID 不能伪装成新候选，已真正激活过的历史版本仍可通过宿主管理回滚。

控制器回执达到 3072 条时自动归档，只保留最近 256 条和仍在预留的条目；显式归档可提前执行。当前策略 revision 的累计尝试次数与计费 token 保留在热状态；完整原回执按 ID 保存在冷目录，防止归档后重复付费评估。`unknown` 仍保留保守费用，不会因归档退款。旧策略回执也保留证据，但不占新 revision 的授权预算。

质量反馈热状态最多 1024 条绑定。完成结果超过阈值时自动归档，只保留最近结果窗口和所有未完成绑定；归档后的反馈仍可精确重放与冲突检查。协调器用只读跨冷热查询确认该业务绑定是否已终结，宿主后来恢复或重跑 verifier 不会覆盖原来的质量决策，也不会把同一业务再算一次。1024 条都是未完成任务时拒绝新绑定，不能丢弃在途归因。

所有冷文件先以私有权限写入并 fsync，再原子更新热状态。中途中断最多留下重复冷副本，不会先删除唯一证据。归档减小热状态，不删除磁盘历史；磁盘容量仍需运维保留策略，不能直接删除需要防重或审计的 archive 文件。
