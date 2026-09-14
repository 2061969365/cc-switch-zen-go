// issued.go: 本进程经手过的 reasoning item ID 集合（Plan A）。
//
// 对标 opencode-sanitize-reasoning 插件的 BOOT_TS 语义：插件只保留本进程
// 签发的 blob，删旧进程残留。网关侧等价物是“本进程响应里出现过的
// reasoning id”——上游经网关返回给客户端的 reasoning，下轮原样带回时
// caller 一致的概率最高；网关没见过的 id（一律视为异 caller 旧毒）丢弃。
//
// 防反噬三件套（key 轮换场景）：
//   - TTL：id 只活 issuedTTL，过期即忘，不给轮换后的旧串放行机会；
//   - 400 反馈：上游报 "was not issued to this caller" 时逐出当轮放行的 id；
//   - LRU 上限：超 cap 逐出最旧，不整表清空（避免热缓存雪崩）。
package main

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// issuedTTL 单个 id 存活期。上游 key 轮换/签发上下文过期后，旧 id 即失效。
const issuedTTL = 45 * time.Minute

// issuedCap 集合上限，LRU 逐出最旧。
const issuedCap = 5000

type issuedEntry struct {
	id  string
	exp time.Time
	el  *list.Element
}

var issuedMu sync.Mutex
var issuedIDs = map[string]*issuedEntry{}
var issuedLRU = list.New()

// learnReasoningIDs 从上游响应体学习 reasoning item id。只读，不改 resp。
func learnReasoningIDs(resp map[string]any) {
	if resp == nil {
		return
	}
	var ids []string
	for _, it := range asArr(resp["output"]) {
		m := asMap(it)
		if asStr(m["type"]) != "reasoning" {
			continue
		}
		if id := getStr(m, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	now := time.Now()
	issuedMu.Lock()
	defer issuedMu.Unlock()
	for _, id := range ids {
		touchIssuedLocked(id, now)
	}
}

func touchIssuedLocked(id string, now time.Time) {
	if e, ok := issuedIDs[id]; ok {
		e.exp = now.Add(issuedTTL)
		issuedLRU.MoveToFront(e.el)
		return
	}
	for len(issuedIDs) >= issuedCap {
		back := issuedLRU.Back()
		if back == nil {
			break
		}
		old := back.Value.(string)
		delete(issuedIDs, old)
		issuedLRU.Remove(back)
	}
	e := &issuedEntry{id: id, exp: now.Add(issuedTTL)}
	e.el = issuedLRU.PushFront(id)
	issuedIDs[id] = e
}

// learnReasoningID 学习单个 reasoning item id（流式嗅探用）。
func learnReasoningID(id string) {
	if id == "" {
		return
	}
	now := time.Now()
	issuedMu.Lock()
	defer issuedMu.Unlock()
	touchIssuedLocked(id, now)
}

// isIssued 本进程是否经手过该 reasoning id（且未过期）。
func isIssued(id string) bool {
	if id == "" {
		return false
	}
	now := time.Now()
	issuedMu.Lock()
	defer issuedMu.Unlock()
	e, ok := issuedIDs[id]
	if !ok {
		return false
	}
	if now.After(e.exp) {
		delete(issuedIDs, id)
		issuedLRU.Remove(e.el)
		return false
	}
	issuedLRU.MoveToFront(e.el)
	return true
}

// evictIssued 逐出指定 id（400 反馈淘汰用）。
func evictIssued(ids ...string) {
	if len(ids) == 0 {
		return
	}
	issuedMu.Lock()
	defer issuedMu.Unlock()
	for _, id := range ids {
		if e, ok := issuedIDs[id]; ok {
			delete(issuedIDs, id)
			issuedLRU.Remove(e.el)
		}
	}
}

// isCallerMismatch400 上游 400 是否为 caller 绑定失败（需淘汰放行 id）。
func isCallerMismatch400(body string) bool {
	return strings.Contains(body, "was not issued to this caller") ||
		strings.Contains(body, "invalid_encrypted_content")
}
