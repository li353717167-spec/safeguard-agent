BIN := bin/safeguard-agent
PKG := ./cmd/agent

.PHONY: help deps build run migrate ping test check fmt vet tidy clean

help:
	@echo "deps    拉取依赖 (go mod tidy)"
	@echo "build   编译到 $(BIN)"
	@echo "run     运行一次问答: make run Q='payment-service 错误率怎么样'"
	@echo "migrate 执行数据库迁移"
	@echo "ping    检查数据库连通性"
	@echo "test    跑单测（不需要数据库与模型端点）"
	@echo "check   提交前自检: fmt + vet + test"
	@echo "fmt/vet 格式化与静态检查"

deps:
	go mod tidy

build: deps
	go build -o $(BIN) $(PKG)

run: build
	$(BIN) -prompt "$(Q)"

migrate: build
	$(BIN) -migrate

ping: build
	$(BIN) -db-ping

test:
	go test ./...

check: fmt vet test

fmt:
	@# 只格式化本项目代码：gofmt 会递归目录，写成 "." 会把 .gomodcache 里的第三方源码一起重写
	gofmt -l -w ./cmd ./internal

vet:
	go vet ./...

clean:
	rm -rf bin
