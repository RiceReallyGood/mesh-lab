package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bytedance/gopkg/cloud/metainfo"
)

// TraceparentKey 是 trace 上下文在 TTHeader StrKV 里的键名。
//
// 用 metainfo 的 persistent 前缀（RPC_PERSIST_）而非普通 key，是为了让它
// 沿调用链一路透传下去。注意前缀是大写的 —— 过 Envoy 时必须开
// header_keys_preserve_case，否则会被小写化导致 Kitex 侧静默失配。
// 这一点由 envoy 单测 TTHeaderMetainfoCaseTest 双向锁定（设计文档 §9.1）。
const TraceparentKey = metainfo.PrefixPersistent + "traceparent"

// W3C traceparent 格式：
//
//	version(2) - trace-id(32) - parent-id(16) - flags(2)
//	00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
//	                                                     ^^ bit0 = sampled
const (
	flagSampled = 0x01
)

// NewTraceparent 生成一个新的 traceparent。sampled 决定 flags 的 bit0。
func NewTraceparent(sampled bool) string {
	var traceID [16]byte
	var spanID [8]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:])
	var flags byte
	if sampled {
		flags |= flagSampled
	}
	return fmt.Sprintf("00-%s-%s-%02x",
		hex.EncodeToString(traceID[:]), hex.EncodeToString(spanID[:]), flags)
}

// ParseTraceparent 解出 trace-id 与 sampled 标志。
// 格式非法时返回 sampled=false —— 宁可漏采也不要因为解析失败而误采，
// 因为误采会在压测下放大成大量意外开销。
func ParseTraceparent(tp string) (traceID string, sampled bool) {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[3]) != 2 {
		return "", false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil || len(flags) != 1 {
		return parts[1], false
	}
	return parts[1], flags[0]&flagSampled != 0
}

// WithTraceparent 把 traceparent 放进 ctx 的 persistent metainfo，
// 由 kitex 的 MetainfoClientHandler 写入 TTHeader 的 StrKV 段。
func WithTraceparent(ctx context.Context, tp string) context.Context {
	return metainfo.WithPersistentValue(ctx, TraceparentKey, tp)
}

// traceContextFrom 从 ctx 里取出 traceparent。
//
// 服务端侧：kitex 的 MetainfoServerHandler 已把收到的 TTHeader StrKV
// 经 metainfo.SetMetaInfoFromMap 放进 ctx（kitex pkg/transmeta/metainfo.go:90）。
// 客户端侧：是本进程刚用 WithTraceparent 写进去的那个。
//
// 这是采样判定的热路径 —— 未采样的请求只走到这里就返回，
// 代价是一次 map 查找加一次位判断。
func traceContextFrom(ctx context.Context) (traceID string, sampled bool) {
	tp, ok := metainfo.GetPersistentValue(ctx, TraceparentKey)
	if !ok {
		return "", false
	}
	return ParseTraceparent(tp)
}
