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
	"strconv"
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
	// 收尾只做一次，**且后到的调用者会阻塞到第一个跑完**。理由见 Close()。
	closeOnce sync.Once

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

// Start 在 RPC 开始时被调用。
//
// 大部分点位的采样判定推迟到 Finish —— 那时才能从 rpcinfo 里读到经 TTHeader
// 传来的 traceparent。netpoll 读探针是例外，两侧的开法完全不同：
//
//   - **客户端**：traceparent 是自己发起前塞进 ctx 的，此刻可读，所以在这里
//     判一次采样再开槽位。探针精确圈在自己那次阻塞读上，未采样零开销。
//   - **服务端**：OnRead 被回调时读**已经做完了**，没有「读之前」可供开启。
//     所以探针在连接级常开（Kitex 的 default_server_handler.OnActive），
//     快照在 OnRead 入口就取好挂进 ctx 了 —— 本函数在服务端什么都不用做。
//
// 因此这里要**避免覆盖已有槽位**：服务端走到这里时 ctx 上已经挂着取好的快照，
// 再 WithNetpollProbe 一次会把它清成空的。
func (t *Tracer) Start(ctx context.Context) context.Context {
	// 写路径槽位：**两侧都在这里挂**。写发生时 trace 早已绑定，采样状态已知，
	// 所以不像读侧那样要分客户端/服务端两套开法。
	// 服务端此刻还读不到 traceparent（下面 sampled 为假），但写探针要到
	// Write() 才用得上，那时 rpcinfo 里已有 —— 所以无条件挂，代价是一个空结构体。
	ctx, _ = stats.WithNetpollWriteProbe(ctx)

	if stats.NetpollProbeFrom(ctx) != nil {
		return ctx // 服务端：读侧快照已由 OnRead 挂好
	}
	if _, sampled := traceContextFrom(ctx); !sampled {
		return ctx
	}
	ctx, _ = stats.WithNetpollProbe(ctx)
	return ctx
}

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

	t.emitNetpoll(ctx, traceID)
	t.emitNetpollWrite(ctx, traceID)
}

// emitNetpoll 输出 netpoll 内部读路径的各阶段时刻。
//
// 这些时刻由 poller goroutine 采集，**不经过 Kitex 的 stats**——
// stats.Record 记的是调用那一刻的时间，等 RPC goroutine 被调度起来再记，
// 恰好抹掉我们要测的那段调度延迟。
//
// 也正因为不走 stats，它们带着 monotonic 读数，可以直接对 monoBase 求差，
// 比上面那套「wall 偏移推算」精确 —— 上面的 Kitex 事件只有墙上钟可用。
func (t *Tracer) emitNetpoll(ctx context.Context, traceID string) {
	p := stats.NetpollProbeFrom(ctx)
	if !p.Valid() {
		return
	}
	// Rounds > 1 表示报文被拆成多个 TCP 段，各时刻只描述最后一段。
	// 原样带出让分析侧决定是否剔除，而不是在这里悄悄丢弃。
	attrs := map[string]string{"rounds": strconv.FormatInt(p.Rounds, 10)}

	for _, np := range []struct {
		name string
		ts   time.Time
	}{
		{"mesh_np_epoll_wake", p.EpollWake},
		{"mesh_np_dispatch", p.Dispatch},
		{"mesh_np_readv_start", p.ReadvStart},
		{"mesh_np_readv_done", p.ReadvDone},
		{"mesh_np_trigger", p.Trigger},
	} {
		if np.ts.IsZero() {
			continue
		}
		t.emit(&Event{
			Host:   t.host,
			Node:   t.node,
			Trace:  traceID,
			Point:  np.name,
			WallNs: np.ts.UnixNano(),
			MonoNs: int64(np.ts.Sub(monoBase)),
			Attrs:  attrs,
		})
	}
}

