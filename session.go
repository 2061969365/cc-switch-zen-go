package main

// v0.3.10：合法会话签发。
//
// 上游 FreeTier 校验 x-opencode-session 的结构（真测钉死）：
// ses_ + 12 位 hex 时间戳 + 14 位 base62 随机，共 26 位；其它形状一律 403。
// 旧逻辑 ses_+32hex 必死。此处逐操作移植 opencode identifier.ts create()。
//
// NOTE: 该时间戳在当前纪元下会被截断高位（48 位字段装不下 53 位值），
// 上游只验结构不验新鲜度（3 天前时间戳实测 200），故直接移植即可。

import (
	"crypto/rand"
	"sync"
	"time"
)

const mintChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const mintHex = "0123456789abcdef"

var (
	mintMu      sync.Mutex
	mintLastTs  int64
	mintCounter int64
)

// mintIDAt 按官方算法签发 prefix_ 开头的 26 位 ID；tsMs 显式传入便于单测。
func mintIDAt(prefix string, tsMs int64) string {
	mintMu.Lock()
	if tsMs != mintLastTs {
		mintLastTs = tsMs
		mintCounter = 0
	}
	mintCounter++
	ctr := mintCounter
	mintMu.Unlock()

	current := uint64(tsMs)*0x1000 + uint64(ctr)
	var sb [30]byte
	copy(sb[:len(prefix)], prefix)
	sb[len(prefix)] = '_'
	for i := 0; i < 6; i++ {
		b := byte((current >> uint(40-8*i)) & 0xff)
		sb[len(prefix)+1+2*i] = mintHex[b>>4]
		sb[len(prefix)+1+2*i+1] = mintHex[b&0x0f]
	}
	var rb [14]byte
	if _, err := rand.Read(rb[:]); err != nil {
		// crypto 源不可用时退化为纳秒混合（实践中不会发生）。
		n := uint64(time.Now().UnixNano())
		for i := range rb {
			rb[i] = byte((n >> uint(8*(i%8))) & 0xff)
		}
	}
	for i, b := range rb {
		sb[len(prefix)+13+i] = mintChars[int(b)%62]
	}
	return string(sb[:])
}

// mintID 用当前时间签发。
func mintID(prefix string) string { return mintIDAt(prefix, time.Now().UnixMilli()) }
