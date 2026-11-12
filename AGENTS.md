# AGENTS.md

桌面宠物记忆系统。

## 设计方向

- `doc/design.html` 是需求与架构的唯一来源，做任何实现前先通读。
- 单文件 HTML：浏览器直接打开即可。mermaid 图写在 `<pre class="mermaid">` 里，由 pele（jsDelivr CDN）加载后渲染为 SVG；断网时退化为原始 mermaid 文本。

## 约束

- 可用编程语言：go/rust
