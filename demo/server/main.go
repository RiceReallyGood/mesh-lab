// Kitex echo server —— 端到端链路打点实验的被调侧。
//
// 与 kitex-benchmark 自带 server 的差别：
//   - 监听 UDS 而非 TCP（机内 sidecar 通信绕开 TCP/IP 栈，见设计文档 §3.6）
//   - 强制 TTHeader 传输协议（benchmark 默认用 Framed，那样 header KV 会丢）
//   - 挂上 stats.Tracer，采集 §7 打点清单里 server 侧的那些点
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cloudwego/kitex-benchmark/codec/thrift/kitex_gen/echo"
	"github.com/cloudwego/kitex-benchmark/codec/thrift/kitex_gen/echo/echoserver"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/stats"
	"github.com/cloudwego/kitex/server"

	"meshlab/demo/probe"
)

type echoImpl struct{}

func (s *echoImpl) Echo(ctx context.Context, req *echo.Request) (*echo.Response, error) {
	return &echo.Response{Action: req.Action, Msg: req.Msg}, nil
}

func (s *echoImpl) EchoComplex(ctx context.Context, req *echo.ComplexRequest) (*echo.ComplexResponse, error) {
	return &echo.ComplexResponse{Action: req.Action, Msg: req.Msg}, nil
}

func main() {
	var (
		addr      = flag.String("addr", "/tmp/kitex-demo/app.sock", "监听地址；含 '/' 视为 UDS，否则视为 TCP")
		traceFile = flag.String("trace", "/tmp/kitex-demo/trace-server.ndjson", "打点输出文件")
		node      = flag.String("node", "kitex-server", "本节点在 trace 中的标识")
	)
	flag.Parse()

	// §8.2：wall clock 只用于粗排序，时长一律用 monotonic，故此处两者都记。
	tr, err := probe.NewTracer(*traceFile, *node)
	if err != nil {
		log.Fatalf("初始化 tracer 失败: %v", err)
	}
	defer tr.Close()

	ln, err := listen(*addr)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", *addr, err)
	}

	svr := echoserver.NewServer(new(echoImpl),
		server.WithServiceAddr(ln),
		// LevelDetailed 才会产生 Read/Write/WaitRead/ServerHandle 这些细粒度事件，
		// LevelBase 只有 RPCStart/RPCFinish。
		server.WithStatsLevel(stats.LevelDetailed),
		server.WithTracer(tr),
	)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("收到退出信号，正在停止…")
		_ = svr.Stop()
		tr.Close()
		os.Exit(0)
	}()

	log.Printf("echo server 监听于 %s，打点写入 %s", *addr, *traceFile)
	if err := svr.Run(); err != nil {
		log.Fatalf("server 退出: %v", err)
	}
}

// listen 按地址形态选择 unix 或 tcp。
// UDS 需要先清理残留 socket 文件，否则重启时 bind 会失败（设计文档 §9.3）。
func listen(addr string) (net.Addr, error) {
	if filepath.IsAbs(addr) {
		if err := os.MkdirAll(filepath.Dir(addr), 0o755); err != nil {
			return nil, err
		}
		_ = os.Remove(addr)
		return net.ResolveUnixAddr("unix", addr)
	}
	return net.ResolveTCPAddr("tcp", addr)
}

var _ = rpcinfo.GetRPCInfo // 保留导入，probe 内部依赖同一 rpcinfo 语义
