// shim_test.go: v0.3.12 harness 旁路单测，纯离线（不起子进程）。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// system/user/function_call_output 混排 → prompt 文本。
func TestShimInputToPrompt(t *testing.T) {
	body := map[string]any{
		"instructions": "sys-prompt",
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hello"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "output_text": "hi"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "function_call_output", "output": map[string]any{"ok": true}},
			}},
		},
	}
	p := shimInputToPrompt(body)
	if !strings.HasPrefix(p, "sys-prompt") {
		t.Fatalf("system 不在前：%q", p)
	}
	if !strings.Contains(p, "user: hello") {
		t.Errorf("user 文本丢失：%q", p)
	}
	if !strings.Contains(p, "[tool result]") {
		t.Errorf("tool 结果未转文字：%q", p)
	}
	if !strings.Contains(p, "assistant: hi") {
		t.Errorf("history 丢失：%q", p)
	}
}

// 脏串（ANSI/裸控制/中文/反斜杠）清洗后必须可 json 序列化且无裸控制字符。
func TestShimSanitize(t *testing.T) {
	dirty := "hi\x1b[7mBOLD\x1b[0m mid\x1b]OSC\x07 end\x01\x7f 中文 F:\\worker\\a.json\nline2\tok"
	clean := shimSanitize(dirty, 24000)
	if strings.Contains(clean, "\x1b") || strings.Contains(clean, "\x01") || strings.Contains(clean, "\x7f") {
		t.Fatalf("控制字符残留：%q", clean)
	}
	if !strings.Contains(clean, "中文") || !strings.Contains(clean, "F:") {
		t.Errorf("正常内容被误删：%q", clean)
	}
	b, err := json.Marshal(map[string]any{"text": clean})
	if err != nil {
		t.Fatalf("Marshal 失败：%v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("回解析失败：%v", err)
	}
	// 截断生效。
	long := strings.Repeat("x", 30000)
	if c := shimSanitize(long, 24000); !strings.HasSuffix(c, "...[truncated]") || len(c) >= 30000 {
		t.Errorf("截断异常：len=%d", len(c))
	}
}

// 同对话同 fp，首轮不同则异。
func TestShimFingerprint(t *testing.T) {
	mk := func(first string) map[string]any {
		return map[string]any{
			"instructions": "sys",
			"input":        []any{map[string]any{"role": "user", "content": first}},
		}
	}
	a1 := mk("hello")
	a2 := mk("hello")
	b := mk("world")
	if shimFingerprint(a1) != shimFingerprint(a2) {
		t.Errorf("同对话 fp 不一致")
	}
	if shimFingerprint(a1) == shimFingerprint(b) {
		t.Errorf("异对话 fp 碰撞")
	}
	// 续会话只取最后一轮 user。
	multi := map[string]any{"input": []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "ack"},
		map[string]any{"role": "user", "content": "second"},
	}}
	if got := shimLastUserText(multi); got != "second" {
		t.Errorf("增量取错：%q", got)
	}
}

// 空文本 → incomplete；有文本 → completed 且 output 结构对。
func TestShimEnvelope(t *testing.T) {
	e0 := shimEnvelope("resp_1", "m", "", shimUsage{})
	if e0["status"] != "incomplete" {
		t.Errorf("空文本应 incomplete：%v", e0["status"])
	}
	e1 := shimEnvelope("resp_2", "m", "hello", shimUsage{input: 10, output: 5, total: 15})
	if e1["status"] != "completed" {
		t.Errorf("有文本应 completed：%v", e1["status"])
	}
	out, _ := e1["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output 项数错误：%d", len(out))
	}
	msg, _ := out[0].(map[string]any)
	if msg["type"] != "message" {
		t.Errorf("类型错误：%v", msg["type"])
	}
	if _, err := json.Marshal(e1); err != nil {
		t.Fatalf("envelope 不可序列化：%v", err)
	}
}

// 本机必须能定位 opencode（PATH 或 APPDATA 回退），否则旁路 502。
func TestShimFindOpencode(t *testing.T) {
	bin, err := shimFindOpencode()
	if err != nil {
		t.Fatalf("本机找不到 opencode：%v", err)
	}
	if bin == "" {
		t.Fatalf("返回空路径")
	}
	t.Logf("opencode: %s", bin)
}

// 路由判定：官方 UA 不进 shim，外来 UA 进 shim。
func TestShimBypassRouting(t *testing.T) {	official := []string{
		"opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14",
		"opencode/1.18.30",
	}
	for _, ua := range official {
		if !isOfficialUA(ua) {
			t.Errorf("官方 UA 被判旁路：%q", ua)
		}
	}
	foreign := []string{
		"deepseek-harness/0.1.6-alpha.1 (+https://github.com/deepseek",
		"curl/8.0",
		"",
	}
	for _, ua := range foreign {
		if isOfficialUA(ua) {
			t.Errorf("外来 UA 被判老链路：%q", ua)
		}
	}
}

