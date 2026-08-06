// Package probe 实现 Kitex 侧的打点采集。
//
// 设计要点（对应设计文档 §8）：
//
//   - 时钟：跨进程比较只能用 wall clock，但精确时长必须用 monotonic。
//     两者都记录，由 merge 工具按 host 决定用哪个。跨 host 相减是被禁止的。
//   - 采样：未采样的请求必须近乎零开销，否则打点本身会主导压测结果。
//   - 落盘：请求路径上不做 IO，事件先进 channel，由单独 goroutine 批量写。
package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/stats"
)

// Event 是落盘的一行 NDJSON。字段语义见设计文档 §8.1。
type Event struct {
	Host    string            `json:"host"`
	Node    string            `json:"node"`
	Trace   string            `json:"trace"`
	Span    string            `json:"span,omitempty"`
	Parent  string            `json:"parent,omitempty"`
	Point   string            `json:"point"`
	WallNs  int64             `json:"wall_ns"`
	MonoNs  int64             `json:"mono_ns"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// monoBase 用于把 Go 的单调时钟转成一个进程内可比较的整数。
// time.Since 内部走单调时钟，不受 NTP 阶跃影响 —— 这正是 §8.2 要求的。
var monoBase = time.Now()

func monoNow() int64 { return int64(time.Since(monoBase)) }

// Tracer 实现 kitex 的 stats.Tracer 接口。
type Tracer struct {
	host string
	node string

	ch     chan *Event
	wg     sync.WaitGroup
	closed atomic.Bool

	// 统计：用于事后核对「采样了多少、丢了多少」。
	// 丢弃计数不为零说明 channel 容量或落盘速度不够，
	// 此时 trace 数据不完整，必须在报告里声明。
	sampled atomic.Uint64
	dropped atomic.Uint64
	total   atomic.Uint64
}

// NewTracer 创建 tracer 并启动落盘 goroutine。
//
// host 标识决定 merge 工具是否允许把两个事件的时间戳相减（§8.2），
// 必须能真正区分机器。不能只靠 os.Hostname()：本实验两台机器的
// hostname 都是 localhost.localdomain，靠它会把跨机误判成同机。
// 优先取环境变量 KITEX_PROBE_HOST。
func NewTracer(path, node string) (*Tracer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	host := os.Getenv("KITEX_PROBE_HOST")
	if host == "" {
		host, _ = os.Hostname()
	}

	t := &Tracer{
		host: host,
		node: node,
		// 容量取得较大：请求路径只做非阻塞投递，宁可占内存也不阻塞业务。
		ch: make(chan *Event, 1<<16),
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer f.Close()
		w := bufio.NewWriterSize(f, 1<<20)
		defer w.Flush()
		enc := json.NewEncoder(w)
		// 定期 flush，避免进程被 kill 时丢掉缓冲区内容
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case ev, ok := <-t.ch:
				if !ok {
					return
				}
				_ = enc.Encode(ev)
			case <-tick.C:
				_ = w.Flush()
			}
		}
	}()
	return t, nil
}

// Start 在 RPC 开始时被调用。此处不做任何事 —— 采样判定推迟到 Finish，
// 因为那时才能从 rpcinfo 里读到经 TTHeader 传来的 traceparent。
func (t *Tracer) Start(ctx context.Context) context.Context { return ctx }

// Finish 在 RPC 结束时被调用，此时 rpcinfo 里已有完整的事件序列。
func (t *Tracer) Finish(ctx context.Context) {
	t.total.Add(1)

	// §8.4.1 头部采样：sampled 标志经 traceparent 传来，本节点只服从不决策。
	// 先判采样再取 stats，未采样的代价仅为一次 map 查找 + 一次位判断。
	traceID, sampled := traceContextFrom(ctx)
	if !sampled {
		return
	}

	ri := rpcinfo.GetRPCInfo(ctx)
	if ri == nil || ri.Stats() == nil {
		return
	}
	t.sampled.Add(1)

	st := ri.Stats()
	wall := time.Now().UnixNano()
	mono := monoNow()

	for _, ne := range allEvents() {
		ev := st.GetEvent(ne.event)
		if ev == nil {
			continue
		}
		// kitex 的 Event.Time() 是 wall clock。为保持 §8.2 的纪律，
		// mono 用「该事件相对本次 Finish 的偏移」推算，保证同进程内可精确相减。
		evWall := ev.Time().UnixNano()
		t.emit(&Event{
			Host:   t.host,
			Node:   t.node,
			Trace:  traceID,
			Point:  ne.name,
			WallNs: evWall,
			MonoNs: mono - (wall - evWall),
			Attrs:  eventAttrs(ev),
		})
	}
}

func (t *Tracer) emit(ev *Event) {
	if t.closed.Load() {
		return
	}
	// 非阻塞投递：绝不因为落盘慢而阻塞业务请求，
	// 那会让打点直接改变被测系统的行为。
	select {
	case t.ch <- ev:
	default:
		t.dropped.Add(1)
	}
}

// Stats 返回采集统计，供运行结束时核对数据完整性。
func (t *Tracer) Stats() (total, sampled, dropped uint64) {
	return t.total.Load(), t.sampled.Load(), t.dropped.Load()
}

func (t *Tracer) Close() {
	if t.closed.Swap(true) {
		return
	}
	close(t.ch)
	t.wg.Wait()
	total, sampled, dropped := t.Stats()
	fmt.Fprintf(os.Stderr, "[probe] node=%s 总请求=%d 采样=%d 丢弃=%d\n",
		t.node, total, sampled, dropped)
	if dropped > 0 {
		fmt.Fprintf(os.Stderr,
			"[probe] 警告：有 %d 条事件被丢弃，trace 数据不完整，不可用于归因结论\n", dropped)
	}
}

func eventAttrs(ev rpcinfo.Event) map[string]string {
	if ev.Info() == "" {
		return nil
	}
	return map[string]string{"info": ev.Info()}
}

// namedEvent 把 kitex 事件与打点名绑定。
// stats.Event 接口只有 Index() 和 Level()，不带名字，所以名字要自己维护。
type namedEvent struct {
	event stats.Event
	name  string
}

// allEvents 返回要采集的 kitex 事件。
// 这些全部是 kitex 预定义的，零源码改动即可获得（设计文档 §3.4）。
func allEvents() []namedEvent {
	return []namedEvent{
		{stats.RPCStart, "rpc_start"},
		{stats.RPCFinish, "rpc_finish"},
		{stats.ClientConnStart, "client_conn_start"},
		{stats.ClientConnFinish, "client_conn_finish"},
		{stats.ReadStart, "read_start"},
		{stats.ReadFinish, "read_finish"},
		{stats.WaitReadStart, "wait_read_start"},
		{stats.WaitReadFinish, "wait_read_finish"},
		{stats.WriteStart, "write_start"},
		{stats.WriteFinish, "write_finish"},
		{stats.ServerHandleStart, "server_handle_start"},
		{stats.ServerHandleFinish, "server_handle_finish"},

		// 以下为本项目补充的细粒度事件（kitex pkg/stats/meshlab_events.go）。
		// 目的是拆开 read_start→read_finish 这个吞掉 97% 时间的黑盒。
		{stats.MeshFirstByte, "mesh_first_byte"},
		{stats.MeshHeaderDecodeStart, "mesh_hdr_decode_start"},
		{stats.MeshHeaderDecodeFinish, "mesh_hdr_decode_finish"},
		{stats.MeshPayloadCodecStart, "mesh_payload_codec_start"},
		{stats.MeshPayloadCodecFinish, "mesh_payload_codec_finish"},
		{stats.MeshHeaderEncodeStart, "mesh_hdr_encode_start"},
		{stats.MeshHeaderEncodeFinish, "mesh_hdr_encode_finish"},
	}
}
