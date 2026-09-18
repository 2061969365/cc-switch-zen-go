// cc-switch-zen-go: OpenCode Zen 匿名免费层 headless 网关（Go 版）。
//
// 只做最小透明转发 + 匿名鉴权头，等价于 Rust 版补丁后的行为：
//   - 上游 Authorization: Bearer public（无 key 时官方客户端行为）
//   - 注入 x-opencode-client/session/request/project + UA opencode/1.18.18
//
// 缺了这组头上游报 MissingSessionID（已实测）。
//
// 路由：
//
//	GET  /v1                      -> 404（与 OpenAI 规范一致）
//	GET  /v1/models               -> 上游 /models
//	POST /v1/chat/completions     -> 上游 /chat/completions（实测 200）
//	POST /v1/messages             -> 上游 /messages（匿名上游 500，非本网关问题）
//	POST /v1/responses            -> 上游 /responses（实测过鉴权）
//
//	GET  /health                -> 200 {"status":"ok"}（容器/Railway 健康检查）
//	POST /v1/chat/completions|messages|responses
//	                              -> 按模型查表：格式一致透传，不一致自动转换，响应逆转
//	POST /conv/v1/...            -> 兼容别名，同上
//	GET  /conv/v1/models          -> 上游 /models（透传）
//
// 环境变量：
//
//	PORT                  监听端口，默认 8080
//	OPENCODE_ZEN_BASE     上游，默认 https://opencode.ai/zen/v1
//	OPENCODE_ZEN_API_KEY  默认 public（匿名免费层；有真 key 可覆盖）
//	OPENCODE_ZEN_PROJECT  默认 default
//	ZEN_DEBUG             设为 1 时打印逐请求调试日志（默认关闭，避免刷屏）
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const zenUA = "opencode/1.18.31"

// P0-1：上游共享连接池+超时。http.DefaultClient 无限等，上游 hang 住会拖死网关。
// zenClient 用于非流式（总超时 600s）；zenStreamClient 用于 SSE/透传（长连接，无总超时，
// 靠请求 ctx + 包体空闲超时兜底，见 AfterFunc 与 idleTimeoutBody）。
// v0.3.11 并发隔离：空闲池 20→100（长流占连接是常态，小池加剧排队），
// 总并发上限 200（防 fd/conntrack 爆，超限请求快速失败而非无限排队）。
var zenTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment, // 认 HTTP(S)_PROXY/NO_PROXY
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   100,
	MaxConnsPerHost:       200,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   30 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
}

var zenClient = &http.Client{Transport: zenTransport, Timeout: 600 * time.Second}

var zenStreamClient = &http.Client{Transport: zenTransport}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// debugOn 是否输出逐请求调试日志（默认关）。开着刷屏会拖慢 Windows 控制台。
func debugOn() bool {
	v := os.Getenv("ZEN_DEBUG")
	return v == "1" || strings.EqualFold(v, "true")
}

// newID 生成带前缀的随机 ID（resp_/msg_/chatcmpl_ 等响应包络用）。
func newID(prefix string) string { return prefix + randHex(12) }

// inheritClientHeaders 把入站客户端头透传到上游请求（最小验证用）。
// conv 路径用 http.NewRequest 新建 fwd，原 setZenHeaders 的 setDefault
// “客户端自带则保留”永不生效——此处先拷贝，setZenHeaders 再补缺。
func inheritClientHeaders(dst, src http.Header) {
	for _, k := range []string{"X-Opencode-Client", "X-Opencode-Session", "X-Opencode-Request", "X-Opencode-Project"} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
	if ua := src.Get("User-Agent"); ua != "" {
		// v0.3.10：上游 FreeTier 连 UA 一起验（非 opencode/ 前缀必 403，实测）。
		// 只有官方 UA 才透传，否则保留 setZenHeaders 的 zenUA。
		if strings.HasPrefix(ua, "opencode/") {
			dst.Set("User-Agent", ua)
		}
	}
	// v0.3.8：真会话透传。test 等非 opencode provider 的下游不带 x-opencode-*，
	// 只带 X-Session-Id / x-session-affinity（opencode request.ts else 分支）。
	// 上游 FreeTier 只认真实会话（现编 ses_ 必 403，实测），故有真值时覆盖
	// setZenHeaders 的现编值；缺失时保留现编值（防 MissingSessionID）。
	if src.Get("X-Opencode-Session") == "" {
		if sid := src.Get("X-Session-Id"); sid != "" {
			dst.Set("X-Opencode-Session", sid)
		} else if sid := src.Get("x-session-affinity"); sid != "" {
			dst.Set("X-Opencode-Session", sid)
		}
	}
}

