# AgentSweep 应用图标

一把墨色扫帚、一颗被扫走的蓝点。名字里的 sweep 直接画出来，
不需要先读文案才能看懂这个 app 是干什么的。

- **墨色扫帚**：`#1B1F26`，沿用设计系统的主色；扇形的刷头是扫帚的识别点。
- **石板蓝圆点**：`#3F6599`，唯一强调色，代表被扫掉的那条会话。
- **暖纸底**：`#F4F2EC`，和主界面的画布同一个颜色。
- 形状是 macOS 标准的 824/1024 连续圆角方，阴影烘进 PNG，不依赖系统补光。

扇形的刷头在 16/32px 会糊成一块，所以小尺寸不是把大图缩小：
`AgentSweep-small.svg` 换了更粗的把手、更宽的扇形、更少的刷毛缝隙和更大的蓝点。

## 文件

| 文件 | 用途 |
|---|---|
| `AgentSweep.svg` | 大尺寸母版（64–1024） |
| `AgentSweep-small.svg` | 16/32 专用简化版 |
| `render.sh` | SVG → `build/icon.iconset` → `build/AgentSweep.icns` |

生成物在 `build/` 下：`AgentSweep.icns`（进 app bundle）、
`icon.iconset/`、`icon-1024.png`（预览/文档用）。它们都已提交，
正常构建不需要重跑图标生成。

## 重新生成

改了 SVG 之后：

```bash
make icon        # 需要 rsvg-convert：brew install librsvg
make app         # 重新打包，把 icns 带进 AgentSweep.app
```

`build/Info.plist` 里的 `CFBundleIconFile` 指向 `AgentSweep`；
少了这个键，Finder 和 Dock 只会显示系统默认的占位图标。
