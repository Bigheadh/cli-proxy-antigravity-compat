# Antigravity 请求兼容模块插件构建与验收脚本（适配 CLIProxyAPI v8.1.2 / 85225381）

PLUGIN_ID      := antigravity-compat
PLUGIN_VERSION := 0.1.0
DIST_DIR       := dist

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),windows)
	LIB_EXT := dll
else ifeq ($(GOOS),darwin)
	LIB_EXT := dylib
else
	LIB_EXT := so
endif

ifeq ($(GOARCH),amd64)
	AMD64_ENV := GOAMD64=v1 CGO_CFLAGS="-march=x86-64"
else
	AMD64_ENV :=
endif

PLUGIN_OUT := $(DIST_DIR)/$(PLUGIN_ID).$(LIB_EXT)
HEADER_OUT := $(DIST_DIR)/$(PLUGIN_ID).h
ARCHIVE_NAME := $(PLUGIN_ID)_$(PLUGIN_VERSION)_$(GOOS)_$(GOARCH).zip

.PHONY: all build build-race test unit-test acceptance package clean

all: build test

build:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) $(AMD64_ENV) \
		go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o $(PLUGIN_OUT) .
	@rm -f $(HEADER_OUT)
	@echo "构建完成: $(PLUGIN_OUT)"

build-race:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) $(AMD64_ENV) \
		go build -race -buildmode=c-shared -o $(PLUGIN_OUT) .
	@rm -f $(HEADER_OUT)
	@echo "构建完成 (race): $(PLUGIN_OUT)"

unit-test:
	CGO_ENABLED=1 go test -v -race ./compat/...

acceptance: build
	CGO_ENABLED=1 go test -v -race -count=1 ./acceptance_test/...

test: build
	CGO_ENABLED=1 go test -v -race -count=1 ./...

package: build
	@cd $(DIST_DIR) && zip -q -j $(ARCHIVE_NAME) $(PLUGIN_ID).$(LIB_EXT) && sha256sum $(ARCHIVE_NAME) > checksums.txt
	@echo "打包完成: $(DIST_DIR)/$(ARCHIVE_NAME)"

clean:
	rm -rf $(DIST_DIR)