// Stream sequence must follow the canonical responses order so pi-ai can
// parse it: created -> item.added -> delta* -> item.done -> completed -> DONE.
// Delta chunks must reassemble to the original text.
func TestShimEmitStreamSequence(t *testing.T) {
 longText := strings.Repeat("ab", 600) + "end"
 env := shimEnvelope("resp_seq1", "m", longText, shimUsage{})
 rec := httptest.NewRecorder()
 shimEmitStream(rec, "resp_seq1", "m", env, shimUsage{})
 body := rec.Body.String()
 if !strings.Contains(body, "data: [DONE]") {
 t.Fatalf("missing [DONE] tail")
 }
 order := []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"}
 last := -1
 for _, ev := range order {
 pos := strings.Index(body, ev)
 if pos < 0 {
 t.Fatalf("missing event %s", ev)
 }
 if pos < last {
 t.Fatalf("event out of order: %s", ev)
 }
 last = pos
 }
 var rebuilt strings.Builder
 for _, line := range strings.Split(body, "\n") {
 line = strings.TrimSpace(line)
 if !strings.HasPrefix(line, "data: ") {
 continue
 }
 payload := strings.TrimPrefix(line, "data: ")
 if payload == "[DONE]" {
 continue
 }
 var m map[string]any
 if err := json.Unmarshal([]byte(payload), &m); err != nil {
 t.Fatalf("event not json: %v", err)
 }
 if m["type"] == "response.output_text.delta" {
 if d, ok := m["delta"].(string); ok {
 rebuilt.WriteString(d)
 }
 }
 }
 if rebuilt.String() != longText {
 t.Fatalf("delta reassembly mismatch: got %d runes, want %d", len([]rune(rebuilt.String())), len([]rune(longText)))
 }
}

// Plan-mode identity sentences are stripped, substance kept; fail-open on
// all-identity input; identity-free input untouched.
// Agent defaults to bypass, SHIM_AGENT overrides.
func TestShimAgentDefault(t *testing.T) {
 os.Unsetenv("SHIM_AGENT")
 if got := shimAgent(); got != "bypass" {
 t.Fatalf("default agent should be bypass, got %q", got)
 }
 t.Setenv("SHIM_AGENT", "plan")
 if got := shimAgent(); got != "plan" {
 t.Fatalf("SHIM_AGENT override failed, got %q", got)
 }
}

// step_finish tokens map to responses usage fields.
func TestShimUsageOf(t *testing.T) {
 ev := map[string]any{"part": map[string]any{"tokens": map[string]any{
 "total": float64(100), "input": float64(80), "output": float64(20),
 }}}
 u := shimUsageOf(ev)
 if u.input != 80 || u.output != 20 || u.total != 100 {
 t.Fatalf("usage mapping wrong: %+v", u)
 }
 if got := shimUsageOf(map[string]any{}); got.total != 0 {
 t.Fatalf("empty event should give zero usage: %+v", got)
 }
}

// prompt永远走stdin：短/中/长均stdin非空、argv不含prompt内容。
func TestShimCmdArgsAlwaysStdin(t *testing.T) {
 prompts := map[string]string{
 "short": "hi",
 "mid": strings.Repeat("ab", 5000),
 "long": strings.Repeat("ab", 20000),
 }
 for name, p := range prompts {
	args, stdin := shimCmdArgs(p, "", "test/m", "high")
 if stdin == nil {
 t.Fatalf("%s: stdin must never be nil", name)
 }
 var sb strings.Builder
 if _, err := io.Copy(&sb, stdin); err != nil || sb.String() != p {
 t.Fatalf("%s: stdin content mismatch", name)
 }
 for _, a := range args {
 if a == p {
 t.Fatalf("%s: prompt leaked into argv", name)
 }
 }
 }
}

// Header session wins: same body + different x-session-id => different keys;
// same header + different body => same key (conversation identity, not content).
func TestShimSessionKeyHeader(t *testing.T) {
 body := map[string]any{"instructions": "sys", "input": []any{map[string]any{"role": "user", "content": "hi"}}}
 h1 := http.Header{"X-Session-Id": []string{"ses-win-A"}}
 h2 := http.Header{"X-Session-Id": []string{"ses-win-B"}}
 k1 := shimSessionKey(h1, body)
 k2 := shimSessionKey(h2, body)
 if k1 == k2 {
 t.Fatalf("different windows must not share a key: %q", k1)
 }
 other := map[string]any{"instructions": "sys2", "input": []any{map[string]any{"role": "user", "content": "other"}}}
 if k3 := shimSessionKey(h1, other); k3 != k1 {
 t.Fatalf("same window must keep its key: %q vs %q", k3, k1)
 }
}