// emitNetpollWrite 输出 netpoll 内部**写**路径的各阶段时刻。
//
// 与读路径两点不同：
//
//  1. 时刻全部由 RPC goroutine 自己在 Flush 里采集，不跨 goroutine，
//     所以**不按 Consistent 整体丢弃** —— 逐个判零即可。整体门控只会把
//     「哪一段没记上」这个信息一起吞掉。
//  2. 因此它**不能挂在 emitNetpoll 末尾**：读探针无效时那边会提前 return，
//     会把写路径一起带走。两者相互独立，各调各的。
func (t *Tracer) emitNetpollWrite(ctx context.Context, traceID string) {
	p := stats.NetpollWriteProbeFrom(ctx)
	if p == nil {
		return
	}
	// writes>1 说明发生了部分写；waited 说明走了 EPOLLOUT 慢路径 ——
	// 那时 writev_done→flush_done 混着等待，不能读成「缓冲区回收」。
	// 原样带出让分析侧决定，不在这里悄悄丢弃。
	attrs := map[string]string{
		"writes":     strconv.FormatInt(p.Writes, 10),
		"waited":     strconv.FormatBool(p.Waited),
		"consistent": strconv.FormatBool(p.Consistent),
	}

	for _, np := range []struct {
		name string
		ts   time.Time
	}{
		{"mesh_np_flush_start", p.FlushStart},
		{"mesh_np_writev_start", p.WritevStart},
		{"mesh_np_writev_done", p.WritevDone},
		{"mesh_np_flush_done", p.FlushDone},
	} {
		if np.ts.IsZero() {
			continue
		}
		t.emit(&Event{
			Host:   t.host,
			Node:   t.node,
			Trace:  traceID,
			Point:  np.name,
			WallNs: np.ts.UnixNano(),
			MonoNs: int64(np.ts.Sub(monoBase)),
			Attrs:  attrs,
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

// Close 刷盘并打印收尾统计。可以被调用多次，后到者阻塞到第一次跑完为止。
//
// **不能写成 `if t.closed.Swap(true) { return }` 提前返回**：server 有两条收尾
// 路径 —— main 里的 defer，和信号 goroutine 里的 `tr.Close(); os.Exit(0)`。
// 用 Swap 的话谁抢到谁去刷盘+打印，另一个立即返回、紧接着 os.Exit(0)
// 就把进程干掉了。实测表现为 **trace 数据刷成功了（wg.Wait 已完成）
// 但汇总行没打出来**，三轮复现一轮 —— 而那行正是 runbook 判定数据完整性的依据，
// 「行不在」会被误读成「数据不全」。sync.Once.Do 让后到者等前者跑完才返回。
func (t *Tracer) Close() {
	t.closeOnce.Do(func() {
		// emit 靠这个标志快速拒收，必须在 close(t.ch) 之前置位。
		t.closed.Store(true)
		close(t.ch)
		t.wg.Wait()
		total, sampled, dropped := t.Stats()
		fmt.Fprintf(os.Stderr, "[probe] node=%s 总请求=%d 采样=%d 丢弃=%d\n",
			t.node, total, sampled, dropped)
		if dropped > 0 {
			fmt.Fprintf(os.Stderr,
				"[probe] 警告：有 %d 条事件被丢弃，trace 数据不完整，不可用于归因结论\n", dropped)
		}
	})
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
		{stats.MeshPayloadEncodeStart, "mesh_payload_encode_start"},
		{stats.MeshPayloadEncodeFinish, "mesh_payload_encode_finish"},
		{stats.MeshHeaderEncodeStart, "mesh_hdr_encode_start"},
		{stats.MeshHeaderEncodeFinish, "mesh_hdr_encode_finish"},
		{stats.MeshNetpollOnRead, "mesh_netpoll_onread"},
		{stats.MeshSocketWriteStart, "mesh_socket_write_start"},
		{stats.MeshSocketWriteFinish, "mesh_socket_write_finish"},
		{stats.MeshSocketReadStart, "mesh_socket_read_start"},
	}
}
