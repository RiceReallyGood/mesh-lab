// Kitex echo client —— 端到端链路打点实验的主调侧，同时充当压测负载源。
//
// 与 kitex-benchmark 自带 client 的差别：
//   - 强制 TTHeader（benchmark 用 Framed，那样 header KV 全丢，Envoy 无从做 L7 路由）
//   - 目标是 sidecar 的 UDS，真实服务名走 TTHeader ToService 由 Envoy 路由
//   - 采样决策在此发起：本 client 是整条链路上唯一决定 sampled 的地方（§8.4.1）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/kitex-benchmark/codec/thrift/kitex_gen/echo"
	"github.com/cloudwego/kitex-benchmark/codec/thrift/kitex_gen/echo/echoserver"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/stats"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/transport"

	"meshlab/demo/probe"
)

func main() {
	var (
		target     = flag.String("target", probe.RunPath("out.sock"), "sidecar 地址（UDS 路径或 host:port）")
		service    = flag.String("service", "echo-server", "真实目标服务名，写入 TTHeader ToService 供 Envoy 路由")
		concurrent = flag.Int("c", 1, "并发数")
		duration   = flag.Duration("d", 5*time.Second, "压测时长；为 0 时只发 -n 个请求")
		total      = flag.Int("n", 1, "-d 为 0 时发送的请求总数")
		payload    = flag.Int("size", 128, "payload 字节数")
		sampleRate = flag.Float64("sample", 1.0, "采样率 0~1；压测时调低（§8.4.3）")
		traceFile  = flag.String("trace", probe.RunPath("trace-client.ndjson"), "打点输出文件")
		node       = flag.String("node", "kitex-client", "本节点在 trace 中的标识")
	)
	flag.Parse()

	tr, err := probe.NewTracer(*traceFile, *node)
	if err != nil {
		log.Fatalf("初始化 tracer 失败: %v", err)
	}
	defer tr.Close()

	cli, err := echoserver.NewClient(*service,
		client.WithHostPorts(*target),
		// 诊断用：打印实际生效的传输协议。
		// SetTransportProtocol 内部是 |= 而非赋值（kitex rpcconfig.go:178），
		// 曾怀疑 TTHeader 被意外 OR 上 Framed，此处用于证实/证伪。
		client.WithMiddleware(func(next endpoint.Endpoint) endpoint.Endpoint {
			var once sync.Once
			return func(ctx context.Context, req, resp interface{}) error {
				once.Do(func() {
					if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
						log.Printf("[诊断] 实际传输协议 = %s", ri.Config().TransportProtocol())
					}
				})
				return next(ctx, req, resp)
			}
		}),
		// TTHeader 是本方案的前提：Framed 没有 header KV 段，
		// trace 上下文与路由字段都无处安放。
		client.WithTransportProtocol(transport.TTHeader),
		// 默认 MetaHandler 只有 MetainfoClientHandler（kitex internal/client/option.go:234）。
		// ClientTTHeaderHandler 才是写 FromService/ToService/ToMethod 等 IntKV 的那个，
		// 不注册的话 TTHeader 里根本没有 IntKV 段，Envoy 的 x-tt-to-service 路由无从匹配。
		client.WithMetaHandler(transmeta.ClientTTHeaderHandler),
		client.WithStatsLevel(stats.LevelDetailed),
		client.WithTracer(tr),
	)
	if err != nil {
		log.Fatalf("创建 client 失败: %v", err)
	}

	msg := string(make([]byte, *payload))
	req := &echo.Request{Action: "echo", Msg: msg}

	var (
		sent    atomic.Int64
		failed  atomic.Int64
		latency = &latencyRecorder{}
		stop    = make(chan struct{})
		wg      sync.WaitGroup
	)

	worker := func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(time.Now().UnixNano() + rand.Int63()))
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 采样决策：整条链路上只有这里决定，下游各跳一律服从（§8.4.1）
			ctx := probe.WithTraceparent(context.Background(),
				probe.NewTraceparent(rng.Float64() < *sampleRate))

			t0 := time.Now()
			_, err := cli.Echo(ctx, req)
			latency.record(time.Since(t0))

			sent.Add(1)
			if err != nil {
				if failed.Add(1) <= 5 {
					log.Printf("请求失败: %v", err)
				}
			}
		}
	}

	start := time.Now()
	if *duration > 0 {
		wg.Add(*concurrent)
		for i := 0; i < *concurrent; i++ {
			go worker()
		}
		time.Sleep(*duration)
		close(stop)
		wg.Wait()
	} else {
		// 单次/少量模式：功能验证用，不做并发
		for i := 0; i < *total; i++ {
			ctx := probe.WithTraceparent(context.Background(), probe.NewTraceparent(true))
			t0 := time.Now()
			resp, err := cli.Echo(ctx, req)
			latency.record(time.Since(t0))
			sent.Add(1)
			if err != nil {
				failed.Add(1)
				log.Printf("请求失败: %v", err)
			} else if i == 0 {
				log.Printf("首个响应: action=%q msg 长度=%d", resp.Action, len(resp.Msg))
			}
		}
	}
	elapsed := time.Since(start)

	n, nf := sent.Load(), failed.Load()
	fmt.Fprintf(os.Stderr, "\n=== 压测结果 ===\n")
	fmt.Fprintf(os.Stderr, "并发=%d 时长=%v 请求=%d 失败=%d QPS=%.0f\n",
		*concurrent, elapsed.Truncate(time.Millisecond), n, nf, float64(n)/elapsed.Seconds())
	latency.report(os.Stderr)
	fmt.Fprintf(os.Stderr, "采样率=%.4f\n", *sampleRate)
}

// latencyRecorder 记录端到端延迟。
// 注意这是 client 侧观测的总时长，它与 server 侧观测的处理时长之差
// 才是跨机网络往返 —— 两者都在各自机器上测量，相减时时钟偏斜抵消（§8.2.3）。
type latencyRecorder struct {
	mu   sync.Mutex
	data []time.Duration
}

func (l *latencyRecorder) record(d time.Duration) {
	l.mu.Lock()
	l.data = append(l.data, d)
	l.mu.Unlock()
}

func (l *latencyRecorder) report(w *os.File) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.data) == 0 {
		return
	}
	sort.Slice(l.data, func(i, j int) bool { return l.data[i] < l.data[j] })
	p := func(q float64) time.Duration {
		idx := int(float64(len(l.data)-1) * q)
		return l.data[idx]
	}
	fmt.Fprintf(w, "延迟 p50=%v p90=%v p99=%v p999=%v max=%v\n",
		p(0.50).Truncate(time.Microsecond), p(0.90).Truncate(time.Microsecond),
		p(0.99).Truncate(time.Microsecond), p(0.999).Truncate(time.Microsecond),
		l.data[len(l.data)-1].Truncate(time.Microsecond))
}