// setZenHeaders 注入上游匿名鉴权头：Bearer + x-opencode 四件套 + UA。
// 缺了这组头上游报 MissingSessionID（已实测）。
func setZenHeaders(h http.Header, apiKey, project string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("X-Goog-Api-Key")

	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", zenUA)
	// 客户端自带则保留（官方 opencode 直连本网关的场景）。
	// v0.3.10：回退签发必须结构合法（mintID），旧 ses_+32hex 必吃上游 403。
	setDefault(h, "X-Opencode-Client", "opencode")
	setDefault(h, "X-Opencode-Session", mintID("ses"))
	setDefault(h, "X-Opencode-Request", mintID("msg"))
	setDefault(h, "X-Opencode-Project", project)
}
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))[:2*n]
	}
	return hex.EncodeToString(b)
}

func main() {
	zenBase, err := url.Parse(env("OPENCODE_ZEN_BASE", "https://opencode.ai/zen/v1"))
	if err != nil {
		log.Fatalf("OPENCODE_ZEN_BASE 非法: %v", err)
	}
	apiKey := env("OPENCODE_ZEN_API_KEY", "public")
	project := env("OPENCODE_ZEN_PROJECT", "default")

	// 入站路径 -> 上游路径
	routes := map[string]string{
		"/v1/models":           "/models",
		"/v1/chat/completions": "/chat/completions",
		"/v1/messages":         "/messages",
		"/v1/responses":        "/responses",
	}

	proxy := &httputil.ReverseProxy{
		// SSE 流式必需：禁用缓冲，逐块刷给客户端。
		FlushInterval: -1,
		// P0-1：透传可能也是 SSE，用无总超时的共享传输层。
		Transport: zenTransport,
		Director: func(req *http.Request) {
			upstreamPath, ok := routes[req.URL.Path]
			if !ok {
				return
			}
			req.URL.Scheme = zenBase.Scheme
			req.URL.Host = zenBase.Host
			req.URL.Path = singleJoin(zenBase.Path, upstreamPath)
			req.Host = zenBase.Host

			// 丢掉客户端带来的鉴权头，统一用网关的匿名身份。
			setZenHeaders(req.Header, apiKey, project)
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logf("上游错误 %s %s: %v", req.Method, req.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream unreachable"}`))
		},
		ModifyResponse: func(resp *http.Response) error {
			logf("%s %s -> 上游 %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
	}

	mux := http.NewServeMux()
	// /health：容器/Railway 健康检查用，固定 200。
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","shim":"v3"}`))
	})
	// /conv/* 兼容别名：与 /v1 同逻辑（模型感知自动转换）。
	mux.HandleFunc("/conv/", func(w http.ResponseWriter, req *http.Request) {
		convHandler(w, req, zenBase, apiKey, project)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		// 三个 POST 入口直接按模型自动转换（未知模型透传，保持旧行为）。
		// v0.3.12：非官方 UA 的 /v1/responses 进 shim 旁路（opencode run 借身份），
		// harness 零改动；官方 UA 走老链路，本体零影响。
		if req.Method == http.MethodPost && req.URL.Path == "/v1/responses" && shimEnabled() && !isOfficialUA(req.Header.Get("User-Agent")) {
			shimHandler(w, req)
			return
		}
		if req.Method == http.MethodPost && convInputFormat(req.URL.Path) != "" {
			convHandlerInner(w, req, req.URL.Path, zenBase, apiKey, project)
			return
		}
		if _, ok := routes[req.URL.Path]; !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		proxy.ServeHTTP(w, req)
	})

	addr := ":" + env("PORT", "8080")
	logf("zen-go headless 监听 %s，上游 %s", addr, zenBase.String())
	seedBuiltinTable()
	go refreshTableFromOfficial()
	// P0-3：全入口请求体上限 200MB（/conv 内部另有更严的 32MB）。
	// v0.3.11：ReadHeaderTimeout 防慢头占连接；IdleTimeout 只杀空闲 keep-alive，
	// 不影响进行中的 SSE 长流（WriteTimeout 故意不设，设了会砍断正常流）。
	srv := &http.Server{
		Addr:              addr,
		Handler:           http.MaxBytesHandler(mux, 200<<20),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func setDefault(h http.Header, key, value string) {
	if h.Get(key) == "" {
		h.Set(key, value)
	}
}

func singleJoin(base, p string) string {
	if len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if len(p) == 0 || p[0] != '/' {
		p = "/" + p
	}
	return base + p
}

// conv 入口路径 -> 输入格式。
func convInputFormat(path string) Format {
	switch path {
	case "/v1/chat/completions":
		return FmtChat
	case "/v1/messages":
		return FmtMessages
	case "/v1/responses":
		return FmtResponses
	}
	return ""
}

func writeConvError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"error": msg})
	_, _ = w.Write(b)
}

