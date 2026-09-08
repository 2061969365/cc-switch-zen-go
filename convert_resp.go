// convert_resp.go: 三协议响应体互转（非流式），只取首选输出，规则参照
// cc-switch transform.rs:897 responses_to_openai_chat、transform.rs openai_to_anthropic、
// transform_responses.rs:2477 responses_to_anthropic、transform_codex_chat.rs:1435。
package main

// ---------- usage 互转 ----------

func chatUsageFromResponses(u map[string]any) map[string]any {
	prompt := toFloat(u["input_tokens"])
	if prompt == 0 {
		prompt = toFloat(u["prompt_tokens"])
	}
	completion := toFloat(u["output_tokens"])
	if completion == 0 {
		completion = toFloat(u["completion_tokens"])
	}
	total := toFloat(u["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	return map[string]any{
		"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total,
	}
}

func responsesUsageFromChat(u map[string]any) map[string]any {
	prompt := toFloat(u["prompt_tokens"])
	completion := toFloat(u["completion_tokens"])
	total := toFloat(u["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	return map[string]any{
		"input_tokens": prompt, "output_tokens": completion, "total_tokens": total,
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func anthropicUsageFromChat(u map[string]any) map[string]any {
	return map[string]any{
		"input_tokens":  toFloat(u["prompt_tokens"]),
		"output_tokens": toFloat(u["completion_tokens"]),
	}
}

func anthropicUsageFromResponses(u map[string]any) map[string]any {
	in := toFloat(u["input_tokens"])
	if in == 0 {
		in = toFloat(u["prompt_tokens"])
	}
	out := toFloat(u["output_tokens"])
	if out == 0 {
		out = toFloat(u["completion_tokens"])
	}
	return map[string]any{"input_tokens": in, "output_tokens": out}
}

func chatUsageFromAnthropic(u map[string]any) map[string]any {
	in := toFloat(u["input_tokens"])
	out := toFloat(u["output_tokens"])
	return map[string]any{
		"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
	}
}

func responsesUsageFromAnthropic(u map[string]any) map[string]any {
	in := toFloat(u["input_tokens"])
	out := toFloat(u["output_tokens"])
	return map[string]any{
		"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
	}
}

// ---------- finish / stop 互转 ----------

func finishFromAnthropicStop(stop string, hasTool bool) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	if hasTool {
		return "tool_calls"
	}
	return "stop"
}

func anthropicStopFromFinish(finish string, hasTool bool) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	if hasTool {
		return "tool_use"
	}
	return "end_turn"
}

// responses status -> chat finish_reason（transform.rs:940 不读 status，按 tool 有无定）。
func finishFromResponsesStatus(status string, hasTool bool) string {
	if hasTool {
		return "tool_calls"
	}
	if status == "incomplete" {
		return "length"
	}
	return "stop"
}

func responsesStatusFromFinish(finish string) (string, map[string]any) {
	if finish == "length" {
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	}
	return "completed", nil
}

// ---------- 1. responses -> chat（transform.rs:897） ----------

func responsesToChatResp(in map[string]any, model string) map[string]any {
	var texts []string
	var toolCalls []any
	var reasoning []string
	for i, it := range asArr(in["output"]) {
		item := asMap(it)
		switch asStr(item["type"]) {
		case "message":
			for _, c := range asArr(item["content"]) {
				cm := asMap(c)
				if t := asStr(cm["type"]); t == "output_text" || t == "text" || t == "refusal" {
					texts = append(texts, asStr(cm["text"]))
					if t == "refusal" && asStr(cm["refusal"]) != "" {
						texts = append(texts, asStr(cm["refusal"]))
					}
				}
			}
		case "function_call":
			// P2b-7：双空给 fallback，不发空 id。
			id := fallbackToolID(firstNonEmpty(getStr(item, "call_id"), getStr(item, "id")), i)
			args := asStr(item["arguments"])
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": getStr(item, "name"), "arguments": args},
			})
		case "reasoning":
			for _, s := range asArr(item["summary"]) {
				if asStr(asMap(s)["type"]) == "summary_text" {
					reasoning = append(reasoning, asStr(asMap(s)["text"]))
				}
			}
			if t := asStr(item["text"]); t != "" {
				reasoning = append(reasoning, t)
			}
		}
	}
	joined := joinNonEmpty(texts, "")
	var content any = joined
	if joined == "" && len(toolCalls) > 0 {
		content = nil
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(reasoning) > 0 {
		msg["reasoning_content"] = joinNonEmpty(reasoning, "\n")
	}
	usage := chatUsageFromResponses(asMap(in["usage"]))
	return map[string]any{
		"id": getStr(in, "id"), "object": "chat.completion",
		"created": 0, "model": firstNonEmpty(getStr(in, "model"), model),
		"choices": []any{map[string]any{
			"index": 0, "message": msg,
			"finish_reason": finishFromResponsesStatus(getStr(in, "status"), len(toolCalls) > 0),
		}},
		"usage": usage,
	}
}

// ---------- 2. chat -> responses（transform_codex_chat.rs:1435 chat_completion_to_response） ----------

func chatToResponsesResp(in map[string]any, model string) map[string]any {
	choices := asArr(in["choices"])
	msg := map[string]any{}
	var finish string
	if len(choices) > 0 {
		cm := asMap(choices[0])
		msg = asMap(cm["message"])
		if len(msg) == 0 {
			msg = asMap(cm["delta"])
		}
		finish = asStr(cm["finish_reason"])
	}
	var output []any
	if t := textOfContent(msg["content"]); t != "" {
		output = append(output, map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": t}},
		})
	}
	for i, tc := range asArr(msg["tool_calls"]) {
		tm := asMap(tc)
		fn := asMap(tm["function"])
		args := asStr(fn["arguments"])
		if args == "" {
			args = "{}"
		}
		// P2b-7：空 id 给 fallback，不发 "fc_" 裸前缀。
		id := fallbackToolID(getStr(tm, "id"), i)
		output = append(output, map[string]any{
			"type": "function_call", "id": "fc_" + id, "call_id": id,
			"name": getStr(fn, "name"), "arguments": args,
		})
	}
	if r := asStr(msg["reasoning_content"]); r != "" {
		output = append(output, map[string]any{
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}
	if output == nil {
		output = []any{}
	}
	status, details := responsesStatusFromFinish(finish)
	id := getStr(in, "id")
	if id == "" {
		id = newID("resp_")
	}
	out := map[string]any{
		"id": id, "object": "response", "model": firstNonEmpty(getStr(in, "model"), model),
		"status": status, "output": output,
		"usage": responsesUsageFromChat(asMap(in["usage"])),
	}
	if details != nil {
		out["incomplete_details"] = details
	}
	return out
}

// ---------- 3. chat -> messages（transform.rs openai_to_anthropic，只取 choices[0]） ----------

func chatToMessagesResp(in map[string]any, model string) map[string]any {
	choices := asArr(in["choices"])
	msg := map[string]any{}
	var finish string
	if len(choices) > 0 {
		cm := asMap(choices[0])
		msg = asMap(cm["message"])
		if len(msg) == 0 {
			msg = asMap(cm["delta"])
		}
		finish = asStr(cm["finish_reason"])
	}
	var content []any
	if r := asStr(msg["reasoning_content"]); r != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": r})
	}
	if t := textOfContent(msg["content"]); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	var toolCalls []any
	for i, tc := range asArr(msg["tool_calls"]) {
		tm := asMap(tc)
		fn := asMap(tm["function"])
		toolCalls = append(toolCalls, map[string]any{
			// P2b-7：空 id 给 fallback，Claude 强校验。
			"type": "tool_use", "id": fallbackToolID(getStr(tm, "id"), i),
			"name": getStr(fn, "name"), "input": parseObj(asStr(fn["arguments"])),
		})
	}
	content = append(content, toolCalls...)
	if content == nil {
		content = []any{}
	}
	return map[string]any{
		"id": getStr(in, "id"), "type": "message", "role": "assistant",
		"model":         firstNonEmpty(getStr(in, "model"), model),
		"content":       content,
		"stop_reason":   anthropicStopFromFinish(finish, len(toolCalls) > 0),
		"stop_sequence": nil,
		"usage":         anthropicUsageFromChat(asMap(in["usage"])),
	}
}

// ---------- 4. messages -> chat（3 的逆映射） ----------

func messagesToChatResp(in map[string]any, model string) map[string]any {
	var texts []string
	var toolCalls []any
	var reasoning []string
	for i, b := range asArr(in["content"]) {
		bm := asMap(b)
		switch asStr(bm["type"]) {
		case "text":
			texts = append(texts, asStr(bm["text"]))
		case "thinking":
			reasoning = append(reasoning, asStr(bm["thinking"]))
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				// P2b-7：空 id 给 fallback。
				"id": fallbackToolID(getStr(bm, "id"), i), "type": "function",
				"function": map[string]any{"name": getStr(bm, "name"), "arguments": canon(bm["input"])},
			})
		}
	}
	joined := joinNonEmpty(texts, "")
	var content any = joined
	if joined == "" && len(toolCalls) > 0 {
		content = nil
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(reasoning) > 0 {
		msg["reasoning_content"] = joinNonEmpty(reasoning, "\n")
	}
	return map[string]any{
		"id": getStr(in, "id"), "object": "chat.completion",
		"created": 0, "model": firstNonEmpty(getStr(in, "model"), model),
		"choices": []any{map[string]any{
			"index": 0, "message": msg,
			"finish_reason": finishFromAnthropicStop(getStr(in, "stop_reason"), len(toolCalls) > 0),
		}},
		"usage": chatUsageFromAnthropic(asMap(in["usage"])),
	}
}

