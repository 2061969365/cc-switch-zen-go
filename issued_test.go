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
	if !isCallerMismatch400("Referenced reasoning item 'rs_6aabda70054be95cf9bf4581:rs_01a0af4d471675fd86b0050a37b0475c' was not found or has expired") {
		t.Fatal("应识别复合 id 过期")
	}
	if isCallerMismatch400("context_length_exceeded") {
		t.Fatal("无关 400 不应误判")
	}
	if isCallerMismatch400("Model 'unknown' execution failed: model_not_found") {
		t.Fatal("model_not_found 不应误判")
	}
	learnReasoningID("rs_rotated")
	evictIssued("rs_rotated")
	if isIssued("rs_rotated") {
		t.Fatal("淘汰后不应放行")
	}
}

func TestPassthroughLearnSSE(t *testing.T) {
	// 上游原生 reasoning done 事件应被学到；有终态时透传 + [DONE]。
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

func TestPassthroughTruncatedUpstream(t *testing.T) {
	// 上游断在帧中间（无终态）：半截帧必须吞掉，不得透传；尾部应为 error 而非 [DONE]。
	raw := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_half\"" // 截断，无结尾
	var sb strings.Builder
	passthroughLearnSSE(&sb, strings.NewReader(raw))
	out := sb.String()
	if strings.Contains(out, "rs_half") {
		t.Fatalf("半截帧不应透传: %q", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("无终态不应补 [DONE]: %q", out)
	}
	if !strings.Contains(out, "upstream closed without terminal event") {
		t.Fatalf("无终态应发 error 帧: %q", out)
	}
	if isIssued("rs_half") {
		t.Fatal("半截帧 id 不应被学习")
	}
	// 正常流（有终态）回归：透传 + [DONE] 不变。
	raw2 := "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	var sb2 strings.Builder
	passthroughLearnSSE(&sb2, strings.NewReader(raw2))
	if !strings.Contains(sb2.String(), "[DONE]") {
		t.Fatalf("有终态应补 [DONE]: %q", sb2.String())
	}
}

func TestCrossFormatNoLeak(t *testing.T) {
	// responses->chat：reasoning 只取 summary 文本转 reasoning_content，不带加密串。
	chatReq := responsesToChatReq(mustJSON(t, `{"model":"m","input":[
		{"type":"reasoning","id":"rs_x","encrypted_content":"encX",
		 "summary":[{"type":"summary_text","text":"想了一下"}]}]}`))
	if strings.Contains(canon(chatReq), "encX") || strings.Contains(canon(chatReq), "encrypted_content") {
		t.Fatalf("跨格式 chat 不应泄漏加密串: %s", canon(chatReq))
	}
	// responses->messages：reasoning 直接丢弃。
	msgReq := responsesToMessagesReq(mustJSON(t, `{"model":"m","input":[
		{"type":"reasoning","id":"rs_x","encrypted_content":"encX","summary":[]}]}`))
	if strings.Contains(canon(msgReq), "encX") {
		t.Fatalf("跨格式 messages 不应泄漏加密串: %s", canon(msgReq))
	}
	// 逆转换造的无 id reasoning：带回时必丢（isIssued("")=false）。
	out := sanitizeResponsesInput(mustJSON(t, `{"model":"m","input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]}]}`))
	if len(asArr(out["input"])) != 0 {
		t.Fatalf("无 id reasoning 应丢弃: %s", canon(out["input"]))
	}
	// Tracked 版同样上报空放行。
	_, allowed := sanitizeResponsesInputTracked(mustJSON(t, `{"model":"m","input":[
		{"type":"reasoning","id":"rs_unknown","encrypted_content":"e","summary":[]}]}`))
	if len(allowed) != 0 {
		t.Fatalf("异源放行应为空: %v", allowed)
	}
}