// Old truncation bug: same 200-byte prefix + same 100-byte instructions
// prefix must NOT collide anymore (full hash).
func TestShimSessionKeyNoPrefixCollision(t *testing.T) {
 ins := strings.Repeat("s", 100) + "-TAIL-A"
 ins2 := strings.Repeat("s", 100) + "-TAIL-B"
 mk := func(first, instructions string) map[string]any {
 return map[string]any{"instructions": instructions,
 "input": []any{map[string]any{"role": "user", "content": first}}}
 }
 a := mk(strings.Repeat("x", 200)+"-A", ins)
 b := mk(strings.Repeat("x", 200)+"-A", ins2)
 if shimSessionKey(http.Header{}, a) == shimSessionKey(http.Header{}, b) {
 t.Fatalf("same-prefix bodies must diverge under full hash")
 }
}

// LRU: hit touches, eviction drops the least-recently-used.
func TestShimSessionLRU(t *testing.T) {
 shimSesMu.Lock()
 shimSes = map[string]*shimSesEntry{}
 shimSesQ = nil
 shimSesMu.Unlock()
 for i := 0; i < 64; i++ {
 shimStoreSession("k-"+string(rune('a'+i%26))+string(rune('0'+i/26)), "ses")
 }
 shimLookupSession("k-a0")
 shimStoreSession("k-new", "ses-new")
 if got := shimLookupSession("k-a0"); got == "" {
 t.Fatalf("recently used key must survive eviction")
 }
 if got := shimLookupSession("k-b0"); got != "" {
 t.Fatalf("least-recently-used key must be evicted, got %q", got)
 }
 shimSesMu.Lock()
 shimSes = map[string]*shimSesEntry{}
 shimSesQ = nil
 shimSesMu.Unlock()
}

// Model whitelist: normal names pass, cmd metachars rejected.
func TestValidShimModel(t *testing.T) {
 for _, m := range []string{"test/muse-spark-1.3-contributor-free", "openai/gpt-4o", "a.b_c:d-e/f1"} {
 if !validShimModel(m) {
 t.Errorf("legit model rejected: %q", m)
 }
 }
 for _, m := range []string{"", "a&b", "a|b", "a;b", "a b", "a$b", "`id`", "$(x)", "a\n", strings.Repeat("a", 129)} {
 if validShimModel(m) {
 t.Errorf("dangerous model accepted: %q", m)
 }
 }
}

// cutStr must not split multibyte runes.
func TestCutStrRuneSafe(t *testing.T) {
 s := "abc中文def"
 if got := cutStr(s, 5); got != "abc中文" {
 t.Fatalf("rune cut wrong: %q", got)
 }
 if got := cutStr(s, 100); got != s {
 t.Fatalf("short string altered: %q", got)
 }
}

// Store on existing key must LRU-touch (hot sessions survive eviction).
func TestShimStoreTouch(t *testing.T) {
 shimSesMu.Lock()
 shimSes = map[string]*shimSesEntry{}
 shimSesQ = nil
 shimSesMu.Unlock()
 for i := 0; i < 64; i++ {
 shimStoreSession("s-"+string(rune('a'+i%26))+string(rune('0'+i/26)), "ses")
 }
 shimStoreSession("s-a0", "ses-new")
 shimStoreSession("s-fresh", "ses-fresh")
 if got := shimLookupSession("s-a0"); got != "ses-new" {
 t.Fatalf("stored value lost: %q", got)
 }
 if got := shimLookupSession("s-b0"); got != "" {
 t.Fatalf("LRU victim wrong, s-b0 should be evicted, got %q", got)
 }
 shimSesMu.Lock()
 shimSes = map[string]*shimSesEntry{}
 shimSesQ = nil
 shimSesMu.Unlock()
}

// Concurrency defaults to 10, env overrides within 1..32.
func TestShimConcurrencyDefault(t *testing.T) {
 os.Unsetenv("SHIM_CONCURRENCY")
 if got := shimConcurrency(); got != 10 {
 t.Fatalf("default concurrency should be 10, got %d", got)
 }
 t.Setenv("SHIM_CONCURRENCY", "3")
 if got := shimConcurrency(); got != 3 {
 t.Fatalf("env override failed, got %d", got)
 }
 t.Setenv("SHIM_CONCURRENCY", "99")
 if got := shimConcurrency(); got != 10 {
 t.Fatalf("out-of-range must fall back to 10, got %d", got)
 }
 if cap(shimSem) != 10 {
 t.Fatalf("semaphore capacity should be 10, got %d", cap(shimSem))
 }
}

