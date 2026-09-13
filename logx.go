package main

// logx.go: 非阻塞日志。
//
// 背景：Windows 控制台一旦进入选中（QuickEdit 标记模式）或输出堆积，
// conhost 即停止消费，同步写 stderr 会阻塞调用方。网关曾在请求处理路径
// 上直接 log.Printf，黑窗口一点就卡，请求被拖住，agent 对话跟着卡死，
// 且日志看上去“卡着不刷”（v0.3.1 现象，与 ZEN_DEBUG 开关无关）。
//
// 对策：日志先进 2048 缓冲 channel，由独立 goroutine 刷盘；队列满直接
// 丢弃，绝不阻塞业务 goroutine。启动失败等致命错误仍用标准 log.Fatal。

import (
	"fmt"
	"os"
	"time"
)

var logCh = make(chan string, 2048)

func init() {
	go func() {
		for s := range logCh {
			fmt.Fprintln(os.Stderr, s)
		}
	}()
}

// logf 非阻塞打日志（替代 log.Printf 的所有业务路径调用）。
func logf(format string, args ...any) {
	msg := fmt.Sprintf(time.Now().Format("2006/01/02 15:04:05 ")+format, args...)
	select {
	case logCh <- msg:
	default:
	}
}
