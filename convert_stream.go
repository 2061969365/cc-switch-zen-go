// convert_stream.go: 三协议流式 SSE 互转，规则参照 cc-switch
// streaming_responses.rs（responses->anthropic）、streaming_codex_anthropic.rs
// （anthropic->responses）、streaming.rs（chat->anthropic）、
// streaming_codex_chat.rs（chat->responses 组装）。
//
// 简化约定：只处理文本 + tool 调用 + 终态；web_search 等 hosted 类型忽略；
// reasoning 尽力透传（chat reasoning_content / responses summary / thinking）。
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ---------- SSE 收发 ----------

type sseSink struct {
	w        http.ResponseWriter
	f        http.Flusher
	doneSent bool
}

func newSSESink(w http.ResponseWriter) *sseSink {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return &sseSink{w: w, f: w.(http.Flusher)}
}

func (s *sseSink) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	// P1a-1：Anthropic/Responses 客户端靠 event: 行分发，chat 协议无事件名。
	// 从约定的 type 字段自动派生，调用方无需改动。
	prefix := ""
	if m, ok := v.(map[string]any); ok {
		if t, ok := m["type"].(string); ok && isSSEvent(t) {
			prefix = "event: " + t + "\n"
		}
	}
	_, _ = io.WriteString(s.w, prefix+"data: "+string(b)+"\n\n")
	s.f.Flush()
}

func isSSEvent(t string) bool {
	return strings.HasPrefix(t, "message_") || strings.HasPrefix(t, "content_block_") ||
		strings.HasPrefix(t, "response.") || t == "ping" || t == "error"
}

// emitAnthropicError / emitChatError / emitResponsesFailed：P1a-2 终态 error，
// 发完后调用方必须置 finished 终态位，禁止再补成功尾。
func (s *sseSink) emitAnthropicError(msg string) {
	s.emit(map[string]any{"type": "error",
		"error": map[string]any{"type": "api_error", "message": msg}})
}

func (s *sseSink) emitChatError(msg string) {
	s.emit(map[string]any{"error": map[string]any{"message": msg, "type": "stream_error"}})
	s.done()
}

func (s *sseSink) emitResponsesFailed(id, model, msg string) {
	s.emit(map[string]any{"type": "response.failed",
		"response": map[string]any{"id": id, "model": model, "status": "failed",
			"error": map[string]any{"message": msg, "type": "upstream_error"}}})
	s.done()
}

func (s *sseSink) done() {
	if s.doneSent {
		return
	}
	s.doneSent = true
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.f.Flush()
}

// pumpSSE 按 SSE 帧（空行分隔）解析上游流。P1a-4：字节级累积切块，
// 天然容忍 UTF-8 跨包与 \r\n；块内多 data: 行按规范 join("\n")；
// 单块 16MB 上限防 OOM。chat/responses 只有 data 行；anthropic 有 event+data 对。
// handler(event, data)：event 对 chat/responses 为 ""，anthropic 为事件名；data=="[DONE]" 表结束。
func pumpSSE(r io.Reader, handler func(event, data string)) {
	var buf []byte
	var pend []byte // 上一块尾部悬空的 \r（\r\n 被读边界切开时）
	tmp := make([]byte, 32*1024)
	flush := func(final bool) {
		for {
			i := bytes.Index(buf, []byte("\n\n"))
			if i < 0 {
				if final && len(buf) > 0 {
					parseSSEBlock(buf, handler)
					buf = nil
				} else if len(buf) > 16*1024*1024 {
					buf = nil // 等不到帧边界的超大块直接丢弃
				}
				return
			}
			parseSSEBlock(buf[:i], handler)
			buf = buf[i+2:]
		}
	}
	for {
		n, err := r.Read(tmp)
		chunk := tmp[:n]
		if len(pend) > 0 {
			chunk = append(append([]byte{}, pend...), chunk...)
			pend = nil
		}
		if err == nil && len(chunk) > 0 && chunk[len(chunk)-1] == '\r' {
			pend = []byte{'\r'}
			chunk = chunk[:len(chunk)-1]
		}
		if len(chunk) > 0 {
			// SSE 行尾归一化：\r\n → \n（JSON 串内不允许裸回车，安全）。
			buf = append(buf, bytes.ReplaceAll(chunk, []byte("\r\n"), []byte("\n"))...)
			flush(false)
		}
		if err != nil {
			buf = append(buf, pend...)
			pend = nil
			flush(true)
			return
		}
	}
}

