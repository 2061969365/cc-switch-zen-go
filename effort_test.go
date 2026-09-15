// effort_test.go: v0.3.5 effort 归一化 + 跨格式透传 + Anthropic 翻译，纯离线。
package main

import (
	"testing"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	// max→high。
	in := mustJSON(t, `{"model":"m","reasoning":{"effort":"max"}}`)
	if got := normalizeReasoningEffort(in); got != "high" {
		t.Fatalf("max 应降级 high，得 %s", got)
	}
	if asStr(asMap(in["reasoning"])["effort"]) != "high" {
		t.Fatalf("应写回 high: %s", canon(in["reasoning"]))
	}
	// 缺省 high。
	in2 := mustJSON(t, `{"model":"m"}`)
	if got := normalizeReasoningEffort(in2); got != "high" {
		t.Fatalf("缺省应 high，得 %s", got)
	}
	// 非法档→high。
	in3 := mustJSON(t, `{"model":"m","reasoning":{"effort":"ultra"}}`)
	if got := normalizeReasoningEffort(in3); got != "high" {
		t.Fatalf("非法档应 high，得 %s", got)
	}
	// 合法档原样，summary 不透传。
	in4 := mustJSON(t, `{"model":"m","reasoning":{"effort":"xhigh","summary":"x"}}`)
	if got := normalizeReasoningEffort(in4); got != "xhigh" {
		t.Fatalf("xhigh 应保留，得 %s", got)
	}
	if _, ok := asMap(in4["reasoning"])["summary"]; ok {
		t.Fatalf("summary 不应透传: %s", canon(in4["reasoning"]))
	}
}

func TestEffortToBudget(t *testing.T) {
	cases := map[string]int{"low": 4096, "medium": 16000, "high": 32000, "xhigh": 48000}
	for e, want := range cases {
		if got := effortToBudget(e); got != want {
			t.Errorf("%s: got %d want %d", e, got, want)
		}
	}
}

func TestEffortCrossFormat(t *testing.T) {
	// chat->responses：effort 透传（max 降级）。
	out := chatToResponsesReq(mustJSON(t, `{"model":"m","reasoning":{"effort":"max"},
		"messages":[{"role":"user","content":"hi"}]}`))
	if asStr(asMap(out["reasoning"])["effort"]) != "high" {
		t.Fatalf("chat->responses max 应降 high: %s", canon(out["reasoning"]))
	}
	// chat->responses 缺省 high。
	out2 := chatToResponsesReq(mustJSON(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if asStr(asMap(out2["reasoning"])["effort"]) != "high" {
		t.Fatalf("chat->responses 缺省应 high: %s", canon(out2["reasoning"]))
	}
	// messages->responses：effort 透传。
	out3 := messagesToResponsesReq(mustJSON(t, `{"model":"m","reasoning":{"effort":"low"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	if asStr(asMap(out3["reasoning"])["effort"]) != "low" {
		t.Fatalf("messages->responses 应透 low: %s", canon(out3["reasoning"]))
	}
	// chat->messages：effort 翻 budget。
	out4 := chatToMessagesReq(mustJSON(t, `{"model":"m","reasoning":{"effort":"xhigh"},
		"messages":[{"role":"user","content":"hi"}]}`))
	if toFloat(asMap(out4["thinking"])["budget_tokens"]) != 48000 {
		t.Fatalf("chat->messages xhigh 应 48000: %s", canon(out4["thinking"]))
	}
	// responses->messages：effort 翻 budget。
	out5 := responsesToMessagesReq(mustJSON(t, `{"model":"m","reasoning":{"effort":"medium"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if toFloat(asMap(out5["thinking"])["budget_tokens"]) != 16000 {
		t.Fatalf("responses->messages medium 应 16000: %s", canon(out5["thinking"]))
	}
	// 自带合法 thinking 不动。
	out6 := chatToMessagesReq(mustJSON(t, `{"model":"m","reasoning":{"effort":"low"},
		"thinking":{"type":"enabled","budget_tokens":9999},
		"messages":[{"role":"user","content":"hi"}]}`))
	if toFloat(asMap(out6["thinking"])["budget_tokens"]) != 9999 {
		t.Fatalf("自带 thinking 不应覆盖: %s", canon(out6["thinking"]))
	}
	// responses->chat：目标无 effort 语义，不写入。
	out7 := responsesToChatReq(mustJSON(t, `{"model":"m","reasoning":{"effort":"low"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if _, ok := out7["reasoning"]; ok {
		t.Fatalf("responses->chat 不应带 reasoning: %s", canon(out7))
	}
}
