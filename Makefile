# AgentSweep 的构建只有 go build 一条路：没有 node、没有 bundler、没有 bindings 生成器。
#
# 前端是 embed 进二进制的纯静态文件，改完 CSS 直接 `make run` 就能看到，
# 不需要先跑一个 dev server —— 少一个进程就少一类「改了没生效」的问题。
APP     := AgentSweep
BIN     := bin/$(APP)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# GUI 通过 cgo 链接系统 WebKit，必须本机编译；CGO_ENABLED=0 只对 CLI 有效。
#
# 部署目标必须显式给：Go 1.27 的运行时对象是按 13.0 编译的，
# 不写就会在每次链接时刷一屏 "built for newer macOS version" 警告。
export MACOSX_DEPLOYMENT_TARGET ?= 13.0
export CGO_CFLAGS  ?= -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
export CGO_LDFLAGS ?= -mmacosx-version-min=$(MACOSX_DEPLOYMENT_TARGET)
.PHONY: build app run test vet fmt clean dev

build:
	@mkdir -p bin
	go build -tags production -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

# macOS 的 .app 是手工拼的：Wails 只是库，不需要它的 CLI 来打包。
app: build
	@rm -rf $(APP).app
	@mkdir -p $(APP).app/Contents/MacOS $(APP).app/Contents/Resources
	@cp $(BIN) $(APP).app/Contents/MacOS/$(APP)
	@sed 's/@VERSION@/$(VERSION)/' build/Info.plist > $(APP).app/Contents/Info.plist
	@if [ -f build/$(APP).icns ]; then cp build/$(APP).icns $(APP).app/Contents/Resources/$(APP).icns; fi
	@codesign --force --deep --sign - $(APP).app 2>/dev/null || true
	@echo "  $(APP).app"

run: app
	@open $(APP).app

dev:
	go run -tags production .

test:
	go vet ./... && go test ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin $(APP).app