// Effort mapping: off omits, minimal kept, max->high, missing->high, illegal->high.
func TestShimEffortVariant(t *testing.T) {
 cases := map[string]string{
 "": "high", "off": "", "minimal": "minimal", "low": "low",
 "medium": "medium", "high": "high", "xhigh": "xhigh",
 "max": "high", "bogus": "high", "OFF": "",
 }
 for in, want := range cases {
 body := map[string]any{"reasoning": map[string]any{"effort": in}}
 if got := shimEffortVariant(body); got != want {
 t.Errorf("effort %q: got %q want %q", in, got, want)
 }
 }
 if got := shimEffortVariant(map[string]any{}); got != "high" {
 t.Errorf("missing reasoning should default high, got %q", got)
 }
}

// Variant flag position: present when set, absent when off; prompt stays in stdin.
func TestShimCmdArgsVariant(t *testing.T) {
 args, _ := shimCmdArgs("hi", "", "test/m", "low")
 found := false
 for i, a := range args {
 if a == "--variant" && i+1 < len(args) && args[i+1] == "low" {
 found = true
 }
 if a == "hi" {
 t.Fatalf("prompt leaked into argv")
 }
 }
 if !found {
 t.Fatalf("--variant low missing in %q", args)
 }
 args2, _ := shimCmdArgs("hi", "", "test/m", "")
 for _, a := range args2 {
 if a == "--variant" {
 t.Fatalf("--variant must be omitted when empty")
 }
 }
}

// Non-stream parser ignores reasoning events: text/usage/session intact.
func TestShimParseIgnoresReasoning(t *testing.T) {
 var buf strings.Builder
 for _, line := range []string{
 `{"type":"step_start","sessionID":"ses_x"}`,
 `{"type":"reasoning","sessionID":"ses_x","part":{"type":"reasoning","text":"think-one"}}`,
 `{"type":"text","sessionID":"ses_x","part":{"type":"text","text":"answer"}}`,
 `{"type":"reasoning","sessionID":"ses_x","part":{"type":"reasoning","text":"think-two"}}`,
 `{"type":"step_finish","sessionID":"ses_x","part":{"tokens":{"total":10,"input":8,"output":2}}}`,
 } {
 buf.WriteString(line + "\n")
 }
 text, ses, usage := shimParseEvents(buf.String())
 if text != "answer" {
 t.Fatalf("text polluted by reasoning: %q", text)
 }
  if ses != "ses_x" || usage.total != 10 {
  t.Fatalf("session/usage lost: %q %+v", ses, usage)
  }
}

// Replayed reasoning items (no role, summary array) must be extractable as
// visible prior-thinking text for new-session fallback.
func TestShimReasoningSummaries(t *testing.T) {
  body := map[string]any{"input": []any{
    map[string]any{"type": "reasoning", "id": "rs_shim_1_1",
      "summary": []any{map[string]any{"type": "summary_text", "text": "prior thought"}}},
    map[string]any{"role": "user", "content": "next?"},
  }}
  got := shimReasoningSummaries(body)
  if len(got) != 1 || got[0] != "prior thought" {
    t.Fatalf("reasoning summary 丢失：%q", got)
  }
}

// 新会话回退必须带上回来的 previous thinking；续会话走增量，不带。
func TestShimSelectPrompt(t *testing.T) {
  body := map[string]any{
    "instructions": "sys",
    "input": []any{
      map[string]any{"role": "user", "content": "first?"},
      map[string]any{"type": "reasoning", "id": "rs_shim_9_1",
        "summary": []any{map[string]any{"type": "summary_text", "text": "prior thought"}}},
      map[string]any{"role": "user", "content": "second?"},
    },
  }
  pNew := shimSelectPrompt("", body)
  if !strings.Contains(pNew, "prior thought") || !strings.Contains(pNew, "[previous thinking]") {
    t.Fatalf("新会话丢了 previous thinking：%q", pNew)
  }
  pCont := shimSelectPrompt("ses_old", body)
  if strings.Contains(pCont, "prior thought") || strings.Contains(pCont, "[previous thinking]") {
    t.Fatalf("续会话不应重发旧 thinking：%q", pCont)
  }
  if pCont != "second?" {
    t.Fatalf("续会话增量取错：%q", pCont)
  }
}

// completed.output 必须收录 reasoning items（缺席则 harness 下轮回放整块消失）。
func TestShimCompletedIncludesReasoning(t *testing.T) {
  msg := map[string]any{"id": "msg_x", "type": "message"}
  rs := []rsPending{{idx: 1, id: "rs_shim_1_1", text: "thought"}}
  out := shimCompletedItems(msg, rs)
  if len(out) != 2 {
    t.Fatalf("output 项数错误：%d", len(out))
  }
  first, _ := out[0].(map[string]any)
  if first["type"] != "reasoning" || first["id"] != "rs_shim_1_1" {
    t.Fatalf("首项应为 reasoning：%v", out[0])
  }
  sum, _ := first["summary"].([]any)
  if len(sum) != 1 {
    t.Fatalf("reasoning 缺 summary：%v", first)
  }
}