// ---------- 5. responses -> messages（transform_responses.rs:2477 responses_to_anthropic） ----------

func responsesToMessagesResp(in map[string]any, model string) map[string]any {
	var content []any
	hasTool := false
	for i, it := range asArr(in["output"]) {
		item := asMap(it)
		switch asStr(item["type"]) {
		case "message":
			for _, c := range asArr(item["content"]) {
				cm := asMap(c)
				switch asStr(cm["type"]) {
				case "output_text", "text":
					if t := asStr(cm["text"]); t != "" {
						content = append(content, map[string]any{"type": "text", "text": t})
					}
				case "refusal":
					if t := firstNonEmpty(asStr(cm["text"]), asStr(cm["refusal"])); t != "" {
						content = append(content, map[string]any{"type": "text", "text": t})
					}
				}
			}
		case "function_call":
			hasTool = true
			// P2b-7：双空给 fallback。
			id := fallbackToolID(firstNonEmpty(getStr(item, "call_id"), getStr(item, "id")), i)
			content = append(content, map[string]any{
				"type": "tool_use", "id": id,
				"name": getStr(item, "name"), "input": parseObj(asStr(item["arguments"])),
			})
		case "reasoning":
			var texts []string
			for _, s := range asArr(item["summary"]) {
				if asStr(asMap(s)["type"]) == "summary_text" {
					texts = append(texts, asStr(asMap(s)["text"]))
				}
			}
			if t := joinNonEmpty(texts, "\n"); t != "" {
				// 无 encrypted_content：退化成普通 thinking（无 signature）。
				content = append(content, map[string]any{"type": "thinking", "thinking": t})
			}
		}
	}
	if content == nil {
		content = []any{}
	}
	status := getStr(in, "status")
	stop := "end_turn"
	switch {
	case hasTool:
		stop = "tool_use"
	case status == "incomplete":
		stop = "max_tokens"
	}
	return map[string]any{
		"id": getStr(in, "id"), "type": "message", "role": "assistant",
		"model":   firstNonEmpty(getStr(in, "model"), model),
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": anthropicUsageFromResponses(asMap(in["usage"])),
	}
}

