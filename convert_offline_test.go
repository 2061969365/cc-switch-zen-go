// convert_offline_test.go: 纯函数离线单测，不碰网络。
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLookupFormat(t *testing.T) {
	cases := map[string]Format{
		"mimo-v2.5-free":                  FmtChat,
		"deepseek-v4-flash":               FmtChat,
		"big-pickle":                      FmtChat,
		"muse-spark-1.3-contributor-free": FmtResponses,
		"gpt-5.5":                         FmtResponses,
		"grok-4.5":                        FmtResponses,
		"claude-sonnet-4-5":               FmtMessages,
		"qwen3.7-max":                     FmtMessages,
		"gemini-3-flash":                  FmtGemini,
	}
	seedBuiltinTable()
	for model, want := range cases {
		got, ok := lookupFormat(model)
		if !ok || got != want {
			t.Errorf("%s: got %q,%v want %q", model, got, ok, want)
		}
	}
	if _, ok := lookupFormat("some-unknown-model-zzz"); ok {
		t.Errorf("未知模型应返回 ok=false")
	}
}

func TestChatResponsesRoundTrip(t *testing.T) {
	chatReq := mustJSON(t, `{"model":"mimo-v2.5-free","messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"你好"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"12:00"}],
		"tools":[{"type":"function","function":{"name":"get_time","description":"查时间","parameters":{"properties":{"x":{"type":"string"}}}}}],
		"tool_choice":"required","max_tokens":100,"temperature":0.5,"stream":false}`)
	r := chatToResponsesReq(chatReq)
	if getStr(r, "instructions") != "你是助手" {
		t.Errorf("instructions 丢失: %v", r["instructions"])
	}
	input := asArr(r["input"])
	if len(input) != 3 {
		t.Fatalf("input 应 3 项（空 content 的 assistant 不产生空 message），得 %d: %s", len(input), canon(input))
	}
	if asStr(asMap(input[1])["type"]) != "function_call" {
		t.Errorf("tool_calls 未转 function_call: %s", canon(input[1]))
	}
	// tool_choice 按 cc-switch 应丢弃
	if _, ok := r["tool_choice"]; ok {
		t.Errorf("tool_choice 应丢弃")
	}
	tools := asArr(r["tools"])
	params := asMap(asMap(tools[0])["parameters"])
	if asStr(params["type"]) != "object" {
		t.Errorf("schema 未补 object: %s", canon(params))
	}

	// 伪造 responses 响应转回 chat
	resp := mustJSON(t, `{"id":"resp_1","model":"mimo-v2.5-free","status":"completed",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"现在是12点"}]}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	c := responsesToChatResp(resp, "mimo-v2.5-free")
	msg := asMap(asMap(asArr(c["choices"])[0])["message"])
	if asStr(msg["content"]) != "现在是12点" {
		t.Errorf("文本回转丢失: %s", canon(msg))
	}
	if asStr(asMap(asArr(c["choices"])[0])["finish_reason"]) != "stop" {
		t.Errorf("finish_reason 应为 stop")
	}
	u := asMap(c["usage"])
	if toFloat(u["prompt_tokens"]) != 10 || toFloat(u["completion_tokens"]) != 5 {
		t.Errorf("usage 映射错: %s", canon(u))
	}
}

func TestMessagesChatRoundTrip(t *testing.T) {
	msgReq := mustJSON(t, `{"model":"claude-sonnet-4-5","system":"sys指令",
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"},
		{"type":"tool_use","id":"tu_1","name":"bash","input":{"cmd":"ls"}}]}],
		"tool_choice":{"type":"any"},"max_tokens":50}`)
	c := messagesToChatReq(msgReq)
	msgs := asArr(c["messages"])
	if getStr(asMap(msgs[0]), "role") != "system" {
		t.Errorf("首条应为 system: %s", canon(msgs))
	}
	if asStr(c["tool_choice"]) != "required" {
		t.Errorf("any 应转 required: %v", c["tool_choice"])
	}
	// image 应转 image_url data URL
	uContent := asArr(asMap(msgs[1])["content"])
	found := false
	for _, p := range uContent {
		pm := asMap(p)
		if asStr(pm["type"]) == "image_url" &&
			strings.HasPrefix(getStr(asMap(pm["image_url"]), "url"), "data:image/png;base64,AAA") {
			found = true
		}
	}
	if !found {
		t.Errorf("image 未转 data URL: %s", canon(uContent))
	}

	// chat 响应转 messages
	chatResp := mustJSON(t, `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,
		"message":{"role":"assistant","content":"done","tool_calls":[
		{"id":"call_9","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},
		"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	m := chatToMessagesResp(chatResp, "m")
	if getStr(m, "stop_reason") != "tool_use" {
		t.Errorf("stop_reason 应为 tool_use: %s", getStr(m, "stop_reason"))
	}
	blocks := asArr(m["content"])
	if len(blocks) != 2 || asStr(asMap(blocks[1])["type"]) != "tool_use" {
		t.Errorf("tool_use 块丢失: %s", canon(blocks))
	}
	if asStr(asMap(asMap(blocks[1])["input"])["cmd"]) != "ls" {
		t.Errorf("arguments 解析错: %s", canon(blocks[1]))
	}
}

func TestResponsesStringInput(t *testing.T) {
	seedBuiltinTable()
	// input 为纯字符串时应转成单条 user 消息，而不是空 messages。
	r := responsesToChatReq(mustJSON(t, `{"model":"m","input":"hi"}`))
	msgs := asArr(r["messages"])
	if len(msgs) != 1 || getStr(asMap(msgs[0]), "content") != "hi" {
		t.Errorf("string input 未转 user 消息: %s", canon(msgs))
	}
	m := responsesToMessagesReq(mustJSON(t, `{"model":"m","input":"hi"}`))
	msgs = asArr(m["messages"])
	if len(msgs) != 1 {
		t.Fatalf("string input 未转 messages: %s", canon(msgs))
	}
	blocks := asArr(asMap(msgs[0])["content"])
	if asStr(asMap(blocks[0])["text"]) != "hi" {
		t.Errorf("文本丢失: %s", canon(blocks))
	}
}

func TestMessagesResponsesRoundTrip(t *testing.T) {
	msgReq := mustJSON(t, `{"model":"muse-spark-1.3-contributor-free",
		"messages":[{"role":"user","content":"讲个笑话"}],
		"tool_choice":{"type":"tool","name":"joke"},"max_tokens":30,
		"tools":[{"name":"joke","description":"讲笑话","input_schema":{}}]}`)
	r := messagesToResponsesReq(msgReq)
	tc := asMap(r["tool_choice"])
	if asStr(tc["type"]) != "function" || getStr(tc, "name") != "joke" {
		t.Errorf("tool_choice 应转 {function,joke}: %s", canon(r["tool_choice"]))
	}
	// 空 schema 应补 object
	params := asMap(asMap(asArr(r["tools"])[0])["parameters"])
	if asStr(params["type"]) != "object" {
		t.Errorf("空 schema 未补 object: %s", canon(params))
	}

	resp := mustJSON(t, `{"id":"resp_2","model":"m","status":"completed",
		"output":[{"type":"function_call","call_id":"c1","name":"joke","arguments":"{}"}],
		"usage":{"input_tokens":3,"output_tokens":8}}`)
	m := responsesToMessagesResp(resp, "m")
	if getStr(m, "stop_reason") != "tool_use" {
		t.Errorf("stop_reason 应为 tool_use")
	}

	// responses 请求转 messages 请求
	rreq := mustJSON(t, `{"model":"claude-sonnet-4-5","instructions":"sys",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"}],
		"max_output_tokens":20}`)
	mreq := responsesToMessagesReq(rreq)
	if getStr(mreq, "system") != "sys" {
		t.Errorf("instructions 未转 system")
	}
	msgs := asArr(mreq["messages"])
	if len(msgs) != 2 || asStr(asMap(asArr(asMap(msgs[1])["content"])[0])["type"]) != "tool_use" {
		t.Errorf("function_call 未转 tool_use: %s", canon(msgs))
	}
}
