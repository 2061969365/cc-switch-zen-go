// convert_stream_test.go: P1a 流式正确性单测，纯离线（httptest + 假 SSE 流）。
package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// oneByteReader 逐字节返回，证明 pumpSSE 块解析不依赖读边界。
type oneByteReader struct{ s string }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	p[0] = r.s[0]
	r.s = r.s[1:]
	return 1, nil
}

func collectEvents(t *testing.T, s string) [][2]string {
	t.Helper()
	var out [][2]string
	pumpSSE(&oneByteReader{s: s}, func(event, data string) {
		out = append(out, [2]string{event, data})
	})
	return out
}

func TestPumpSSEBlockJoin(t *testing.T) {
	// 多 data 行 join、注释跳过、\r\n、event 派发，全走单字节读。
	raw := ": ping line\r\n" +
		"event: message_start\r\n" +
		"data: {\"a\":1}\r\n" +
		"\r\n" +
		"event: content_block_delta\r\n" +
		"data: {\"x\":\"中\"}\r\n" +
		"data: {\"y\":\"文\"}\r\n" +
		"\n"
	evs := collectEvents(t, raw)
	if len(evs) != 2 {
		t.Fatalf("应 2 帧，得 %d: %v", len(evs), evs)
	}
	if evs[0][0] != "message_start" || evs[0][1] != `{"a":1}` {
		t.Errorf("首帧错: %v", evs[0])
	}
	if evs[1][0] != "content_block_delta" || evs[1][1] != "{\"x\":\"中\"}\n{\"y\":\"文\"}" {
		t.Errorf("多 data join 错: %q", evs[1][1])
	}
}

func TestEmitEventLines(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := newSSESink(rec)
	sink.emit(map[string]any{"type": "message_start"})
	sink.emit(chatChunk("chatcmpl-1", "m", map[string]any{}, ""))
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start\n") {
		t.Errorf("anthropic 事件缺 event 行:\n%s", body)
	}
	if strings.Contains(body, "event: chat.completion") {
		t.Errorf("chat chunk 不应有 event 行:\n%s", body)
	}
	// responses 事件名应原样透传为 event 行。
	rec2 := httptest.NewRecorder()
	sink2 := newSSESink(rec2)
	sink2.emit(map[string]any{"type": "response.output_text.delta"})
	if !strings.Contains(rec2.Body.String(), "event: response.output_text.delta\n") {
		t.Errorf("responses 事件缺 event 行:\n%s", rec2.Body.String())
	}
}

func TestStreamChatToResponsesLengthIncomplete(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("length 应标 incomplete:\n%s", body)
	}
	if strings.Contains(body, "response.failed") {
		t.Errorf("正常终态不应 failed:\n%s", body)
	}
}

func TestStreamEmptyEOF(t *testing.T) {
	// 空流：chat->responses 应 failed 而非 completed。
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(""), "mimo-v2.5-free")
	if body := rec.Body.String(); !strings.Contains(body, "response.failed") {
		t.Errorf("空流应 failed:\n%s", body)
	}
	// 空流：messages->chat 应 error 而非 stop。
	rec2 := httptest.NewRecorder()
	streamMessagesToChat(rec2, strings.NewReader(""), "claude-sonnet-4-5")
	if body := rec2.Body.String(); !strings.Contains(body, "stream_error") {
		t.Errorf("空流应 stream_error:\n%s", body)
	}
}

func TestStreamResponsesToChatFailed(t *testing.T) {
	up := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToChat(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, "stream_error") {
		t.Errorf("failed 应转 error:\n%s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("failed 不应补成功尾:\n%s", body)
	}
}

func TestStreamResponsesToChatIncomplete(t *testing.T) {
	up := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToChat(rec, strings.NewReader(up), "muse-spark-1.3")
	if body := rec.Body.String(); !strings.Contains(body, `"finish_reason":"length"`) {
		t.Errorf("incomplete 应标 length:\n%s", body)
	}
}

func TestStreamResponsesToMessagesFailed(t *testing.T) {
	up := "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("failed 应转 anthropic error:\n%s", body)
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Errorf("failed 不应补 end_turn:\n%s", body)
	}
}

func TestStreamResponsesToMessagesIncomplete(t *testing.T) {
	up := "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	if body := rec.Body.String(); !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Errorf("incomplete 应标 max_tokens:\n%s", body)
	}
}

func TestStreamMessagesToResponsesMaxTokens(t *testing.T) {
	up := "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"incomplete"`) {
		t.Errorf("max_tokens 应标 incomplete:\n%s", body)
	}
	if !strings.Contains(body, "max_output_tokens") {
		t.Errorf("应带 incomplete_details:\n%s", body)
	}
}

// ---------- P2b：usage 常带 + 工具 id 保底 ----------

func TestP2bChatToResponsesUsageAlwaysPresent(t *testing.T) {
	// 上游全程无 usage，completed 仍须带零值 usage。
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"input_tokens":0`) || !strings.Contains(body, `"total_tokens":0`) {
		t.Errorf("completed 缺零值 usage:\n%s", body)
	}
}

func TestP2bMessagesToResponsesUsageAlwaysPresent(t *testing.T) {
	up := "event: message_start\ndata: {\"message\":{\"id\":\"m1\",\"model\":\"mm\"}}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `"input_tokens":0`) {
		t.Errorf("completed 缺零值 usage:\n%s", body)
	}
}

func TestP2bChatToMessagesDeltaUsageComplete(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"usage":{"input_tokens":0,"output_tokens":0}`) {
		t.Errorf("message_delta usage 形状不全:\n%s", body)
	}
}

