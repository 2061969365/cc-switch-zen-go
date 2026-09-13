package main

// logx_test.go: 非阻塞日志回归单测，纯离线。
import (
	"testing"
	"time"
)

func TestLogfNonBlocking(t *testing.T) {
	// 尽量占满队列（后台消费者可能同时取走，不影响断言）。
	for i := 0; i < cap(logCh)+100; i++ {
		select {
		case logCh <- "fill":
		default:
		}
	}
	// 队列满时 logf 必须立即返回，绝不阻塞业务 goroutine。
	done := make(chan struct{})
	go func() { logf("x"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("logf 阻塞超过 2s，业务请求会被控制台回压拖住")
	}
	// 排空队列，保持环境干净。
	time.Sleep(100 * time.Millisecond)
	for {
		select {
		case <-logCh:
		default:
			return
		}
	}
}