// convHandler：/conv/v1/<入口> 兼容别名，与 /v1 同逻辑。
// 未知模型按输入同义端点透传；gemini 直接 400。
func convHandler(w http.ResponseWriter, req *http.Request, zenBase *url.URL, apiKey, project string) {
	convHandlerInner(w, req, strings.TrimPrefix(req.URL.Path, "/conv"), zenBase, apiKey, project)
}

// convHandlerInner：按模型所需格式转换后发上游，响应逆转。inner 为 /v1/... 内层路径。
func convHandlerInner(w http.ResponseWriter, req *http.Request, inner string, zenBase *url.URL, apiKey, project string) {
	if inner == "/v1/models" && req.Method == http.MethodGet {
		upstream := *zenBase
		upstream.Path = singleJoin(zenBase.Path, "/models")
		// v0.3.11：绑下游 ctx，客户端断开即放上游连接。
		fwd, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstream.String(), nil)
		if err != nil {
			writeConvError(w, http.StatusBadGateway, "upstream unreachable")
			return
		}
		setZenHeaders(fwd.Header, apiKey, project)
		resp, err := zenClient.Do(fwd)
		if err != nil {
			writeConvError(w, http.StatusBadGateway, "upstream unreachable")
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<20))
		return
	}
	inFmt := convInputFormat(inner)
	if inFmt == "" || req.Method != http.MethodPost {
		writeConvError(w, http.StatusNotFound, "not found")
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
	// P1b-5：递归删除 "_" 开头私有字段，防上游 "Extra inputs" 400。
	if f, ok := filterPrivateParams(in).(map[string]any); ok {
		in = f
	}
	// P1b-10：请求级 reqID + 分段计时。流式只量到响应头（io.Copy 会阻塞到流结束）。
	t0 := time.Now()
	reqID := randHex(8)
	var model string
	var stream bool
	var target Format
	var tRead, tConv1, tUp, tDec, tConv2 time.Time
	var upStatus int
	var allowedReasoningIDs []string
	// v0.3.9-debug：记录下游会话（诊断 FreeTier 403 用：上游只认真实会话）。
	// 取 x-opencode-session，无则取 X-Session-Id / x-session-affinity。
	downSes := req.Header.Get("X-Opencode-Session")
	if downSes == "" {
		downSes = req.Header.Get("X-Session-Id")
	}
	if downSes == "" {
		downSes = req.Header.Get("x-session-affinity")
	}
	downUA := req.Header.Get("User-Agent")
	if len(downUA) > 60 {
		downUA = downUA[:60]
	}
	defer func() {
		ms := func(t time.Time) int64 {
			if t.IsZero() {
				return -1
			}
			return t.Sub(t0).Milliseconds()
		}
		logf("[REQ %s] %s model=%s stream=%v in=%s out=%s up=%d ses=%s ua=%q read=%dms conv1=%dms up=%dms dec=%dms conv2=%dms total=%dms",
			reqID, inner, model, stream, inFmt, target, upStatus, downSes, downUA,
			ms(tRead), ms(tConv1), ms(tUp), ms(tDec), ms(tConv2), time.Since(t0).Milliseconds())
	}()
	tRead = time.Now()
	model = getStr(in, "model")
	if model == "" {
		writeConvError(w, http.StatusBadRequest, "missing model")
		return
	}
	// P2a-1：剥离 [1M] 后缀（Claude Code 上下文标记，上游不认），写回 in 让各转换/透传统一。
	model = stripOneMSuffix(model)
	in["model"] = model
	if t, ok := lookupFormat(model); ok {
		target = t
	} else {
		// P2a-3：未知模型默认走 chat（cc-switch 同款），不再原端点碰运气。
		target = FmtChat
	}
	if target == FmtGemini {
		writeConvError(w, http.StatusBadRequest, "gemini models use /v1/models/<id>, /conv cannot convert")
		return
	}
	stream = asBool(in["stream"])
	upReq := in
	if target != inFmt {
		switch {
		case inFmt == FmtChat && target == FmtResponses:
			upReq = chatToResponsesReq(in)
		case inFmt == FmtResponses && target == FmtChat:
			upReq = responsesToChatReq(in)
		case inFmt == FmtMessages && target == FmtChat:
			upReq = messagesToChatReq(in)
		case inFmt == FmtChat && target == FmtMessages:
			upReq = chatToMessagesReq(in)
		case inFmt == FmtMessages && target == FmtResponses:
			upReq = messagesToResponsesReq(in)
		case inFmt == FmtResponses && target == FmtMessages:
			upReq = responsesToMessagesReq(in)
		}
	} else if inFmt == FmtResponses {
		// responses 同格式透传：客户端下一轮可能带回上轮网关现编的
		// reasoning/function_call 条目（rs_/fc_ 前缀 ID），上游不认，先清洗。
		// Plan A：记录本轮放行的 reasoning id，上游报 caller 绑定失败时淘汰。
		upReq, allowedReasoningIDs = sanitizeResponsesInputTracked(in)
		// v0.3.5：同格式也归一化 effort（max→high，缺省 high），防非法档直达上游。
		normalizeReasoningEffort(upReq)
	} else if inFmt == FmtMessages {
		// P1b-7：messages 同格式透传：清理多轮带回的杂散 signature 与顶层 thinking。
		stripThinkingSignature(in)
	}
	if target == FmtMessages {
		ensureMessagesDefaults(upReq)
	}
	if stream && inFmt == FmtChat {
		// chat 流式默认不带 usage，强制加上，终态转换需要它。
		upReq["stream_options"] = map[string]any{"include_usage": true}
	}
	// ZEN_DEBUG=1 时打印实际发上游的 body 前 2KB（默认关闭：量大刷屏，
	// Windows 控制台回压会拖慢请求）。
	if debugOn() && strings.Contains(model, "muse-spark") {
		if b, _ := json.Marshal(upReq); len(b) > 0 {
			if len(b) > 2048 {
				b = b[:2048]
			}
			logf("[REQ %s] spark upReq: %.2048s", reqID, string(b))
		}
	}
	tConv1 = time.Now()
	upBody, err := json.Marshal(upReq)
	if err != nil {
		writeConvError(w, http.StatusBadRequest, "encode upstream request: "+err.Error())
		return
	}
	upstream := *zenBase
	upstream.Path = singleJoin(zenBase.Path, target.upstreamPath())
	// doUpstream 发往上游（首发与自愈重试共用）。
	// v0.3.11：绑下游 req.Context()，客户端断开/超时即取消上游请求，
	// 不再靠 handler 返回才释放（此前 Background 导致断开也泄漏连接）。
	doUpstream := func(body []byte) (*http.Response, error) {
		fwd, err := http.NewRequestWithContext(req.Context(), http.MethodPost, upstream.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		setZenHeaders(fwd.Header, apiKey, project)
		// 最小验证：先透传客户端头，再补缺（setZenHeaders 内 setDefault 只补空位）。
		inheritClientHeaders(fwd.Header, req.Header)
		fwd.Header.Set("Content-Type", "application/json")
		// P0-1：流式用无总超时的 client，非流式用 600s 总超时。
		upstreamClient := zenClient
		if stream {
			upstreamClient = zenStreamClient
		}
		return upstreamClient.Do(fwd)
	}
	resp, err := doUpstream(upBody)
	if err != nil {
		logf("[REQ %s] Do err: %v", reqID, err)
		writeConvError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	// v0.3.11：下游断开即关上游包体，卡在 pumpSSE/r.Read 的 goroutine 立刻报错
	// 退出，连接回池。Body.Close 多次调用安全；重试分支各自 defer 不动。
	// 闭包捕获 resp 变量，重试后指向最新 body，旧 body 由上一行 defer 收。
	stopUpstream := context.AfterFunc(req.Context(), func() { resp.Body.Close() })
	defer stopUpstream()
	tUp = time.Now()
	upStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// P1b-9：按客户端协议包规范 error envelope，不再原样透传。
		// 同时把上游原文打到网关日志，定位校验失败原因（spark 1.3 400 专用）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		rawStr := strings.TrimSpace(string(raw))
		// Plan A 反馈淘汰：caller 绑定失败说明放行的 id 已过期，逐出避免下轮再送。
		if isCallerMismatch400(rawStr) {
			evictIssued(allowedReasoningIDs...)
		}
		// 自愈重试：caller 绑定失败且本轮放行过 reasoning（responses 同格式）时，
		// 去串重发一次，把“用户手动重跑才好”变成网关自己消化。
		// 重试点在向客户端写任何字节之前，流式/非流式均安全；仅重试一次。
		retriedOK := false
		if isCallerMismatch400(rawStr) && len(allowedReasoningIDs) > 0 && inFmt == FmtResponses {
			logf("[REQ %s] caller-mismatch 400, evicted %d ids, retry without reasoning",
				reqID, len(allowedReasoningIDs))
			upReqRetry, _ := sanitizeResponsesInputTracked(in)
			normalizeReasoningEffort(upReqRetry)
			if upBodyRetry, merr := json.Marshal(upReqRetry); merr == nil {
				if r2, rerr := doUpstream(upBodyRetry); rerr == nil {
					resp = r2
					defer resp.Body.Close()
					tUp = time.Now()
					upStatus = resp.StatusCode
					allowedReasoningIDs = nil
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						retriedOK = true
					} else {
						raw, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<10))
					}
				}
			}
		}
		if !retriedOK {
			if len(raw) > 0 {
				logf("[REQ %s] upstream %d body: %.800s", reqID, resp.StatusCode, strings.TrimSpace(string(raw)))
			} else {
				logf("[REQ %s] upstream %d body: <empty>", reqID, resp.StatusCode)
			}
			// 已消费 body，重包一个 Reader 给 writeUpstreamError 复用
			writeUpstreamError(w, inFmt, resp.StatusCode, bytes.NewReader(raw))
			return
		}
	}
	if stream {
		// v0.3.11：包体空闲超时。ResponseHeaderTimeout 只保响应头，
		// 上游中途 stall（有连接、无字节、无EOF）此前永久挂起。
		// 超时走各路径既有 error 收尾，不伪造 DONE。
		sbody := newIdleTimeoutBody(resp.Body, streamIdleTimeout())
		defer sbody.Close()
		switch {
		case inFmt == FmtChat && target == FmtResponses:
			streamResponsesToChat(w, sbody, model)
		case inFmt == FmtResponses && target == FmtChat:
			streamChatToResponses(w, sbody, model)
		case inFmt == FmtMessages && target == FmtChat:
			streamChatToMessages(w, sbody, model)
		case inFmt == FmtChat && target == FmtMessages:
			streamMessagesToChat(w, sbody, model)
		case inFmt == FmtMessages && target == FmtResponses:
			streamResponsesToMessages(w, sbody, model)
		case inFmt == FmtResponses && target == FmtMessages:
			streamMessagesToResponses(w, sbody, model)
		default:
			// 同格式：SSE 原样透传，顺带嗅探上游原生 reasoning id（Plan A 流式学习）。
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			passthroughLearnSSE(w, sbody)
		}
		return
	}
	var upResp map[string]any
	// P0-2：非流式全量进内存，限 64MB 防 OOM。
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&upResp); err != nil {
		writeConvError(w, http.StatusBadGateway, "decode upstream response: "+err.Error())
		return
	}
	tDec = time.Now()
	// Plan A：非流式响应先学习 reasoning id，再做逆转换/透传。
	learnReasoningIDs(upResp)
	out := upResp
	if target != inFmt {
		switch {
		case inFmt == FmtChat && target == FmtResponses:
			out = responsesToChatResp(upResp, model)
		case inFmt == FmtResponses && target == FmtChat:
			out = chatToResponsesResp(upResp, model)
		case inFmt == FmtMessages && target == FmtChat:
			out = chatToMessagesResp(upResp, model)
		case inFmt == FmtChat && target == FmtMessages:
			out = messagesToChatResp(upResp, model)
		case inFmt == FmtMessages && target == FmtResponses:
			out = responsesToMessagesResp(upResp, model)
		case inFmt == FmtResponses && target == FmtMessages:
			out = messagesToResponsesResp(upResp, model)
		}
	}
	tConv2 = time.Now()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(out)
}

// P1b-9：上游非 2xx 按客户端协议包规范 error envelope（保留状态码），
// 避免 HTML/空体/异构 JSON 穿透搞崩客户端解析。错误体截前 500 字，
// 若上游已是规范形状则透传其 message。
func writeUpstreamError(w http.ResponseWriter, inFmt Format, status int, r io.Reader) {
	raw, _ := io.ReadAll(io.LimitReader(r, 64<<20))
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = http.StatusText(status)
	}
	if m := parseSSEData(msg); m != nil {
		if em := extractUpstreamMsg(m); em != "" {
			msg = em
		}
	}
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var out map[string]any
	switch inFmt {
	case FmtMessages:
		out = map[string]any{"type": "error",
			"error": map[string]any{"type": "api_error", "message": msg}}
	case FmtChat:
		out = map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": status}}
	default: // FmtResponses
		out = map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": status}}
	}
	b, _ := json.Marshal(out)
	_, _ = w.Write(b)
}

func extractUpstreamMsg(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		if s, ok := e["message"].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := m["message"].(string); ok && s != "" {
		return s
	}
	return ""
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
