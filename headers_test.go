// headers_test.go: v0.3.8 真会话透传单测，纯离线。
package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 下游是 test provider（无 x-opencode-*，只有 X-Session-Id）：上游必须拿到真会话。
func TestInheritRealSessionFromAffinityHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("X-Session-Id", "ses_f53389880ffeCKjjiZ4JP2GyFA")
	src.Set("x-session-affinity", "ses_f53389880ffeCKjjiZ4JP2GyFA")
	src.Set("User-Agent", "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14")

	dst := http.Header{}
	setZenHeaders(dst, "public", "default")
	fabricated := dst.Get("X-Opencode-Session")
	if !strings.HasPrefix(fabricated, "ses_") {
		t.Fatalf("setZenHeaders 应先现编 ses_，实际 %q", fabricated)
	}
	inheritClientHeaders(dst, src)
	if got := dst.Get("X-Opencode-Session"); got != "ses_f53389880ffeCKjjiZ4JP2GyFA" {
		t.Errorf("真会话未覆盖现编值：got %q", got)
	}
	if got := dst.Get("User-Agent"); !strings.HasPrefix(got, "opencode/1.18.31") {
		t.Errorf("UA 未透传：got %q", got)
	}
}

// 下游是 opencode provider（自带 x-opencode-session）：保持原值，不被覆盖。
func TestInheritKeepsNativeOpencodeSession(t *testing.T) {
	src := http.Header{}
	src.Set("X-Opencode-Session", "ses_native001")
	src.Set("X-Opencode-Client", "cli")

	dst := http.Header{}
	setZenHeaders(dst, "public", "default")
	inheritClientHeaders(dst, src)
	if got := dst.Get("X-Opencode-Session"); got != "ses_native001" {
		t.Errorf("原生会话被覆盖：got %q", got)
	}
	if got := dst.Get("X-Opencode-Client"); got != "cli" {
		t.Errorf("client 未透传：got %q", got)
	}
}

// v0.3.10：签发结构必须合法（ses_ + 12hex + 14base62），否则上游 403。
func TestMintSessionFormat(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := mintID("ses")
		if len(id) != 30 || !strings.HasPrefix(id, "ses_") {
			t.Fatalf("长度/前缀错误：%q", id)
		}
		for _, c := range id[4:16] {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("时间部位非hex：%q", id)
			}
		}
		for _, c := range id[16:30] {
			if !strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", c) {
				t.Fatalf("随机部位非base62：%q", id)
			}
		}
	}
}

// 固定时间戳可精确断言（ts=1ms, ctr=1 → current=0x1001）。
func TestMintFixedTimestamp(t *testing.T) {
	id := mintIDAt("ses", 1)
	if !strings.HasPrefix(id, "ses_000000001001") {
		t.Errorf("时间部位错误：%q", id)
	}
	a, b := mintID("msg"), mintID("msg")
	if a == b {
		t.Errorf("连续签发重复：%q", a)
	}
}

// 回退签发也要合法（无会话下游不断 upstream 403）。
func TestSetZenHeadersFallbackMintValid(t *testing.T) {
	h := http.Header{}
	setZenHeaders(h, "public", "default")
	ses := h.Get("X-Opencode-Session")
	if len(ses) != 30 || !strings.HasPrefix(ses, "ses_") {
		t.Errorf("回退会话非法：%q", ses)
	}
	if req := h.Get("X-Opencode-Request"); len(req) != 30 || !strings.HasPrefix(req, "msg_") {
		t.Errorf("回退请求非法：%q", req)
	}
}

// 下游无任何会话头（curl 等）：保留现编值，防上游 MissingSessionID。
func TestInheritFallbackFabricatedSession(t *testing.T) {
	src := http.Header{}
	dst := http.Header{}
	setZenHeaders(dst, "public", "default")
	inheritClientHeaders(dst, src)
	if got := dst.Get("X-Opencode-Session"); !strings.HasPrefix(got, "ses_") {
		t.Errorf("回退现编值丢失：got %q", got)
	}
}

// v0.3.11：空闲超时的包体必须按时报错，不能永久阻塞。
func TestIdleTimeoutBodyFires(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	b := newIdleTimeoutBody(pr, 80*time.Millisecond)
	defer b.Close()
	start := time.Now()
	buf := make([]byte, 64)
	_, err := b.Read(buf)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("空闲超时未触发：err=%v elapsed=%v", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("错误不是超时：%v", err)
	}
	// 粘性：后续读同样报错。
	if _, err2 := b.Read(buf); err2 == nil {
		t.Fatalf("超时后应持续报错")
	}
}

// 有数据流动时不超时；正常 EOF 透传。
func TestIdleTimeoutBodyHealthy(t *testing.T) {
	pr, pw := io.Pipe()
	b := newIdleTimeoutBody(pr, 300*time.Millisecond)
	defer b.Close()
	go func() {
		_, _ = pw.Write([]byte("data: x\n\n"))
		time.Sleep(50 * time.Millisecond)
		_, _ = pw.Write([]byte("data: y\n\n"))
		pw.Close()
	}()
	var out []byte
	tmp := make([]byte, 32)
	for {
		n, err := b.Read(tmp)
		out = append(out, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if string(out) != "data: x\n\ndata: y\n\n" {
		t.Errorf("数据损坏：%q", out)
	}
	// Close 幂等。
	if err := b.Close(); err != nil {
		t.Errorf("二次 Close 报错：%v", err)
	}
}

// 不实现 Flusher 的 Writer 不得 panic。
type bareWriter struct{ header http.Header }

func (w *bareWriter) Header() http.Header         { return w.header }
func (w *bareWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *bareWriter) WriteHeader(int)             {}

func TestNewSSESinkNoFlusher(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic：%v", r)
		}
	}()
	s := newSSESink(&bareWriter{header: http.Header{}})
	s.emit(map[string]any{"type": "ping"})
	s.done()
}

// v0.3.10：非官方 UA 不得透传（上游连 UA 一起验），保留网关 zenUA。
func TestInheritForeignUAReplaced(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "deepseek-harness/0.1.5-rc.2 (+https://github.com/test)")
	src.Set("X-Session-Id", "ses_f53389880ffeCKjjiZ4JP2GyFA")

	dst := http.Header{}
	setZenHeaders(dst, "public", "default")
	inheritClientHeaders(dst, src)
	if got := dst.Get("User-Agent"); got != zenUA {
		t.Errorf("外来 UA 透传了：got %q", got)
	}
	if got := dst.Get("X-Opencode-Session"); got != "ses_f53389880ffeCKjjiZ4JP2GyFA" {
		t.Errorf("真会话丢失：got %q", got)
	}
}

// 官方 UA 仍透传原值。
func TestInheritOfficialUAPreserved(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14")

	dst := http.Header{}
	setZenHeaders(dst, "public", "default")
	inheritClientHeaders(dst, src)
	if got := dst.Get("User-Agent"); !strings.HasPrefix(got, "opencode/1.18.31") {
		t.Errorf("官方 UA 未保留：got %q", got)
	}
}
