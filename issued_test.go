// issued_test.go: Plan A 签发集合 + TTL + 400 淘汰 + 流式嗅探，纯离线。
package main

import (
	"strings"
	"testing"
	"time"
)

func TestIssuedSetAllowlist(t *testing.T) {
	// 学习：响应 output 里的 reasoning id 应被记住。
	learnReasoningIDs(mustJSON(t, `{"output":[
		{"type":"reasoning","id":"rs_mine","encrypted_content":"e30=","summary":[]},
		{"type":"message","content":[]}]}`))
	if !isIssued("rs_mine") {
		t.Fatal("自家签发 id 应放行")
	}
	// 未知 id 不放行。
	if isIssued("rs_old_caller") {
		t.Fatal("异源 id 不应放行")
	}
	// 放行路径：自家 id 的 reasoning（含串）应保留。
	out := sanitizeResponsesInput(mustJSON(t, `{"model":"m","input":[
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","id":"rs_mine","encrypted_content":"e30=","summary":[]},
		{"type":"reasoning","id":"rs_old_caller","encrypted_content":"encX","summary":[]},
		{"role":"user","content":[{"type":"input_text","text":"go"}]}]}`))
	items := asArr(out["input"])
	if len(items) != 3 {
		t.Fatalf("应剩 3 条（2 user + 1 自家 reasoning），得 %d: %s", len(items), canon(items))
	}
	for _, it := range items {
		m := asMap(it)
		if asStr(m["type"]) == "reasoning" && getStr(m, "id") != "rs_mine" {
			t.Fatalf("只应保留自家 id: %s", canon(m))
		}
	}
}

func TestIssuedTTLExpiry(t *testing.T) {
	learnReasoningID("rs_ttl")
	if !isIssued("rs_ttl") {
		t.Fatal("刚学习应放行")
	}
	// 人工过期。
	issuedMu.Lock()
	issuedIDs["rs_ttl"].exp = time.Now().Add(-time.Second)
	issuedMu.Unlock()
	if isIssued("rs_ttl") {
		t.Fatal("过期 id 不应放行")
	}
}

func TestCallerMismatchEvict(t *testing.T) {
	if !isCallerMismatch400("reasoning `encrypted_content` was not issued to this caller") {
		t.Fatal("应识别 caller 绑定失败")
	}
	if !isCallerMismatch400("invalid_encrypted_content") {
		t.Fatal("应识别 invalid_encrypted_content")
	}
	if isCallerMismatch400("context_length_exceeded") {
		t.Fatal("无关 400 不应误判")
	}
	learnReasoningID("rs_rotated")
	evictIssued("rs_rotated")
	if isIssued("rs_rotated") {
		t.Fatal("淘汰后不应放行")
	}
}

func TestPassthroughLearnSSE(t *testing.T) {
	// 上游原生 reasoning done 事件应被学到；透传字节应原样。
	raw := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_stream_native\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	var sb strings.Builder
	passthroughLearnSSE(&sb, strings.NewReader(raw))
	if !isIssued("rs_stream_native") {
		t.Fatal("流式原生 id 应被学习")
	}
	out := sb.String()
	if !strings.Contains(out, "rs_stream_native") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("透传不应改写帧: %q", out)
	}
	evictIssued("rs_stream_native")
}
