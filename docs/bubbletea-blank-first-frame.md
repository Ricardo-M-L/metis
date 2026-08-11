# Bubble Tea v2：启动时 `GetSize` 返回 0 导致首帧空白

本文记录 Metis 在部分 tmux/PTY 环境遇到的首帧问题、当前客户端规避方式，
以及尚未应用的上游修复建议。

## 当前依赖状态

- `go.mod` 使用 `charm.land/bubbletea/v2 v2.0.8`。
- 依赖通过 `replace` 指向 `vendor-patches/bubbletea-v2`。
- 当前 vendored v2.0.8 的 `Program.Run` 仍会直接接受 `term.GetSize`
  返回的宽高，没有为 `w <= 0 || h <= 0` 设置 fallback。
- 本地 fork 没有应用本文后面的 Bubble Tea fallback；Metis 依靠客户端规避。

## 现象

在终端尺寸尚未协商完成的 tmux/PTY 中启动程序时，首屏可能完全空白，
看起来像进程卡住。恢复显示需要 renderer 收到一个非零尺寸；通常由
`SIGWINCH`、真实的 `WindowSizeMsg`，或显式 `tea.RequestWindowSize()` 触发。
普通按键只会触发一次更新，并不保证修正仍为 `0x0` 的 renderer viewport。

Metis 的主聊天界面和启动前的 “trust this folder?” 界面都可能经过这条路径。

## 根因

当前 Bubble Tea v2.0.8 的启动流程大致为：

```go
w, h, err := term.GetSize(p.ttyOutput.Fd())
if err != nil {
    return p.initialModel, err
}
p.width, p.height = w, h
resizeMsg := WindowSizeMsg{Width: p.width, Height: p.height}
go p.Send(resizeMsg)
p.renderer.resize(resizeMsg.Width, resizeMsg.Height)
// ...
p.render(model)
```

某些 PTY 会在启动瞬间返回 `(0, 0, nil)`。Bubble Tea 因而把 renderer
viewport 设成 `0x0`。v2.0.8 确实会在进入 event loop 前调用一次
`p.render(model)`；问题不是“首次消息前完全不 render”，而是这次初始渲染被
`0x0` viewport 裁掉。随后送入的 `WindowSizeMsg{0,0}` 也不会让画面恢复。

这个问题独立于业务 `View()` 是否读取宽高：即使 `View()` 返回了非空文本，
renderer 仍可能把它裁成空白。

## Metis 已采用的规避

Metis 没有修改 vendored Bubble Tea 的启动尺寸逻辑，而是在两个客户端入口做防御：

1. `internal/tui/tui.go` 创建主 `Model` 时预置 `80x24`，避免业务布局在第一次
   `View()` 时按零宽度折叠。
2. `internal/tui/tui_update.go` 的 `Model.Init()` 延迟约 60 ms 发送
   `tea.RequestWindowSize()`；终端完成协商后，renderer 会收到真实尺寸。
3. 同一文件的 `WindowSizeMsg` 分支忽略非正宽高，保留当前有效布局尺寸。
4. `cmd/metis/trust.go` 的 `trustModel.Init()` 也延迟约 60 ms 请求一次窗口尺寸，
   覆盖主 TUI 初始化前的独立 trust prompt。

这里有两层保护：默认尺寸保证主模型能生成首帧，延迟重查负责修正 Bubble Tea
renderer 自身的 viewport。只做其中一层并不足以覆盖所有启动时序。

## 尚未应用的上游修复建议

更根本的修复是在 Bubble Tea 读取初始尺寸时拒绝非正值，例如：

```go
w, h, err := term.GetSize(p.ttyOutput.Fd())
if err != nil {
    return p.initialModel, fmt.Errorf("bubbletea: error getting terminal size: %w", err)
}
if w <= 0 || h <= 0 {
    w, h = 80, 24
}
width, height = w, h
```

这只是候选 patch：当前仓库没有应用它，也没有在本文记录已提交的上游 PR。
如要提交上游，应先用最小 Bubble Tea v2.0.8 程序在可稳定返回 `0x0` 的 PTY
环境建立回归测试，再确认 fallback 不影响 headless、非 TTY 和显式尺寸配置。
