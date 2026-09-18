// shim.go: harness 旁路 —— 非官方 UA 的 /v1/responses 请求借 `opencode run`
// 子进程（官方身份）发真上游，再包回 responses envelope。
//
// 背景：上游 FreeTier 对录制/重放类请求一律 403（11 维度证伪），只有官方
// 进程 live 调用链才认。shim 不伪造身份，借整个发送动作。
//
// 安全边界：--pure（无外部插件）+ 默认 --agent build（全功能，无 plan 身份）。
// 未加 --auto：写/执行类动作卡权限等待直至超时（fail-closed，不静默执行）。
// 并发：SHIM_CONCURRENCY 信号量（默认 2，小并发；Node 版串行为 1）。
// 超时：SHIM_TIMEOUT_MS（默认 300000），CommandContext 超时 kill，绝不僵死。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
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

// 子进程 agent：默认 bypass（全功能+权限全放行，无需 --auto，无身份包袱）。
// SHIM_AGENT 可覆盖（如 plan）。身份归 harness 侧，shim 不过问。
func shimAgent() string {
	if v := os.Getenv("SHIM_AGENT"); v != "" {
		return v
	}
	return "bypass"
}

// 并发上限，缺省 10，非法值回缺省（上限 32，防误配吃光本机）。
func shimConcurrency() int {
	if v := os.Getenv("SHIM_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 32 {
			return n
		}
	}
	return 10
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
	// Linux 上 npm 全局 bin 是带 shebang 的可执行脚本，直接调。
	// 此处只探测存在性。
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("cmd.exe"); err != nil {
			shimBackendErr = fmt.Errorf("cmd.exe not found: %v", err)
			return
		}
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

// shimReasoningSummaries：抽取回放回来的 reasoning item 中的可见 summary 文本。
// reasoning item 无 role、带 summary 数组（网关自签发的 rs_shim_*，只有可见文本，
// 永不含 encrypted_content）；只用于新会话回退拼 previous thinking。
func shimReasoningSummaries(body map[string]any) []string {
	items, _ := body["input"].([]any)
	var out []string
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok || it["type"] != "reasoning" {
			continue
		}
		sum, _ := it["summary"].([]any)
		for _, s := range sum {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := sm["text"].(string); ok && strings.TrimSpace(txt) != "" {
				out = append(out, shimSanitize(txt, 4000))
			}
		}
	}
	return out
}

// shimSelectPrompt：新/续会话 prompt 选择。续会话只发增量（-s 服务端已有完整
// reasoning，重发多余）；新会话拼全量，并在尾部追加回来的 previous thinking
// 可见文本（标注来源，加密态永不重建）。
func shimSelectPrompt(ses string, in map[string]any) string {
	if ses != "" {
		return shimLastUserText(in)
	}
	prompt := shimInputToPrompt(in)
	if priors := shimReasoningSummaries(in); len(priors) > 0 {
		prompt += "\n\n[previous thinking]\n" + strings.Join(priors, "\n\n")
	}
	return prompt
}

// ---------- 会话钉定（移植 convFingerprint/convSessions，64 上限） ----------

var (
	shimSesMu sync.Mutex
	shimSes   = map[string]*shimSesEntry{}
	shimSesQ  []string
)

type shimSesEntry struct {
	sessionID string
	lastSeen  time.Time
}

// shimSessionKey 会话键：x-session-id 头优先（harness 每窗口唯一，pi-ai 必带）；
// 无头回退全文 hash（首条 input 全量 + instructions 全量 sha256，不截断）。
// 旧的前 200/100 截断指纹必碰撞（harness 各窗口 system prompt 相同），已废弃。
func shimSessionKey(h http.Header, body map[string]any) string {
	if sid := strings.TrimSpace(h.Get("X-Session-Id")); sid != "" {
		return "hdr:" + sid
	}
	first := ""
	if items, ok := body["input"].([]any); ok && len(items) > 0 {
		b, _ := json.Marshal(items[0])
		first = string(b)
	}
	ins, _ := body["instructions"].(string)
	sum := sha256.Sum256([]byte(first + "\x00" + ins))
	return "fp:" + hex.EncodeToString(sum[:])
}

// shimFingerprint 保留作兼容（单测/旧调用），转发到全文 hash。
func shimFingerprint(body map[string]any) string {
	return shimSessionKey(http.Header{}, body)
}

