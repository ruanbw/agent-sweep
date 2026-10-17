# 发布 AgentSweep

这份文档写给两类人：三个月后的自己，和第一次发版的贡献者。
核心只有一句：**产物名是契约**。应用内更新器靠文件名找包、靠 `checksums.txt` 验包，
名字一改，用户那边不会报错，只会永远显示「已是最新版本」。

## 版本策略

- 严格 [SemVer](https://semver.org/lang/zh-CN/)：`MAJOR.MINOR.PATCH`，git tag 带 `v` 前缀，`v0.1.0`。
- **首个版本 `v0.1.0`。** `0.x` 阶段不承诺向后兼容：清理逻辑、索引格式、适配器字段都可能在小版本里改。
- 产物里的版本号**一律不带 `v`**：`CFBundleShortVersionString`、`agentsweep version` 的输出、
  文件名里的 `<semver>`，都是 `0.1.0`。`v` 只是给人看的标签。
- 版本号只有一个来源：`make dist VERSION=x.y.z` 通过 `-X main.version=` 注入二进制，
  同一份值写进 `Info.plist`。不要手改任何产物里的版本。
- 修 bug、补 Agent 目录 → `PATCH`；加功能、改 UI 结构 → `MINOR`；改数据格式 / 删功能 → 等 `1.0` 之后才谈 `MAJOR`。

## 一次发布的完整流程

```bash
# 1. 在 master 上，工作区力求干净（不传 VERSION 时，dirty 工作区会被 make dist 拒绝）
git switch master && git pull
make test

# 2. 本地打一次发布包并自检（这一步顺便验证 Intel 主机编 arm64 是否可用）
make dist VERSION=0.1.0

# 3. 检查产物：架构、zip 根、版本号、CLI 输出
lipo -archs AgentSweep.app/Contents/MacOS/AgentSweep     # 期望同时含 x86_64 与 arm64
unzip -Z1 dist/AgentSweep-v0.1.0-darwin-universal.zip | head -1   # 期望 AgentSweep.app/
plutil -extract CFBundleShortVersionString raw -o - AgentSweep.app/Contents/Info.plist
tar -xzf dist/agentsweep-v0.1.0-darwin-universal.tar.gz -O agentsweep > /tmp/agentsweep-term
chmod +x /tmp/agentsweep-term && /tmp/agentsweep-term version   # 期望 0.1.0
(cd dist && shasum -a 256 -c checksums.txt)

# 4. 打 tag 并推送 —— 推送 tag 就是发布动作，CI 会自动建 Release
git tag -a v0.1.0 -m "AgentSweep v0.1.0"
git push origin master
git push origin v0.1.0
```

推完 tag 去 Actions 看 `Release` 工作流：它会重跑 `make test`、`make dist`，
校验三个资产都存在且是 universal，再以**非 draft** 方式创建 GitHub Release（release notes 自动生成）。
任何一步失败都不会发出半成品。

> 不要用 `gh release create` 手动补发。手动发的包不会经过 workflow 里的校验，
> 而且容易和 tag 里的版本不一致。

## 资产命名契约（更新器按这个找包）

每次 Release 恰好三个资产：

| 资产 | 内容 |
|---|---|
| `AgentSweep-v<semver>-darwin-universal.zip` | 通用 `.app`（arm64 + x86_64），zip 根是 `AgentSweep.app/` |
| `agentsweep-v<semver>-darwin-universal.tar.gz` | 裸 CLI 二进制，包内文件名是 `agentsweep` |
| `checksums.txt` | `shasum -a 256` 格式：`<sha256>  <filename>`，只含上面两个文件 |

应用内更新器的选择顺序：

1. 优先 `AgentSweep-v<semver>-darwin-universal.zip`；
2. 没有则回退到 `AgentSweep-v<semver>-darwin-<arch>.zip`（`arm64` / `amd64`，为将来拆分留的口子）；
3. 下载后从 `checksums.txt` 取同名行的 sha256 校验，**不匹配就放弃安装**；
4. 解压要求根目录就是 `AgentSweep.app/`，多套一层目录会被判为坏包。

所以：

- 不要改前后缀，不要在文件名里加 `-signed`、`-beta` 之类的后缀；
- 不要换压缩工具。zip 用 `ditto -c -k --sequesterRsrc --keepParent`，
  它保留符号链接与资源分叉；用 `zip -r` 打出来的包在替换 `.app` 时可能丢东西；
- 签名变化不算资产变化。包名不变，更新器就能平滑升级。

## 构建产物从哪来

`make dist VERSION=x.y.z` 一次做完下面这些，缺任何前置文件都会立刻失败：

1. `CGO_ENABLED=1 GOARCH=arm64` 和 `GOARCH=amd64` 各编一遍（部署目标 13.0），`lipo -create` 成通用二进制。
   CC 优先用 CommandLineTools 的 clang 并带 `-isysroot`：本机 Xcode 自带的 `libclang_rt.osx.a`
   可能只有 x86_64 slice，裸 `clang -arch arm64` 会在链接时报
   `Undefined symbols ... ___isPlatformVersionAtLeast`；CLT 那份是 universal 的。
   只有 CLT 不存在时才退回 PATH 里的 `clang`；
2. `make bundle` 把通用二进制 + `Info.plist` + `AgentSweep.icns` 拼成 `AgentSweep.app`，
   ad-hoc 签名（`codesign --sign -`）；
3. `ditto` 压 zip，`tar` 打 CLI，`shasum` 写 `checksums.txt`；
4. 自检：zip 根、两个架构、`Info.plist` 版本、`agentsweep version` 输出，全对才算成功。

CI 跑在 `macos-latest`（Apple Silicon）上，与本地 Intel 主机互为验证：
两台机器都能编出两个架构，通用包才算真的成立。

## 签名与 Gatekeeper

当前是 **ad-hoc 签名，未做 Apple 公证**（需要付费开发者账号与 notarization 流程）。
后果：用户首次打开可能看到「无法验证开发者」，需要右键 →「打开」，
或执行 `xattr -dr com.apple.quarantine /Applications/AgentSweep.app`。

这不是 bug，README 的「下载与安装」里已经写明。等有了 Developer ID 再补公证，
届时资产名与更新流程都不变，只多一步 `codesign`/`notarytool`。

## 回滚

更新器只认「更高的 semver」，所以**永远不要复用已经发布过的版本号**。

- 包有问题（启动崩溃、数据损坏）：先修，再发 `PATCH` 版本（如 `0.1.1`）。
  用户更新器会自动升到新版本，旧包留在 Release 里也无妨。
- 严重问题需要立刻止损：在 GitHub 上把对应 Release 标为 **pre-release** 或删除，
  同时把 tag 删掉（`git push origin :refs/tags/v0.1.0`），然后发一个更高的 `PATCH`。
  已经装上的用户不会自动降级 —— 这也是为什么发版前 `make test` + 本地 `make dist` 不能省。
- 不要 force-push 或移动已发布的 tag：对已经下载过的用户不会生效，只会让 `git describe` 与发布记录对不上。

## 常见失败

| 现象 | 原因 |
|---|---|
| `make dist` 报「需要干净的发布版本号」 | 没传 `VERSION`，或工作区 dirty / 不是 tag 提交 |
| `clang: error: unknown target ... arm64` | Xcode CLT 太旧，升级 Command Line Tools |
| 链接报 `Undefined symbols ... ___isPlatformVersionAtLeast`（arm64） | 用了 Xcode 自带、只有 x86_64 slice 的 `libclang_rt.osx.a`；`make dist` 已优先走 CLT clang，若仍出现说明 CLT 缺失或损坏，重装 `xcode-select --install` |
| `缺少 build/AgentSweep.icns` | 先 `make icon`（需要 `brew install librsvg`） |
| 用户更新后打不开 | 检查 zip 根是不是 `AgentSweep.app/`、`Info.plist` 版本是否对、`LSMinimumSystemVersion` 是否被误改 |
| Release 工作流没触发 | tag 不是 `v*` 形式，或 tag 推到了 fork |
