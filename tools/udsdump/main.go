// udsdump 是一个 UDS 上的透明转发代理，把双向字节流原样打印出来。
//
// 用途：当 Envoy 报「解码失败」时，先看清 Kitex 真正发出的字节，
// 而不是靠推测。放在 client 与 server 之间即可：
//
//	client --> udsdump(-listen) --> server(-upstream)
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
)

func main() {
	listenPath := flag.String("listen", "/tmp/kitex-demo/dump.sock", "监听的 UDS 路径")
	upstream := flag.String("upstream", "/tmp/kitex-demo/app.sock", "上游 UDS 路径")
	maxDump := flag.Int("max", 256, "每个方向最多打印多少字节")
	flag.Parse()

	_ = os.Remove(*listenPath)
	ln, err := net.Listen("unix", *listenPath)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	log.Printf("udsdump: %s -> %s", *listenPath, *upstream)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(c, *upstream, *maxDump)
	}
}

func handle(down net.Conn, upstreamPath string, maxDump int) {
	defer down.Close()
	up, err := net.Dial("unix", upstreamPath)
	if err != nil {
		log.Printf("连接上游失败: %v", err)
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, up, down, "client->server", maxDump)
	go pipe(&wg, down, up, "server->client", maxDump)
	wg.Wait()
}

func pipe(wg *sync.WaitGroup, dst io.Writer, src io.Reader, dir string, maxDump int) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	dumped := 0
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if dumped < maxDump {
				end := n
				if dumped+end > maxDump {
					end = maxDump - dumped
				}
				fmt.Printf("\n=== %s (%d 字节，前 %d) ===\n%s", dir, n, end,
					hex.Dump(buf[:end]))
				// 同时给出连续 hex，便于与 fixture 直接比对
				fmt.Printf("hex: %s\n", hex.EncodeToString(buf[:end]))
				dumped += end
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
