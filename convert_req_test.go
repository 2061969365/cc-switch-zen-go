// convert_req_test.go: P1b 请求清洗单测，纯离线。
package main

import (
	"strings"
	"testing"
)

func TestFilterPrivateParams(t *testing.T) {
	in := mustJSON(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"_session_token":"x","_internal":{"a":1},
		"tools":[{"type":"function","function":{"name":"f","parameters":
			{"type":"object","properties":{"_keep":{"type":"string"},"ok":{"type":"string"}}}}}],
		"metadata":{"_a":1,"b":2}}`)
	out, ok := filterPrivateParams(in).(map[string]any)
	if !ok {
		t.Fatal("应返回 map")
	}
	for _, k := range []string{"_session_token", "_internal"} {
		if _, found := out[k]; found {
			t.Errorf("顶层 %s 未删", k)
		}
	}
	if _, found := asMap(out["metadata"])["_a"]; found {
		t.Errorf("嵌套 _a 未删")
	}
	// properties 下划线 key 豁免。
	props := asMap(asMap(asMap(asMap(asArr(out["tools"])[0])["function"])["parameters"])["properties"])
	if _, found := props["_keep"]; !found {
		t.Errorf("properties 下 _keep 应保留")
	}
	// 不污染输入。
	if _, found := in["_session_token"]; !found {
		t.Errorf("输入被污染")
	}
	if _, found := asMap(in["metadata"])["_a"]; !found {
		t.Errorf("输入嵌套被污染")
	}
}

func TestCanonicalArguments(t *testing.T) {
	if got := canonicalArguments(""); got != "{}" {
		t.Errorf("空串应 {}, 得 %s", got)
	}
	if got := canonicalArguments("  "); got != "{}" {
		t.Errorf("空白应 {}, 得 %s", got)
	}
	if got := canonicalArguments("{bad json"); got != "{}" {
		t.Errorf("非法应 {}, 得 %s", got)
	}
	if got := canonicalArguments("[1,2]"); got != "{}" {
		t.Errorf("非对象应 {}, 得 %s", got)
	}
	got := canonicalArguments(`{"z":1,"a":2,"m":{"y":1,"b":2}}`)
	if got != `{"a":2,"m":{"b":2,"y":1},"z":1}` {
		t.Errorf("键未排序: %s", got)
	}
}

func TestStripThinkingSignature(t *testing.T) {
	in := mustJSON(t, `{"model":"m","thinking":{"type":"enabled","budget_tokens":1000},
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"t","signature":"sig123"},
			{"type":"text","text":"hi"}]}]}`)
	stripThinkingSignature(in)
	if _, found := in["thinking"]; found {
		t.Errorf("顶层 thinking 未删")
	}
	blocks := asArr(asMap(asArr(in["messages"])[0])["content"])
	if _, found := asMap(blocks[0])["signature"]; found {
		t.Errorf("signature 未删")
	}
	if asStr(asMap(blocks[0])["thinking"]) != "t" {
		t.Errorf("thinking 内容块应保留")
	}
}

func TestClampMediaURL(t *testing.T) {
	short := "data:image/png;base64,AAAA"
	if got := clampMediaURL(short); got != short {
		t.Errorf("短 URL 不应动")
	}
	httpURL := "https://example.com/" + strings.Repeat("x", 100*1024)
	if got := clampMediaURL(httpURL); got != httpURL {
		t.Errorf("http URL 不应动")
	}
	big := "data:image/png;base64," + strings.Repeat("A", 100*1024)
	got := clampMediaURL(big)
	if len(got) >= len(big) || !strings.Contains(got, "[omitted ") || !strings.Contains(got, " bytes]") {
		t.Errorf("大 data URL 未钳制: len=%d", len(got))
	}
	if !strings.HasPrefix(got, "data:image/png;base64,[omitted ") {
		t.Errorf("钳制格式错: %.60s", got)
	}
}

func TestMediaPlaceholderInConverters(t *testing.T) {
	// messages->chat：document 块应转占位文本，不静默丢。
	out := messagesToChatReq(mustJSON(t, `{"model":"m","messages":[
		{"role":"user","content":[{"type":"document","source":{"type":"text","data":"pdf"}}]}]}`))
	msgs := asArr(out["messages"])
	if len(msgs) != 1 {
		t.Fatalf("应 1 条消息，得 %d", len(msgs))
	}
	c := asStr(asMap(msgs[0])["content"])
	if !strings.Contains(c, "unsupported media omitted: document") {
		t.Errorf("缺占位: %v", asMap(msgs[0])["content"])
	}
	// chat->responses：超大图片应被钳制。
	bigImg := "data:image/png;base64," + strings.Repeat("B", 100*1024)
	out2 := chatToResponsesReq(mustJSON(t, `{"model":"m","messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"`+bigImg+`"}}]}]}`))
	items := asArr(out2["input"])
	u := asStr(asMap(asMap(items[0])["content"].([]any)[0])["image_url"])
	if !strings.Contains(u, "[omitted ") {
		t.Errorf("图片未钳制: len=%d", len(u))
	}
	// chat->messages：tool_calls 空 arguments 应 canonical 为 {}。
	out3 := chatToMessagesReq(mustJSON(t, `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":""}}]}]}`))
	tcs := asArr(asMap(asArr(out3["messages"])[0])["content"])
	if got := canon(asMap(tcs[0])["input"]); got != "{}" {
		t.Errorf("空 arguments 应 {}, 得 %s", got)
	}
	// responses->chat：乱序 arguments 应重排。
	out4 := responsesToChatReq(mustJSON(t, `{"model":"m","input":[
		{"type":"function_call","call_id":"c9","name":"f","arguments":"{\"z\":1,\"a\":2}"}]}`))
	fc := asMap(asArr(asMap(asArr(out4["messages"])[0])["tool_calls"])[0])
	if got := asStr(asMap(fc["function"])["arguments"]); got != `{"a":2,"z":1}` {
		t.Errorf("arguments 未 canonical: %s", got)
	}
}

