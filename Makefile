.PHONY: all build clean dev deps test fmt lint release

# 变量
BINARY_NAME=heliox-mon
BUILD_DIR=build
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-s -w -X main.Version=$(VERSION)

# 目标平台
PLATFORMS=linux/amd64 linux/arm64

# 默认目标
all: deps build

# 安装依赖
deps:
	go mod tidy
	go mod download

# 本地开发构建
dev:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/heliox-mon

# 生产构建（Linux AMD64）
build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/heliox-mon
	@echo "构建完成: $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64"

# 发布构建（同时构建 AMD64 和 ARM64）
release:
	@mkdir -p $(BUILD_DIR)
	@echo "正在构建 Linux/AMD64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/heliox-mon
	@echo "正在构建 Linux/ARM64..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/heliox-mon
	@echo "发布构建完成: $(BUILD_DIR)"

# 清理
clean:
	rm -rf $(BUILD_DIR)

# 运行测试
test:
	go test -v ./...

# 格式化代码（goimports 未安装时用 go run 临时拉取，避免 make fmt 报错）
fmt:
	go fmt ./...
	go run golang.org/x/tools/cmd/goimports@latest -w .

# 静态检查
lint:
	golangci-lint run
