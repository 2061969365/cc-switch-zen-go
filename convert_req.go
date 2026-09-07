// convert_req.go: 三协议请求体互转（非流式），规则参照
// cc-switch transform.rs / transform_responses.rs / transform_codex_chat.rs。
//
// 通用约定（与 cc-switch 一致）：
//   - thinking / redacted_thinking 输入块默认丢弃
//   - tool_choice any -> required
//   - tools 缺 input_schema/parameters 时 clean_schema 补 {type:object,properties:{}}
//   - arguments 序列化用 canonical JSON（Go map 默认键排序，即 canonical）
//   - cache_control / billing 头等缓存指纹字段一律丢弃
package main

import (
	"encoding/json"
	"strings"
)

// ---------- 通用 helper ----------

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func asArr(v any) []any {
	a, _ := v.([]any)
	return a
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func getStr(m map[string]any, k string) string { return asStr(m[k]) }

// canon canonical JSON 序列化，失败回 "{}"。
func canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil || v == nil {
		return "{}"
	}
	return string(b)
}

// parseObj 把 arguments 字符串解析成对象，失败回 {}。
func parseObj(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return map[string]any{}
	}
	if _, ok := v.(map[string]any); !ok {
		return map[string]any{}
	}
	return v
}

// cleanSchema 递归清洗 JSON schema：root 缺 type 补 object、缺 properties 补 {}，
// 删 format:uri。参照 cc-switch clean_schema。
func cleanSchema(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, val := range m {
		out[k] = val
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if out["type"] == "object" {
		if _, ok := out["properties"]; !ok {
			out["properties"] = map[string]any{}
		}
	}
	if out["format"] == "uri" {
		delete(out, "format")
	}
	if props, ok := out["properties"].(map[string]any); ok {
		cp := map[string]any{}
		for k, val := range props {
			cp[k] = cleanSchema(val)
		}
		out["properties"] = cp
	}
	switch items := out["items"].(type) {
	case map[string]any:
		out["items"] = cleanSchema(items)
	case []any:
		for i, it := range items {
			items[i] = cleanSchema(it)
		}
	}
	return out
}

func copyPassthrough(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
}

// textOfContent 取 chat content（string 或 parts 数组）里的纯文本。
func textOfContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var sb strings.Builder
	for _, p := range asArr(content) {
		pm := asMap(p)
		if t := asStr(pm["type"]); t == "text" || t == "input_text" || t == "output_text" {
			sb.WriteString(asStr(pm["text"]))
		}
	}
	return sb.String()
}

// ---------- 1. chat -> responses（transform.rs:750 openai_chat_to_responses） ----------

func chatToResponsesReq(in map[string]any) map[string]any {
	out := map[string]any{}
	if m := getStr(in, "model"); m != "" {
		out["model"] = m
	}
	var instructions []string
	var input []any
	flushMsg := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		input = append(input, map[string]any{"role": role, "content": parts})
	}
	for _, m := range asArr(in["messages"]) {
		msg := asMap(m)
		role := getStr(msg, "role")
		if role == "" {
			role = "user"
		}
		content := msg["content"]
		if role == "system" {
			if t := textOfContent(content); t != "" {
				instructions = append(instructions, t)
			}
			continue
		}
		if role == "tool" {
			var output any = ""
			if s, ok := content.(string); ok {
				output = s
			} else if content != nil {
				output = canon(content)
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": getStr(msg, "tool_call_id"),
				"output":  output,
			})
			continue
		}
		var parts []any
		if s, ok := content.(string); ok {
			if s != "" {
				typ := "input_text"
				if role == "assistant" {
					typ = "output_text"
				}
				parts = append(parts, map[string]any{"type": typ, "text": s})
			}
		} else {
			for _, p := range asArr(content) {
				pm := asMap(p)
				switch asStr(pm["type"]) {
				case "text", "input_text", "output_text":
					if t := asStr(pm["text"]); t != "" {
						typ := "input_text"
						if role == "assistant" {
							typ = "output_text"
						}
						parts = append(parts, map[string]any{"type": typ, "text": t})
					}
				case "image_url":
					iu := pm["image_url"]
					if s, ok := iu.(string); ok {
						iu = map[string]any{"url": s}
					}
					if u := getStr(asMap(iu), "url"); u != "" {
						parts = append(parts, map[string]any{"type": "input_image", "image_url": u})
					}
				}
			}
		}
		flushMsg(role, parts)
		for _, tc := range asArr(msg["tool_calls"]) {
			tm := asMap(tc)
			fn := asMap(tm["function"])
			args := asStr(fn["arguments"])
			if args == "" {
				args = "{}"
			}
			input = append(input, map[string]any{
				"type": "function_call", "call_id": getStr(tm, "id"),
				"name": getStr(fn, "name"), "arguments": args,
			})
		}
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	if input == nil {
		input = []any{}
	}
	out["input"] = input
	for _, t := range asArr(in["tools"]) {
		tm := asMap(t)
		fn := asMap(tm["function"])
		if asStr(tm["type"]) != "function" || len(fn) == 0 {
			continue
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]any{}
		}
		out["tools"] = append(asArr(out["tools"]), map[string]any{
			"type": "function", "name": getStr(fn, "name"),
			"description": asStr(fn["description"]),
			"parameters":  cleanSchema(schema),
		})
	}
	if v, ok := in["max_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := in["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	// tool_choice / response_format：cc-switch 同样丢弃，不映射。
	copyPassthrough(out, in, "temperature", "top_p", "stream")
	return out
}

