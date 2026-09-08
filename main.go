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
package main

import (
	"bytes"
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

const zenUA = "opencode/1.18.18"

// P0-1：上游共享连接池+超时。http.DefaultClient 无限等，上游 hang 住会拖死网关。
// zenClient 用于非流式（总超时 600s）；zenStreamClient 用于 SSE/透传（长连接，无总超时）。
var zenTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment, // 认 HTTP(S)_PROXY/NO_PROXY
	MaxIdleConnsPerHost:   20,
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

// newID 生成带前缀的随机 ID（resp_/msg_/chatcmpl_ 等响应包络用）。
func newID(prefix string) string { return prefix + randHex(12) }

// setZenHeaders 注入上游匿名鉴权头：Bearer + x-opencode 四件套 + UA。
// 缺了这组头上游报 MissingSessionID（已实测）。
func setZenHeaders(h http.Header, apiKey, project string) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("X-Goog-Api-Key")

	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", zenUA)
	tag := randHex(16)
	// 客户端自带则保留（官方 opencode 直连本网关的场景）。
	setDefault(h, "X-Opencode-Client", "opencode")
	setDefault(h, "X-Opencode-Session", "ses_"+tag)
	setDefault(h, "X-Opencode-Request", "req_"+tag)
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
			log.Printf("上游错误 %s %s: %v", req.Method, req.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream unreachable"}`))
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Printf("%s %s -> 上游 %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
	}

	mux := http.NewServeMux()
	// /health：容器/Railway 健康检查用，固定 200。
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	log.Printf("zen-go headless 监听 %s，上游 %s", addr, zenBase.String())
	seedBuiltinTable()
	go refreshTableFromOfficial()
	// P0-3：全入口请求体上限 200MB（/conv 内部另有更严的 32MB）。
	log.Fatal(http.ListenAndServe(addr, http.MaxBytesHandler(mux, 200<<20)))
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
		fwd, err := http.NewRequest(http.MethodGet, upstream.String(), nil)
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
	model := getStr(in, "model")
	if model == "" {
		writeConvError(w, http.StatusBadRequest, "missing model")
		return
	}
	target, ok := lookupFormat(model)
	if !ok {
		target = inFmt // 未知模型：透传碰运气
	}
	if target == FmtGemini {
		writeConvError(w, http.StatusBadRequest, "gemini models use /v1/models/<id>, /conv cannot convert")
		return
	}
	stream := asBool(in["stream"])
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
		upReq = sanitizeResponsesInput(in)
	}
	if stream && inFmt == FmtChat {
		// chat 流式默认不带 usage，强制加上，终态转换需要它。
		upReq["stream_options"] = map[string]any{"include_usage": true}
	}
	upBody, err := json.Marshal(upReq)
	if err != nil {
		writeConvError(w, http.StatusBadRequest, "encode upstream request: "+err.Error())
		return
	}
	upstream := *zenBase
	upstream.Path = singleJoin(zenBase.Path, target.upstreamPath())
	fwd, err := http.NewRequest(http.MethodPost, upstream.String(), bytes.NewReader(upBody))
	if err != nil {
		writeConvError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	setZenHeaders(fwd.Header, apiKey, project)
	fwd.Header.Set("Content-Type", "application/json")
	// P0-1：流式用无总超时的 client，非流式用 600s 总超时。
	upstreamClient := zenClient
	if stream {
		upstreamClient = zenStreamClient
	}
	resp, err := upstreamClient.Do(fwd)
	if err != nil {
		writeConvError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	log.Printf("CONV %s %s -> %s 上游 %d", inner, model, target.upstreamPath(), resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 上游错误原样透传，不转换（P0-2：错误体同样限流）。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<20))
		return
	}
	if stream {
		switch {
		case inFmt == FmtChat && target == FmtResponses:
			streamResponsesToChat(w, resp.Body, model)
		case inFmt == FmtResponses && target == FmtChat:
			streamChatToResponses(w, resp.Body, model)
		case inFmt == FmtMessages && target == FmtChat:
			streamChatToMessages(w, resp.Body, model)
		case inFmt == FmtChat && target == FmtMessages:
			streamMessagesToChat(w, resp.Body, model)
		case inFmt == FmtMessages && target == FmtResponses:
			streamResponsesToMessages(w, resp.Body, model)
		case inFmt == FmtResponses && target == FmtMessages:
			streamMessagesToResponses(w, resp.Body, model)
		default:
			// 同格式：SSE 原样透传。
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}
	var upResp map[string]any
	// P0-2：非流式全量进内存，限 64MB 防 OOM。
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&upResp); err != nil {
		writeConvError(w, http.StatusBadGateway, "decode upstream response: "+err.Error())
		return
	}
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
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(out)
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
