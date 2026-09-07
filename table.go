// table.go: 模型 -> 所需上游格式映射。
//
// 数据来源：opencode 官网 zen 文档（opencode.ai/docs/zen），源文件位于
// anomalyco/opencode@dev:packages/web/src/content/docs/zen.mdx。
// 网关启动时抓取该 raw 文件解析 Endpoints 表格，失败则用内置快照兜底。
// 覆盖方式：环境变量 ZEN_TABLE_URL 换地址，ZEN_TABLE_REFRESH=0 关闭刷新。
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Format 是模型在上游所需的协议格式。
type Format string

const (
	FmtChat      Format = "chat"      // /chat/completions
	FmtResponses Format = "responses" // /responses
	FmtMessages  Format = "messages"  // /messages
	FmtGemini    Format = "gemini"    // /models/<id>，/conv 不支持转换
)

// 上游路径。
func (f Format) upstreamPath() string {
	switch f {
	case FmtChat:
		return "/chat/completions"
	case FmtMessages:
		return "/messages"
	case FmtResponses:
		return "/responses"
	}
	return ""
}

// modelTable 精确模型 ID -> 格式。启动时先装内置快照，再用官网刷新增补。
var modelTable = map[string]Format{}

// 内置快照：官网 zen 文档 2026-09-07。前缀规则见 guessFormatByPrefix。
func seedBuiltinTable() {
	responses := []string{
		"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-pro", "gpt-5.4-mini", "gpt-5.4-nano",
		"gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-5.2-codex",
		"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-max", "gpt-5.1-codex-mini",
		"gpt-5", "gpt-5-codex", "gpt-5-nano",
		"grok-4.6", "grok-4.5", "grok-build-0.1",
		"muse-spark-1.3", "muse-spark-1.2", "muse-spark-1.3-contributor-free",
	}
	messages := []string{
		"claude-fable-5-1", "claude-fable-5", "claude-opus-5", "claude-opus-4-8",
		"claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5",
		"claude-sonnet-5", "claude-sonnet-4-6", "claude-sonnet-4-5", "claude-haiku-4-5",
		"qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus",
	}
	chat := []string{
		"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp",
		"minimax-m3", "minimax-m2.7", "minimax-m2.5",
		"glm-5.3-flash", "glm-5.3", "glm-5.2", "glm-5.1", "glm-5",
		"kimi-k2.5", "kimi-k2.6", "kimi-k2.7-code", "kimi-k3",
		"big-pickle", "mimo-v2.5-free", "ling-3.0-flash-fin-free",
		"nemotron-3-ultra-free", "nemotron-3.5-lightning-free",
	}
	gemini := []string{
		"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash",
		"gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.1-pro", "gemini-3-flash",
	}
	for _, m := range responses {
		modelTable[m] = FmtResponses
	}
	for _, m := range messages {
		modelTable[m] = FmtMessages
	}
	for _, m := range chat {
		modelTable[m] = FmtChat
	}
	for _, m := range gemini {
		modelTable[m] = FmtGemini
	}
}

// guessFormatByPrefix 按命名前缀兜底未知模型。返回 "" 表示连猜都猜不出。
func guessFormatByPrefix(model string) Format {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gemini-"):
		return FmtGemini
	case strings.HasPrefix(m, "gpt-"),
		strings.HasPrefix(m, "grok-"),
		strings.HasPrefix(m, "muse-spark"):
		return FmtResponses
	case strings.HasPrefix(m, "claude-"),
		strings.HasPrefix(m, "qwen"):
		return FmtMessages
	case strings.HasPrefix(m, "deepseek"),
		strings.HasPrefix(m, "minimax-"),
		strings.HasPrefix(m, "glm-"),
		strings.HasPrefix(m, "kimi-"),
		strings.HasPrefix(m, "mimo-"),
		strings.HasPrefix(m, "ling-"),
		strings.HasPrefix(m, "nemotron-"),
		m == "big-pickle":
		return FmtChat
	}
	return ""
}

// lookupFormat 查模型所需格式。ok=false 表示未知模型（调用方决定透传）。
func lookupFormat(model string) (Format, bool) {
	if f, ok := modelTable[model]; ok {
		return f, true
	}
	if f := guessFormatByPrefix(model); f != "" {
		return f, true
	}
	return "", false
}

// 表格行形如：| GPT 6 Astra | gpt-6-astra | `https://opencode.ai/zen/v1/responses` | `@ai-sdk/openai` |
var mdxRowRe = regexp.MustCompile(`^\|\s*[^|]+\|\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*\|\s*` + "`([^`]+)`")

func formatFromEndpoint(ep string) Format {
	switch {
	case strings.HasSuffix(ep, "/responses"):
		return FmtResponses
	case strings.HasSuffix(ep, "/messages"):
		return FmtMessages
	case strings.HasSuffix(ep, "/chat/completions"):
		return FmtChat
	case strings.Contains(ep, "/models/"):
		return FmtGemini
	}
	return ""
}

// refreshTableFromOfficial 抓官网 mdx 解析 Endpoints 表，增补 modelTable。
func refreshTableFromOfficial() {
	if os.Getenv("ZEN_TABLE_REFRESH") == "0" {
		return
	}
	url := os.Getenv("ZEN_TABLE_URL")
	if url == "" {
		url = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("映射表刷新失败（用内置快照）: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("映射表刷新 HTTP %d（用内置快照）", resp.StatusCode)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("映射表读取失败（用内置快照）: %v", err)
		return
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		m := mdxRowRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if f := formatFromEndpoint(m[2]); f != "" {
			modelTable[m[1]] = f
			n++
		}
	}
	log.Printf("映射表刷新：官网解析 %d 个模型，当前共 %d 个", n, len(modelTable))
}
