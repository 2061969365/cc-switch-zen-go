// headers_test.go: v0.3.8 真会话透传单测，纯离线。
package main

import (
	"net/http"
	"strings"
	"testing"
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