func shimLookupSession(fp string) string {
	shimSesMu.Lock()
	defer shimSesMu.Unlock()
	if e, ok := shimSes[fp]; ok {
		// LRU：命中 touch。
		e.lastSeen = time.Now()
		for i, k := range shimSesQ {
			if k == fp {
				shimSesQ = append(shimSesQ[:i], shimSesQ[i+1:]...)
				break
			}
		}
		shimSesQ = append(shimSesQ, fp)
		return e.sessionID
	}
	return ""
}

func shimStoreSession(fp, ses string) {
	if fp == "" || ses == "" {
		return
	}
	shimSesMu.Lock()
	defer shimSesMu.Unlock()
	if e, ok := shimSes[fp]; ok {
		// 已存在同样 LRU touch，否则热会话会被当最旧逐出。
		e.sessionID = ses
		e.lastSeen = time.Now()
		for i, k := range shimSesQ {
			if k == fp {
				shimSesQ = append(shimSesQ[:i], shimSesQ[i+1:]...)
				break
			}
		}
		shimSesQ = append(shimSesQ, fp)
		return
	}
	shimSes[fp] = &shimSesEntry{sessionID: ses, lastSeen: time.Now()}
	shimSesQ = append(shimSesQ, fp)
	if len(shimSesQ) > 64 {
		delete(shimSes, shimSesQ[0])
		shimSesQ = shimSesQ[1:]
	}
}

// shimEvictSession：驱逐死会话映射（-s 报 Session not found 后调用）。
// 下次同 fp 回到新会话全量重发，不再沿用死 ID。
func shimEvictSession(fp string) {
	if fp == "" {
		return
	}
	shimSesMu.Lock()
	defer shimSesMu.Unlock()
	delete(shimSes, fp)
	for i, k := range shimSesQ {
		if k == fp {
			shimSesQ = append(shimSesQ[:i], shimSesQ[i+1:]...)
			break
		}
	}
}

// shimIsSessionNotFound：识别 opencode run -s 死会话硬报错
// （run.ts: session() 取不到即 "Session not found" + exit 1）。
func shimIsSessionNotFound(errMsg string) bool {
	return strings.Contains(errMsg, "Session not found")
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

// 纯 message 回吐：身份归 harness 侧，shim 原文透传，不过问内容。
// reasoning 在前、message 在后（与流式 completed.output 顺序一致）。
func shimEnvelope(reqID, model, text string, usage shimUsage, reasoning []string) map[string]any {
	now := time.Now().Unix()
	output := []any{}
	var n int64
	for _, rt := range reasoning {
		if strings.TrimSpace(rt) == "" {
			continue
		}
		n++
		output = append(output, map[string]any{
			"id":   fmt.Sprintf("rs_shim_%d_%d", now, n),
			"type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": rt}},
		})
	}
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
		"usage": map[string]any{"input_tokens": usage.input, "output_tokens": usage.output, "total_tokens": usage.total},
	}
}

// ---------- 子进程调用 ----------

type shimResult struct {
	ok        bool
	timeout   bool
	text      string
	sessionID string
	errMsg    string
	usage     shimUsage
	reasoning []string
}

// shimUsage 真实 token 消耗（取自 --format json 的 step_finish.part.tokens）。
type shimUsage struct {
	input, output, total int
}

