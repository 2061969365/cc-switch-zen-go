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
// 环境变量：
//
//	PORT                  监听端口，默认 8080
//	OPENCODE_ZEN_BASE     上游，默认 https://opencode.ai/zen/v1
//	OPENCODE_ZEN_API_KEY  默认 public（匿名免费层；有真 key 可覆盖）
//	OPENCODE_ZEN_PROJECT  默认 default
package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

const zenUA = "opencode/1.18.18"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// randHex 返回 n 字节的随机 hex（uuid 的轻量替代，stdlib 零依赖）。
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
			req.Header.Del("Authorization")
			req.Header.Del("X-Api-Key")
			req.Header.Del("X-Goog-Api-Key")

			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("User-Agent", zenUA)
			tag := randHex(16)
			// 客户端自带则保留（官方 opencode 直连本网关的场景）。
			setDefault(req.Header, "X-Opencode-Client", "opencode")
			setDefault(req.Header, "X-Opencode-Session", "ses_"+tag)
			setDefault(req.Header, "X-Opencode-Request", "req_"+tag)
			setDefault(req.Header, "X-Opencode-Project", project)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
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
	log.Fatal(http.ListenAndServe(addr, mux))
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