// ---------- 2. responses -> chat（transform_codex_chat.rs:258 responses_to_chat_completions + 逆推） ----------

func responsesToChatReq(in map[string]any) map[string]any {
	out := map[string]any{}
	if m := getStr(in, "model"); m != "" {
		out["model"] = m
	}
	var messages []any
	if ins := getStr(in, "instructions"); ins != "" {
		messages = append(messages, map[string]any{"role": "system", "content": ins})
	}
	if s, ok := in["input"].(string); ok {
		// responses 允许 input 为纯字符串。
		if s != "" {
			messages = append(messages, map[string]any{"role": "user", "content": s})
		}
	} else {
		for _, it := range asArr(in["input"]) {
			item := asMap(it)
			switch asStr(item["type"]) {
			case "function_call":
				id := getStr(item, "call_id")
				if id == "" {
					id = getStr(item, "id")
				}
				args := asStr(item["arguments"])
				if args == "" {
					args = "{}"
				}
				messages = append(messages, map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []any{map[string]any{
						"id": id, "type": "function",
						"function": map[string]any{"name": getStr(item, "name"), "arguments": args},
					}},
				})
			case "function_call_output":
				var output any = ""
				if s, ok := item["output"].(string); ok {
					output = s
				} else if item["output"] != nil {
					output = canon(item["output"])
				}
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": getStr(item, "call_id"), "content": output,
				})
			case "reasoning":
				var sb strings.Builder
				for _, s := range asArr(item["summary"]) {
					if asStr(asMap(s)["type"]) == "summary_text" {
						sb.WriteString(asStr(asMap(s)["text"]))
					}
				}
				if sb.Len() > 0 {
					messages = append(messages, map[string]any{
						"role": "assistant", "content": nil, "reasoning_content": sb.String(),
					})
				}
			default:
				role := asStr(item["role"])
				if role == "" {
					continue
				}
				if role == "system" {
					messages = append(messages, map[string]any{"role": "system", "content": textOfContent(item["content"])})
					continue
				}
				var parts []any
				if s, ok := item["content"].(string); ok {
					if s != "" {
						parts = append(parts, map[string]any{"type": "text", "text": s})
					}
				} else {
					for _, p := range asArr(item["content"]) {
						pm := asMap(p)
						switch asStr(pm["type"]) {
						case "input_text", "output_text", "text":
							if t := asStr(pm["text"]); t != "" {
								parts = append(parts, map[string]any{"type": "text", "text": t})
							}
						case "input_image":
							if u := asStr(pm["image_url"]); u != "" {
								parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
							}
						}
					}
				}
				content := any(nil)
				if len(parts) == 1 && asStr(asMap(parts[0])["type"]) == "text" {
					content = asStr(asMap(parts[0])["text"])
				} else if len(parts) > 0 {
					content = parts
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
		}
	}
	if messages == nil {
		messages = []any{}
	}
	out["messages"] = messages
	for _, t := range asArr(in["tools"]) {
		tm := asMap(t)
		if asStr(tm["type"]) != "function" {
			continue
		}
		params := tm["parameters"]
		if params == nil {
			params = map[string]any{}
		}
		out["tools"] = append(asArr(out["tools"]), map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": getStr(tm, "name"), "description": asStr(tm["description"]),
				"parameters": cleanSchema(params),
			},
		})
	}
	// tool_choice：字符串原样透传，对象尽力而为。
	if tc, ok := in["tool_choice"]; ok {
		if s, ok := tc.(string); ok {
			out["tool_choice"] = s
		} else if tm := asMap(tc); asStr(tm["type"]) == "function" {
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": getStr(tm, "name")}}
		}
	}
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	copyPassthrough(out, in, "temperature", "top_p", "stream")
	return out
}