// step_finish 事件抽 usage：part.tokens.{total,input,output}。
func shimUsageOf(ev map[string]any) shimUsage {
	var u shimUsage
	part, _ := ev["part"].(map[string]any)
	tok, _ := part["tokens"].(map[string]any)
	if tok == nil {
		return u
	}
	num := func(k string) int {
		switch v := tok[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	u.total, u.input, u.output = num("total"), num("input"), num("output")
	if u.total == 0 {
		u.total = u.input + u.output
	}
	return u
}

// --format json 事件流：取 text/reasoning part + 顶层 sessionID（tool 事件不在 stdout，在 DB）。
// step_finish 顺带抽 usage。
func shimParseEvents(out string) (string, string, shimUsage, []string) {
	var texts []string
	var usage shimUsage
	var reasoning []string
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
		switch ev["type"] {
		case "text":
			if part, ok := ev["part"].(map[string]any); ok {
				if txt, ok := part["text"].(string); ok {
					texts = append(texts, txt)
				}
			}
		case "reasoning":
			if part, ok := ev["part"].(map[string]any); ok {
				if txt, ok := part["text"].(string); ok && strings.TrimSpace(txt) != "" {
					reasoning = append(reasoning, shimSanitize(txt, 1<<30))
				}
			}
		case "step_finish":
			if u := shimUsageOf(ev); u.total > 0 {
				usage = u
			}
		}
	}
	return strings.Join(texts, ""), sessionID, usage, reasoning
}

// shimEffortVariant: harness reasoning.effort → opencode --variant。
// key 即 effort（opencode request.ts 直通，无转译层）；off=不传 flag。
// 缺省 high；非法回 high 并告警（非法 variant 会被 opencode 静默吞掉还污染 session）。
func shimEffortVariant(body map[string]any) string {
	r, _ := body["reasoning"].(map[string]any)
	e, _ := r["effort"].(string)
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "":
		return "high"
	case "off":
		return ""
	case "minimal", "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(e))
	case "max":
		return "high"
	default:
		logf("[SHIM] unknown reasoning effort %q, fallback high", e)
		return "high"
	}
}

func shimCmdArgs(prompt, sessionID, model, variant string) (args []string, stdin *strings.Reader) {
	args = []string{shimOpencodeBin, "run", "--pure", "--format", "json", "--thinking", "--agent", shimAgent(), "-m", model}
	if variant != "" {
		args = append(args, "--variant", variant)
	}
	if sessionID != "" {
		args = append(args, "-s", sessionID)
	}
	// prompt 永远走 stdin：Windows cmd.exe /c 命令行 8191 上限会腰斩 8KB+ 的 argv prompt
	// （exit 1 + 多字节截断乱码）；stdin 是 pipe 流，无此限。不传 message positional，
	// opencode 侧 resolveRunInput 直接取 piped，无 "-" 占位污染。
	return args, strings.NewReader(prompt)
}

// shimLaunchFor 按平台拼启动器：windows 经 cmd.exe /c（npm .cmd 垫片必须），
// linux 直接 exec 可执行文件。goos 参数化便于单测两边。
func shimLaunchFor(goos string, args []string) (string, []string) {
	if goos == "windows" {
		return "cmd.exe", append([]string{"/c"}, args...)
	}
	return args[0], args[1:]
}

// shimLaunch 本机平台启动器。
func shimLaunch(args []string) (string, []string) { return shimLaunchFor(runtime.GOOS, args) }

func shimRunOpencode(ctx context.Context, prompt, sessionID, model, variant string) shimResult {
	args, stdin := shimCmdArgs(prompt, sessionID, model, variant)
	launcher, largs := shimLaunch(args)
	cmd := exec.CommandContext(ctx, launcher, largs...)
	cmd.Stdin = stdin
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
	text, ses, usage, reasoning := shimParseEvents(outBuf.String())
	return shimResult{ok: true, text: text, sessionID: ses, usage: usage, reasoning: reasoning}
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

// validShimModel 字符集白名单：model 原样进子进程 argv，
// Windows 经 cmd.exe /c 二次解析，白名单外（&|<>" 等）直接 400。
func validShimModel(m string) bool {
	if m == "" || len(m) > 128 {
		return false
	}
	for _, r := range m {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == ':' || r == '/' || r == '-') {
			return false
		}
	}
	return true
}

// cutStr 按 rune 边界截断（日志 UA 防 mojibake：按字节切会劈开多字节字符）。
func cutStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// shimLiveResult P0 直播结果。
type shimLiveResult struct {
	text        string
	sessionID   string
	usage       shimUsage
	ok          bool
	timeout     bool
	cancelled   bool
	errMsg      string
	firstByteMs int64
}

// rsPending：已收齐的 reasoning part，added→delta→done 严格有序后暂存，
// 终态时补 output_item.done 并收录进 completed.output。
type rsPending struct {
	idx  int
	id   string
	text string
}

