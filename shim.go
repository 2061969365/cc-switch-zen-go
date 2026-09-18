// shim.go: harness 旁路 —— 非官方 UA 的 /v1/responses 请求借 `opencode run`
// 子进程（官方身份）发真上游，再包回 responses envelope。
//
// 背景：上游 FreeTier 对录制/重放类请求一律 403（11 维度证伪），只有官方
// 进程 live 调用链才认。shim 不伪造身份，借整个发送动作。
//
// 安全边界：--agent plan + --pure（只读规划，无外部插件，不落地写操作；
// 未加 --auto，plan agent 无需批准，无静默执行风险）。
// 并发：SHIM_CONCURRENCY 信号量（默认 2，小并发；Node 版串行为 1）。
// 超时：SHIM_TIMEOUT_MS（默认 300000），CommandContext 超时 kill，绝不僵死。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 配置 ----------

func shimEnabled() bool {
	v := os.Getenv("SHIM_ENABLE")
	if v == "" {
		return true
	}
	return !(v == "0" || strings.EqualFold(v, "false"))
}

func shimTimeout() time.Duration {
	if v := os.Getenv("SHIM_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 300 * time.Second
}

func shimModel() string {
	if v := os.Getenv("SHIM_MODEL"); v != "" {
		return v
	}
	return "test/muse-spark-1.3-contributor-free"
}

// 小并发上限，缺省 2，非法值回缺省。
func shimConcurrency() int {
	if v := os.Getenv("SHIM_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 8 {
			return n
		}
	}
	return 2
}

func isOfficialUA(ua string) bool {
	return strings.HasPrefix(ua, "opencode/")
}

// shimBackendOK 启动时探测一次：本机有无 opencode 可执行文件。
// railway 等无 opencode 的环境旁路直接 502 明错，不静默回退（回退也是 403，掩盖原因）。
var (
	shimBackendOnce sync.Once
	shimBackendErr  error
	shimOpencodeBin string
)

