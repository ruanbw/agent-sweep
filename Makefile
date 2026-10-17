# AgentSweep 的构建只有 go build 一条路：没有 node、没有 bundler、没有 bindings 生成器。
#
# 前端是 embed 进二进制的纯静态文件，改完 CSS 直接 `make run` 就能看到，
# 不需要先跑一个 dev server —— 少一个进程就少一类「改了没生效」的问题。
APP     := AgentSweep
BIN     := bin/$(APP)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# 标签写成 v0.1.0，但版本号必须是裸 semver：CFBundleShortVersionString 不接受前导 v，
# `agentsweep version` 与应用内更新器也都按纯数字比较。用 override 是因为命令行传入的
# VERSION 优先级高于普通赋值，而 `make dist VERSION=v0.1.0` 同样应该被纠正。
override VERSION := $(patsubst v%,%,$(VERSION))
LDFLAGS := -s -w -X main.version=$(VERSION)

# 发版产物。名字是应用内更新器的契约，改名前先看 docs/RELEASING.md。
DIST      := dist
STAGE     := $(DIST)/.stage
ZIP       := $(DIST)/$(APP)-v$(VERSION)-darwin-universal.zip
TARBALL   := $(DIST)/agentsweep-v$(VERSION)-darwin-universal.tar.gz
CHECKSUMS := $(DIST)/checksums.txt

# GUI 通过 cgo 链接系统 WebKit，必须本机编译；CGO_ENABLED=0 只对 CLI 有效。
#
# 部署目标必须显式给：Go 1.27 的运行时对象是按 13.0 编译的，
# 不写就会在每次链接时刷一屏 "built for newer macOS version" 警告。
export MACOSX_DEPLOYMENT_TARGET ?= 13.0
export CGO_CFLAGS  ?= -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
export CGO_LDFLAGS ?= -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)

# 交叉编译用哪套 clang：这件事踩过坑，别随手改回裸 `clang`。
#
# 本机 Xcode 自带的 libclang_rt.osx.a 只有 x86_64 一个 slice（Intel 主机上实测），
# 于是 CC="clang -arch arm64" 会在链接阶段报
#   Undefined symbols for architecture arm64: ___isPlatformVersionAtLeast
# CommandLineTools 里的那份是 universal 的，所以优先用 CLT 的 clang 并显式给 -isysroot；
# 只有 CLT 不存在时才退回 PATH 里的 clang。GitHub 的 macos runner 两者都在，
# 本地 Intel 与 CI 的 Apple Silicon 走的是同一条命令。
CLT_CLANG  := /Library/Developer/CommandLineTools/usr/bin/clang
DARWIN_SDK := $(shell xcrun --sdk macosx --show-sdk-path 2>/dev/null)
ifeq ($(wildcard $(CLT_CLANG)),)
  CC_ARM64 := clang -arch arm64
  CC_AMD64 := clang -arch x86_64
else
  CC_ARM64 := $(CLT_CLANG) -arch arm64 -isysroot $(DARWIN_SDK)
  CC_AMD64 := $(CLT_CLANG) -arch x86_64 -isysroot $(DARWIN_SDK)
endif
.PHONY: build app bundle universal run test vet fmt clean dev icon dist

build:
	@mkdir -p bin
	@# dist 会在同一路径留下 lipo 拼出来的 fat 文件，而 go build 拒绝覆盖「不是目标文件」
	@# 的已有文件（cmd/go 的 checkDstOverwrite）。先删掉，否则 make dist 之后 make build 会失败。
	@rm -f $(BIN)
	go build -tags production -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

# 只打包 .app，不碰编译。
#
# 单独拆出来是因为 dist 里 lipo 出来的通用二进制也要走同一段逻辑：
# 本地 `make app` 和发版 zip 里的 .app 必须结构完全一致，否则「本地能开、下载的打不开」
# 这类问题只会在用户机器上出现。缺文件直接报错退出，不做「尽力而为」的打包。
bundle:
	@test -x $(BIN) || { echo "缺少 $(BIN)：先 make build（或 make dist）" >&2; exit 1; }
	@test -f build/Info.plist || { echo "缺少 build/Info.plist" >&2; exit 1; }
	@rm -rf $(APP).app
	@mkdir -p $(APP).app/Contents/MacOS $(APP).app/Contents/Resources
	@cp $(BIN) $(APP).app/Contents/MacOS/$(APP)
	@sed 's/@VERSION@/$(VERSION)/' build/Info.plist > $(APP).app/Contents/Info.plist
	@if [ -f build/$(APP).icns ]; then cp build/$(APP).icns $(APP).app/Contents/Resources/$(APP).icns; fi
	@codesign --force --deep --sign - $(APP).app
	@echo "  $(APP).app ($(VERSION))"