// ---------- 3. messages -> chat（transform.rs anthropic_to_openai） ----------

func anthropicToolChoiceToChat(tc any) any {
	if s, ok := tc.(string); ok {
		tc = map[string]any{"type": s}
	}
	tm := asMap(tc)
	switch asStr(tm["type"]) {
	case "any":
		return "required"
	case "auto":
		return "auto"
	case "none":
		return "none"
	}
	if asStr(tm["type"]) == "tool" && getStr(tm, "name") != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": getStr(tm, "name")}}
	}
	return tc
}

func messagesToChatReq(in map[string]any) map[string]any {
	out := map[string]any{}
	if m := getStr(in, "model"); m != "" {
		out["model"] = m
	}
	var messages []any
	if sys, ok := in["system"]; ok {
		var texts []string
		if s, ok := sys.(string); ok {
			texts = append(texts, s)
		} else {
			for _, b := range asArr(sys) {
				if t := asStr(asMap(b)["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
		if t := strings.Join(texts, "\n"); t != "" {
			messages = append(messages, map[string]any{"role": "system", "content": t})
		}
	}
	for _, m := range asArr(in["messages"]) {
		msg := asMap(m)
		role := getStr(msg, "role")
		if role == "" {
			role = "user"
		}
		content := msg["content"]
		if s, ok := content.(string); ok {
			messages = append(messages, map[string]any{"role": role, "content": s})
			continue
		}
		var parts []any
		var toolCalls []any
		for _, b := range asArr(content) {
			bm := asMap(b)
			switch asStr(bm["type"]) {
			case "text":
				if t := asStr(bm["text"]); t != "" {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "image":
				src := asMap(bm["source"])
				switch asStr(src["type"]) {
				case "base64":
					mt := asStr(src["media_type"])
					if mt == "" {
						mt = "image/png"
					}
					parts = append(parts, map[string]any{"type": "image_url",
						"image_url": map[string]any{"url": "data:" + mt + ";base64," + asStr(src["data"])}})
				case "url":
					if u := asStr(src["url"]); u != "" {
						parts = append(parts, map[string]any{"type": "image_url",
							"image_url": map[string]any{"url": u}})
					}
				}
			case "tool_use":
				input := bm["input"]
				if input == nil {
					input = map[string]any{}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": getStr(bm, "id"), "type": "function",
					"function": map[string]any{"name": getStr(bm, "name"), "arguments": canon(input)},
				})
			case "tool_result":
				var c string
				if s, ok := bm["content"].(string); ok {
					c = s
				} else if bm["content"] != nil {
					c = canon(bm["content"])
				}
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": getStr(bm, "tool_use_id"), "content": c,
				})
			case "thinking", "redacted_thinking":
				// 默认丢弃（cc-switch 行为）。
			}
		}
		var c any
		if len(parts) == 1 && asStr(asMap(parts[0])["type"]) == "text" && len(toolCalls) == 0 {
			c = asStr(asMap(parts[0])["text"])
		} else if len(parts) > 0 {
			c = parts
		}
		if c != nil || len(toolCalls) > 0 {
			cm := map[string]any{"role": role, "content": c}
			if len(toolCalls) > 0 {
				cm["tool_calls"] = toolCalls
			}
			messages = append(messages, cm)
		}
	}
	if messages == nil {
		messages = []any{}
	}
	out["messages"] = messages
	for _, t := range asArr(in["tools"]) {
		tm := asMap(t)
		schema := tm["input_schema"]
		if schema == nil {
			schema = map[string]any{}
		}
		out["tools"] = append(asArr(out["tools"]), map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": getStr(tm, "name"), "description": asStr(tm["description"]),
				"parameters": cleanSchema(schema),
			},
		})
	}
	if tc, ok := in["tool_choice"]; ok {
		out["tool_choice"] = anthropicToolChoiceToChat(tc)
	}
	if v, ok := in["stop_sequences"]; ok {
		out["stop"] = v
	}
	copyPassthrough(out, in, "max_tokens", "temperature", "top_p", "stream")
	return out
}