// ---------- 6. messages -> responses（5 的逆映射） ----------

func messagesToResponsesResp(in map[string]any, model string) map[string]any {
	var output []any
	var texts []string
	flushText := func() {
		if t := joinNonEmpty(texts, ""); t != "" {
			output = append(output, map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": t}},
			})
		}
		texts = nil
	}
	for i, b := range asArr(in["content"]) {
		bm := asMap(b)
		switch asStr(bm["type"]) {
		case "text":
			texts = append(texts, asStr(bm["text"]))
		case "thinking":
			flushText()
			if t := asStr(bm["thinking"]); t != "" {
				output = append(output, map[string]any{
					"type":    "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": t}},
				})
			}
		case "tool_use":
			flushText()
			// P2b-7：空 id 给 fallback，不发 "fc_" 裸前缀。
			id := fallbackToolID(getStr(bm, "id"), i)
			args := canon(bm["input"])
			output = append(output, map[string]any{
				"type": "function_call", "id": "fc_" + id, "call_id": id,
				"name": getStr(bm, "name"), "arguments": args,
			})
		}
	}
	flushText()
	if output == nil {
		output = []any{}
	}
	stop := getStr(in, "stop_reason")
	status := "completed"
	var details map[string]any
	if stop == "max_tokens" {
		status = "incomplete"
		details = map[string]any{"reason": "max_output_tokens"}
	}
	id := getStr(in, "id")
	if id == "" {
		id = newID("resp_")
	}
	out := map[string]any{
		"id": id, "object": "response",
		"model":  firstNonEmpty(getStr(in, "model"), model),
		"status": status, "output": output,
		"usage": responsesUsageFromAnthropic(asMap(in["usage"])),
	}
	if details != nil {
		out["incomplete_details"] = details
	}
	return out
}

// ---------- 小工具 ----------

func joinNonEmpty(parts []string, sep string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	if sep == "" {
		s := ""
		for _, p := range kept {
			s += p
		}
		return s
	}
	s := kept[0]
	for _, p := range kept[1:] {
		s += sep + p
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