func TestStripOneMSuffix(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5[1M]": "claude-sonnet-4-5",
		"gpt-5[1m]":             "gpt-5",
		"mimo-v2.5-free":        "mimo-v2.5-free",
		"m[1M]m":                "m[1M]m",
		"claude-x[1M] ":         "claude-x",
		"":                      "",
	}
	for in, want := range cases {
		if got := stripOneMSuffix(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestEnsureMessagesDefaults(t *testing.T) {
	// 缺 max_tokens 补 8192。
	u := mustJSON(t, `{"model":"m","messages":[]}`)
	ensureMessagesDefaults(u)
	if toFloat(u["max_tokens"]) != 8192 {
		t.Errorf("应补 8192，得 %v", u["max_tokens"])
	}
	// 已有不覆盖。
	u2 := mustJSON(t, `{"model":"m","max_tokens":100}`)
	ensureMessagesDefaults(u2)
	if toFloat(u2["max_tokens"]) != 100 {
		t.Errorf("不应覆盖，得 %v", u2["max_tokens"])
	}
	// thinking budget 过小矫正。
	u3 := mustJSON(t, `{"model":"m","max_tokens":64000,
		"thinking":{"type":"enabled","budget_tokens":512}}`)
	ensureMessagesDefaults(u3)
	if toFloat(asMap(u3["thinking"])["budget_tokens"]) != 32000 {
		t.Errorf("budget 应矫为 32000，得 %v", asMap(u3["thinking"])["budget_tokens"])
	}
	// budget>=max_tokens 时 max_tokens 最小抬升。
	u4 := mustJSON(t, `{"model":"m","max_tokens":2000,
		"thinking":{"type":"enabled","budget_tokens":5000}}`)
	ensureMessagesDefaults(u4)
	if toFloat(asMap(u4["thinking"])["budget_tokens"]) != 5000 {
		t.Errorf("合法 budget 不应动")
	}
	if toFloat(u4["max_tokens"]) != 6024 {
		t.Errorf("max 应抬为 6024，得 %v", u4["max_tokens"])
	}
	// disabled 不动。
	u5 := mustJSON(t, `{"model":"m","thinking":{"type":"disabled"}}`)
	ensureMessagesDefaults(u5)
	if _, found := asMap(u5["thinking"])["budget_tokens"]; found {
		t.Errorf("disabled 不应加 budget")
	}
}