# macOS 的 .app 是手工拼的：Wails 只是库，不需要它的 CLI 来打包。
app: build
	@$(MAKE) --no-print-directory bundle VERSION="$(VERSION)"

# 通用二进制：每个架构各编一遍，再用 lipo 拼成一个文件。
#
# cgo 链接的是系统 WebKit，所以不能靠 Go 自己的交叉编译，必须让 clang 换 target：
# CC 里带 -arch 这层包装是关键，具体用哪个 clang 见上面的 CC_ARM64/CC_AMD64。
# Intel 机器上编 arm64 也成立 —— 发布前用 `make dist` 真编一次验证，不要只信架构表。
universal:
	@mkdir -p bin
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 CC="$(CC_ARM64)" \
		go build -tags production -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-arm64 .
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 CC="$(CC_AMD64)" \
		go build -tags production -trimpath -ldflags="$(LDFLAGS)" -o bin/$(APP)-amd64 .
	@rm -f $(BIN)
	lipo -create -output $(BIN) bin/$(APP)-arm64 bin/$(APP)-amd64
	@lipo -info $(BIN)

# 发布产物。产物名是契约，应用内更新器按
#   AgentSweep-v<semver>-darwin-universal.zip
# 找包，并从 checksums.txt 核对 sha256。改名 = 更新器静默认为「没有新版本」。
#
# 所有前置检查都放在最前面：CI 上宁可立刻红，也不要产出一个名字对、内容错的包。
dist:
	@case "$(VERSION)" in \
	  dev|*dirty*|*-g[0-9a-f]*) \
	    echo "make dist 需要干净的发布版本号：make dist VERSION=0.1.0（当前 $(VERSION)）" >&2; exit 1;; \
	esac
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z][0-9A-Za-z.-]*)?$$' || \
	  { echo "VERSION 不是 semver：$(VERSION)" >&2; exit 1; }
	@test -f build/Info.plist || { echo "缺少 build/Info.plist" >&2; exit 1; }
	@test -f build/$(APP).icns || { echo "缺少 build/$(APP).icns：先 make icon" >&2; exit 1; }
	@command -v lipo >/dev/null || { echo "缺少 lipo：需要 Xcode Command Line Tools" >&2; exit 1; }
	@if [ -x $(CLT_CLANG) ]; then \
	  test -n "$(DARWIN_SDK)" || { echo "找不到 macOS SDK：xcrun --sdk macosx --show-sdk-path 为空" >&2; exit 1; }; \
	fi
	@rm -rf $(DIST) $(APP).app
	@mkdir -p $(DIST) $(STAGE)
	@$(MAKE) --no-print-directory universal VERSION="$(VERSION)"
	@$(MAKE) --no-print-directory bundle VERSION="$(VERSION)"
	@echo "  打包 $(ZIP)"
	@ditto -c -k --sequesterRsrc --keepParent $(APP).app "$(ZIP)"
	@cp $(BIN) $(STAGE)/agentsweep
	@COPYFILE_DISABLE=1 tar --no-xattrs -czf "$(TARBALL)" -C $(STAGE) agentsweep
	@rm -rf $(STAGE)
	@cd $(DIST) && shasum -a 256 $(notdir $(ZIP)) $(notdir $(TARBALL)) > $(notdir $(CHECKSUMS))
	@# 打包完当场自检：zip 根必须是 AgentSweep.app/，架构、版本号、CLI 输出都要对。
	@unzip -Z1 "$(ZIP)" | head -1 | grep -qx '$(APP).app/' || \
	  { echo "zip 根不是 $(APP).app/：更新器会解压失败" >&2; exit 1; }
	@lipo -archs $(APP).app/Contents/MacOS/$(APP) | grep -qw arm64 || \
	  { echo "缺少 arm64 slice" >&2; exit 1; }
	@lipo -archs $(APP).app/Contents/MacOS/$(APP) | grep -qw x86_64 || \
	  { echo "缺少 x86_64 slice" >&2; exit 1; }
	@test "$$(plutil -extract CFBundleShortVersionString raw -o - $(APP).app/Contents/Info.plist)" = "$(VERSION)" || \
	  { echo "Info.plist 版本号与 $(VERSION) 不一致" >&2; exit 1; }
	@test "$$($(BIN) version)" = "$(VERSION)" || \
	  { echo "二进制打印的版本号与 $(VERSION) 不一致" >&2; exit 1; }
	@echo "  $(DIST)/"
	@ls -lh $(ZIP) $(TARBALL) $(CHECKSUMS) | sed 's/^/    /'

# 图标是 SVG → iconset → icns，只在改了 build/icon/*.svg 时才需要跑。
# 依赖 librsvg 的 rsvg-convert（brew install librsvg）与系统自带的 iconutil。
icon:
	@build/icon/render.sh

run: app
	@open $(APP).app

dev:
	go run -tags production .

test:
	go vet ./... && go test ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin $(APP).app $(DIST)