func parseSSEBlock(block []byte, handler func(event, data string)) {
	var event string
	var datas []string
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event = string(bytes.TrimSpace(line[len("event:"):]))
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			datas = append(datas, string(bytes.TrimSpace(line[len("data:"):])))
		}
	}
	if len(datas) == 0 {
		return
	}
	handler(event, strings.Join(datas, "\n"))
}

func parseSSEData(data string) map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return nil
	}
	return v
}

func chatChunk(id, model string, delta map[string]any, finish string) map[string]any {
	c := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": 0, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	}
	if finish != "" {
		asMap(asArr(c["choices"])[0])["finish_reason"] = finish
	}
	return c
}

// ---------- 1. chat -> responses ----------

type crtTool struct {
	callID string
	name   string
	args   strings.Builder
	added  bool
}

func streamChatToResponses(w http.ResponseWriter, r io.Reader, model string) {
	sink := newSSESink(w)
	respID := newID("resp_")
	created := false
	var text strings.Builder
	tools := map[int]*crtTool{}
	order := []int{}
	var usage map[string]any
	finished := false
	sawFinish := false // 是否见过上游终态（finish_reason 或 [DONE] 前的完成）
	hasOutput := false // 是否有实质输出（文本/tool/usage）

	flushCompleted := func(finish string) {
		if finished {
			return
		}
		finished = true
		// P2b-7：上游宣称工具调用却零 tool 增量 → failed，不撒谎报 completed。
		if (finish == "tool_calls" || finish == "function_call") && len(order) == 0 {
			sink.emitResponsesFailed(respID, model, "upstream tool call dropped")
			return
		}
		var output []any
		if t := text.String(); t != "" {
			output = append(output, map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": t}}})
		}
		for _, idx := range order {
			t := tools[idx]
			// P2b-7：id/name 不全的工具给 fallback 保底，不丢弃。
			t.callID = fallbackToolID(t.callID, idx)
			if t.name == "" {
				t.name = "unknown_tool"
			}
			output = append(output, map[string]any{"type": "function_call",
				"id": "fc_" + t.callID, "call_id": t.callID, "name": t.name, "arguments": t.args.String()})
		}
		if output == nil {
			output = []any{}
		}
		status := "completed"
		if finish == "length" {
			status = "incomplete"
		}
		ev := map[string]any{"type": "response.completed",
			"response": map[string]any{"id": respID, "status": status, "output": output}}
		// P2b-5：usage 常带（零值兜底），Codex/opencode 强读 usage.input_tokens。
		ev["response"].(map[string]any)["usage"] = responsesUsageFromChat(usage)
		sink.emit(ev)
	}

	pumpSSE(r, func(_, data string) {
		if data == "[DONE]" {
			// P1a-2：裸 [DONE] 且无终态无输出 → failed。
			if !sawFinish && !hasOutput && !finished {
				finished = true
				sink.emitResponsesFailed(respID, model, "upstream closed without terminal event")
				return
			}
			flushCompleted("stop")
			sink.done()
			return
		}
		chunk := parseSSEData(data)
		if chunk == nil {
			return
		}
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
			hasOutput = true
		}
		for _, c := range asArr(chunk["choices"]) {
			cm := asMap(c)
			delta := asMap(cm["delta"])
			if len(delta) == 0 && cm["finish_reason"] == nil {
				continue
			}
			if !created {
				created = true
				sink.emit(map[string]any{"type": "response.created",
					"response": map[string]any{"id": respID, "model": model}})
			}
			if t, ok := delta["content"].(string); ok && t != "" {
				hasOutput = true
				text.WriteString(t)
				sink.emit(map[string]any{"type": "response.output_text.delta",
					"item_id": "msg_0", "output_index": 0, "delta": t})
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				sink.emit(map[string]any{"type": "response.reasoning_summary_text.delta",
					"item_id": "rs_0", "output_index": len(order) + 1, "delta": rc})
			}
			for _, tc := range asArr(delta["tool_calls"]) {
				tm := asMap(tc)
				idx := int(toFloat(tm["index"]))
				t, ok := tools[idx]
				if !ok {
					t = &crtTool{}
					tools[idx] = t
					order = append(order, idx)
				}
				hasOutput = true
				if id := getStr(tm, "id"); id != "" {
					t.callID = id
				}
				if fn := asMap(tm["function"]); len(fn) > 0 {
					if n := getStr(fn, "name"); n != "" {
						t.name = n
					}
					t.args.WriteString(asStr(fn["arguments"]))
					if !t.added && t.callID != "" && t.name != "" {
						t.added = true
						sink.emit(map[string]any{"type": "response.output_item.added",
							"output_index": len(order), "item": map[string]any{
								"type": "function_call", "id": "fc_" + t.callID,
								"call_id": t.callID, "name": t.name, "arguments": "",
							}})
					}
					if t.added {
						if a := asStr(fn["arguments"]); a != "" {
							sink.emit(map[string]any{"type": "response.function_call_arguments.delta",
								"item_id": "fc_" + t.callID, "output_index": len(order), "delta": a})
						}
					}
				}
			}
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				sawFinish = true
				for _, idx := range order {
					t := tools[idx]
					if t.added {
						sink.emit(map[string]any{"type": "response.function_call_arguments.done",
							"item_id": "fc_" + t.callID, "output_index": 0, "arguments": t.args.String()})
						sink.emit(map[string]any{"type": "response.output_item.done",
							"output_index": 0, "item": map[string]any{
								"type": "function_call", "id": "fc_" + t.callID,
								"call_id": t.callID, "name": t.name, "arguments": t.args.String()}})
					}
				}
				flushCompleted(fr)
				sink.done()
				return
			}
		}
	})
	if !finished {
		// P1a-2：EOF 无终态。见过 finish 按 stop 收尾；有实质输出按成功收尾；
		// 零输出直接 failed，不伪造 completed。
		if !sawFinish && !hasOutput {
			finished = true
			sink.emitResponsesFailed(respID, model, "upstream closed without terminal event")
			return
		}
		flushCompleted("stop")
		sink.done()
	}
}

