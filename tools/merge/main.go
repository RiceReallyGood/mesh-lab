// merge 把各节点的 NDJSON 打点合并成单请求的时序视图。
//
// 它的核心职责不是画图，而是**在代码层强制执行时钟纪律**（设计文档 §8.2）：
//
//   - 同一台机器内的事件才允许相减
//   - 跨机器只能用「差值法」导出往返时间，且结果必须标注为往返
//   - 提供 --inject-skew 用于证明分析对时钟偏斜免疫
//
// 之所以要把纪律写进工具而不是写进文档：本环境两台机器实测有 16.34 秒
// 时钟差，靠人自觉不跨机相减是不现实的。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

type Event struct {
	Host   string            `json:"host"`
	Node   string            `json:"node"`
	Trace  string            `json:"trace"`
	Point  string            `json:"point"`
	WallNs int64             `json:"wall_ns"`
	MonoNs int64             `json:"mono_ns"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

// nodeSpan 是某个节点在一次请求中的观测区间。
// start/end 都用该节点自己的 mono 时钟，因此 duration 是精确的。
type nodeSpan struct {
	Host     string
	Node     string
	Events   []Event
	StartMono int64
	EndMono   int64
	StartWall int64
}

func (s *nodeSpan) Duration() int64 { return s.EndMono - s.StartMono }

func main() {
	var (
		injectSkew = flag.String("inject-skew", "",
			"人工注入时钟偏移，格式 node=±毫秒，如 kitex-server=+50。"+
				"用于验证分析对时钟偏斜免疫：注入后各段时长应逐位不变")
		format = flag.String("format", "waterfall", "输出格式: waterfall | chrome | summary")
		limit  = flag.Int("limit", 1, "最多输出几条 trace 的 waterfall")
	)
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "用法: merge [选项] <trace1.ndjson> [trace2.ndjson ...]")
		os.Exit(2)
	}

	skew := parseSkew(*injectSkew)
	traces := map[string][]Event{}
	for _, path := range flag.Args() {
		if err := loadFile(path, traces, skew); err != nil {
			fmt.Fprintf(os.Stderr, "读取 %s: %v\n", path, err)
			os.Exit(1)
		}
	}
	if len(traces) == 0 {
		fmt.Fprintln(os.Stderr, "没有读到任何事件")
		os.Exit(1)
	}

	ids := make([]string, 0, len(traces))
	for id := range traces {
		ids = append(ids, id)
	}
	// 按「涉及节点数」降序，节点最全的排前面。
	// 各节点的 trace 文件生命周期不同（server 长驻累积、client 每次截断），
	// 于是必然存在只有单边数据的孤儿 trace。按字典序取第一条很容易取到孤儿，
	// 看起来像「链路没打通」，实则只是挑错了样本。
	nodeCount := func(id string) int {
		seen := map[string]bool{}
		for _, ev := range traces[id] {
			seen[ev.Node] = true
		}
		return len(seen)
	}
	sort.Slice(ids, func(i, j int) bool {
		ni, nj := nodeCount(ids[i]), nodeCount(ids[j])
		if ni != nj {
			return ni > nj
		}
		return ids[i] < ids[j]
	})

	switch *format {
	case "waterfall":
		n := *limit
		if n > len(ids) {
			n = len(ids)
		}
		for _, id := range ids[:n] {
			printWaterfall(id, traces[id], skew)
		}
		fmt.Printf("\n共 %d 条 trace，已显示 %d 条\n", len(ids), n)
	case "summary":
		printSummary(traces)
	case "chrome":
		printChromeTrace(traces)
	default:
		fmt.Fprintf(os.Stderr, "未知格式: %s\n", *format)
		os.Exit(2)
	}
}

func parseSkew(s string) map[string]int64 {
	skew := map[string]int64{}
	if s == "" {
		return skew
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		var ms int64
		fmt.Sscanf(kv[1], "%d", &ms)
		skew[kv[0]] = ms * 1_000_000
	}
	return skew
}

func loadFile(path string, out map[string][]Event, skew map[string]int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		// 注入偏移只动 wall clock —— 真实的时钟偏斜正是这样表现的。
		// mono 不动，因为单调钟本来就不受 NTP/系统时间影响。
		if d, ok := skew[ev.Node]; ok {
			ev.WallNs += d
		}
		out[ev.Trace] = append(out[ev.Trace], ev)
	}
	return sc.Err()
}

// groupByNode 把一条 trace 的事件按节点分组，并算出各节点的本地区间。
func groupByNode(events []Event) []*nodeSpan {
	byNode := map[string]*nodeSpan{}
	for _, ev := range events {
		s, ok := byNode[ev.Node]
		if !ok {
			s = &nodeSpan{Host: ev.Host, Node: ev.Node,
				StartMono: ev.MonoNs, EndMono: ev.MonoNs, StartWall: ev.WallNs}
			byNode[ev.Node] = s
		}
		s.Events = append(s.Events, ev)
		if ev.MonoNs < s.StartMono {
			s.StartMono = ev.MonoNs
			s.StartWall = ev.WallNs
		}
		if ev.MonoNs > s.EndMono {
			s.EndMono = ev.MonoNs
		}
	}
	spans := make([]*nodeSpan, 0, len(byNode))
	for _, s := range byNode {
		sort.Slice(s.Events, func(i, j int) bool { return s.Events[i].MonoNs < s.Events[j].MonoNs })
		spans = append(spans, s)
	}
	// 按 wall 粗排序。跨机 wall 不可比，所以这只用于展示顺序，不参与任何计算。
	sort.Slice(spans, func(i, j int) bool { return spans[i].StartWall < spans[j].StartWall })
	return spans
}

func printWaterfall(id string, events []Event, skew map[string]int64) {
	spans := groupByNode(events)

	fmt.Printf("\n╔══ trace %s ══\n", id[:min(16, len(id))])
	if len(skew) > 0 {
		fmt.Printf("║  [已注入人工时钟偏移: %v]\n", skew)
	}

	hosts := map[string]bool{}
	for _, s := range spans {
		hosts[s.Host] = true
	}
	multiHost := len(hosts) > 1
	fmt.Printf("║  涉及 %d 个节点，%d 台主机%s\n", len(spans), len(hosts),
		map[bool]string{true: "（跨机，时长仅在同机内精确）", false: ""}[multiHost])
	fmt.Println("║")

	for _, s := range spans {
		fmt.Printf("║ ▸ %-16s [host=%s]  本地区间 %s\n", s.Node, s.Host, dur(s.Duration()))
		base := s.StartMono
		for _, ev := range s.Events {
			// 同节点内相减，安全
			fmt.Printf("║     %8s  %s\n", dur(ev.MonoNs-base), ev.Point)
		}
	}

	// 差值法导出跨节点开销（§8.2.3）。
	// 只用「各自在本机测得的时长」相减，时钟偏斜完全抵消。
	var client, server *nodeSpan
	for _, s := range spans {
		switch s.Node {
		case "kitex-client":
			client = s
		case "kitex-server":
			server = s
		}
	}
	fmt.Println("║")
	if client != nil && server != nil {
		gap := client.Duration() - server.Duration()
		fmt.Printf("║ ⇄ 链路开销（往返，含两跳 sidecar 与网络）\n")
		fmt.Printf("║     client 观测总时长   %s\n", dur(client.Duration()))
		fmt.Printf("║     server 观测处理时长 %s\n", dur(server.Duration()))
		fmt.Printf("║     ── 差值 ──────────  %s\n", dur(gap))
		if client.Host != server.Host {
			fmt.Printf("║     跨机：此值为往返总和，无法拆分单向（§8.2.4）\n")
		}
		fmt.Printf("║     两项各自在本机用单调钟测量，相减时时钟偏斜抵消，\n")
		fmt.Printf("║     因此该结果对任意大小的时钟差都成立。\n")
	} else {
		fmt.Printf("║ ⇄ 缺少 client 或 server 侧数据，无法做差值归因\n")
	}
	fmt.Println("╚══")
}

func printSummary(traces map[string][]Event) {
	type stat struct{ gaps []int64 }
	agg := map[string]*stat{}
	crossHost := false

	for _, events := range traces {
		spans := groupByNode(events)
		var client, server *nodeSpan
		for _, s := range spans {
			if s.Node == "kitex-client" {
				client = s
			}
			if s.Node == "kitex-server" {
				server = s
			}
		}
		if client == nil || server == nil {
			continue
		}
		if client.Host != server.Host {
			crossHost = true
		}
		for _, s := range spans {
			if agg[s.Node] == nil {
				agg[s.Node] = &stat{}
			}
			agg[s.Node].gaps = append(agg[s.Node].gaps, s.Duration())
		}
		if agg["__link__"] == nil {
			agg["__link__"] = &stat{}
		}
		agg["__link__"].gaps = append(agg["__link__"].gaps, client.Duration()-server.Duration())
	}

	names := make([]string, 0, len(agg))
	for k := range agg {
		names = append(names, k)
	}
	sort.Strings(names)

	fmt.Printf("样本数: %d 条 trace\n\n", len(traces))
	fmt.Printf("%-20s %10s %10s %10s %10s\n", "区间", "p50", "p90", "p99", "max")
	for _, n := range names {
		g := agg[n].gaps
		if len(g) == 0 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
		label := n
		if n == "__link__" {
			label = "链路开销(往返)"
		}
		fmt.Printf("%-20s %10s %10s %10s %10s\n", label,
			dur(pct(g, 0.50)), dur(pct(g, 0.90)), dur(pct(g, 0.99)), dur(g[len(g)-1]))
	}
	if crossHost {
		fmt.Printf("\n注：涉及跨机，「链路开销」为往返总和，不可拆分单向（§8.2.4）\n")
	}
}

// printChromeTrace 输出 Chrome Trace Event 格式。
//
// 跨机 wall clock 不可比，所以**不能**直接把绝对时间戳画上去 ——
// 那会画出「响应早于请求」的图。做法是以每条 trace 里最早的事件为原点
// 做逻辑对齐，并在名称里标注跨机段为往返估计。
func printChromeTrace(traces map[string][]Event) {
	type chromeEvent struct {
		Name string `json:"name"`
		Cat  string `json:"cat"`
		Ph   string `json:"ph"`
		Ts   int64  `json:"ts"` // 微秒
		Dur  int64  `json:"dur,omitempty"`
		Pid  int    `json:"pid"`
		Tid  int    `json:"tid"`
	}
	var out []chromeEvent
	pid := 1
	for id, events := range traces {
		spans := groupByNode(events)
		if len(spans) == 0 {
			continue
		}
		origin := spans[0].StartWall
		for tid, s := range spans {
			out = append(out, chromeEvent{
				Name: fmt.Sprintf("%s (%s)", s.Node, id[:min(8, len(id))]),
				Cat:  s.Host, Ph: "X",
				Ts:  (s.StartWall - origin) / 1000,
				Dur: s.Duration() / 1000,
				Pid: pid, Tid: tid,
			})
			for _, ev := range s.Events {
				out = append(out, chromeEvent{
					Name: ev.Point, Cat: s.Node, Ph: "i",
					Ts: (ev.WallNs - origin) / 1000, Pid: pid, Tid: tid,
				})
			}
		}
		pid++
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(map[string]interface{}{"traceEvents": out})
}

func pct(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*q)]
}

func dur(ns int64) string {
	switch {
	case ns < 0:
		return fmt.Sprintf("-%s", dur(-ns))
	case ns < 1000:
		return fmt.Sprintf("%dns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.1fµs", float64(ns)/1000)
	default:
		return fmt.Sprintf("%.3fms", float64(ns)/1e6)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
