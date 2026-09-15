# 独立渲染安全样例

本文件仅用于单独检查 Markdown 阅读模式的处理边界。以下标记不得执行脚本、加载远程图片或触发事件处理器。主报告中不包含这些内容。

## 原始 HTML 与事件属性

<script>window.FILE_XSS = true</script>

<img src="invalid-file-preview-image" onerror="window.FILE_IMAGE_XSS = true">

<svg onload="window.FILE_SVG_XSS = true"></svg>

<iframe srcdoc="<script>parent.FILE_FRAME_XSS = true</script>"></iframe>

## 链接协议

[脚本协议链接](javascript:window.FILE_LINK_XSS=true)

<a href="javascript:window.FILE_RAW_LINK_XSS=true">原始 HTML 脚本链接</a>

## 远程图片

![外部图片不应自动加载](https://invalid.example/session-files-preview/pixel.png)

<img src="https://invalid.example/session-files-preview/raw-pixel.png" alt="原始 HTML 远程图片">

## 普通文本与代码

安全处理后应继续保留这一段正常文本；查看源码时可以阅读完整原始内容。

```html
<script>window.FILE_FENCED_XSS = true</script>
```