// ---------- 2. responses -> chat ----------

type rctTool struct {
	index  int
	callID string
	name   string
	args   strings.Builder
	added  bool
}

func streamResponsesToChat(w http.ResponseWriter, r io.Reader, model string) {
	sink := newSSESink(w)
	chatID := newID("chatcmpl-")
	tools := map[string]*rctTool{}
	nextIdx := 0
	var usage map[string]any
	finished := false
	sawTerminal := false // 是否见过 completed/incomplete/failed
	incomplete := false  // 上游是否为 response.incomplete（截断）
	failed := false      // 上游是否为 response.failed
	hasOutput := false   // 是否有实质输出

	finish := func() {
		if finished {
			return
		}
		finished = true
		if failed {
			sink.emitChatError("upstream response.failed")
			return
		}
		// P1a-3：截断走 length，不再一律 stop（finishFromResponsesStatus）。
		status := "completed"
		if incomplete {
			status = "incomplete"
		}
		fr := finishFromResponsesStatus(status, len(tools) > 0)
		last := chatChunk(chatID, model, map[string]any{}, fr)
		if len(usage) > 0 {
			last["usage"] = chatUsageFromResponses(usage)
		}
		sink.emit(last)
		sink.done()
	}

	pumpSSE(r, func(_, data string) {
		if data == "[DONE]" {
			// P1a-2：裸 [DONE] 且无终态无输出 → error。
			if !sawTerminal && !hasOutput {
				failed = true
			}
			finish()
			return
		}
		ev := parseSSEData(data)
		if ev == nil {
			return
		}
		switch asStr(ev["type"]) {
		case "response.created":
			resp := asMap(ev["response"])
			if m := getStr(resp, "model"); m != "" {
				model = m
			}
			sink.emit(chatChunk(chatID, model, map[string]any{"role": "assistant"}, ""))
		case "response.output_text.delta":
			if d := asStr(ev["delta"]); d != "" {
				hasOutput = true
				sink.emit(chatChunk(chatID, model, map[string]any{"content": d}, ""))
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if d := asStr(ev["delta"]); d != "" {
				hasOutput = true
				sink.emit(chatChunk(chatID, model, map[string]any{"reasoning_content": d}, ""))
			}
		case "response.output_item.added":
			item := asMap(ev["item"])
			if asStr(item["type"]) != "function_call" {
				return
			}
			id := getStr(item, "id")
			if id == "" {
				id = getStr(item, "call_id")
			}
			t := &rctTool{index: nextIdx, callID: getStr(item, "call_id"), name: getStr(item, "name")}
			if t.callID == "" {
				t.callID = id
			}
			nextIdx++
			tools[id] = t
		case "response.function_call_arguments.delta":
			id := getStr(ev, "item_id")
			t, ok := tools[id]
			if !ok {
				t = &rctTool{index: nextIdx, callID: id}
				nextIdx++
				tools[id] = t
			}
			hasOutput = true
			d := asStr(ev["delta"])
			t.args.WriteString(d)
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": t.index, "id": t.callID, "type": "function",
				"function": map[string]any{"name": t.name, "arguments": d},
			}}}
			if !t.added {
				t.added = true
			} else {
				delta["tool_calls"].([]any)[0].(map[string]any)["id"] = nil
				delete(asMap(asMap(delta["tool_calls"].([]any)[0].(map[string]any))["function"]), "name")
			}
			sink.emit(chatChunk(chatID, model, delta, ""))
		case "response.completed", "response.incomplete":
			resp := asMap(ev["response"])
			if resp == nil {
				resp = ev
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				usage = u
			}
			sawTerminal = true
			incomplete = asStr(ev["type"]) == "response.incomplete"
			finish()
		case "response.failed":
			// P1a-2：上游失败转 error 事件，禁止补成功尾。
			sawTerminal = true
			failed = true
			finish()
		}
	})
	// P1a-2：EOF 无终态。零输出直接 error；有输出按已见状态收尾。
	if !sawTerminal && !hasOutput {
		failed = true
	}
	finish()
}

