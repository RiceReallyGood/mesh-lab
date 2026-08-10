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
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
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
		format = flag.String("format", "waterfall", "输出格式: waterfall | detail | table | chrome | summary")
		limit  = flag.Int("limit", 1,
			"最多输出几条 trace。table 格式下 <=0 表示全部（聚合行始终基于全量样本）")
	)
	flag.Parse()

	// 区间定义的自检。放在最前面：宁可起不来，也不要输出被静默合并过的数据。
	if err := checkPhaseNames(phases()); err != nil {
		fmt.Fprintln(os.Stderr, "区间定义有误:", err)
		os.Exit(2)
	}
	if err := checkNetCols(phases(), netCols()); err != nil {
		fmt.Fprintln(os.Stderr, "网络单程列定义有误:", err)
		os.Exit(2)
	}

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
		printBreakdown(traces)
	case "detail":
		printDetail(traces)
	case "table":
		printTable(traces, ids, *limit)
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

// placedEvent 是摆到「本机统一时间轴」上的一个事件。
type placedEvent struct {
	Offset int64 // 相对本机原点的偏移
	Node   string
	Point  string
}

// printHostTimelines 按物理机分块，块内所有节点的事件合并成一条时间线。
//
// **为什么原点只能取 wall**：同机的两个进程共享 CLOCK_REALTIME，wall 在同机内可比；
// 而 mono 不可比 —— Go 侧的 monoBase 是**进程相对**的（tracer.go 里
// `monoBase = time.Now()`），Envoy 侧的 MonotonicTime 是**开机相对**的，
// 两者根本不是一个基准。
//
// **但精度仍由 mono 保证**：只有「节点起始点之间的相对位置」用 wall，
// 节点内部各点的偏移照旧用 mono 相减。所以相邻两点的时长精度不受任何影响，
// 只有跨节点的衔接处引入 wall 的读数抖动（同机实测在微秒级）。
//
// 跨机不做对齐：两台机器各自一块，中间画分隔。**不做任何形式的跨机原点估算**
// （例如按「上一跳 up_write_done + 半个往返」去摆对端）—— 那等于把估算值
// 画成事实，正是 §8.2 要防的操作。
func printHostTimelines(spans []*nodeSpan) {
	order := []string{}
	byHost := map[string][]*nodeSpan{}
	for _, s := range spans {
		if _, ok := byHost[s.Host]; !ok {
			order = append(order, s.Host)
		}
		byHost[s.Host] = append(byHost[s.Host], s)
	}

	// 主机之间**按角色排序，不按时钟** —— 请求发起方那台排前面。
	//
	// 不能沿用 spans 的 wall 排序：跨机 wall 差多少全看两台机器的时钟偏斜
	// （本环境实测 16 秒），排出来的先后与因果无关。实测就出现过
	// 920B 排在 950 前面 —— 而请求明明发起于 950，这是纯粹的假象。
	//
	// 判据取「哪台机器上有 kitex-client」。找不到就维持原顺序，
	// 并在标题里说明顺序不可靠。
	clientHost := ""
	for _, s := range spans {
		if s.Node == "kitex-client" {
			clientHost = s.Host
			break
		}
	}
	if clientHost != "" {
		reordered := []string{clientHost}
		for _, h := range order {
			if h != clientHost {
				reordered = append(reordered, h)
			}
		}
		order = reordered
	}

	for hi, host := range order {
		group := byHost[host]

		// 本机原点 = 该机上最早开始的那个节点的起始 wall
		originWall := group[0].StartWall
		for _, s := range group {
			if s.StartWall < originWall {
				originWall = s.StartWall
			}
		}

		placed := []placedEvent{}
		for _, s := range group {
			// 节点起始点相对本机原点的位置（用 wall，同机合法）
			nodeBase := s.StartWall - originWall
			for _, ev := range s.Events {
				// 节点内部偏移用 mono，精度不打折
				placed = append(placed, placedEvent{
					Offset: nodeBase + (ev.MonoNs - s.StartMono),
					Node:   s.Node,
					Point:  ev.Point,
				})
			}
		}
		sort.SliceStable(placed, func(i, j int) bool { return placed[i].Offset < placed[j].Offset })

		if hi > 0 {
			fmt.Println("║")
			fmt.Println("║  ╌╌╌ 跨机分隔：以下是另一台机器，时间轴独立，与上面不可对齐 ╌╌╌")
		}
		nodeNames := make([]string, 0, len(group))
		for _, s := range group {
			nodeNames = append(nodeNames, s.Node)
		}
		fmt.Printf("║ ┌─ host=%s  节点: %s   原点 = 本机最早事件\n", host, strings.Join(nodeNames, " + "))

		prevNode := ""
		prevOffset := int64(0)
		for i, p := range placed {
			// 换节点时标注与上一行的间隔。
			//
			// **措辞刻意保守**：这是「相邻两行之差」，不一定是因果配对。
			// 例如 client 写完 socket 后还会记 write_finish / read_start /
			// mesh_socket_read_start 三个点，才轮到 envoy 的第一个点 ——
			// 此时相邻行之差比真正的 UDS 传递时间小。要因果配对请自己挑两个点相减，
			// 工具不替你猜哪两个点构成一次传递。
			mark := ""
			if i > 0 && p.Node != prevNode {
				gap := p.Offset - prevOffset
				if gap < 0 {
					// 两个进程各自读 wall 的抖动，同机也可能出现小幅倒挂。
					// 标注出来，不假装它是负延迟。
					mark = "  ◀ 距上一行 <0（wall 抖动）"
				} else {
					mark = fmt.Sprintf("  ◀ 距上一行 %s", dur(gap))
				}
			}
			fmt.Printf("║ │ %9s  %-14s %s%s\n", dur(p.Offset), p.Node, p.Point, mark)
			prevNode = p.Node
			prevOffset = p.Offset
		}
		fmt.Println("║ └─")
	}
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

	// 主动检查 host 标识是否可疑。
	//
	// 光靠「正确配置 host」是不够的：本实验两台机器的 hostname 都是
	// localhost.localdomain，一旦忘记设 KITEX_PROBE_HOST，跨机会被
	// 静默误判成同机 —— 而跨机相减正是 §8.2 要防的最危险操作。
	// 与其信任配置，不如让工具自己起疑。
	for h := range hosts {
		if h == "localhost" || h == "localhost.localdomain" || h == "unknown" || h == "" {
			fmt.Printf("║  ⚠ host 标识为 %q，无法区分机器。\n", h)
			fmt.Printf("║    若本次为跨机部署，请给各节点设置 KITEX_PROBE_HOST，\n")
			fmt.Printf("║    否则跨机事件会被误判为同机，差值法的前提不再成立。\n")
			break
		}
	}
	fmt.Println("║")

	printHostTimelines(spans)

	// 本条 trace 的端到端分解。与 summary 用的是同一个 decompose()——
	// 两处各写一遍迟早会算出对不上的两个数。
	//
	// 单条上不需要「只能用 avg」那条限制：这是一条具体请求的实际用时，
	// 各段相加恰好等于端到端，不涉及分位数可加性。
	fmt.Println("║")
	rows, total, ok := decompose(spans)
	if !ok {
		fmt.Printf("║ ⇄ 链条不完整（需要 %v 四个节点齐全），无法分解\n", chainOrder)
		fmt.Println("╚══")
		return
	}
	fmt.Printf("║ ⇄ 本条 trace 的端到端分解（相加 = 端到端）\n")
	var acc int64
	for _, r := range rows {
		acc += r.v
		pct := float64(r.v) / float64(total) * 100
		// 单条上的「往返」可能为负：它是两次独立测量之差，噪声足以翻号。
		// 原样显示，不钳零 —— 钳了就看不出这条样本不该拿来单独下结论。
		note := ""
		if r.v < 0 {
			note = "   ← 为负：两次测量之差的噪声，单条不可据此下结论"
		}
		fmt.Printf("║     %s %10s  %5.1f%%%s\n", padRight(r.label, 30), dur(r.v), pct, note)
	}
	fmt.Printf("║     %s %10s  %5s\n", padRight(strings.Repeat("─", 15), 30), strings.Repeat("─", 8), "─────")
	fmt.Printf("║     %s %10s\n", padRight("合计（= client 总时长）", 30), dur(acc))
	if multiHost {
		fmt.Printf("║     跨机那一段为往返总和，无法拆分单向（§8.2.4）。\n")
	}
	fmt.Printf("║     每段都由两个「各自在本机测得的时长」相减而来，时钟偏斜完全抵消。\n")
	fmt.Println("╚══")
}

