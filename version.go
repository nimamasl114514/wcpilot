// version.go —— 版本与构建信息（发布用：go build -ldflags "-X main.version=8.0.0"）
package main

import (
	"fmt"
	"runtime"
	"strings"
)

// version 语义化版本；构建时可被 -ldflags 覆盖
var version = "8.0.0"

// commit 构建时的 git commit（release 脚本注入）
var commit = "dev"

// buildTime 构建时间（release 脚本注入）
var buildTime = "unknown"

const banner = "wcpilot —— AI 电脑操作工具（Windows / Go 标准库 / 零第三方依赖）"

func versionString() string {
	c := commit
	if len(c) > 12 {
		c = c[:12]
	}
	return fmt.Sprintf("wcpilot %s (commit %s, built %s, %s %s/%s)",
		version, c, buildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// shortVersion 单行版（doctor 输出用）
func shortVersion() string {
	return strings.TrimSpace(version)
}
