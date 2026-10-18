module github.com/mechrevo/safeguard-agent

go 1.24.0

// 依赖由 `go mod tidy` 解析后写入（首次构建前请先执行，见 README「快速开始」）。
// D1 唯一的外部依赖是 MySQL 驱动：
//   github.com/go-sql-driver/mysql
// 模型调用、迁移执行、幂等 key 全部使用标准库实现，不引入框架。

require (
	github.com/go-sql-driver/mysql v1.10.1
	gorm.io/driver/mysql v1.6.0
	gorm.io/gorm v1.31.2
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	golang.org/x/text v0.20.0 // indirect
)