// ---------- 4. chat -> messages（3 的逆映射，自写，cc-switch 无此方向） ----------

func chatToolChoiceToAnthropic(tc any) any {
	if s, ok := tc.(string); ok {
		switch s {
		case "required":
			return map[string]any{"type": "any"}
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		}
		return tc
	}
	tm := asMap(tc)
	if asStr(tm["type"]) == "function" {
		return map[string]any{"type": "tool", "name": getStr(asMap(tm["function"]), "name")}
	}
	return tc
}

// dataURLToImageSource 把 data:mime;base64,... 或 http url 转 anthropic image source。
func dataURLToImageSource(u string) map[string]any {
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		parts := strings.SplitN(rest, ";base64,", 2)
		mt := "image/png"
		data := ""
		if len(parts) == 2 {
			if parts[0] != "" {
				mt = strings.Split(parts[0], ";")[0]
			}
			data = parts[1]
		}
		return map[string]any{"type": "base64", "media_type": mt, "data": data}
	}
	return map[string]any{"type": "url", "url": u}
}

func chatToMessagesReq(in map[string]any) map[string]any {
	out := map[string]any{}
	if m := getStr(in, "model"); m != "" {
		out["model"] = m
	}
	var system []any
	var messages []any
	for _, m := range asArr(in["messages"]) {
		msg := asMap(m)
		role := getStr(msg, "role")
		if role == "" {
			role = "user"
		}
		content := msg["content"]
		if role == "system" {
			if t := textOfContent(content); t != "" {
				system = append(system, map[string]any{"type": "text", "text": t})
			}
			continue
		}
		if role == "tool" {
			var c any = asStr(content)
			if _, ok := content.(string); !ok && content != nil {
				c = []any{map[string]any{"type": "text", "text": canon(content)}}
			}
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": getStr(msg, "tool_call_id"), "content": c,
				}},
			})
			continue
		}
		var blocks []any
		if s, ok := content.(string); ok {
			if s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
		} else {
			for _, p := range asArr(content) {
				pm := asMap(p)
				switch asStr(pm["type"]) {
				case "text", "input_text", "output_text":
					if t := asStr(pm["text"]); t != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": t})
					}
				case "image_url":
					iu := pm["image_url"]
					if s, ok := iu.(string); ok {
						iu = map[string]any{"url": s}
					}
					if u := getStr(asMap(iu), "url"); u != "" {
						blocks = append(blocks, map[string]any{"type": "image", "source": dataURLToImageSource(u)})
					}
				}
			}
		}
		for _, tc := range asArr(msg["tool_calls"]) {
			tm := asMap(tc)
			fn := asMap(tm["function"])
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": getStr(tm, "id"),
				"name": getStr(fn, "name"), "input": parseObj(asStr(fn["arguments"])),
			})
		}
		if len(blocks) == 0 {
			continue
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	if len(system) == 1 {
		out["system"] = asStr(asMap(system[0])["text"])
	} else if len(system) > 1 {
		out["system"] = system
	}
	if messages == nil {
		messages = []any{}
	}
	out["messages"] = messages
	for _, t := range asArr(in["tools"]) {
		fn := asMap(asMap(t)["function"])
		if len(fn) == 0 {
			continue
		}
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{}
		}
		out["tools"] = append(asArr(out["tools"]), map[string]any{
			"name": getStr(fn, "name"), "description": asStr(fn["description"]),
			"input_schema": cleanSchema(params),
		})
	}
	if tc, ok := in["tool_choice"]; ok {
		out["tool_choice"] = chatToolChoiceToAnthropic(tc)
	}
	if v, ok := in["stop"]; ok {
		out["stop_sequences"] = v
	}
	copyPassthrough(out, in, "max_tokens", "temperature", "top_p", "stream")
	return out
}