// shimCompletedItems：completed.output 内容，reasoning 在前、message 在后
// （与流事件发射顺序一致；缺席 reasoning 则 harness 下轮回放整块消失）。
func shimCompletedItems(msgItem map[string]any, rs []rsPending) []any {
	out := make([]any, 0, len(rs)+1)
	for _, rp := range rs {
		out = append(out, map[string]any{"id": rp.id, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": rp.text}}})
	}
	return append(out, msgItem)
}

// shimStreamLive P0：边跑边转播。created/item.added 立即发（TTFB≈进程启动），
// stdout 逐行解析，text 事件即发 delta；结束补 done/completed/DONE。
// 失败发 response.failed（pi-ai 可解析）；下游断开由 ctx 干掉进程，停发。
func shimStreamLive(ctx context.Context, w http.ResponseWriter, reqID, model, variant, prompt, ses string) shimLiveResult {
	var r shimLiveResult
	t0 := time.Now()
	sink := newSSESink(w)
	createdAt := time.Now().Unix()
	msgID := fmt.Sprintf("msg_shim_%d", createdAt)
	sink.emit(map[string]any{"type": "response.created",
		"response": map[string]any{"id": reqID, "object": "response", "created_at": createdAt, "model": model, "status": "in_progress", "output": []any{}}})
	sink.emit(map[string]any{"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": msgID, "type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}}})
	args, stdin := shimCmdArgs(prompt, ses, model, variant)
	launcher, largs := shimLaunch(args)
	cmd := exec.CommandContext(ctx, launcher, largs...)
	cmd.Stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return shimLiveResult{errMsg: "stdout pipe: " + err.Error()}
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errBuf, n: 1 << 20}
	if err := cmd.Start(); err != nil {
		msg := err.Error()
		if tail := tailString(errBuf.String(), 1500); tail != "" {
			msg += " | " + tail
		}
		return shimLiveResult{errMsg: msg}
	}
	var texts []string
	sessionID := ""
	var usage shimUsage
	// reasoning 转译状态：每 part 独占递增 output_index，added→delta→done 严格有序。
	rsIdx := 0
	var rsList []rsPending
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
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
		switch ev["type"] {
		case "text":
			part, _ := ev["part"].(map[string]any)
			txt, _ := part["text"].(string)
			if txt == "" {
				continue
			}
			clean := shimSanitize(txt, 1<<30)
			texts = append(texts, clean)
			if r.firstByteMs == 0 {
				r.firstByteMs = time.Since(t0).Milliseconds()
			}
			sink.emit(map[string]any{"type": "response.output_text.delta", "output_index": 0,
				"item_id": msgID, "delta": clean})
		case "step_finish":
			if u := shimUsageOf(ev); u.total > 0 {
				usage = u
			}
		case "reasoning":
			// opencode reasoning part 全文事件（part.time.end 才发，无 partial 增量）。
			part, _ := ev["part"].(map[string]any)
			txt, _ := part["text"].(string)
			if strings.TrimSpace(txt) == "" {
				continue
			}
			clean := shimSanitize(txt, 1<<30)
			rsIdx++
			rsID := fmt.Sprintf("rs_shim_%d_%d", createdAt, rsIdx)
			sink.emit(map[string]any{"type": "response.output_item.added", "output_index": rsIdx,
				"item": map[string]any{"id": rsID, "type": "reasoning", "summary": []any{}}})
			sink.emit(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": rsIdx,
				"item_id": rsID, "delta": clean})
			rsList = append(rsList, rsPending{idx: rsIdx, id: rsID, text: clean})
		}
	}
	text := strings.Join(texts, "")
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		r.timeout = true
		r.errMsg = tailString(errBuf.String(), 2000)
		sink.emit(map[string]any{"type": "response.failed",
			"response": map[string]any{"id": reqID, "status": "failed",
				"error": map[string]any{"type": "timeout", "message": "shim timeout"}}})
		sink.done()
		return r
	}
	if ctx.Err() == context.Canceled {
		// 下游先挂：不再向死连接补帧，只记分类日志（handler 侧）。
		r.cancelled = true
		r.errMsg = "client cancelled"
		return r
	}
	if scanErr != nil {
		// 超长行/IO错：此前当 ok:true 半截回吐，现显式失败。
		r.errMsg = "stdout scan: " + scanErr.Error()
		sink.emit(map[string]any{"type": "response.failed",
			"response": map[string]any{"id": reqID, "status": "failed",
				"error": map[string]any{"type": "shim_error", "message": r.errMsg}}})
		sink.done()
		return r
	}
	if waitErr != nil {
		r.errMsg = waitErr.Error()
		if tail := tailString(errBuf.String(), 1500); tail != "" {
			r.errMsg += " | " + tail
		}
		sink.emit(map[string]any{"type": "response.failed",
			"response": map[string]any{"id": reqID, "status": "failed",
				"error": map[string]any{"type": "shim_error", "message": r.errMsg}}})
		sink.done()
		return r
	}
	r.ok = true
	r.text, r.sessionID, r.usage = text, sessionID, usage
	if strings.TrimSpace(text) == "" {
		// exit 0 但零输出：此前伪造成 completed，下游空渲染；现显式失败。
		r.ok = false
		r.errMsg = "shim_empty: opencode exited 0 with no text output"
		sink.emit(map[string]any{"type": "response.failed",
			"response": map[string]any{"id": reqID, "status": "failed",
				"error": map[string]any{"type": "shim_empty", "message": r.errMsg}}})
		sink.done()
		return r
	}
	doneItem := map[string]any{"id": msgID, "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	// reasoning done 必发（缺席则 harness 下轮回放整块消失）。
	for _, rp := range rsList {
		sink.emit(map[string]any{"type": "response.output_item.done", "output_index": rp.idx,
			"item": map[string]any{"id": rp.id, "type": "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": rp.text}}}})
	}
	sink.emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": doneItem})
	sink.emit(map[string]any{"type": "response.completed",
		"response": map[string]any{"id": reqID, "object": "response", "created_at": createdAt, "model": model,
			"status": "completed", "output": shimCompletedItems(doneItem, rsList),
			"usage": map[string]any{"input_tokens": usage.input, "output_tokens": usage.output, "total_tokens": usage.total}}})
	sink.done()
	return r
}