func TestP2bResponsesToMessagesDeltaUsageComplete(t *testing.T) {
	up := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	body := rec.Body.String()
	if !strings.Contains(body, `"usage":{"input_tokens":0,"output_tokens":0}`) {
		t.Errorf("message_delta usage 形状不全:\n%s", body)
	}
}

func TestP2bChatToMessagesToolFallbackID(t *testing.T) {
	// tool_calls 有名无 id：终态须 fallback 开块，不静默丢。
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"call_0"`) {
		t.Errorf("缺 fallback id:\n%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Errorf("应 tool_use 收尾:\n%s", body)
	}
}

func TestP2bChatToMessagesToolDroppedFailed(t *testing.T) {
	// finish 宣称 tool_calls 却零 tool 增量：须 error，不伪造成功。
	up := "data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	streamChatToMessages(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, "upstream tool call dropped") {
		t.Errorf("丢光应 failed:\n%s", body)
	}
	if strings.Contains(body, `"stop_reason"`) {
		t.Errorf("failed 不应补 message_delta:\n%s", body)
	}
}

func TestP2bResponsesToMessagesToolFallbackID(t *testing.T) {
	up := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"f\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	streamResponsesToMessages(rec, strings.NewReader(up), "claude-sonnet-4-5")
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"call_1"`) {
		t.Errorf("缺 fallback id:\n%s", body)
	}
	if strings.Contains(body, `"id":""`) {
		t.Errorf("不应有空 id:\n%s", body)
	}
}

func TestP2bChatToResponsesToolFallbackAndDrop(t *testing.T) {
	// a. 有名无 id：completed 输出须带 fallback call_id。
	up := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2,\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"
	rec := httptest.NewRecorder()
	streamChatToResponses(rec, strings.NewReader(up), "mimo-v2.5-free")
	body := rec.Body.String()
	if !strings.Contains(body, `"call_id":"call_2"`) {
		t.Errorf("缺 fallback call_id:\n%s", body)
	}
	// b. 宣称 tool 却零增量：failed。
	rec2 := httptest.NewRecorder()
	streamChatToResponses(rec2,
		strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n"), "mimo-v2.5-free")
	if body := rec2.Body.String(); !strings.Contains(body, "upstream tool call dropped") {
		t.Errorf("丢光应 failed:\n%s", body)
	}
}

func TestP2bMessagesToResponsesToolFallbackID(t *testing.T) {
	up := "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"\",\"name\":\"f\"}}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	rec := httptest.NewRecorder()
	streamMessagesToResponses(rec, strings.NewReader(up), "muse-spark-1.3")
	body := rec.Body.String()
	if !strings.Contains(body, `"call_id":"call_0"`) {
		t.Errorf("缺 fallback call_id:\n%s", body)
	}
	if strings.Contains(body, `"id":"fc_"`) {
		t.Errorf("不应有裸 fc_ 前缀:\n%s", body)
	}
}

func TestP2bNonStreamToolFallbackIDs(t *testing.T) {
	// responses->chat：双空 id。
	out := responsesToChatResp(mustJSON(t, `{"output":[{"type":"function_call","name":"f","arguments":"{}"}]}`), "m")
	tc := asMap(asArr(asMap(asArr(out["messages"])[0])["tool_calls"])[0])
	if got := getStr(tc, "id"); got == "" {
		t.Errorf("responses->chat 空 id 未 fallback")
	}
	// chat->responses：空 id。
	out2 := chatToResponsesResp(mustJSON(t, `{"choices":[{"message":{"role":"assistant","content":null,
		"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), "m")
	if !strings.Contains(canon(out2), `"id":"fc_call_0"`) {
		t.Errorf("chat->responses 空 id 未 fallback: %s", canon(out2))
	}
	// chat->messages：空 id。
	out3 := chatToMessagesResp(mustJSON(t, `{"choices":[{"message":{"role":"assistant","content":null,
		"tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), "m")
	blocks := asArr(out3["content"])
	if got := getStr(asMap(blocks[0]), "id"); got == "" {
		t.Errorf("chat->messages 空 id 未 fallback: %s", canon(blocks))
	}
	// messages->chat：空 id。
	out4 := messagesToChatResp(mustJSON(t, `{"content":[{"type":"tool_use","name":"f","input":{}}]}`), "m")
	mtc := asMap(asArr(asMap(asMap(asArr(out4["choices"])[0])["message"])["tool_calls"])[0])
	if got := getStr(mtc, "id"); got == "" {
		t.Errorf("messages->chat 空 id 未 fallback")
	}
	// responses->messages：双空 id。
	out5 := responsesToMessagesResp(mustJSON(t, `{"output":[{"type":"function_call","name":"f","arguments":"{}"}]}`), "m")
	if got := getStr(asMap(asArr(out5["content"])[0]), "id"); got == "" {
		t.Errorf("responses->messages 空 id 未 fallback")
	}
	// messages->responses：空 id。
	out6 := messagesToResponsesResp(mustJSON(t, `{"content":[{"type":"tool_use","name":"f","input":{}}]}`), "m")
	if !strings.Contains(canon(out6), `"call_id":"call_0"`) {
		t.Errorf("messages->responses 空 id 未 fallback: %s", canon(out6))
	}
}