// dispWidth 返回字符串在等宽终端里占的列数。
//
// Go 的 %-30s 按**字节**补齐，而 CJK 字符是 3 字节却只占 2 列 ——
// 中英混排的标签用 %-Ns 必然对不齐，列越多歪得越厉害。
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && (r <= 0x115F || // 韩文字母
			r == 0x2329 || r == 0x232A ||
			(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) || // CJK 及部首
			(r >= 0xAC00 && r <= 0xD7A3) || // 韩文音节
			(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容
			(r >= 0xFE30 && r <= 0xFE6F) || // CJK 标点
			(r >= 0xFF00 && r <= 0xFF60) || // 全角
			(r >= 0xFFE0 && r <= 0xFFE6) ||
			(r >= 0x20000 && r <= 0x3FFFD)):
			w += 2
		default:
			w++
		}
	}
	return w
}

// padRight 按显示宽度右填充。宽度不够时不截断 —— 宁可这一行歪，也别把标签切掉。
func padRight(s string, width int) string {
	if d := width - dispWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// chainLink 描述端到端分解里的一段：要么是某个节点自身的处理，
// 要么是相邻两节点之间的一次往返传输。
type chainLink struct {
	label   string
	samples []int64
}

// waitOf 返回某节点「在等下一跳」的那段时间。
//
// 端到端分解的全部依据就是这一个量：**节点总时长 − 它等下一跳的时间 = 节点自身处理**，
// 而**它等下一跳的时间 − 下一跳的总时长 = 两者之间的往返传输**。
// 两式交替套用，从 client 一路推到 server，各段相加恰好等于端到端（望远镜求和）。
//
// Envoy 侧取「纯等待」（up_write_done → up_epoll_wake）而不是 ⑤→⑥：
// epoll 就绪之后的 readv、buffer、filter 派发是本跳自己的 CPU 时间，
// 算进「传输」会高估链路。
func waitOf(s *nodeSpan) (int64, bool) {
	at := map[string]int64{}
	for _, ev := range s.Events {
		if _, ok := at[ev.Point]; !ok {
			at[ev.Point] = ev.MonoNs
		}
	}
	var a, b string
	switch s.Node {
	case "kitex-client", "kitex-server":
		a, b = "mesh_socket_read_start", "mesh_first_byte"
	default: // Envoy 各跳
		a, b = "up_write_done", "up_epoll_wake"
	}
	x, oka := at[a]
	y, okb := at[b]
	if !oka || !okb || y < x {
		return 0, false
	}
	return y - x, true
}

// chainOrder 是请求流经的顺序。分解必须按这个顺序套用差值，
// 不能按 map 遍历或字典序。
var chainOrder = []string{"kitex-client", "envoy-out", "envoy-in", "kitex-server"}

// printBreakdown 输出端到端分解：各节点自身处理 + 各段往返传输，**相加等于端到端**。
//
// 这是对旧 summary 的替代。旧表给的是各节点的观测区间，而那些区间是**嵌套**的
// （client 套着 envoy-out，套着 envoy-in，套着 server），既不能相加也算不出占比，
// 只能看量级。分解表是一个划分，能加到 100%。
// decompRow 是端到端分解里的一段。
type decompRow struct {
	label string
	v     int64
}

// decompose 把**一条 trace** 分解成「各节点自身 + 各段往返」，返回值相加等于端到端。
//
// 抽出来是为了让聚合视图（summary）与单条视图（waterfall 末尾）用同一套算法 ——
// 两处各写一遍的话，迟早会算出对不上的两个数。
//
// 任一节点缺失或拿不到「等下一跳」的时间就返回 ok=false：分解是环环相扣的，
// 缺一环后面全部对不上，不如整条不算。
func decompose(spans []*nodeSpan) (rows []decompRow, total int64, ok bool) {
	byNode := map[string]*nodeSpan{}
	for _, s := range spans {
		byNode[s.Node] = s
	}
	chain := []*nodeSpan{}
	for _, n := range chainOrder {
		s, has := byNode[n]
		if !has {
			return nil, 0, false
		}
		chain = append(chain, s)
	}

	for i, s := range chain {
		if i == len(chain)-1 {
			// 最后一跳（server）不等任何人，总时长就是它自己的处理
			rows = append(rows, decompRow{s.Node + " 自身", s.Duration()})
			break
		}
		w, has := waitOf(s)
		if !has {
			return nil, 0, false
		}
		next := chain[i+1]
		rows = append(rows, decompRow{s.Node + " 自身", s.Duration() - w})
		kind := "UDS"
		if s.Host != next.Host {
			kind = "跨机"
		}
		rows = append(rows, decompRow{fmt.Sprintf("%s往返 %s↔%s", kind, s.Node, next.Node), w - next.Duration()})
	}
	return rows, chain[0].Duration(), true
}

func printBreakdown(traces map[string][]Event) {
	links := []*chainLink{}
	idx := map[string]*chainLink{}
	get := func(label string) *chainLink {
		if l, ok := idx[label]; ok {
			return l
		}
		l := &chainLink{label: label}
		idx[label] = l
		links = append(links, l)
		return l
	}
	totals := []int64{}

	for _, events := range traces {
		rows, total, ok := decompose(groupByNode(events))
		if !ok {
			continue
		}
		for _, r := range rows {
			get(r.label).samples = append(get(r.label).samples, r.v)
		}
		totals = append(totals, total)
	}

	fmt.Printf("样本数: %d 条 trace\n\n", len(traces))
	if len(totals) == 0 {
		fmt.Printf("链条不完整（需要 %v 四个节点齐全），无法做端到端分解\n\n", chainOrder)
		printNodeSpans(traces)
		return
	}

	fmt.Printf("── 端到端分解（avg，可相加）\n")
	total := mean(totals)
	var acc int64
	for _, l := range links {
		if len(l.samples) == 0 {
			continue
		}
		v := mean(l.samples)
		acc += v
		fmt.Printf("  %s %10s   %5.1f%%\n", padRight(l.label, 30), dur(v), float64(v)/float64(total)*100)
	}
	fmt.Printf("  %s %10s   %5s\n", padRight(strings.Repeat("─", 15), 30), strings.Repeat("─", 8), "─────")
	fmt.Printf("  %s %10s   %5.1f%%\n", padRight("合计", 30), dur(acc), float64(acc)/float64(total)*100)
	fmt.Printf("  %s %10s\n", padRight("端到端（client 总时长）", 30), dur(total))

	fmt.Printf("\n── 这两个量到底是什么\n")
	fmt.Printf("\n")
	fmt.Printf("  设「N 总」= 节点 N 观测到的最早点到最晚点，\n")
	fmt.Printf("    「N 等」= 节点 N 花在等下一跳身上的时间，取值为：\n")
	fmt.Printf("        Envoy 各跳     up_write_done → up_epoll_wake   （请求写出 → 响应把 epoll 唤醒）\n")
	fmt.Printf("        kitex-client   mesh_socket_read_start → mesh_first_byte\n")
	fmt.Printf("\n")
	fmt.Printf("  **「N 自身」= N 总 − N 等**\n")
	fmt.Printf("      N 真正占用 CPU 的时间：解析、路由、编解码、业务逻辑。\n")
	fmt.Printf("      **不含**它坐等下一跳的那段。链路末端（server）不等任何人，自身 = 总。\n")
	fmt.Printf("\n")
	fmt.Printf("  **「A↔B 往返」= A 等 − B 总**\n")
	fmt.Printf("      A 在等 B 的那段里，扣掉 B 自己干活的时间，剩下的就是数据在两者之间\n")
	fmt.Printf("      走了一个来回（去程 + 回程）。两项各自在本机用单调钟测得，\n")
	fmt.Printf("      相减时时钟偏斜完全抵消 —— 这是跨机也成立的原因（§8.2.3）。\n")
	fmt.Printf("\n")
	fmt.Printf("  两式交替套用即可从 client 一路推到 server，各段相加恰好等于端到端\n")
	fmt.Printf("  （望远镜求和：中间每个「N 总」都被加一次减一次）。\n")
	fmt.Printf("\n")
	fmt.Printf("  三条限制：\n")
	fmt.Printf("  1. **只有 avg 能这么相加，分位数不可加** —— 所以本表只给 avg（§9.3 ①）。\n")
	fmt.Printf("  2. **「往返」不可拆分单向**：跨机是物理限制（需 PTP + 硬件时间戳网卡）；\n")
	fmt.Printf("     同机 UDS 同理 —— 差值法给出的本来就是一个来回。\n")
	fmt.Printf("  3. **「往返」不是纯线路时间**：B 起算点之前的排队（listener/worker 队列、\n")
	fmt.Printf("     goroutine 调度）落在 B 的测量区间之外，全被这个差值吸收。\n")
	fmt.Printf("     实测 Envoy worker 从 384 压到 2 时，同机 UDS「往返」从 21µs 涨到 128µs——\n")
	fmt.Printf("     多出来的一百微秒是排队不是传输。低负载无排队时才近似真实传输时间。\n\n")

	printNodeSpans(traces)
}

// printNodeSpans 是旧 summary 的内容：各节点的观测区间。
// 保留它是因为这些数是差值法的原始输入，也是既有报告里引用的口径。
func printNodeSpans(traces map[string][]Event) {
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

	fmt.Printf("── 各节点观测区间（**嵌套，不可相加**，差值法的原始输入）\n")
	fmt.Printf("%-20s %10s %10s %10s %10s %10s\n", "区间", "avg", "p50", "p90", "p99", "max")
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
		fmt.Printf("%-20s %10s %10s %10s %10s %10s\n", label,
			dur(mean(g)), dur(pct(g, 0.50)), dur(pct(g, 0.90)), dur(pct(g, 0.99)), dur(g[len(g)-1]))
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

// mean 求算术平均。不要求输入有序。
//
// 为什么要和分位数并列看：
//
//   - avg 远大于 p50 说明分布右偏、尾部重（少数慢请求把均值拉高了）。
//     只看 p50 会低估系统的实际负担，只看 avg 会被离群点误导。
//   - **平均值可加，分位数不可加**（§9.3 ①）。各阶段 p50 之和 ≠ 总时长 p50，
//     但各阶段 avg 之和 ≈ 总时长 avg（期望的线性性）。
//     所以做「各段占比」这类加减法时，avg 才是数学上站得住的那个。
func mean(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	var sum int64
	for _, x := range v {
		sum += x
	}
	return sum / int64(len(v))
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


// ---------------------------------------------------------------------------
// detail:逐段命名区间 + 跨节点分层分解
//
// waterfall 给的是「各点相对本节点起始的偏移」，读者要自己做减法；
// summary 只给各节点总时长。两者都看不到「这一跳的时间具体花在哪个阶段」。
// detail 直接把相邻点之间的区间命名并统计。
// ---------------------------------------------------------------------------

// phase 定义一个命名区间：从 from 点到 to 点。
type phase struct {
	node string
	name string
	from string
	to   string
}

// 各节点内部的阶段划分。含义见 docs/probe-points.md。
//
// **顺序即请求流向**：client → envoy-out → envoy-in → server。
// detail 就是按这个顺序分节输出的，读者从上往下读就是一次请求的完整旅程。
// 原来是 envoy 两跳在前、kitex 两端在后，读者得自己在脑子里重排。
func phases() []phase {
	ph := []phase{}
	ph = append(ph, kitexClientPhases()...)
	ph = append(ph, envoyPhases("envoy-out")...)
	ph = append(ph, envoyPhases("envoy-in")...)
	ph = append(ph, kitexServerPhases()...)
	return ph
}

// envoyPhases 给出一跳 Envoy 的阶段划分。两跳的划分完全相同，只有节点名不同 ——
// 以前是照抄两份，改一处漏一处只是时间问题。
func envoyPhases(node string) []phase {
	return []phase{
		// ↓ 下游收包：请求进入本跳的 socket 边界。
		//   补这三段之前，本跳最早的点是 dn_first_byte，而它已经在 socket 读、
		//   buffer append、filter chain 派发**之后** —— 于是这一整段被算进了
		//   **上一跳的「纯等待」**里。上一跳那段占端到端 79%，却因此从未被分解过。
		{node, "⓪ 下游收包(到dn_first_byte)", "dn_epoll_wake", "dn_first_byte"},
		{node, "   ├ ★下游事件循环排队", "dn_epoll_wake", "dn_readv_start"},
		{node, "   ├ 下游readv系统调用", "dn_readv_start", "dn_readv_done"},
		{node, "   └ 下游buffer+filter派发", "dn_readv_done", "dn_first_byte"},
		{node, "①→② TTHeader解析", "dn_first_byte", "hdr_decoded"},
		{node, "②→③ 协议层解头", "hdr_decoded", "msg_begin"},
		{node, "③→④ 路由匹配", "msg_begin", "route_resolved"},
		{node, "④→ 取上游连接", "route_resolved", "up_conn_reused"},
		{node, "  编码", "up_conn_reused", "up_encode_done"},
		// **「写socket」只是入队**：ConnectionImpl::write() 做的是
		// write_buffer_->move(data) + activateFileEvents，真正的 writev 在
		// onWriteReady() 里由事件循环稍后执行。名字里写明，免得再被当成系统调用。
		{node, "  写socket(仅入队)", "up_encode_done", "up_socket_write_done"},
		{node, "  ↳ 入队→真正写出", "up_socket_write_done", "up_writev_start"},
		{node, "  ↳ ★writev系统调用", "up_writev_start", "up_writev_done"},
		{node, "⑤→⑥ ★等待上游", "up_write_done", "up_first_byte"},
		// ↓ 把上面那段「等待上游」拆开。⑤→epoll唤醒 才是真正在等，
		//   其余三段是本进程自己的开销，此前全被算作等待。
		{node, "   ├ 纯等待(到epoll就绪)", "up_write_done", "up_epoll_wake"},
		{node, "   ├ ★事件循环排队", "up_epoll_wake", "up_readv_start"},
		{node, "   ├ readv系统调用", "up_readv_start", "up_readv_done"},
		{node, "   └ buffer+filter派发", "up_readv_done", "up_first_byte"},
		{node, "⑥→⑦ 响应解码", "up_first_byte", "resp_decoded"},
		// ↓ 下游回写：响应离开本跳的 socket 边界。与上游侧的「编码/写socket」对称。
		{node, "⑦→⑧ 下游回写", "resp_decoded", "dn_socket_write_done"},
		{node, "   ├ 响应编码", "resp_decoded", "dn_encode_done"},
		{node, "   └ 写socket(下游)", "dn_encode_done", "dn_socket_write_done"},
	}
}

// kitexPhases 是 Kitex 两侧的阶段划分。client 与 server 用**同一套骨架**：
//
//	（唤醒 →）编码/读取 → 等待 → 解码 → handler → 编码 → 发送
//
// 两条纪律，都是被实测打脸后才立的：
//
//  1. **按真实时序排，不按写代码时想到的顺序。** 原来是声明顺序，于是 server 那栏
//     出现「payload解码」排在「业务handler」前面、「读取+解码(整段)」排在
//     「等待请求体」前面 —— 读者会以为那就是执行顺序。1000 条 trace 的中位偏移
//     排序见 docs/probe-points.md。
//
//  2. **父区间与子区间用缩进表达。** 「读取+解码(整段)」是 read_start→read_finish，
//     它**包含**等待对端、TTHeader 解码、等待响应体三段；平铺在一起会被误读成并列。
//
// 两组容易被名字误导的区间，都在 2026-08-10 改过名，理由记在这里：
//
//   - mesh_payload_codec_* 插在 encodePayload 前后，**只在发送路径上**，
//     所以两侧都叫「payload编码」。曾标成「payload解码」是错的 ——
//     client 侧实测落在 3.17µs（写 socket 之前），server 侧落在 11.13µs
//     （handler 之后），都在发送路径。
//
//   - wait_read_* 包住的是 unmarshalThriftData（见 kitex
//     pkg/remote/codec/thrift/thrift.go），**那是反序列化，不是等待**。
//     曾叫「等待请求体 / 等待响应体」，会让人以为主要开销在等字节，
//     实际是 thrift 解码（server 550ns / client 400ns，与编码同量级）。
//     所以**接收方向的反序列化一直有点位，只是名字骗人**。
//
// 服务端的 mesh_socket_read_start→mesh_first_byte 也**不叫「等待对端」**，
// 理由见下面 kitexServerPhases 里的注释。
func kitexClientPhases() []phase {
	return []phase{
		{"kitex-client", "取连接", "client_conn_start", "client_conn_finish"},
		{"kitex-client", "编码(发送前)", "write_start", "mesh_socket_write_start"},
		{"kitex-client", "   ├ TTHeader编码", "mesh_hdr_encode_start", "mesh_hdr_encode_finish"},
		{"kitex-client", "   └ payload编码", "mesh_payload_encode_start", "mesh_payload_encode_finish"},
				{"kitex-client", "写socket", "mesh_socket_write_start", "mesh_socket_write_finish"},
		// ↓ 把 netpoll 的 Flush 拆开。此前这一整段是个黑盒，
		//   「Go 侧写 socket 比 Envoy 贵 7–10 倍」只能观察到现象、无法归因。
		//   waited=true 的样本（走了 EPOLLOUT 慢路径）里最后一段混着等待，
		//   attrs 已带出，分析时按需过滤。
		{"kitex-client", "   ├ LinkBuffer整理+拼iovec", "mesh_np_flush_start", "mesh_np_writev_start"},
		{"kitex-client", "   ├ ★writev系统调用", "mesh_np_writev_start", "mesh_np_writev_done"},
		{"kitex-client", "   └ 缓冲区回收", "mesh_np_writev_done", "mesh_np_flush_done"},
		{"kitex-client", "读取+解码(整段)", "read_start", "read_finish"},
		{"kitex-client", "   ├ ★等待对端(纯网络)", "mesh_socket_read_start", "mesh_first_byte"},
		// ↓ netpoll 内部拆解。只在客户端可用（采样判定必须早于阻塞读，
		//   服务端此刻还读不到 TTHeader 里的 traceparent，见 probe-points.md §2.5）。
		//   横跨多轮读的快照在 netpoll 侧已被 Consistent 校验剔除，不会出现在这里。
		{"kitex-client", "   │  ├ 纯等待(到epoll就绪)", "mesh_socket_read_start", "mesh_np_epoll_wake"},
		{"kitex-client", "   │  ├ ★poller事件排队", "mesh_np_epoll_wake", "mesh_np_dispatch"},
		{"kitex-client", "   │  ├ readv系统调用", "mesh_np_readv_start", "mesh_np_readv_done"},
		{"kitex-client", "   │  ├ LinkBuffer入队", "mesh_np_readv_done", "mesh_np_trigger"},
		{"kitex-client", "   │  └ ★goroutine调度延迟", "mesh_np_trigger", "mesh_first_byte"},
		{"kitex-client", "   ├ TTHeader解码", "mesh_hdr_decode_start", "mesh_hdr_decode_finish"},
		{"kitex-client", "   ├ 协议层解消息头+校验", "mesh_hdr_decode_finish", "wait_read_start"},
		{"kitex-client", "   ├ payload反序列化", "wait_read_start", "wait_read_finish"},
		{"kitex-client", "   └ 缓冲区释放+收尾", "wait_read_finish", "read_finish"},

	}
}

// kitexServerPhases 与 client 共用上面那套骨架，见 kitexClientPhases 的注释。
func kitexServerPhases() []phase {
	return []phase{
		// ↓ netpoll 内部拆解。2026-08-10 起服务端也有了 —— 与客户端的区别只在开关：
		//   客户端能把探针精确圈在自己那次阻塞读上（发起前 traceparent 已知），
		//   服务端 OnRead 时读已做完，所以探针在**连接级常开**，OnRead 入口取快照。
		//   由此白捡一个客户端没有的点：trigger→onread 就是服务端的调度延迟。
		{"kitex-server", "⓪ netpoll收包(到OnRead)", "mesh_np_epoll_wake", "mesh_netpoll_onread"},
		{"kitex-server", "   ├ ★poller事件排队", "mesh_np_epoll_wake", "mesh_np_dispatch"},
		{"kitex-server", "   ├ readv系统调用", "mesh_np_readv_start", "mesh_np_readv_done"},
		{"kitex-server", "   ├ LinkBuffer入队", "mesh_np_readv_done", "mesh_np_trigger"},
		{"kitex-server", "   └ ★goroutine调度延迟", "mesh_np_trigger", "mesh_netpoll_onread"},
		{"kitex-server", "OnRead→开始读", "mesh_netpoll_onread", "mesh_socket_read_start"},
		{"kitex-server", "读取+解码(整段)", "read_start", "read_finish"},
		// ↓ **不叫「等待对端」**：服务端的 OnRead 只在 netpoll 已把数据放进 LinkBuffer
		//   之后才触发，此处 Peek 立即返回，没有任何网络等待（实测 310ns）。
		//   服务端真正的「等对端」发生在两个请求之间的空闲期，不属于这条 RPC。
		{"kitex-server", "   ├ 取首字节(Peek)", "mesh_socket_read_start", "mesh_first_byte"},
		{"kitex-server", "   ├ TTHeader解码", "mesh_hdr_decode_start", "mesh_hdr_decode_finish"},
		{"kitex-server", "   ├ 协议层解消息头+校验", "mesh_hdr_decode_finish", "wait_read_start"},
		{"kitex-server", "   ├ payload反序列化", "wait_read_start", "wait_read_finish"},
		{"kitex-server", "   └ 缓冲区释放+收尾", "wait_read_finish", "read_finish"},
		{"kitex-server", "业务handler", "server_handle_start", "server_handle_finish"},
		{"kitex-server", "编码(发送前)", "write_start", "mesh_socket_write_start"},
		{"kitex-server", "   ├ TTHeader编码", "mesh_hdr_encode_start", "mesh_hdr_encode_finish"},
		{"kitex-server", "   └ payload编码", "mesh_payload_encode_start", "mesh_payload_encode_finish"},
				{"kitex-server", "写socket", "mesh_socket_write_start", "mesh_socket_write_finish"},
		// ↓ 把 netpoll 的 Flush 拆开。此前这一整段是个黑盒，
		//   「Go 侧写 socket 比 Envoy 贵 7–10 倍」只能观察到现象、无法归因。
		//   waited=true 的样本（走了 EPOLLOUT 慢路径）里最后一段混着等待，
		//   attrs 已带出，分析时按需过滤。
		{"kitex-server", "   ├ LinkBuffer整理+拼iovec", "mesh_np_flush_start", "mesh_np_writev_start"},
		{"kitex-server", "   ├ ★writev系统调用", "mesh_np_writev_start", "mesh_np_writev_done"},
		{"kitex-server", "   └ 缓冲区回收", "mesh_np_writev_done", "mesh_np_flush_done"},
	}
}

// checkPhaseNames 保证同一节点下不出现重名区间。
//
// detail / table 都用 `node|name` 当聚合键，重名不会报错，而是**静默把两个
// 不同区间的样本合进同一个桶** —— 输出看起来完全正常，只有 n= 翻倍这一个
// 微弱线索。补下游点位时就踩了一次：新加的「buffer+filter派发」与上游同名，
// 两处显示成一模一样的数值、n=2000。
//
// 与其指望下次还能看出 n= 不对，不如让它直接起不来。
// phaseKey 把区间名归一成稳定的标识：剥掉树形缩进（空格与 ├└│─ 这些画线字符）。
//
// 区间名里带缩进是为了在 detail 里表达父子关系，但那纯属显示。**任何按名字
// 做的查找都必须先归一**，否则调整一次缩进就会静默改掉键 —— 踩过一次：
// 「★等待对端(纯网络)」挪到「读取+解码(整段)」底下加了 "   ├ " 前缀，
// netCols() 那边还写着旧名，于是 net.client↔envoy-out 整列 NaN、29 行全空。
// 光 TrimSpace 不够，├ 不是空白字符。
func phaseKey(s string) string {
	return strings.TrimSpace(strings.TrimLeft(s, " \t│├└─"))
}

func checkPhaseNames(ph []phase) error {
	seen := map[string]bool{}
	trimmed := map[string]bool{}
	for _, p := range ph {
		key := p.node + "|" + p.name
		if seen[key] {
			return fmt.Errorf("区间重名: %s —— 同名会被静默合并成一个桶，必须改名", key)
		}
		seen[key] = true
		// 去掉树形缩进后也必须唯一：table 的列名走 colName()，那里会 TrimSpace，
		// 两个只差缩进的区间会产生同一个 CSV 列，后者静默覆盖前者。
		tk := p.node + "|" + phaseKey(p.name)
		if trimmed[tk] {
			return fmt.Errorf("区间去缩进后重名: %s —— CSV 列名会撞车，必须改名", tk)
		}
		trimmed[tk] = true
	}
	return nil
}

// checkNetCols 保证每个网络单程列都能在 phases() 里找到它依赖的区间。
//
// 这两处是**分开写的字符串**，改了一边忘了另一边不会报错，只会让整列变空。
// 实测踩过：detail 把「★等待对端(纯网络)」挪到「读取+解码(整段)」底下、
// 名字前加了树形缩进，而 netCols() 还写着没缩进的旧名 ——
// net.client↔envoy-out单程(UDS) 整列 NaN，29 行全空，没有任何提示。
//
// 现在查找已按去缩进后的名字匹配，这道自检再挡一层拼写与改名。
func checkNetCols(ph []phase, nc []netCol) error {
	have := map[string]bool{}
	nodes := map[string]bool{}
	for _, p := range ph {
		have[p.node+"|"+phaseKey(p.name)] = true
		nodes[p.node] = true
	}
	for _, c := range nc {
		if !have[c.outerNode+"|"+phaseKey(c.outerPhase)] {
			return fmt.Errorf("%s 依赖的区间不存在: %s|%s", c.name, c.outerNode, c.outerPhase)
		}
		if !nodes[c.innerNode] {
			return fmt.Errorf("%s 依赖的内层节点不存在: %s", c.name, c.innerNode)
		}
	}
	return nil
}

func printDetail(traces map[string][]Event) {
	// 收集每个阶段的样本
	samples := map[string][]int64{}
	nodeTotal := map[string][]int64{}
	nodeHost := map[string]string{}
	ph := phases()

	for _, events := range traces {
		spans := groupByNode(events)
		for _, s := range spans {
			nodeTotal[s.Node] = append(nodeTotal[s.Node], s.Duration())
			nodeHost[s.Node] = s.Host
			at := map[string]int64{}
			for _, ev := range s.Events {
				// 同名点取最早一次，避免重试等场景重复
				if _, ok := at[ev.Point]; !ok {
					at[ev.Point] = ev.MonoNs
				}
			}
			for _, p := range ph {
				if p.node != s.Node {
					continue
				}
				a, oka := at[p.from]
				b, okb := at[p.to]
				if oka && okb && b >= a {
					key := p.node + "|" + p.name
					samples[key] = append(samples[key], b-a)
				}
			}
		}
	}

	fmt.Printf("样本数: %d 条 trace\n\n", len(traces))

	// 1) 各节点内部阶段
	lastNode := ""
	for _, p := range ph {
		key := p.node + "|" + p.name
		v := samples[key]
		if len(v) == 0 {
			continue
		}
		if p.node != lastNode {
			fmt.Printf("── %s [host=%s]  总时长 avg=%s p50=%s\n", p.node, nodeHost[p.node],
				meanOf(nodeTotal[p.node]), durOf(nodeTotal[p.node], 0.50))
			lastNode = p.node
		}
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		fmt.Printf("     %-22s avg=%-10s p50=%-10s p90=%-10s p99=%-10s (n=%d)\n",
			p.name, dur(mean(v)), dur(pct(v, 0.50)), dur(pct(v, 0.90)), dur(pct(v, 0.99)), len(v))
	}

	// 2) 跨节点分层分解（差值法，§8.2.3）
	fmt.Printf("\n── 跨节点分层分解（差值法，对时钟偏斜免疫）\n")
	chain := [][2]string{
		{"kitex-client", "envoy-out"},
		{"envoy-out", "envoy-in"},
		{"envoy-in", "kitex-server"},
	}
	for _, c := range chain {
		outer, inner := nodeTotal[c[0]], nodeTotal[c[1]]
		if len(outer) == 0 || len(inner) == 0 {
			continue
		}
		sort.Slice(outer, func(i, j int) bool { return outer[i] < outer[j] })
		sort.Slice(inner, func(i, j int) bool { return inner[i] < inner[j] })
		// 用各自的 p50 相减：两项都在本机测得，偏斜抵消
		diff := pct(outer, 0.50) - pct(inner, 0.50)
		// avg 同理相减。与 p50 不同的是**均值可加**，所以做占比/加总时用这一列
		diffAvg := mean(outer) - mean(inner)
		cross := ""
		if nodeHost[c[0]] != nodeHost[c[1]] {
			cross = "  [跨机→往返，不可拆单向]"
		}
		fmt.Printf("     %-14s − %-14s = avg %-10s p50 %-10s%s\n",
			c[0], c[1], dur(diffAvg), dur(diff), cross)
	}
	fmt.Printf("\n     解读：每项 = 外层节点自身处理 + 到内层节点的往返传输。\n")
	fmt.Printf("     两项各自在本机用单调钟测量，相减时时钟偏斜完全抵消。\n")
	fmt.Printf("     做加减法/算占比请用 avg 列：均值可加，分位数不可加（§9.3 ①）。\n")
}

// meanOf 是 mean 的字符串封装，空切片返回 NA，与 durOf 对称。
func meanOf(v []int64) string {
	if len(v) == 0 {
		return "NA"
	}
	return dur(mean(v))
}

func durOf(v []int64, q float64) string {
	if len(v) == 0 {
		return "NA"
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return dur(pct(s, q))
}

// ───────────────────────── table 格式 ─────────────────────────
//
// 一行一条 trace，一列一个时间段，单位微秒。
// 开头四行是全量样本的 avg / p50 / p90 / p99。
//
// 与 detail 的区别：detail 只给分位数，看不到单条请求内部各段的相关性
// （比如「尾延迟那些请求，是卡在排队还是卡在网络」）。table 保留原始逐条数据，
// 便于导进表格或 pandas 自己筛。

// netCol 是一跳的单程传输时间估算。
//
// **两重不确定性，用之前必须知道。**
//
// 其一，它不是纯线路时间。内层节点的「总时长」有一个起算点，早于它的一切
// 都落在内层的测量区间之外，于是全被这个差值吸收进来。实测把 Envoy worker
// 从 384 压到 2，同机 UDS 的「单程」从 21µs 涨到 128µs：多出来的一百微秒
// 是排队，不是传输。想看纯传输，只能在低负载、无排队时读这一列。
//
// **2026-08-10 起这个起算点前移了**：Envoy 节点从 dn_first_byte 改成
// dn_epoll_wake（补了下游读点位），于是「下游 epoll 唤醒 → readv → filter 派发」
// 这一段不再被吸收，改由内层自己认领。剩下仍被吸收的是 listener/worker 队列
// 里的等待 —— 即 epoll 就绪**之前**的部分。
// **跨版本比较 net.* 列时要注意这个口径变化**：新口径的值会比旧口径小一些
// （实测跨机双跳小 4.2µs），不是网络变快了。
//
// 其二，差值法只能得到往返：
//
//	外层节点的「等待」 − 内层节点的总时长 = 往返传输时间
//
// 再除以 2 得单程，隐含假设是**去程与回程对称**。这个假设在同机 UDS 上
// 基本成立，在跨机链路上不一定 —— 上下行路径、队列深度、网卡中断合并
// 都可能不对称。要真正拆开单向，需要 PTP + 硬件时间戳网卡，
// 那是物理限制，不是插桩能力问题（见 docs/probe-coverage-audit.md §4）。
//
// 单条 trace 上这个差值可能为负：外层的「等待」与内层的「总时长」是两次
// 独立测量，各自有噪声，当往返时间本身很小时噪声会盖过它。
// 聚合分位数不受影响，但逐条看时负值是正常的，不是 bug。
type netCol struct {
	name       string
	outerNode  string
	outerPhase string // 外层「等待下游」那一段的阶段名
	innerNode  string // 内层节点，取其总时长
}

func netCols() []netCol {
	return []netCol{
		{"net.client↔envoy-out单程(UDS)", "kitex-client", "★等待对端(纯网络)", "envoy-out"},
		{"net.envoy-out↔envoy-in单程(跨机)", "envoy-out", "⑤→⑥ ★等待上游", "envoy-in"},
		{"net.envoy-in↔server单程(UDS)", "envoy-in", "⑤→⑥ ★等待上游", "kitex-server"},
	}
}

// colName 把阶段名清理成适合做表头的形式：去掉序号、树线、星号。
func colName(node, phase string) string {
	s := phase
	for _, junk := range []string{"①→②", "②→③", "③→④", "④→⑤", "⑤→⑥", "⑥→⑦", "④→", "├", "└", "★"} {
		s = strings.ReplaceAll(s, junk, "")
	}
	return node + "." + phaseKey(s)
}

func printTable(traces map[string][]Event, ids []string, limit int) {
	ph := phases()
	nc := netCols()

	// 列顺序：节点总时长 → 各节点内部阶段 → 网络单程
	var cols []string
	seenNode := map[string]bool{}
	for _, p := range ph {
		if !seenNode[p.node] {
			cols = append(cols, p.node+".总时长")
			seenNode[p.node] = true
		}
		cols = append(cols, colName(p.node, p.name))
	}
	for _, c := range nc {
		cols = append(cols, c.name)
	}

	// 逐条 trace 求值。缺列用 NaN 表示，聚合时跳过，输出时留空。
	rows := make(map[string][]float64, len(traces))
	for id, events := range traces {
		vals := make([]float64, len(cols))
		for i := range vals {
			vals[i] = math.NaN()
		}
		idx := map[string]int{}
		for i, c := range cols {
			idx[c] = i
		}

		nodeTotal := map[string]int64{}
		nodePhase := map[string]int64{} // node|phase → 时长
		for _, s := range groupByNode(events) {
			nodeTotal[s.Node] = s.Duration()
			if i, ok := idx[s.Node+".总时长"]; ok {
				vals[i] = float64(s.Duration()) / 1000
			}
			at := map[string]int64{}
			for _, ev := range s.Events {
				if _, ok := at[ev.Point]; !ok {
					at[ev.Point] = ev.MonoNs
				}
			}
			for _, p := range ph {
				if p.node != s.Node {
					continue
				}
				a, oka := at[p.from]
				b, okb := at[p.to]
				if oka && okb && b >= a {
					nodePhase[p.node+"|"+phaseKey(p.name)] = b - a
					if i, ok := idx[colName(p.node, p.name)]; ok {
						vals[i] = float64(b-a) / 1000
					}
				}
			}
		}

		// 网络单程 = (外层等待 − 内层总时长) / 2
		for _, c := range nc {
			wait, okw := nodePhase[c.outerNode+"|"+phaseKey(c.outerPhase)]
			inner, oki := nodeTotal[c.innerNode]
			if okw && oki {
				vals[idx[c.name]] = float64(wait-inner) / 2 / 1000
			}
		}
		rows[id] = vals
	}

	// 聚合行始终基于全量样本，与 -limit 无关。
	agg := func(q float64, mean bool) []string {
		out := make([]string, len(cols))
		for i := range cols {
			var v []float64
			var sum float64
			for _, r := range rows {
				if !math.IsNaN(r[i]) {
					v = append(v, r[i])
					sum += r[i]
				}
			}
			if len(v) == 0 {
				out[i] = ""
				continue
			}
			if mean {
				out[i] = fmt.Sprintf("%.3f", sum/float64(len(v)))
				continue
			}
			sort.Float64s(v)
			k := int(float64(len(v)-1)*q + 0.5)
			out[i] = fmt.Sprintf("%.3f", v[k])
		}
		return out
	}

	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	fmt.Printf("# 单位: 微秒(µs)。样本 %d 条 trace。\n", len(traces))
	fmt.Printf("# 前四行为全量样本的聚合值，与 -limit 无关。\n")
	fmt.Printf("# net.* = (外层等待 − 内层总时长) / 2。**不是纯线路时间**：内层总时长从该节点最早的点起算\n")
	fmt.Printf("#   （Envoy 侧 2026-08-10 起是 dn_epoll_wake，此前是 dn_first_byte），\n")
	fmt.Printf("#   更早的 listener/worker 队列等待落在测量区间之外，于是被算进了这一列。\n")
	fmt.Printf("#   高负载下它以排队为主 —— 实测把 Envoy worker 从 384 压到 2，UDS 单程从 21µs 涨到 128µs。\n")
	fmt.Printf("#   除以 2 还隐含「去程回程对称」的假设。单条为负属正常，见源码注释。\n")
	fmt.Printf("# 注意分位数不可加：各列 p50 之和 ≠ 总时长 p50。\n")

	_ = w.Write(append([]string{"trace_id"}, cols...))
	_ = w.Write(append([]string{"__avg__"}, agg(0, true)...))
	_ = w.Write(append([]string{"__p50__"}, agg(0.50, false)...))
	_ = w.Write(append([]string{"__p90__"}, agg(0.90, false)...))
	_ = w.Write(append([]string{"__p99__"}, agg(0.99, false)...))

	n := limit
	if n <= 0 || n > len(ids) {
		n = len(ids)
	}
	for _, id := range ids[:n] {
		rec := make([]string, 0, len(cols)+1)
		rec = append(rec, id)
		for _, v := range rows[id] {
			if math.IsNaN(v) {
				rec = append(rec, "")
			} else {
				rec = append(rec, fmt.Sprintf("%.3f", v))
			}
		}
		_ = w.Write(rec)
	}
}