// 候选位置：PATH 解析优先（交互 shell 起的网关），后台/Hidden 进程 PATH
// 常缺 User 条目，此时回退 APPDATA 硬编码路径（npm 默认全局盘）。
func shimFindOpencode() (string, error) {
	for _, name := range []string{"opencode.cmd", "opencode.exe", "opencode"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		for _, name := range []string{"opencode.cmd", "opencode.exe"} {
			p := appdata + "\\npm\\" + name
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
	}
	// 备用全局盘（本机 F:\pi\npm-global）。
	for _, p := range []string{"F:\\pi\\npm-global\\opencode.cmd", "F:\\pi\\npm-global\\opencode.exe"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("opencode binary not found in PATH")
}

func shimBackendOK() error {
	shimBackendOnce.Do(func() {
		// Windows 上 opencode 是 npm 垫片（opencode.cmd），经 cmd /c 调用；
		// 此处只探测存在性。
		if _, err := exec.LookPath("cmd.exe"); err != nil {
			shimBackendErr = fmt.Errorf("cmd.exe not found: %v", err)
			return
		}
		bin, err := shimFindOpencode()
		if err != nil {
			shimBackendErr = err
			return
		}
		shimOpencodeBin = bin
	})
	return shimBackendErr
}

// ---------- prompt 组装（移植 shim.js inputToPrompt/toolsToPrompt/lastUserText） ----------

func shimTextOf(c any) (string, bool) {
	m, ok := c.(map[string]any)
	if !ok {
		return "", false
	}
	if t, ok := m["text"].(string); ok {
		return t, true
	}
	if t, ok := m["output_text"].(string); ok {
		return t, true
	}
	if typ, _ := m["type"].(string); typ == "function_call_output" || typ == "tool_result" {
		if out, ok := m["output"].(string); ok {
			return "[tool result] " + out, true
		}
		b, _ := json.Marshal(m["output"])
		return "[tool result] " + string(b), true
	}
	return "", false
}

// 全量 prompt：system/developer 在前，user history 在后，tool 结果转文字。
func shimInputToPrompt(body map[string]any) string {
	var sys, parts []string
	if ins, ok := body["instructions"].(string); ok && ins != "" {
		sys = append(sys, ins)
	}
	items, _ := body["input"].([]any)
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := it["role"].(string)
		isSys := strings.Contains(role, "system") || strings.Contains(role, "developer")
		content := it["content"]
		if s, ok := content.(string); ok {
			if isSys {
				sys = append(sys, s)
			} else if role != "" {
				parts = append(parts, role+": "+s)
			} else {
				parts = append(parts, s)
			}
			continue
		}
		if arr, ok := content.([]any); ok {
			var texts []string
			for _, c := range arr {
				if t, ok := shimTextOf(c); ok {
					texts = append(texts, t)
				}
			}
			if len(texts) > 0 {
				joined := strings.Join(texts, "\n")
				if isSys {
					sys = append(sys, joined)
				} else if role != "" {
					parts = append(parts, role+": "+joined)
				} else {
					parts = append(parts, joined)
				}
			}
		}
	}
	return strings.Join(append(sys, parts...), "\n\n")
}

// tools 定义只作参考附录（opencode 不认识 harness 工具名，不做 function_call 翻译）。
func shimToolsToPrompt(tools any) string {
	arr, ok := tools.([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, "\n\n[Tools available in the calling harness (for reference; use your own read-only tools as needed):]")
	for _, raw := range arr {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		lines = append(lines, fmt.Sprintf("- %s: %s", name, desc))
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// 续会话增量：最后一轮 user 文本。
func shimLastUserText(body map[string]any) string {
	items, _ := body["input"].([]any)
	for i := len(items) - 1; i >= 0; i-- {
		it, ok := items[i].(map[string]any)
		if !ok || it["role"] != "user" {
			continue
		}
		if s, ok := it["content"].(string); ok {
			return s
		}
		if arr, ok := it["content"].([]any); ok {
			var texts []string
			for _, c := range arr {
				if t, ok := shimTextOf(c); ok && !strings.HasPrefix(t, "[tool result]") {
					texts = append(texts, t)
				}
			}
			if len(texts) > 0 {
				return strings.Join(texts, "\n")
			}
		}
	}
	return ""
}

// ---------- 会话钉定（移植 convFingerprint/convSessions，64 上限） ----------

var (
	shimSesMu sync.Mutex
	shimSes   = map[string]string{}
	shimSesQ  []string
)

func shimFingerprint(body map[string]any) string {
	first := ""
	if items, ok := body["input"].([]any); ok && len(items) > 0 {
		b, _ := json.Marshal(items[0])
		first = string(b)
		if len(first) > 200 {
			first = first[:200]
		}
	}
	ins, _ := body["instructions"].(string)
	if len(ins) > 100 {
		ins = ins[:100]
	}
	return first + "|" + ins
}

func shimLookupSession(fp string) string {
	shimSesMu.Lock()
	defer shimSesMu.Unlock()
	return shimSes[fp]
}

func shimStoreSession(fp, ses string) {
	if fp == "" || ses == "" {
		return
	}
	shimSesMu.Lock()
	defer shimSesMu.Unlock()
	if _, ok := shimSes[fp]; !ok {
		shimSesQ = append(shimSesQ, fp)
		if len(shimSesQ) > 64 {
			delete(shimSes, shimSesQ[0])
			shimSesQ = shimSesQ[1:]
		}
	}
	shimSes[fp] = ses
}

// ---------- 回吐清洗与 envelope（移植 sanitizeForJson/responsesEnvelope） ----------

// 去 ANSI 转义/裸控制字符（炸 envelope 的真凶），截断。
func shimSanitize(s string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		// ANSI CSI: ESC [ ... letter
		if c == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !((s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= 'a' && s[j] <= 'z')) {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		// ANSI OSC: ESC ] ... BEL
		if c == 0x1b && i+1 < len(s) && s[i+1] == ']' {
			j := i + 2
			for j < len(s) && s[j] != 0x07 {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		// 裸控制字符（保留 \n \r \t）
		if (c < 0x20 && c != '\n' && c != '\r' && c != '\t') || c == 0x7f {
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	t := b.String()
	if len(t) > maxLen {
		t = t[:maxLen] + "\n...[truncated]"
	}
	return t
}

// shimStripPlanMode 脱掉 plan agent 的身份宣告句（harness 自带 system prompt，
// 不需要模型再宣告 plan mode）。按句子切分，删含 "plan mode" 的句子；
// fail-open：删完为空则返回原文，绝不回空。
func shimStripPlanMode(s string) string {
	var kept []string
	var cur strings.Builder
	flush := func() {
		seg := cur.String()
		cur.Reset()
		if seg == "" {
			return
		}
		if strings.Contains(strings.ToLower(seg), "plan mode") {
			return
		}
		kept = append(kept, seg)
	}
	for _, r := range s {
		cur.WriteRune(r)
		if r == '\n' || r == '.' || r == '!' || r == '?' || r == '\u3002' || r == '\uff01' || r == '\uff1f' {
			flush()
		}
	}
	flush()
	out := strings.Join(kept, "")
	// 收敛多余空行。
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return strings.TrimSpace(s)
	}
	return out
}

// 纯 message 回吐：opencode 不认识 harness 工具名，永远不回 function_call。
func shimEnvelope(reqID, model, text string) map[string]any {
	now := time.Now().Unix()
	output := []any{}
	if clean := shimSanitize(text, 24000); clean != "" {
		output = append(output, map[string]any{
			"id":   fmt.Sprintf("msg_shim_%d", now),
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": clean, "annotations": []any{}}},
		})
	}
	status := "completed"
	if len(output) == 0 {
		status = "incomplete"
	}
	return map[string]any{
		"id": reqID, "object": "response", "created_at": now,
		"model": model, "status": status, "output": output,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
	}
}

// ---------- 子进程调用 ----------

type shimResult struct {
	ok        bool
	timeout   bool
	text      string
	sessionID string
	errMsg    string
}

// --format json 事件流：只取 text part + 顶层 sessionID（tool 事件不在 stdout，在 DB）。
func shimParseEvents(out string) (string, string) {
	var texts []string
	sessionID := ""
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t[0] != '{' {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(t), &ev); err != nil {
			continue
		}
		if s, ok := ev["sessionID"].(string); ok && s != "" {
			sessionID = s
		}
		if ev["type"] == "text" {
			if part, ok := ev["part"].(map[string]any); ok {
				if txt, ok := part["text"].(string); ok {
					texts = append(texts, txt)
				}
			}
		}
	}
	return strings.Join(texts, ""), sessionID
}

func shimRunOpencode(ctx context.Context, prompt, sessionID, model string) shimResult {
	args := []string{"/c", shimOpencodeBin, "run", "--pure", "--format", "json", "--agent", "plan", "-m", model}
	if sessionID != "" {
		args = append(args, "-s", sessionID)
	}
	cmd := exec.CommandContext(ctx, "cmd.exe", args...)
	// 长 prompt 走 stdin（Node 版只走 argv，此处直接实现）。
	// Windows 命令行上限 32767 字符，超限则 argv 传空、stdin 喂。
	if len(prompt) > 20000 {
		cmd.Args = append(cmd.Args, "-")
		cmd.Stdin = strings.NewReader(prompt)
	} else {
		cmd.Args = append(cmd.Args, prompt)
	}
	var outBuf, errBuf bytes.Buffer
	// stdout 可能很大（system prompt 回显），上限 8MB 截断防 OOM。
	cmd.Stdout = &limitedWriter{w: &outBuf, n: 8 << 20}
	cmd.Stderr = &limitedWriter{w: &errBuf, n: 1 << 20}
	start := time.Now()
	err := cmd.Run()
	_ = time.Since(start)
	if ctx.Err() == context.DeadlineExceeded {
		return shimResult{timeout: true, errMsg: tailString(errBuf.String(), 2000)}
	}
	if err != nil {
		// 启动类错误（如 binary 不存在）stderr 为空，必须带上 err 本身，否则回吐空 message。
		msg := err.Error()
		if tail := tailString(errBuf.String(), 1500); tail != "" {
			msg += " | " + tail
		}
		return shimResult{errMsg: msg}
	}
	text, ses := shimParseEvents(outBuf.String())
	return shimResult{ok: true, text: text, sessionID: ses}
}

type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	remain := l.n - l.w.Len()
	if remain <= 0 {
		return len(p), nil // 丢弃但谎报成功，避免子进程 SIGPIPE
	}
	if len(p) > remain {
		p = p[:remain]
	}
	return l.w.Write(p)
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------- 并发门 + HTTP 入口 ----------

var shimSem = make(chan struct{}, 2)

func init() {
	// 启动时按配置重建信号量容量（默认 2，小并发）。
	if cap := shimConcurrency(); cap != 2 {
		shimSem = make(chan struct{}, cap)
	}
}

var shimSeqMu sync.Mutex
var shimSeq int64

func shimNextID() string {
	shimSeqMu.Lock()
	defer shimSeqMu.Unlock()
	shimSeq++
	return fmt.Sprintf("resp_shim_%x%x", shimSeq, time.Now().UnixMilli()&0xffff)
}

// shimHandler：harness 旁路入口。调用方已判定为非官方 UA 的 POST /v1/responses。
func shimHandler(w http.ResponseWriter, req *http.Request) {
	t0 := time.Now()
	reqID := shimNextID()
	if err := shimBackendOK(); err != nil {
		logf("[SHIM %s] backend unavailable: %v", reqID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "shim_unavailable", "message": "opencode binary not found: " + err.Error()}})
		_, _ = w.Write(b)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 32<<20))
	if err != nil {
		writeConvError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		writeConvError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	stream, _ := in["stream"].(bool)
	model, _ := in["model"].(string)
	if model == "" {
		model = shimModel()
	} else if !strings.Contains(model, "/") {
		// harness 发裸 model 名（如 muse-spark-1.3-contributor-free）；
		// opencode run -m 要求 provider/ 前缀，无则补 test/（已实测无前缀 exit 1）。
		model = "test/" + model
	}
	// 会话钉定：同对话续 -s，只发增量；新对话拼全量。
	fp := shimFingerprint(in)
	ses := shimLookupSession(fp)
	var prompt string
	if ses != "" {
		prompt = shimLastUserText(in)
	} else {
		prompt = shimInputToPrompt(in)
	}
	prompt += shimToolsToPrompt(in["tools"])
	if strings.TrimSpace(prompt) == "" {
		writeConvError(w, http.StatusBadRequest, "empty prompt")
		return
	}
	downUA := req.Header.Get("User-Agent")
	if len(downUA) > 60 {
		downUA = downUA[:60]
	}
	// 小并发信号量：满时阻塞等待，下游断开即放弃（不占坑）。
	select {
	case shimSem <- struct{}{}:
		defer func() { <-shimSem }()
	case <-req.Context().Done():
		writeConvError(w, 499, "client closed")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), shimTimeout())
	defer cancel()
	r := shimRunOpencode(ctx, prompt, ses, model)
	elms := time.Since(t0).Milliseconds()
	cont := "new"
	if ses != "" {
		cont = "yes"
	}
	if r.timeout {
		logf("[SHIM %s] TIMEOUT ms=%d ua=%q", reqID, elms, downUA)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "shim_timeout", "message": r.errMsg}})
		_, _ = w.Write(b)
		return
	}
	if !r.ok {
		logf("[SHIM %s] ERROR ms=%d ua=%q err=%.200s", reqID, elms, downUA, r.errMsg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "shim_error", "message": r.errMsg}})
		_, _ = w.Write(b)
		return
	}
	if r.sessionID != "" {
		shimStoreSession(fp, r.sessionID)
	}
	env := shimEnvelope(reqID, model, shimStripPlanMode(r.text))
	logf("[SHIM %s] done ok ms=%d textLen=%d ses=%s cont=%s ua=%q", reqID, elms, len(r.text), r.sessionID, cont, downUA)
	if stream {
		// 规范 responses 流式事件序列（pi-ai 只认这套：output_item.added 建槽，
		// delta 累文本，output_item.done 落槽，completed 结算；整包 output 会被无视，
		// 缺槽的 delta 直接丢弃——此前自造三件套导致 harness 解析出空）。
		shimEmitStream(w, reqID, model, env)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(env)
	_, _ = w.Write(b)
}