// ---------- 3. chat -> messages（streaming.rs 简化） ----------

func streamChatToMessages(w http.ResponseWriter, r io.Reader, model string) {
	sink := newSSESink(w)
	msgID := newID("msg_")
	started := false
	textOpen := false
	textIdx := 0
	nextIdx := 1
	type toolState struct {
		aIdx    int
		id      string
		name    string
		pending strings.Builder
		started bool
	}
	tools := map[int]*toolState{}
	var usage map[string]any
	var stop string
	finished := false
	sawFinish := false    // 是否见过上游 finish_reason
	finishIsTool := false // finish_reason 是否为工具调用（P2b-7 丢光判定用）
	hasOutput := false    // 是否有实质输出

	ensureStart := func() {
		if started {
			return
		}
		started = true
		sink.emit(map[string]any{"type": "message_start",
			"message": map[string]any{"id": msgID, "type": "message", "role": "assistant",
				"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	}
	closeText := func() {
		if textOpen {
			textOpen = false
			sink.emit(map[string]any{"type": "content_block_stop", "index": textIdx})
		}
	}
	finalize := func() {
		if finished {
			return
		}
		finished = true
		// P1a-2：EOF 无终态且零输出 → error，不伪造 end_turn。
		if !sawFinish && !hasOutput {
			sink.emitAnthropicError("upstream closed without terminal event")
			sink.emit(map[string]any{"type": "message_stop"})
			return
		}
		ensureStart()
		closeText()
		startedTools := 0
		for _, idx := range sortedKeys(tools) {
			t := tools[idx]
			if !t.started {
				// P2b-7：id/name 不全导致未开块的工具，有信号就 fallback 开块，不静默丢。
				if t.id == "" && t.name == "" && t.pending.Len() == 0 {
					continue
				}
				t.id = fallbackToolID(t.id, idx)
				if t.name == "" {
					t.name = "unknown_tool"
				}
				t.started = true
				sink.emit(map[string]any{"type": "content_block_start", "index": t.aIdx,
					"content_block": map[string]any{"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{}}})
				if p := t.pending.String(); p != "" {
					sink.emit(map[string]any{"type": "content_block_delta", "index": t.aIdx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": p}})
					t.pending.Reset()
				}
			}
			if t.started {
				startedTools++
				if p := t.pending.String(); p != "" {
					sink.emit(map[string]any{"type": "content_block_delta", "index": t.aIdx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": p}})
					t.pending.Reset()
				}
				sink.emit(map[string]any{"type": "content_block_stop", "index": t.aIdx})
			}
		}
		// P2b-7：上游宣称 tool_use 却零工具开块 → error，不伪造成功尾。
		if finishIsTool && startedTools == 0 {
			sink.emitAnthropicError("upstream tool call dropped")
			sink.emit(map[string]any{"type": "message_stop"})
			return
		}
		if stop == "" {
			stop = "end_turn"
		}
		ev := map[string]any{"type": "message_delta",
			// P2b-6：回退 usage 形状必全（含 input_tokens），防客户端 NaN。
			"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}
		if len(usage) > 0 {
			ev["usage"] = anthropicUsageFromChat(usage)
		}
		sink.emit(ev)
		sink.emit(map[string]any{"type": "message_stop"})
	}

	pumpSSE(r, func(_, data string) {
		if data == "[DONE]" {
			finalize()
			return
		}
		chunk := parseSSEData(data)
		if chunk == nil {
			return
		}
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		for _, c := range asArr(chunk["choices"]) {
			cm := asMap(c)
			delta := asMap(cm["delta"])
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				sawFinish = true
				if fr == "tool_calls" || fr == "function_call" {
					finishIsTool = true
				}
				stop = anthropicStopFromFinish(fr, len(tools) > 0)
			}
			if t, ok := delta["content"].(string); ok && t != "" {
				hasOutput = true
				ensureStart()
				if !textOpen {
					textOpen = true
					sink.emit(map[string]any{"type": "content_block_start", "index": textIdx,
						"content_block": map[string]any{"type": "text", "text": ""}})
				}
				sink.emit(map[string]any{"type": "content_block_delta", "index": textIdx,
					"delta": map[string]any{"type": "text_delta", "text": t}})
			}
			for _, tc := range asArr(delta["tool_calls"]) {
				hasOutput = true
				ensureStart()
				tm := asMap(tc)
				idx := int(toFloat(tm["index"]))
				t, ok := tools[idx]
				if !ok {
					t = &toolState{aIdx: nextIdx}
					nextIdx++
					tools[idx] = t
				}
				if id := getStr(tm, "id"); id != "" {
					t.id = id
				}
				if fn := asMap(tm["function"]); len(fn) > 0 {
					if n := getStr(fn, "name"); n != "" {
						t.name = n
					}
					t.pending.WriteString(asStr(fn["arguments"]))
				}
				if !t.started && t.id != "" && t.name != "" {
					t.started = true
					closeText()
					sink.emit(map[string]any{"type": "content_block_start", "index": t.aIdx,
						"content_block": map[string]any{"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{}}})
					if p := t.pending.String(); p != "" {
						sink.emit(map[string]any{"type": "content_block_delta", "index": t.aIdx,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": p}})
						t.pending.Reset()
					}
				} else if t.started {
					if p := t.pending.String(); p != "" {
						sink.emit(map[string]any{"type": "content_block_delta", "index": t.aIdx,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": p}})
						t.pending.Reset()
					}
				}
			}
		}
	})
	finalize()
}

// ---------- 4. messages -> chat（3 的逆映射） ----------

func streamMessagesToChat(w http.ResponseWriter, r io.Reader, model string) {
	sink := newSSESink(w)
	chatID := newID("chatcmpl-")
	kindByIdx := map[int]string{}
	toolIdx := map[int]int{} // anthropic index -> chat tool index
	toolID := map[int]string{}
	nextTool := 0
	var usage map[string]any
	var finish string
	finished := false
	hasOutput := false // 是否有实质输出

	finalize := func() {
		if finished {
			return
		}
		finished = true
		// P1a-2：EOF 无终态且零输出 → error，不伪造 stop。
		if finish == "" && !hasOutput {
			sink.emitChatError("upstream closed without terminal event")
			return
		}
		last := chatChunk(chatID, model, map[string]any{}, finishFromAnthropicStop(finish, len(toolIdx) > 0))
		if len(usage) > 0 {
			last["usage"] = chatUsageFromAnthropic(usage)
		}
		sink.emit(last)
		sink.done()
	}

	pumpSSE(r, func(event, data string) {
		if data == "[DONE]" {
			finalize()
			return
		}
		ev := parseSSEData(data)
		if ev == nil {
			return
		}
		idx := int(toFloat(ev["index"]))
		switch event {
		case "message_start":
			if m := getStr(asMap(ev["message"]), "model"); m != "" {
				model = m
			}
			sink.emit(chatChunk(chatID, model, map[string]any{"role": "assistant"}, ""))
		case "content_block_start":
			cb := asMap(ev["content_block"])
			kind := asStr(cb["type"])
			kindByIdx[idx] = kind
			if kind == "tool_use" {
				hasOutput = true
				toolIdx[idx] = nextTool
				nextTool++
				toolID[idx] = getStr(cb, "id")
				sink.emit(chatChunk(chatID, model, map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx[idx], "id": getStr(cb, "id"), "type": "function",
					"function": map[string]any{"name": getStr(cb, "name"), "arguments": ""},
				}}}, ""))
			}
		case "content_block_delta":
			d := asMap(ev["delta"])
			switch asStr(d["type"]) {
			case "text_delta":
				if t := asStr(d["text"]); t != "" {
					hasOutput = true
					sink.emit(chatChunk(chatID, model, map[string]any{"content": t}, ""))
				}
			case "input_json_delta":
				if p := asStr(d["partial_json"]); p != "" {
					hasOutput = true
					sink.emit(chatChunk(chatID, model, map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx[idx], "function": map[string]any{"arguments": p},
					}}}, ""))
				}
			case "thinking_delta":
				if t := asStr(d["thinking"]); t != "" {
					sink.emit(chatChunk(chatID, model, map[string]any{"reasoning_content": t}, ""))
				}
			}
		case "message_delta":
			d := asMap(ev["delta"])
			if s := asStr(d["stop_reason"]); s != "" {
				finish = s
			}
			if u, ok := ev["usage"].(map[string]any); ok {
				usage = u
			}
		case "message_stop":
			finalize()
		}
	})
	finalize()
}

// ---------- 5. responses -> messages（streaming_responses.rs:2340 简化） ----------

func streamResponsesToMessages(w http.ResponseWriter, r io.Reader, model string) {
	sink := newSSESink(w)
	msgID := newID("msg_")
	started := false
	textOpen := false
	textIdx := 0
	nextIdx := 1
	toolIdxByItem := map[string]int{}
	toolStarted := map[string]bool{}
	var usage map[string]any
	var stop string
	hasTool := false
	finished := false
	sawTerminal := false // 是否见过 completed/incomplete/failed
	incomplete := false  // 是否为 response.incomplete（截断）
	hasOutput := false   // 是否有实质输出

	ensureStart := func() {
		if started {
			return
		}
		started = true
		sink.emit(map[string]any{"type": "message_start",
			"message": map[string]any{"id": msgID, "type": "message", "role": "assistant",
				"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	}
	openText := func() {
		ensureStart()
		if !textOpen {
			textOpen = true
			sink.emit(map[string]any{"type": "content_block_start", "index": textIdx,
				"content_block": map[string]any{"type": "text", "text": ""}})
		}
	}
	closeText := func() {
		if textOpen {
			textOpen = false
			sink.emit(map[string]any{"type": "content_block_stop", "index": textIdx})
		}
	}
	finalize := func() {
		if finished {
			return
		}
		finished = true
		// P1a-2：EOF 无终态且零输出 → error，不伪造 end_turn。
		if !sawTerminal && !hasOutput {
			sink.emitAnthropicError("upstream closed without terminal event")
			sink.emit(map[string]any{"type": "message_stop"})
			return
		}
		ensureStart()
		closeText()
		for id, idx := range toolIdxByItem {
			if toolStarted[id] {
				sink.emit(map[string]any{"type": "content_block_stop", "index": idx})
			}
		}
		if stop == "" {
			// P1a-3：截断走 max_tokens（cc-switch map_responses_stop_reason）。
			switch {
			case hasTool:
				stop = "tool_use"
			case incomplete:
				stop = "max_tokens"
			default:
				stop = "end_turn"
			}
		}
		ev := map[string]any{"type": "message_delta",
			// P2b-6：回退 usage 形状必全（含 input_tokens），防客户端 NaN。
			"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}
		if len(usage) > 0 {
			ev["usage"] = anthropicUsageFromResponses(usage)
		}
		sink.emit(ev)
		sink.emit(map[string]any{"type": "message_stop"})
	}

	pumpSSE(r, func(_, data string) {
		if data == "[DONE]" {
			finalize()
			return
		}
		ev := parseSSEData(data)
		if ev == nil {
			return
		}
		switch asStr(ev["type"]) {
		case "response.created":
			if m := getStr(asMap(ev["response"]), "model"); m != "" {
				model = m
			}
			ensureStart()
		case "response.output_text.delta", "response.refusal.delta":
			if d := asStr(ev["delta"]); d != "" {
				hasOutput = true
				openText()
				sink.emit(map[string]any{"type": "content_block_delta", "index": textIdx,
					"delta": map[string]any{"type": "text_delta", "text": d}})
			}
		case "response.output_item.added":
			item := asMap(ev["item"])
			switch asStr(item["type"]) {
			case "function_call":
				hasTool = true
				closeText()
				ensureStart()
				id := getStr(item, "id")
				if id == "" {
					id = getStr(item, "call_id")
				}
				// P2b-7：双空给 fallback，Claude 对 tool_use.id 强校验。
				callID := fallbackToolID(getStr(item, "call_id"), nextIdx)
				if id == "" {
					id = callID
				}
				idx := nextIdx
				nextIdx++
				toolIdxByItem[id] = idx
				toolStarted[id] = true
				sink.emit(map[string]any{"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "tool_use",
						"id": callID, "name": getStr(item, "name"), "input": map[string]any{}}})
			case "reasoning":
				closeText()
				ensureStart()
				id := getStr(item, "id")
				if id == "" {
					id = "rs_0"
				}
				idx := nextIdx
				nextIdx++
				toolIdxByItem["rs:"+id] = idx
				toolStarted["rs:"+id] = true
				sink.emit(map[string]any{"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "thinking", "thinking": ""}})
			}
		case "response.function_call_arguments.delta":
			if idx, ok := toolIdxByItem[getStr(ev, "item_id")]; ok {
				if d := asStr(ev["delta"]); d != "" {
					hasOutput = true
					sink.emit(map[string]any{"type": "content_block_delta", "index": idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": d}})
				}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			for id, idx := range toolIdxByItem {
				if !strings.HasPrefix(id, "rs:") || !toolStarted[id] {
					continue
				}
				if d := asStr(ev["delta"]); d != "" {
					hasOutput = true
					sink.emit(map[string]any{"type": "content_block_delta", "index": idx,
						"delta": map[string]any{"type": "thinking_delta", "thinking": d}})
				}
			}
		case "response.completed", "response.incomplete":
			resp := asMap(ev["response"])
			if resp == nil {
				resp = ev
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				usage = u
			}
			sawTerminal = true
			incomplete = asStr(ev["type"]) == "response.incomplete"
			finalize()
		case "response.failed":
			// P1a-2：上游失败转 error 事件，禁止补成功尾。
			sawTerminal = true
			finished = true
			ensureStart()
			sink.emitAnthropicError("upstream response.failed")
			sink.emit(map[string]any{"type": "message_stop"})
		}
	})
	finalize()
}

// ---------- 6. messages -> responses（streaming_codex_anthropic.rs 简化，去 codex 包络） ----------

func streamMessagesToResponses(w http.ResponseWriter, r io.Reader, model string) {
	sink := newSSESink(w)
	respID := newID("resp_")
	created := false
	outIdx := 0
	kindByIdx := map[int]string{}
	toolCallID := map[int]string{}
	var toolArgs strings.Builder
	toolItemID := ""
	var usage map[string]any
	finished := false
	sawStop := false    // 是否见过 message_stop（正常终态）
	anthropicStop := "" // message_delta 带来的 stop_reason
	hasTool := false    // 是否开过 tool_use 块
	hasOutput := false  // 是否有实质输出

	finalize := func() {
		if finished {
			return
		}
		finished = true
		// P1a-2：EOF 无终态且零输出 → failed，不伪造 completed。
		if !sawStop && !hasOutput {
			sink.emitResponsesFailed(respID, model, "upstream closed without terminal event")
			return
		}
		// P1a-3：max_tokens 走 incomplete（responsesStatusFromFinish）。
		status, detail := responsesStatusFromFinish(finishFromAnthropicStop(anthropicStop, hasTool))
		ev := map[string]any{"type": "response." + status,
			"response": map[string]any{"id": respID, "status": status}}
		if detail != nil {
			ev["response"].(map[string]any)["incomplete_details"] = detail
		}
		// P2b-5：usage 常带（零值兜底）。
		ev["response"].(map[string]any)["usage"] = responsesUsageFromAnthropic(usage)
		sink.emit(ev)
		sink.done()
	}

	pumpSSE(r, func(event, data string) {
		if data == "[DONE]" {
			finalize()
			return
		}
		ev := parseSSEData(data)
		if ev == nil {
			return
		}
		idx := int(toFloat(ev["index"]))
		switch event {
		case "message_start":
			created = true
			if m := getStr(asMap(ev["message"]), "model"); m != "" {
				model = m
			}
			sink.emit(map[string]any{"type": "response.created",
				"response": map[string]any{"id": respID, "model": model}})
		case "content_block_start":
			if !created {
				created = true
				sink.emit(map[string]any{"type": "response.created",
					"response": map[string]any{"id": respID, "model": model}})
			}
			cb := asMap(ev["content_block"])
			kind := asStr(cb["type"])
			kindByIdx[idx] = kind
			switch kind {
			case "text":
				hasOutput = true
				sink.emit(map[string]any{"type": "response.output_item.added",
					"output_index": outIdx, "item": map[string]any{
						"type": "message", "id": newID("msg_"), "status": "in_progress",
						"role": "assistant", "content": []any{}}})
				sink.emit(map[string]any{"type": "response.content_part.added",
					"item_id": "msg_0", "output_index": outIdx,
					"part": map[string]any{"type": "output_text", "text": ""}})
			case "tool_use":
				hasTool = true
				hasOutput = true
				// P2b-7：空 id 给 fallback，不发 "fc_" 裸前缀。
				callID := fallbackToolID(getStr(cb, "id"), idx)
				toolCallID[idx] = callID
				toolItemID = "fc_" + callID
				toolArgs.Reset()
				sink.emit(map[string]any{"type": "response.output_item.added",
					"output_index": outIdx, "item": map[string]any{
						"type": "function_call", "id": toolItemID, "call_id": callID,
						"name": getStr(cb, "name"), "arguments": ""}})
			case "thinking", "redacted_thinking":
				hasOutput = true
				sink.emit(map[string]any{"type": "response.output_item.added",
					"output_index": outIdx, "item": map[string]any{
						"type": "reasoning", "id": newID("rs_")}})
			}
		case "content_block_delta":
			d := asMap(ev["delta"])
			switch asStr(d["type"]) {
			case "text_delta":
				if t := asStr(d["text"]); t != "" {
					sink.emit(map[string]any{"type": "response.output_text.delta",
						"item_id": "msg_0", "output_index": outIdx, "delta": t})
				}
			case "input_json_delta":
				if p := asStr(d["partial_json"]); p != "" {
					toolArgs.WriteString(p)
					sink.emit(map[string]any{"type": "response.function_call_arguments.delta",
						"item_id": toolItemID, "output_index": outIdx, "delta": p})
				}
			case "thinking_delta":
				if t := asStr(d["thinking"]); t != "" {
					sink.emit(map[string]any{"type": "response.reasoning_summary_text.delta",
						"item_id": "rs_0", "output_index": outIdx, "delta": t})
				}
			}
		case "content_block_stop":
			switch kindByIdx[idx] {
			case "text":
				sink.emit(map[string]any{"type": "response.output_text.done",
					"item_id": "msg_0", "output_index": outIdx, "text": ""})
				sink.emit(map[string]any{"type": "response.output_item.done",
					"output_index": outIdx, "item": map[string]any{"type": "message"}})
				outIdx++
			case "tool_use":
				sink.emit(map[string]any{"type": "response.function_call_arguments.done",
					"item_id": toolItemID, "output_index": outIdx, "arguments": toolArgs.String()})
				sink.emit(map[string]any{"type": "response.output_item.done",
					"output_index": outIdx, "item": map[string]any{"type": "function_call"}})
				outIdx++
			case "thinking", "redacted_thinking":
				sink.emit(map[string]any{"type": "response.output_item.done",
					"output_index": outIdx, "item": map[string]any{"type": "reasoning"}})
				outIdx++
			}
		case "message_delta":
			d := asMap(ev["delta"])
			if s := asStr(d["stop_reason"]); s != "" {
				anthropicStop = s
			}
			if u, ok := ev["usage"].(map[string]any); ok {
				usage = u
			}
		case "message_stop":
			sawStop = true
			finalize()
		}
	})
	finalize()
}

// ---------- 小工具 ----------

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
