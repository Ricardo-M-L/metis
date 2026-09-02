package i18n

// zhCN — 简体中文 locale. Must mirror en's key set exactly; the
// parity test fails if any en key is missing here.
var zhCN = map[string]string{
	// Input box
	"input.placeholder": "输入消息 · /commands · alt+回车换行",
	"input.bash_mode":   "bash 模式 — `!cmd` 直接执行不调 LLM",
	"input.send_hint":   "回车发送 · Alt+回车换行",

	// 权限模式使用 Claude Code 的规范公开名称。
	"mode.default":           "默认询问",
	"mode.acceptEdits":       "接受编辑",
	"mode.bypassPermissions": "绕过权限",
	"mode.fullAccess":        "完全访问",
	"mode.plan":              "计划",
	"mode.dontAsk":           "不询问",
	"mode.cycle_hint":        "shift+tab 切换",

	// Permission prompt
	"perm.allow":        "是",
	"perm.allow_always": "总是允许",
	"perm.deny":         "否",
	"perm.cancel":       "取消本轮",
	"perm.q.action":     "允许此操作?",
	"perm.q.tool":       "允许此 %s 命令?",

	// Status bar / footer
	"footer.tokens":  "tokens",
	"footer.context": "上下文",
	"footer.session": "会话",
	"footer.branch":  "分支",
	"footer.cost":    "费用",

	// Common verbs / phrases
	"verb.cogitating": "思考中",
	"verb.thinking":   "推理中",
	"verb.processing": "处理中",
	"verb.compiling":  "组织中",
	"verb.brewing":    "酝酿中",
	"verb.simmering":  "梳理中",

	// Errors / info rows
	"err.network":           "网络错误",
	"err.rate_limit":        "请求过于频繁，稍后重试",
	"err.context_full":      "上下文窗口已满 — 正在压缩",
	"info.compacted":        "上下文已压缩",
	"info.snipped":          "工具输出已截断",
	"info.copied":           "已复制到剪贴板",
	"info.dialog_dismissed": "对话框已关闭",

	// Search / palette
	"search.no_match":  "无匹配",
	"search.next":      "下一个",
	"search.prev":      "上一个",
	"palette.no_match": "无匹配命令",

	// Update notice
	"update.available": "metis %s 已发布 — 运行 `metis update` 升级",
}