// ---------- 5. messages -> responses（transform_responses.rs:1770 anthropic_to_responses） ----------

func responsesToolChoiceFromAnthropic(tc any) any {
	if s, ok := tc.(string); ok {
		return s
	}
	tm := asMap(tc)
	switch asStr(tm["type"]) {
	case "any":
		return "required"
	case "auto":
		return "auto"
	case "none":
		return "none"
	}
	if asStr(tm["type"]) == "tool" && getStr(tm, "name") != "" {
		return map[string]any{"type": "function", "name": getStr(tm, "name")}
	}
	return tc
}

func messagesToResponsesReq(in map[string]any) map[string]any {
	out := map[string]any{}
	if m := getStr(in, "model"); m != "" {
		out["model"] = m
	}
	if sys, ok := in["system"]; ok {
		var texts []string
		if s, ok := sys.(string); ok {
			texts = append(texts, s)
		} else {
			for _, b := range asArr(sys) {
				if t := asStr(asMap(b)["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
		if t := strings.Join(texts, "\n\n"); t != "" {
			out["instructions"] = t
		}
	}
	var input []any
	var pendingParts []any
	pendingRole := ""
	flushPending := func() {
		if len(pendingParts) == 0 {
			return
		}
		input = append(input, map[string]any{"role": pendingRole, "content": pendingParts})
		pendingParts = nil
	}
	for _, m := range asArr(in["messages"]) {
		msg := asMap(m)
		role := getStr(msg, "role")
		if role == "" {
			role = "user"
		}
		content := msg["content"]
		if s, ok := content.(string); ok {
			if s == "" {
				continue
			}
			typ := "input_text"
			if role == "assistant" {
				typ = "output_text"
			}
			if pendingRole != "" && pendingRole != role {
				flushPending()
			}
			pendingRole = role
			pendingParts = append(pendingParts, map[string]any{"type": typ, "text": s})
			continue
		}
		for _, b := range asArr(content) {
			bm := asMap(b)
			switch asStr(bm["type"]) {
			case "text":
				if t := asStr(bm["text"]); t != "" {
					typ := "input_text"
					if role == "assistant" {
						typ = "output_text"
					}
					if pendingRole != "" && pendingRole != role {
						flushPending()
					}
					pendingRole = role
					pendingParts = append(pendingParts, map[string]any{"type": typ, "text": t})
				}
			case "image":
				flushPending()
				src := asMap(bm["source"])
				var u string
				switch asStr(src["type"]) {
				case "url":
					u = asStr(src["url"])
				case "base64":
					mt := asStr(src["media_type"])
					if mt == "" {
						mt = "image/png"
					}
					u = "data:" + mt + ";base64," + asStr(src["data"])
				}
				if u != "" {
					input = append(input, map[string]any{"type": "input_image", "image_url": u})
				}
			case "tool_use":
				flushPending()
				args := bm["input"]
				if args == nil {
					args = map[string]any{}
				}
				input = append(input, map[string]any{
					"type": "function_call", "call_id": getStr(bm, "id"),
					"name": getStr(bm, "name"), "arguments": canon(args),
				})
			case "tool_result":
				flushPending()
				var output any
				if s, ok := bm["content"].(string); ok {
					output = s
				} else if bm["content"] != nil {
					output = canon(bm["content"])
				} else {
					output = ""
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": getStr(bm, "tool_use_id"), "output": output,
				})
			case "thinking", "redacted_thinking":
				// 原生 thinking 无签名一律丢弃（cc-switch 行为）。
			}
		}
	}
	flushPending()
	if input == nil {
		input = []any{}
	}
	out["input"] = input
	for _, t := range asArr(in["tools"]) {
		tm := asMap(t)
		schema := tm["input_schema"]
		if schema == nil {
			schema = map[string]any{}
		}
		out["tools"] = append(asArr(out["tools"]), map[string]any{
			"type": "function", "name": getStr(tm, "name"),
			"description": asStr(tm["description"]),
			"parameters":  cleanSchema(schema),
		})
	}
	if tc, ok := in["tool_choice"]; ok {
		out["tool_choice"] = responsesToolChoiceFromAnthropic(tc)
	}
	// stop_sequences：responses 无对应字段，丢弃（cc-switch 行为）。
	if v, ok := in["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	copyPassthrough(out, in, "temperature", "top_p", "stream")
	return out
}

// ---------- 6. responses -> messages（5 的逆映射，自写） ----------

func responsesToMessagesReq(in map[string]any) map[string]any {
	out := map[string]any{}
	if m := getStr(in, "model"); m != "" {
		out["model"] = m
	}
	if ins := getStr(in, "instructions"); ins != "" {
		out["system"] = ins
	}
	var messages []any
	var pendingBlocks []any
	pendingRole := ""
	flushPending := func() {
		if len(pendingBlocks) == 0 {
			return
		}
		messages = append(messages, map[string]any{"role": pendingRole, "content": pendingBlocks})
		pendingBlocks = nil
	}
	pushText := func(role, text string) {
		if text == "" {
			return
		}
		if pendingRole != "" && pendingRole != role {
			flushPending()
		}
		pendingRole = role
		pendingBlocks = append(pendingBlocks, map[string]any{"type": "text", "text": text})
	}
	if s, ok := in["input"].(string); ok {
		// responses 允许 input 为纯字符串。
		pushText("user", s)
	} else {
		for _, it := range asArr(in["input"]) {
			item := asMap(it)
			switch asStr(item["type"]) {
			case "function_call":
				flushPending()
				id := getStr(item, "call_id")
				if id == "" {
					id = getStr(item, "id")
				}
				messages = append(messages, map[string]any{
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "tool_use", "id": id,
						"name": getStr(item, "name"), "input": parseObj(asStr(item["arguments"])),
					}},
				})
			case "function_call_output":
				flushPending()
				var c any = asStr(item["output"])
				if _, ok := item["output"].(string); !ok && item["output"] != nil {
					c = []any{map[string]any{"type": "text", "text": canon(item["output"])}}
				}
				messages = append(messages, map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type": "tool_result", "tool_use_id": getStr(item, "call_id"), "content": c,
					}},
				})
			case "reasoning":
				// 丢弃（同 messages 原生 thinking 处理）。
			default:
				role := asStr(item["role"])
				if role == "" {
					continue
				}
				if role == "system" {
					continue
				}
				if s, ok := item["content"].(string); ok {
					pushText(role, s)
					continue
				}
				for _, p := range asArr(item["content"]) {
					pm := asMap(p)
					switch asStr(pm["type"]) {
					case "input_text", "output_text", "text":
						pushText(role, asStr(pm["text"]))
					case "input_image":
						flushPending()
						if u := asStr(pm["image_url"]); u != "" {
							messages = append(messages, map[string]any{
								"role": role,
								"content": []any{map[string]any{
									"type": "image", "source": dataURLToImageSource(u),
								}},
							})
						}
					}
				}
			}
		}
	}
	flushPending()
	if messages == nil {
		messages = []any{}
	}
	out["messages"] = messages
	for _, t := range asArr(in["tools"]) {
		tm := asMap(t)
		if asStr(tm["type"]) != "function" {
			continue
		}
		params := tm["parameters"]
		if params == nil {
			params = map[string]any{}
		}
		out["tools"] = append(asArr(out["tools"]), map[string]any{
			"name": getStr(tm, "name"), "description": asStr(tm["description"]),
			"input_schema": cleanSchema(params),
		})
	}
	if tc, ok := in["tool_choice"]; ok {
		out["tool_choice"] = responsesToolChoiceToChat(tc)
	}
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	copyPassthrough(out, in, "temperature", "top_p", "stream")
	return out
}

// responsesToolChoiceToChat 把 responses tool_choice 转成 chat/messages 通用形式。
// 字符串原样，{type:function,name} 转 messages 风格 {type:tool,name}，调用方再转。
func responsesToolChoiceToChat(tc any) any {
	if s, ok := tc.(string); ok {
		return s
	}
	tm := asMap(tc)
	if asStr(tm["type"]) == "function" && getStr(tm, "name") != "" {
		return map[string]any{"type": "tool", "name": getStr(tm, "name")}
	}
	return tc
}