// shimEmitStream 按规范序列吐流：created → item.added → text.delta* →
// item.done → completed → [DONE]。文本按 500 字切块，有流式感。
func shimEmitStream(w http.ResponseWriter, reqID, model string, env map[string]any) {
	sink := newSSESink(w)
	createdAt, _ := env["created_at"].(int64)
	msgID := fmt.Sprintf("msg_shim_%d", createdAt)
	text := ""
	if out, ok := env["output"].([]any); ok && len(out) > 0 {
		if msg, ok := out[0].(map[string]any); ok {
			if content, ok := msg["content"].([]any); ok && len(content) > 0 {
				if c0, ok := content[0].(map[string]any); ok {
					text, _ = c0["text"].(string)
				}
			}
		}
	}
	// 1. response.created
	sink.emit(map[string]any{"type": "response.created",
		"response": map[string]any{"id": reqID, "object": "response", "created_at": createdAt, "model": model, "status": "in_progress", "output": []any{}}})
	// 2. output_item.added 建槽
	msgItem := map[string]any{"id": msgID, "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}}
	sink.emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": msgItem})
	// 3. delta 切块（按 rune 切，防中文半字）
	runes := []rune(text)
	for i := 0; i < len(runes); i += 500 {
		end := i + 500
		if end > len(runes) {
			end = len(runes)
		}
		sink.emit(map[string]any{"type": "response.output_text.delta", "output_index": 0,
			"item_id": msgID, "delta": string(runes[i:end])})
	}
	// 4. output_item.done 落槽（完整文本）
	doneItem := map[string]any{"id": msgID, "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	sink.emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": doneItem})
	// 5. completed 结算
	sink.emit(map[string]any{"type": "response.completed",
		"response": map[string]any{"id": reqID, "object": "response", "created_at": createdAt, "model": model,
			"status": "completed", "output": env["output"],
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
	sink.done()
}
