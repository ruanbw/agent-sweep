# 发布 AgentSweep

这份文档写给两类人：三个月后的自己，和第一次发版的贡献者。
核心只有一句：**产物名是契约**。应用内更新器靠文件名挑包、靠 `checksums.txt` 验包，
名字一改，用户那边不会报错，只会永远显示「已是最新版本」。

## 版本策略

- 严格 [SemVer](https://semver.org/lang/zh-CN/)：`MAJOR.MINOR.PATCH`，git tag 带 `v` 前缀，`v0.1.0`。
- **首个版本 `v0.1.0`。** `0.x` 阶段不承诺向后兼容：清理逻辑、索引格式、适配器字段都可能在小版本里改。
- 产物里的版本号**一律不带 `v`**：`CFBundleShortVersionString`、`agentsweep version` 的输出、
  文件名里的 `<semver>`，都是 `0.1.0`。`v` 只是给人看的标签。
- 版本号只有一个来源：`make dist VERSION=x.y.z` 通过 `-X main.version=` 注入二进制，
  同一份值写进 `Info.plist`。不要手改任何产物里的版本。
- 修 bug、补 Agent 目录 → `PATCH`；加功能、改 UI 结构 → `MINOR`；改数据格式 / 删功能 → 等 `1.0` 之后才谈 `MAJOR`。
- **含连字符的版本是预发布**（`v0.2.0-rc.1`、`v0.2.0-beta.2`）。发布流水线会自动把它标成
  prerelease；更新器读的是 GitHub 的 `/releases/latest`，而 latest 会跳过 prerelease ——
  所以 RC 不会被推给普通用户，只有手动下载或 `agentsweep update --check` 之外的显式渠道才会看到。

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

# 4. 打 tag 并推送 —— 推送 tag 就是发布动作
git tag -a v0.1.0 -m "AgentSweep v0.1.0"
git push origin master
git push origin v0.1.0
```

推完 tag 去 Actions 看 `Release` 工作流：它先重跑 `make test` 和 `make dist`，
然后 **先建 draft release → 上传三个资产 → 校验远端资产集合与 sha256 → 最后才公开**。
任何一步失败，Release 都停在 draft：匿名用户和 `/releases/latest` 都看不到，
更新器也看不到 —— 不会出现「提示有新版本、却下载失败」。

> 不要用 `gh release create` 手动补发。手动发的包不会经过 workflow 里的校验，
> 而且容易和 tag 里的版本不一致。

## 发布流水线：draft → 校验 → 公开

`release.yml` 的每一步都是「失败即停」，顺序如下：

1. **draft**：`gh release create <tag> --draft --verify-tag --generate-notes`，此时不带资产。
   预发布版本同时加 `--prerelease`。
2. **上传**：`gh release upload <tag> <三个资产> --clobber`。
3. **核对资产集合**：`gh release view --json assets` 必须**恰好**是三个预期文件名，
   多一个少一个都失败（`diff` 报错即停）。
4. **回读校验**：把远端已上传的三个文件下载到临时目录，`cmp` 远端 `checksums.txt`
   与本地一致，再对下载副本跑 `shasum -a 256 -c checksums.txt`。
   这证明「CDN 上的字节 = 刚打出来的字节」，而不只是本地文件正确。
5. **公开**：`gh release edit <tag> --draft=false`（预发布再加 `--prerelease`），
   并断言 `isDraft=false`、`isPrerelease` 与 tag 通道一致。

失败后的收尾：draft 可以直接在 GitHub 上删除；tag 也可以删掉重打
（`git push origin :refs/tags/v0.1.0`），因为还没有人能看到这次发布。

CI（`ci.yml`）在每次 push/PR 都会跑 `go vet`、`go test -race` 和
`make dist VERSION=0.0.0-ci` 冒烟：发版脚本本身也是代码，不能等打了 tag 才发现坏了。

## 资产命名契约（更新器按这个找包）

每次 Release 恰好三个资产（预发布也一样，只差版本号里的 `-rc.N`）：

| 资产 | 内容 |
|---|---|
| `AgentSweep-v<semver>-darwin-universal.zip` | 通用 `.app`（arm64 + x86_64），zip 根是 `AgentSweep.app/` |
| `agentsweep-v<semver>-darwin-universal.tar.gz` | 裸 CLI 二进制，包内文件名是 `agentsweep` |
| `checksums.txt` | `shasum -a 256` 格式：`<sha256>  <filename>`，只含上面两个文件 |

`internal/update.Client.AssetFor` 的实际挑包规则（与代码保持一致）：

1. 只看文件名同时含 `agentsweep` 和 `darwin`、且以 `.zip` 结尾的附件；
2. 名字含 `universal` 的优先（一个包覆盖两种 CPU）；
3. 否则按本机架构选：`amd64` → `darwin-amd64.zip` 或 `darwin-intel.zip`；
   `arm64` → `darwin-arm64.zip` 或 `darwin-apple-silicon.zip`；
4. 选中后从 `checksums.txt` 取**同名行**的 sha256；没有这一行就拒绝安装。

所以：

- 不要改前后缀，不要在文件名里加 `-signed`、`-beta` 之类的额外后缀；
- 压缩包必须只有一个顶层目录，且以 `.app` 结尾（正常就是 `AgentSweep.app/`）。
  更新器用 Go 标准库 `archive/zip` 解压，会忽略 `ditto` 放进来的 `__MACOSX/`，
  拒绝多个顶层目录、`../` 越界路径和可疑符号链接；
- 用 `ditto -c -k --sequesterRsrc --keepParent` 打 zip 只是 macOS 的打包惯例，
  **不是更新器的硬要求**：更新器解压后会核对包内 `Info.plist` 版本，并在签名失效时重做 ad-hoc 签名，
  不依赖资源分叉或扩展属性。真正不能变的是「zip 根是单个 `AgentSweep.app/`」。
- 签名变化不算资产变化。包名不变，更新器就能平滑升级。

## 校验能证明什么

`checksums.txt` 解决的是**传输损坏与上传错包**：下载到的 zip 必须与发布附件里的清单一致，
否则直接放弃安装；它还会核对包内 `Info.plist` 版本等于发布 tag。

它**不能证明发布本身没被替换**：`checksums.txt` 和 zip 来自同一个 GitHub Release，
仓库或账号被攻破时两者可以一起改。真正的信任边界是 GitHub 账号、仓库权限和 HTTPS，
不是这个文件。当前构建是 **ad-hoc 签名、未做 Apple 公证**，也不提供可核验的开发者身份。
如果威胁模型里包含「发布渠道被攻破」，需要的是 Developer ID + notarization，甚至独立签名，
而不是更长的 checksum 列表。

完整的威胁模型与每条边界对应到哪段代码，见 [`SECURITY.md`](SECURITY.md)。

## 发布纪律：清单、附件、签名

更新器只能证明「下载到的字节 == 同一个发布声明的字节」。这条链成立的前提是发布格式
本身没被破坏，所以下面三条不是建议，是发布的一部分。

**一、`checksums.txt` 必须和 zip 在同一次 Release 里。**

`Checksums` 只在该 Release 的资产列表里找 `checksums.txt`，找不到就拒绝安装；
zip 里有、清单里没有同名行，同样拒绝。清单不能滞后、不能补发、不能放到另一个
release 或 README 里——它和包必须能被同一份发布元数据同时看到，否则「同源」这个
前提就不成立。预发布也一样，三个资产一个不少。

**二、不要手工改动已发布的附件。**

附件一旦公开，用户可能已经下载；重新上传会让 CDN 上的字节与 tag 不再对应，
而更新器只核对「包与清单是否一致」，不会去仓库里比对历史。具体来说：

- 不要 `gh release upload --clobber` 覆盖**已公开**的资产（workflow 对 draft 用 `--clobber`
  是为了可重跑，那时还没有人能看到）；
- 不要为了修一句 release note 顺手重传 zip；
- 不要移动已发布的 tag 去「修版本号」。真要止损，按文末「回滚」一节把 release
  标成 prerelease 或删除，而不是重传一个同名包。

包有问题就走新版本：修完发一个更高的 `PATCH`，旧包留在 Release 里也无妨。
已经装上的用户只认更高的 semver，覆盖附件不会让他们更新，只会让新装用户拿到一个
与发布记录对不上的包。

**三、加固是可选路径，改了它就要同步改更新器。**

当前是 ad-hoc 签名，这只能让包跑起来，不证明发布者身份。两条加固路线都存在，
但不做也不影响发布：

- **Developer ID 签名 + `notarytool` 公证**：Gatekeeper 能回答「谁发布的」，
  代价是付费开发者账号；资产名、zip 根、更新流程都不变，只多一步签名与公证；
- **Minisign/EdDSA 签 `checksums.txt`**：公钥编进二进制，私钥只在 CI secret 或
  离线介质，代价是密钥轮换必须发新版。

无论选哪条，**校验点在更新器的下载完成之后、解压之前**——那时还是发布方签过的
原始字节；解压后再校验，只能证明包内自洽，等于什么都没证明。
在签名校验落地之前，不要在发布说明里暗示「有签名 = 可信」。

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

## 更新器实际做了什么

`internal/update` 是 GUI 与 CLI 共用的同一份实现：

1. **检查**：`GET /repos/ruanbw/agent-sweep/releases/latest`。GUI 在启动后于后台检查，
   但**最多每 6 小时联网一次**（结果缓存在 `update.json`，设置里可关闭自动检查）；
   `agentsweep update --check` 是显式检查，不受缓存限制。
2. **下载**：按上面的规则挑 zip，存到 `~/Library/Application Support/AgentSweep/` 下的临时目录。
3. **校验**：sha256 对 `checksums.txt`；解压后核对包内版本等于发布版本、且比当前版本新。
4. **替换**：先把旧包改名为 `AgentSweep.app.old-<unix 时间戳>`，把新包复制到原路径并校验，
   成功后才删掉旧包；随后去掉 `com.apple.quarantine`，并校验签名（失效则重做 ad-hoc 签名）。
5. **重启**：GUI 写一个 `relaunch.sh`，等当前进程退出后再 `open -n` 新包。

两个必须知道的边界：

- **替换不是一步原子的。** 旧包改名与新包复制之间如果崩溃/断电，目录里会只剩
  `AgentSweep.app.old-<时间戳>`，应用「消失」。手动恢复：
  `mv /Applications/AgentSweep.app.old-<时间戳> /Applications/AgentSweep.app`。
  替换的是**当前正在运行的那个 bundle**（可能在 `/Applications`，也可能在 `~/Applications`），
  用户数据目录 `~/Library/Application Support/AgentSweep/` 不参与替换。
- **v0.1.0 的更新器装完后只退出、不会自动重启**，需要手动打开一次；从 v0.1.1 起自动重启。
  所以 `v0.1.0 → v0.1.1` 这一次升级会表现为「应用自己关了」，不是崩溃。

## 签名与 Gatekeeper

当前是 **ad-hoc 签名，未做 Apple 公证**（需要付费开发者账号与 notarization 流程）。
后果：用户首次打开可能看到「无法验证开发者」，需要右键 →「打开」，
或执行 `xattr -dr com.apple.quarantine /Applications/AgentSweep.app`（更新器会自动做这一步）。

这不是 bug，README 的「下载与安装」里已经写明。等有了 Developer ID 再补公证，
届时资产名与更新流程都不变，只多一步 `codesign`/`notarytool`。

## 回滚

更新器只认「更高的 semver」，所以**永远不要复用已经发布过的版本号**。

- 包有问题（启动崩溃、数据损坏）：先修，再发 `PATCH` 版本（如 `0.1.1`）。
  用户更新器会自动升到新版本，旧包留在 Release 里也无妨。
- 严重问题需要立刻止损：在 GitHub 上把对应 Release 标为 **pre-release** 或删除，
  同时把 tag 删掉（`git push origin :refs/tags/v0.1.0`），然后发一个更高的 `PATCH`。
  已经装上的用户不会自动降级 —— 这也是为什么发版前 `make test` + 本地 `make dist` 不能省。
- 更新过程本身可能留下现场：见上一节「替换不是一步原子的」。发布说明里如果要指导用户，
  给的是手动 `mv` 恢复命令，而不是「重装即可」——数据不会丢，但应用会暂时消失。
- 不要 force-push 或移动已发布的 tag：对已经下载过的用户不会生效，只会让 `git describe` 与发布记录对不上。

## 常见失败

| 现象 | 原因 |
|---|---|
| `make dist` 报「需要干净的发布版本号」 | 版本号是 `dev`、含 `dirty`、是 `*-g<sha>*` 的 git describe 结果，或不是 semver。发布时显式传 `VERSION=x.y.z`；不带 `VERSION` 时要求当前 commit 有干净 tag |
| `make dist` 报「VERSION 不是 semver」 | 同上：只接受 `MAJOR.MINOR.PATCH` 或带连字符/加号的预发布后缀 |
| `clang: error: unknown target ... arm64` | Xcode CLT 太旧，升级 Command Line Tools |
| 链接报 `Undefined symbols ... ___isPlatformVersionAtLeast`（arm64） | 用了 Xcode 自带、只有 x86_64 slice 的 `libclang_rt.osx.a`；`make dist` 已优先走 CLT clang，若仍出现说明 CLT 缺失或损坏，重装 `xcode-select --install` |
| `缺少 build/AgentSweep.icns` | 先 `make icon`（需要 `brew install librsvg`） |
| 用户更新后打不开 | 检查 zip 根是不是单个 `AgentSweep.app/`、`Info.plist` 版本是否对、`LSMinimumSystemVersion` 是否被误改 |
| 更新后应用「消失」，只剩 `AgentSweep.app.old-*` | 替换过程中崩溃；把 `.old-*` 改回 `AgentSweep.app` 即可 |
| Release 工作流停在 draft | 该次发布失败，draft 对用户不可见；看 Actions 日志修好后可删 draft/tag 重发 |
| Release 工作流没触发 | tag 不是 `v*` 形式，或 tag 推到了 fork |
| RC 出现在了「最新版本」 | 发布时漏了 `--prerelease`（workflow 会按 tag 里的连字符自动加；手动发布别漏） |