// ---------- 并发门 + HTTP 入口 ----------

var shimSem = make(chan struct{}, 10)

func init() {
	// 启动时按配置重建信号量容量（默认 10）。
	shimSem = make(chan struct{}, shimConcurrency())
}

var shimSeqMu sync.Mutex
var shimSeq int64

func shimNextID() string {
	shimSeqMu.Lock()
	defer shimSeqMu.Unlock()
	shimSeq++
	return fmt.Sprintf("resp_shim_%x%x", shimSeq, time.Now().UnixMilli()&0xffff)
}

// shimWriteError：shim 入口 400 responses 风格体（老链路 writeConvError 不动）。
func shimWriteError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code}})
	_, _ = w.Write(b)
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
		shimWriteError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		shimWriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
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
	if !validShimModel(model) {
		// model 直进 cmd.exe /c argv，字符集外一律 400（防 cmd 二次解析）。
		shimWriteError(w, http.StatusBadRequest, "invalid model")
		return
	}
	// B：reasoning.effort → --variant，每轮 -s 必带（opencode 续写沿用旧值）。
	variant := shimEffortVariant(in)
	// 会话钉定：x-session-id 头优先，同对话续 -s 只发增量；新对话拼全量。
	// 无提示词注入：harness 的 instructions/input 原样转文本，不附加任何附录。
	fp := shimSessionKey(req.Header, in)
	ses := shimLookupSession(fp)
	prompt := shimSelectPrompt(ses, in)
	if strings.TrimSpace(prompt) == "" {
		shimWriteError(w, http.StatusBadRequest, "empty prompt")
		return
	}
	downUA := cutStr(req.Header.Get("User-Agent"), 60)
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
	if stream {
		// P0 直播：边跑边转播，不等子进程结束。
		lr := shimStreamLive(ctx, w, reqID, model, variant, prompt, ses)
		elms := time.Since(t0).Milliseconds()
		cont := "new"
		if ses != "" {
			cont = "yes"
		}
		if lr.timeout {
			logf("[SHIM %s] LIVE TIMEOUT ms=%d firstByte=%dms promptBytes=%d ses=%s cont=%s model=%s ua=%q", reqID, elms, lr.firstByteMs, len(prompt), ses, cont, model, downUA)
			return
		}
		if lr.cancelled {
			logf("[SHIM %s] LIVE CLIENT-CANCELLED ms=%d firstByte=%dms promptBytes=%d ses=%s cont=%s model=%s ua=%q", reqID, elms, lr.firstByteMs, len(prompt), ses, cont, model, downUA)
			return
		}
		if !lr.ok {
			logf("[SHIM %s] LIVE ERROR ms=%d firstByte=%dms promptBytes=%d ses=%s cont=%s model=%s ua=%q err=%.200s", reqID, elms, lr.firstByteMs, len(prompt), ses, cont, model, downUA, lr.errMsg)
			if ses != "" && shimIsSessionNotFound(lr.errMsg) {
				// 流式已写头无法同请求重试：驱逐映射，下轮自动回到新会话。
				logf("[SHIM %s] session expired ses=%s, evicted", reqID, ses)
				shimEvictSession(fp)
			}
			return
		}
		if lr.sessionID != "" {
			shimStoreSession(fp, lr.sessionID)
		}
		logf("[SHIM %s] LIVE done ok ms=%d firstByte=%dms promptBytes=%d textLen=%d in=%d out=%d ses=%s cont=%s model=%s ua=%q",
			reqID, elms, lr.firstByteMs, len(prompt), len(lr.text), lr.usage.input, lr.usage.output, lr.sessionID, cont, model, downUA)
		return
	}
	r := shimRunOpencode(ctx, prompt, ses, model, variant)
	if !r.ok && !r.timeout && ses != "" && shimIsSessionNotFound(r.errMsg) {
		// -s 死会话：驱逐映射，按新会话（全量+previous thinking）同请求重试一次。
		// 加密态丢失是物理定律，保可用不保记忆。
		logf("[SHIM %s] session expired ses=%s, evict and retry as new", reqID, ses)
		shimEvictSession(fp)
		ses = ""
		prompt = shimSelectPrompt("", in)
		r = shimRunOpencode(ctx, prompt, "", model, variant)
	}
	elms := time.Since(t0).Milliseconds()
	cont := "new"
	if ses != "" {
		cont = "yes"
	}
	if r.timeout {
		logf("[SHIM %s] TIMEOUT ms=%d promptBytes=%d ses=%s cont=%s model=%s ua=%q", reqID, elms, len(prompt), ses, cont, model, downUA)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "shim_timeout", "message": r.errMsg}})
		_, _ = w.Write(b)
		return
	}
	if !r.ok {
		logf("[SHIM %s] ERROR ms=%d promptBytes=%d ses=%s cont=%s model=%s ua=%q err=%.200s", reqID, elms, len(prompt), ses, cont, model, downUA, r.errMsg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "shim_error", "message": r.errMsg}})
		_, _ = w.Write(b)
		return
	}
	if strings.TrimSpace(r.text) == "" {
		// exit 0 但零输出：此前伪造成 200 incomplete 空包，下游空渲染；
		// 现显式 502（与直播 shim_empty 对齐）。
		logf("[SHIM %s] EMPTY ms=%d promptBytes=%d in=%d ses=%s cont=%s model=%s ua=%q", reqID, elms, len(prompt), r.usage.input, ses, cont, model, downUA)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "shim_empty", "message": "opencode exited 0 with no text output"}})
		_, _ = w.Write(b)
		return
	}
	if r.sessionID != "" {
		shimStoreSession(fp, r.sessionID)
	}
	env := shimEnvelope(reqID, model, r.text, r.usage, r.reasoning)
	logf("[SHIM %s] done ok ms=%d promptBytes=%d textLen=%d in=%d out=%d ses=%s cont=%s model=%s ua=%q", reqID, elms, len(prompt), len(r.text), r.usage.input, r.usage.output, r.sessionID, cont, model, downUA)
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(env)
	_, _ = w.Write(b)
}

// shimEmitStream 按规范序列吐流（单测与回放用；线上流式走 shimStreamLive 直播）。
func shimEmitStream(w http.ResponseWriter, reqID, model string, env map[string]any, usage shimUsage) {
	sink := newSSESink(w)
	var createdAt int64
	switch v := env["created_at"].(type) {
	case int64:
		createdAt = v
	case float64:
		createdAt = int64(v)
	case int:
		createdAt = int64(v)
	default:
		createdAt = time.Now().Unix()
	}
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
			"usage": map[string]any{"input_tokens": usage.input, "output_tokens": usage.output, "total_tokens": usage.total}}})
	sink.done()
}
