# Kitex × Envoy Sidecar 端到端时延归因 —— 测试报告

- 日期：2026-08-07（取代 2026-08-06 版）
- 对应设计：`docs/specs/2026-08-06-kitex-envoy-e2e-trace-design.md`
- 打点清单：`docs/probe-points.md`
- 复现步骤：`docs/runbook-reproduce.md`
- **原始 merge 输出：`docs/results/2026-08-07/`**（每个结论都能回查到具体文件，见附录 A）

**与上一版的差别**：上一版只有跨机双跳一个点，且 client 的「等待对端」（195.9 µs）
和 Envoy 的「等待上游」（156.3 µs）还是两个黑盒。本版做了两件事：

1. **加了归因阶梯** —— 直连 / 单跳 / 双跳三级，三级都跨机，每级只多一个 sidecar，
   所以级差就是「加一个 sidecar 的净代价」。上一版没有直连基线，
   「双跳贵多少」只能对着一个绝对值猜。
2. **把两个黑盒拆开了** —— netpoll 读路径 5 个点位 + Envoy 事件循环 3 个点位
   已经实跑验证并有实测数据。上一版把这些列在「尚未覆盖」里。

---

## 0. 一页结论

**低负载（c=1）跨机三级阶梯，payload 64 B，p50：**

| | 端到端 | 比直连多 | 相对直连 |
|---|---:|---:|---:|
| **直连**（无 mesh） | **147.3 µs** | — | — |
| **单跳**（出向 sidecar） | **163.2 µs** | **+15.9 µs** | +10.8 % |
| **双跳**（完整 mesh） | **228.2 µs** | **+80.9 µs** | **+54.9 %** |

五条结论：

1. **完整 mesh 的代价是 +80.9 µs，即 +54.9 %。** 其中出向 sidecar 只占 15.9 µs，
   入向 sidecar 占 65.0 µs —— **两跳严重不对称，4.1 倍。**

2. **不对称是机器差异，不是入向路径更贵。** 跨机时 envoy-in 的**每一个阶段**都比
   envoy-out 慢 1.6~2.4 倍（连纯 CPU 的编解码都是）。把两个 Envoy 放到同一台机器上
   跑同样的参数，两者各阶段耗时**逐项相等**（编码 5.0 vs 5.0 µs，解析 3.1 vs 3.1 µs）。
   见 §6 的对照实验。

3. **加出向 sidecar 之所以只要 15.9 µs，是因为它同时替客户端省了钱。**
   client 自身开销从 27.9 µs 降到 10.9 µs（−17 µs）—— 它不再直接读跨机 TCP，
   改读本机 UDS，`写socket` 从 7.8 µs 降到 1.8 µs，
   **goroutine 调度延迟从 11.4 µs 降到 2.8 µs（4.1 倍）**。sidecar 自身花掉的 16.3 µs
   几乎被这笔节省抵消，净增只有 15.9 µs。这条上一版完全看不到。

4. **业务 handler 240 ns。** 三级拓扑下逐项一致（240/240/220 ns）。
   测的几乎全是框架与传输开销，没有业务噪声。

5. **Envoy 自身处理是稳定的**：envoy-out 在单跳和双跳里分别是 16.3 µs 和 15.9 µs，
   差 2.5 %。**加一跳不会让已有那跳变慢**，代价是可加的。

> **重要限制**：以上分解基于 p50，而**分位数不可加**。各项之和与端到端 p50 会差几个
> µs，属正常。这些数字用于判断**量级与占比**，不可当作精确的加法分解。

---

## 1. 测试环境

| | suzhou950（主调侧） | suzhou920B（被调侧） |
|---|---|---|
| 角色 | kitex-client + envoy-out | envoy-in + kitex-server |
| 平台 | openEuler 24.03 SP3，**aarch64**，物理机 | openEuler **22.03** SP3，**aarch64**，物理机 |
| 内核 | **6.6.0** | **5.10.0** |
| CPU / 内存 | **384 核 / 1379 GB** | **160 核 / 502 GB** |
| glibc | 2.38 | 2.34 |
| 地址 | 192.168.25.145 | 192.168.25.51 |
| 外网 | 有（经代理） | **无** |

- 两机 `systemd-detect-virt` 均为 `none`，非虚机
- 两机 RTT **0.068 ms**（`ping -c 3` min/avg/max = 0.047/0.068/0.104 ms），0 % 丢包
- **两机时钟相差 −15.46 秒**（950 未启用 NTP）。分析对此免疫，见 §2.4
- 920B 开着 **firewalld**，只有 15006 放行，且无 root 改不了规则 —— 三级拓扑共用此端口

**软件**：Envoy 1.40.0-dev（自建，`448c7f0f`，含 TTHeader transport 与探针）、
Kitex v0.16.3（自建，`98311385`）、netpoll（`64d86026`）、Go 1.26.5、bazel 8.7.0。
**两台机器跑的是同一个 `envoy-static` 二进制**（在 950 上编译，rsync 到 920B）——
这是 §6 能把差异归给机器的前提。

**拓扑（三级阶梯，全部跨机）**：

```
直连   client(950) ────────────────TCP:15006──────────────▶ server(920B)

单跳   client(950) ─UDS─▶ envoy-out(950) ──TCP:15006──────▶ server(920B)

双跳   client(950) ─UDS─▶ envoy-out(950) ──TCP:15006──▶ envoy-in(920B) ─UDS─▶ server(920B)
```

全程 **TTHeader + Thrift Binary**，不降级。payload 64 B。

---

## 2. 方法论

### 2.1 归因阶梯为什么必须三级都跨机

**不能在一台机器上做这个阶梯。** 同机的话每加一跳就多一组 Envoy 进程
（默认每个按核数开 worker，950 上是 384 个线程）挤同一批 CPU，
三级的资源竞争程度根本不同，**级差里混着 CPU 争抢，不能归给「多一跳」**。

跨机版三级都保留同一段跨机 TCP，每级只多一个 sidecar，级差才是干净的。
三级还共用同一个 TCP 端口（15006），防火墙、路由、conntrack 行为完全一致。

**三级的 `ENVOY_CONCURRENCY` 必须取同一个值**（本次为 4）。不固定的话两级默认
按核数开 worker，线程规模差异会混进级差。

### 2.2 有一处三级之间无法拉齐，读数时要知道

双跳下 server 是 **UDS 监听、对端是本机的 envoy-in**；直连和单跳下它是
**TCP 监听、对端是跨机的 950**。

**这不是配置疏漏，而是「加一个入向 sidecar」的定义本身** —— 入向 sidecar 的作用就是
终结远端连接、在本地重新发起。但看 `kitex-server` 那一节时要知道三级之间它的处境不同
（双跳下 server 总时长 17.7 µs < 单跳 21.3 µs < 直连 23.2 µs，正是因为它的对端越来越近）。

### 2.3 必须交错轮次，不能一组跑完再跑另一组

本次是 **3 轮 × 3 拓扑 × 2 负载 = 18 组，按 r1(直连→单跳→双跳) → r2 → r3 交错**，
每组 20 秒，三轮合并分析。

不交错的话后跑的那组会白占缓存预热和 CPU 频率爬升的便宜。
这个坑踩过 —— 当时得出过「加了插桩反而快 4.9 %」这种物理上不可能的结论。

交错有效的证据是三轮之间高度一致：

| | r1 | r2 | r3 |
|---|---:|---:|---:|
| 直连 QPS | 6614 | 6464 | 6294 |
| 单跳 QPS | 6021 | 6046 | 5884 |
| 双跳 QPS | 4455 | 4212 | 4274 |

18 组全部 **失败 = 0**。

### 2.4 时钟：跨机绝对时间戳不可相减

两机墙钟差 **15.46 秒**。分析用差值法（两个各自在本机用单调钟测得的时长相减），
对偏斜免疫。

**本次验证**：注入额外 50 秒人工偏移后，`detail` 输出与不注入时**逐字节相同**
（`docs/results/2026-08-07/clock-skew-check.txt`）。

```bash
./bin/merge -format detail --inject-skew kitex-server=+50 $FILES > /tmp/skew.txt
./bin/merge -format detail                              $FILES > /tmp/noskew.txt
diff -q /tmp/skew.txt /tmp/noskew.txt      # 一致
```

唯一受影响的是 `waterfall` 的视觉呈现（时间轴错乱）。

### 2.5 归因必须在非饱和区

饱和后排队延迟主导端到端时间，「时间花在哪」会退化成「在队列里等」，对归因无意义。
本次主结论全部取自 **c=1**（无排队）。c=16 那组单列在 §5，用于观察排队点位，
**不用于阶梯的级差计算**。

---

## 3. 归因阶梯（低负载 c=1）

**样本：直连 19,495 条 / 单跳 18,007 条 / 双跳 12,783 条 trace。**

### 3.1 三级总览

各节点本地总时长 p50：

| 节点 | 直连 | 单跳 | 双跳 |
|---|---:|---:|---:|
| kitex-client | **147.3 µs** | **163.2 µs** | **228.2 µs** |
| envoy-out | — | 131.3 µs | 197.0 µs |
| envoy-in | — | — | 83.1 µs |
| kitex-server | 23.2 µs | 21.3 µs | 17.7 µs |

原始输出：`results/2026-08-07/{direct,single,two}-lo-summary.txt`

### 3.2 加一个 sidecar 要多少钱

把「自身处理」定义为 `节点总时长 − 该节点的等待时间`：

| | 直连 | 单跳 | 双跳 |
|---|---:|---:|---:|
| client 自身 | **27.9 µs** | **10.9 µs** | **10.5 µs** |
| envoy-out 自身 | — | **16.3 µs** | **15.9 µs** |
| envoy-in 自身 | — | — | **27.6 µs** |
| server 自身（总时长） | 23.2 µs | 21.3 µs | 17.7 µs |
| **端到端** | **147.3** | **163.2** | **228.2** |

两个读法：

**① envoy-out 自身在单跳和双跳里几乎相同（16.3 vs 15.9 µs，差 2.5 %）。**
说明加第二跳不会让第一跳变慢，sidecar 的代价是可加的 —— 容量规划时可以线性外推。

**② 加出向 sidecar 只要 15.9 µs，因为它同时替客户端省了钱。**

| client 侧 | 直连 | 单跳 | 变化 |
|---|---:|---:|---|
| 写 socket | **7.8 µs** | **1.8 µs** | **−77 %**（跨机 TCP → 本机 UDS）|
| **goroutine 调度延迟** | **11.4 µs** | **2.8 µs** | **−75 %** |
| readv 系统调用 | 2.9 µs | 2.4 µs | −17 % |
| TTHeader 解码 | 3.2 µs | 720 ns | −78 % |
| client 自身合计 | 27.9 µs | 10.9 µs | **−17 µs** |

**直连时客户端自己在干重活**：直接读写跨机 TCP socket，被网卡中断唤醒，
goroutine 调度延迟 11.4 µs。挂上本机 sidecar 之后，客户端只跟本机 UDS 打交道，
这些成本转移给了 Envoy —— 而 Envoy 干同样的事只花 16.3 µs。

净账：`+16.3（sidecar 自身） − 17.0（client 省下） + 16.6（多一次进程间往返） ≈ +15.9 µs`。

> 按 §0 的限制，这是量级说明不是精确加法。

**③ 入向 sidecar 贵 4.1 倍（65.0 vs 15.9 µs）。** 原因见 §6 —— 是机器不是路径。

### 3.3 逐节点分解

完整逐阶段数据在 `results/2026-08-07/*-lo-detail.txt`，这里只摘关键行。

**kitex-server（三级对比，p50）** —— 除对端远近外三级完全一致：

| 阶段 | 直连 | 单跳 | 双跳 |
|---|---:|---:|---:|
| epoll 唤醒 → 开始读 | 1.1 µs | 1.0 µs | 950 ns |
| TTHeader 解码 | 2.2 µs | 2.0 µs | 1.7 µs |
| payload 解码 | 500 ns | 500 ns | 470 ns |
| **业务 handler** | **240 ns** | **240 ns** | **220 ns** |
| 编码（发送前） | 1.8 µs | 1.8 µs | 1.6 µs |
| 写 socket | 7.3 µs | 7.4 µs | **5.7 µs** |
| 总时长 | 23.2 µs | 21.3 µs | **17.7 µs** |

双跳下 server 写 socket 便宜 23 %（5.7 vs 7.4 µs）—— 它写的是本机 UDS 而不是跨机 TCP。
这是入向 sidecar 唯一算得上「收益」的地方，但远不足以抵消它自己的 27.6 µs。

**Envoy（双跳，p50）**：

| 阶段 | envoy-out (950) | envoy-in (920B) |
|---|---:|---:|
| ①→② TTHeader 解析 | 2.9 µs | 4.7 µs |
| ②→③ 协议层解头 | 490 ns | 910 ns |
| ③→④ 路由匹配 | 470 ns | 860 ns |
| ④→ 取上游连接 | 1.6 µs | 3.5 µs |
| 　编码 | 4.8 µs | 7.5 µs |
| 　写 socket | 290 ns | 710 ns |
| ⑤→⑥ ★等待上游 | 181.1 µs | 55.5 µs |
| ⑥→⑦ 响应解码 | 4.2 µs | 7.0 µs |
| **自身合计** | **15.9 µs** | **27.6 µs** |

### 3.4 跨节点分层分解（差值法）

```
双跳：
client 总 228.2
  ├─ client 自身                          10.5
  ├─ [client ↔ envoy-out UDS 往返]        ~20.7
  └─ envoy-out 总 197.0
       ├─ envoy-out 自身                  15.9
       ├─ [跨机网络往返]                   ~98.0
       └─ envoy-in 总 83.1
            ├─ envoy-in 自身              27.6
            ├─ [envoy-in ↔ server UDS 往返] ~37.8
            └─ server 总 17.7
```

merge 直接输出的差值（单位 µs）：

| 差值 | 单跳 | 双跳 | 跨机？ |
|---|---:|---:|---|
| client 总 − envoy-out 总 | **31.9** | **31.2** | 同机，精确 |
| envoy-out 总 − envoy-in 总 | — | **113.9** | **跨机 → 往返，不可拆单向** |
| envoy-in 总 − server 总 | — | **65.4** | 同机，精确 |

**`client 总 − envoy-out 总` 在单跳和双跳里几乎相同（31.9 vs 31.2 µs）** ——
又一个佐证：加第二跳不影响第一跳。

---

## 4. 把两个「等待」黑盒拆开

这是本版相对上一版最大的增量。上一版这两行各是一个数字，现在各拆成 4~5 段。

### 4.1 client 侧：「等待对端」拆成五段

`netpoll` 读路径 5 个点位。双跳低负载，p50：

| 段 | 值 | 含义 |
|---|---:|---|
| **纯等待（到 epoll 就绪）** | **211.8 µs** | 对端真的还没回。这才是「网络 + 对端处理」 |
| poller 事件排队 | 190 ns | 同批 epoll 事件里排在本连接前面的连接占用的时间 |
| readv 系统调用 | 2.5 µs | 收包 |
| LinkBuffer 入队 | 70 ns | 缓冲区管理 |
| **goroutine 调度延迟** | **2.8 µs** | 数据已就绪，但 RPC goroutine 还没被调度起来 |
| 合计（★等待对端） | 217.7 µs | |

**上一版这一整段是一个数（195.9 µs），无法判断该优化网络还是优化调度。**
现在能说：97.3 % 是真等待，1.3 % 是 goroutine 调度，其余是系统调用与缓冲管理。

**goroutine 调度延迟对传输方式极其敏感**（§0 结论 3）：

| | 直连（跨机 TCP） | 单跳（本机 UDS） | 双跳（本机 UDS） |
|---|---:|---:|---:|
| goroutine 调度延迟 p50 | **11.4 µs** | 2.8 µs | 2.8 µs |
| p99 | 27.4 µs | 15.8 µs | 15.5 µs |

**4.1 倍差距。** 读跨机 TCP 时唤醒要穿过网卡中断 → softirq → epoll，
读本机 UDS 时是进程直接唤醒。这条此前完全测不到。

### 4.2 Envoy 侧：「等待上游」拆成四段

Envoy 事件循环 3 个点位。双跳低负载 envoy-out，p50：

| 段 | 值 | 含义 |
|---|---:|---|
| **纯等待（到 epoll 就绪）** | **177.4 µs** | 上游真的还没回 |
| **事件循环排队** | **130 ns** | 同批 epoll 事件里排在本连接前的连接占用的时间 |
| readv 系统调用 | 3.5 µs | 收包 |
| buffer + filter 派发 | 150 ns | |
| 合计（⑤→⑥ 等待上游） | 181.1 µs | |

**低负载下「事件循环排队」只有 130~310 ns —— 这是正确读数，不是探针坏了。**
c=1 时压根没有并发连接可排队。它在负载下才有意义，见 §5。

### 4.3 一条 waterfall

`results/2026-08-07/two-lo-waterfall.txt` 里的第一条：

```
▸ envoy-in    [host=suzhou920B]  本地区间 80.2µs
        0ns  dn_first_byte          收到下游第一个字节
      4.5µs  hdr_decoded            TTHeader 解析完
      6.1µs  route_resolved         路由匹配完
      9.6µs  up_conn_reused         复用上游连接（不是新建）
     17.6µs  up_socket_write_done   已写给 kitex-server
     18.9µs  req_done
     64.9µs  up_epoll_wake          ★ 中间这 46µs 全在等
     65.2µs  up_readv_start
     71.3µs  up_readv_done
     78.4µs  resp_decoded
     80.2µs  rpc_done
▸ kitex-server [host=suzhou920B]  本地区间 17.3µs
        0ns  mesh_netpoll_onread
      1.2µs  mesh_first_byte
      2.9µs  mesh_hdr_decode_finish
      9.0µs  server_handle_start
      9.2µs  server_handle_finish   ★ 业务逻辑只有 200ns
     11.4µs  mesh_socket_write_start
     17.0µs  mesh_socket_write_finish
     17.3µs  rpc_finish
```

envoy-in 等了 46 µs，而它等的 server 只花 17.3 µs —— 差额是两次 UDS 传递与唤醒。

---

## 5. 并发下的变化（c=16）

**这一组不用于阶梯级差**（§2.5），只用来验证排队点位在有排队时确实有读数。

**样本：直连 39,306 / 单跳 27,313 / 双跳 20,436 条 trace。**

端到端 p50：直连 **215.1 µs** → 单跳 **330.5 µs** → 双跳 **459.6 µs**；
QPS 分别约 65.5k / 45.4k / 34.0k。

### 5.1 排队点位开始有读数

| 事件循环排队 | c=1 | c=16 p50 | c=16 p90 | c=16 p99 |
|---|---:|---:|---:|---:|
| envoy-out（单跳） | 130 ns | 470 ns | **20.7 µs** | **48.7 µs** |
| envoy-out（双跳） | 130 ns | 420 ns | **15.5 µs** | **41.5 µs** |
| envoy-in（双跳） | 310 ns | 650 ns | **24.0 µs** | **51.7 µs** |

**读法**：p50 仍在亚微秒 —— 多数请求不排队；但 p90 已经到 15~24 µs，
p99 到 41~52 µs。**排队是尾延迟现象，不是中位数现象。**
只看 p50 会得出「没有排队」的错误结论。

这正是这三个点位的价值：**它把「Envoy 处理慢」和「Envoy 排不过来」分开了**。
本次数据里 Envoy 的处理各阶段在 c=1 和 c=16 之间几乎没变，涨的全是排队。

### 5.2 client 侧的 goroutine 调度延迟

| | c=1 | c=16 p50 | c=16 p99 |
|---|---:|---:|---:|
| 直连 | 11.4 µs | 5.5 µs | **59.5 µs** |
| 单跳 | 2.8 µs | 4.4 µs | 42.4 µs |
| 双跳 | 2.8 µs | 3.3 µs | 26.1 µs |

直连在 c=16 时 p50 反而降到 5.5 µs —— 并发上来后 poller 持续有活干，
不需要每次从空闲状态被唤醒。但 p99 涨到 59.5 µs。

---

## 6. 两跳不对称：是机器不是路径（对照实验）

§3 里 envoy-in 自身 27.6 µs，是 envoy-out（15.9 µs）的 1.74 倍。
两个假设：**(A) 入向路径本身更贵**，**(B) 920B 这台机器更慢**。

**判定方法**：把两个 Envoy 放到**同一台机器**（950）上跑同样的参数。
若 (A) 成立，差距应当保留；若 (B) 成立，差距应当消失。

**结果（同机双跳，c=1，64 B，20 s × 3 轮，`ENVOY_CONCURRENCY=4`，21,091 条 trace）**：

| 阶段 | envoy-out | envoy-in | 比值 |
|---|---:|---:|---:|
| TTHeader 解析 | 3.1 µs | 3.1 µs | **1.00×** |
| 协议层解头 | 590 ns | 570 ns | 0.97× |
| 路由匹配 | 550 ns | 460 ns | 0.84× |
| 取上游连接 | 1.7 µs | 1.7 µs | **1.00×** |
| 编码 | 5.0 µs | 5.0 µs | **1.00×** |
| 写 socket | 310 ns | 300 ns | 0.97× |
| 响应解码 | 4.5 µs | 4.3 µs | 0.96× |

**逐项相等。假设 (A) 被否定：入向路径本身一点都不贵。**

原始输出：`results/2026-08-07/ctrl-samehost-two-lo-detail.txt`

### 6.1 进一步：机器差异能拆成两部分

同一个 `envoy-static` 二进制，同样的工作，在两台机器上的耗时：

| | 950（在同机对照里） | 920B（在跨机双跳里） | 倍数 |
|---|---:|---:|---:|
| TTHeader 解析 | 3.1 µs | 4.7 µs | 1.52× |
| 编码 | 5.0 µs | 7.5 µs | 1.50× |
| 响应解码 | 4.3 µs | 7.0 µs | 1.63× |
| 路由匹配 | 460 ns | 860 ns | 1.87× |
| **readv 系统调用** | **2.2 µs** | **6.3 µs** | **2.86×** |

**纯 CPU 工作约慢 1.5~1.9 倍，而 `readv` 系统调用慢 2.86 倍。**
前者对应 CPU 主频与微架构，后者还叠加了内核版本差异（6.6.0 vs 5.10.0）。

> **这个对照本身有一处不严格**：同机对照里两个 Envoy 与 client、server 共享 950 的
> CPU，而跨机时 envoy-out 独占 950。但这只会让同机组更慢，
> 而同机组反而更快 —— 结论方向是安全的。

**容量规划含义**：不要假设两跳成本相同。sidecar 的成本几乎线性正比于所在机器的单核性能。

---

## 7. 打点自身的开销 —— ⚠️ 数据已过期，需重测

### 7.1 现有数据及其失效原因

上一版测得（四组对照，c=16，6 轮交错，取中位数与 [最小, 最大] 区间）：

| 组 | 相对基线 | 判读 |
|---|---:|---|
| 探针激活 / 采样 0 | +2.4 % | 区间与基线重叠 → **低于噪声下限** |
| 探针激活 / 采样 1 % | +0.9 % | 区间与基线重叠 → **低于噪声下限** |
| **探针激活 / 采样 100 %** | **−6.6 %** | 区间分离 → **效应可辨** |

**这组数据不覆盖本报告 §4 用到的 8 个新点位。** 时间线：

```
2026-08-06 17:57  ← 上面这组开销测量在此完成
2026-08-06 19:28  kitex   464dc41b  add epoll wakeup and socket syscall events
2026-08-07 00:33  netpoll 64d8602   instrument the read path        （5 个点位）
2026-08-07 00:33  envoy   efeda767  split the upstream socket receive path（3 个点位）
2026-08-07 00:33  kitex   98311385  carry netpoll read-path timestamps out
2026-08-07 10:08  envoy   6af429ba  don't use connection id 0 as the sentinel
2026-08-07 10:38  envoy   448c7f0f  take the epoll wake time from approximateMonotonicTime
```

**8 个新点位全部晚于测量。** 所以上表只能说明「旧的那套点位开销可忽略」，
**不能外推到当前版本**。

### 7.2 新增的开销里有一项是无条件的

多数打点成本只落在采样命中的请求上，但 netpoll 的读路径探针引入了一项
**与采样无关、每次 epoll 唤醒都要付的成本**：

```go
// netpoll/probe_meshlab.go
var probeActive int64                       // 当前开启读打点的连接数
func probeAnyActive() bool { return atomic.LoadInt64(&probeActive) > 0 }
```

poller 每次 epoll 唤醒都要判断要不要取时间戳，而那一刻还不知道本批事件属于哪些连接。
用全局计数做前置判断，**没有任何连接在打点时每次唤醒也要多一次原子读**（约 1 ns）。

设计上这已经是最省的写法（无条件调 `time.Now()` 约 40 ns），但它是一笔
**旧测量里不存在的新支出**，且压测下 poller 每秒唤醒十万次量级。
它究竟是否仍低于 3 % 的噪声下限，**没有实测数据，不能断言**。

Envoy 侧同理：通用读路径（`connection_impl.cc` 的 epoll/readv 点位）服务全进程所有连接，
靠 `onPoolReady` 里查一次 `isSampled` 后挂的裸标志做门控，热路径只判零 ——
同样是设计上最省，同样没有重测。

### 7.3 结论与行动项

- **可以继续沿用的操作建议**：压测归因用 1 %~5 % 采样，不要用 100 %。
  这条建议的方向不因新点位而改变 —— 新点位只会让 100 % 采样更贵。
- **不可引用的数字**：上表的 +0.9 % / −6.6 %。它们描述的是旧版本。
- **行动项（P1）**：用 `scripts/bench.sh matrix` 在当前代码上重跑四组对照。
  一次约 20 分钟，本次因优先做归因阶梯而未做。

---

## 8. 局限与未决问题

### 8.1 测量方法的局限

| 局限 | 影响 |
|---|---|
| **分位数不可加** | §0、§3 的分解表用于判断量级，不可当作精确加法 |
| **噪声下限约 3 %** | 小于此的效应无法分辨 |
| **跨机只能得往返** | 无法拆分去程/回程，需 PTP + 硬件时间戳网卡，物理限制 |
| **`net.*` 列不是纯线路时间** | 它是 `(外层等待 − 内层总时长)/2`，高负载下以排队为主 |
| **最后一批 trace 可能滞留** | 流量停止后不足一个刷盘间隔的事件留在内存；对万级样本无影响 |
| §6 对照实验的 CPU 共享不严格 | 见 §6.1 的方框，结论方向安全 |

### 8.2 尚未覆盖的点位

| 缺口 | 价值 | 状态 |
|---|---|---|
| **netpoll 写路径（writev 边界）** | **P0** —— 见下方待解释现象 | **未实现** |
| **打点开销重测** | **P1** —— 现有数据早于全部 8 个新点位，见 §7 | **未做** |
| 中间件链开销 | 预计 1 µs 量级 | 未实现；demo 里测不出东西，迁真实业务前再做 |
| 协议栈内部（网卡 → 内核 → socket） | 评测网卡时是唯一该看的部分 | 需 SO_TIMESTAMPING / eBPF，见 `probe-coverage-audit.md` §5 |
| listener accept | 连接级而非请求级 | 已由 `client_conn_*` 与 `up_conn_new` 间接覆盖 |

> **上一版把「netpoll 内部（epoll_wait 返回 → OnRead 回调）」列在这里 —— 那是过时的。**
> 读路径 5 个点位已实现并有实测数据（§4.1）。**写路径仍是黑盒。**

**一个部分回答了的现象**：

| 写 socket | p50（双跳，c=1） |
|---|---:|
| envoy-out（C++，本机 UDS） | **290 ns** |
| envoy-in（C++，本机 UDS） | 710 ns |
| kitex-client（Go，本机 UDS） | 1.8 µs |
| kitex-server（Go，本机 UDS） | **5.7 µs** |
| kitex-client（Go，跨机 TCP，直连） | **7.8 µs** |

上一版说「Go 侧比 Envoy 贵 7~10 倍，原因需进一步分辨」。现在能确认的部分：

- **传输方式的影响已经量化**：同为 Go client，跨机 TCP（7.8 µs）是本机 UDS（1.8 µs）的 4.3 倍。
- **机器的影响已经量化**：kitex-server 在 920B 上写 UDS 要 5.7 µs（§6.1 的 1.5~2.9 倍系数）。

**但「Go 的写路径里，syscall 本身与 netpoll 缓冲管理各占多少」仍然无法回答** ——
写路径没有插桩。这是 §8.2 里唯一的 P0。框架可以直接复用读路径那套
（`ReadProbe` 的原子槽位 + 因果校验），估计一天的工作量。

### 8.3 环境相关

- 950 **未启用 NTP**，两机时钟差 15.46 s。分析免疫（§2.4），仅 waterfall 视觉错乱。
- 两机 CPU 与内核不同（384 核 / 6.6.0 vs 160 核 / 5.10.0），**两跳成本不对称 1.74 倍**，
  原因已定位（§6）。容量规划时不能假设两跳成本相同。
- 920B 开着 firewalld 且无 root，只有 15006 可用。

---

## 9. 复现方式

完整步骤见 [`runbook-reproduce.md`](runbook-reproduce.md)。本报告的数据这样产出：

```bash
cd ~/envoy_kitex/mesh-lab
export ENVOY_CONCURRENCY=4          # 三级必须同值，否则线程规模差异混进级差

# ── 归因阶梯：3 轮 × 3 拓扑，交错（§2.3）──
for r in 1 2 3; do
  for topo in direct single two; do
    TOPO=$topo ./scripts/run-cross-machine.sh stop >/dev/null 2>&1; sleep 2
    rm -rf /tmp/kitex-demo; mkdir -p /tmp/kitex-demo
    TOPO=$topo ./scripts/run-cross-machine.sh start >/dev/null 2>&1
    TOPO=$topo ./scripts/run-cross-machine.sh status >/dev/null || continue
    tgt=$(TOPO=$topo ./scripts/run-cross-machine.sh target)
    ( cd demo && KITEX_PROBE_HOST=suzhou950 ./bin/client \
        -target "$tgt" -service echo-server -c 1 -size 64 -d 20s -sample 0.05 )
    # 顺序不能换：stop（线程退出才刷盘）→ collect（拉回对端数据）
    TOPO=$topo ./scripts/run-cross-machine.sh stop >/dev/null 2>&1; sleep 5
    TOPO=$topo ./scripts/run-cross-machine.sh collect >/dev/null 2>&1
    mkdir -p ~/ladder/$topo-lo/r$r && mv /tmp/kitex-demo/trace-* ~/ladder/$topo-lo/r$r/
  done
done
# c=16 那组把 -c 1 -sample 0.05 换成 -c 16 -sample 0.01，其余不变

# ── §6 的对照实验：同机双跳 ──
./scripts/run-two-hop.sh start          # 两个 Envoy 都在 950
# 同样的 client 参数，3 轮

# ── 分析 ──
cd demo
FILES=$(find ~/ladder/two-lo -name 'trace-*.ndjson*' -type f | sort | tr '\n' ' ')
./bin/merge -format summary   $FILES
./bin/merge -format detail    $FILES
./bin/merge -format waterfall -limit 2 $FILES
./bin/merge -format table -limit 0 $FILES > trace-table.csv

# ── 时钟鲁棒性（§2.4）──
./bin/merge -format detail --inject-skew kitex-server=+50 $FILES > /tmp/skew.txt
./bin/merge -format detail                               $FILES > /tmp/noskew.txt
diff -q /tmp/skew.txt /tmp/noskew.txt && echo "对时钟偏斜免疫"
```

---

## 附录 A：原始 merge 输出索引

全部在 `docs/results/2026-08-07/`。每个文件开头的注释行记录了该组的参数与各轮 client 自报值。

| 文件 | 内容 | 对应章节 |
|---|---|---|
| `{direct,single,two}-lo-summary.txt` | 粗略分段：各节点总时长 | §3.1 |
| `{direct,single,two}-lo-detail.txt` | **详细分段：含 8 个新点位** | §3.3、§4 |
| `{direct,single,two}-lo-waterfall.txt` | 单请求逐点时刻，各 2 条 | §4.3 |
| `{direct,single,two}-hi-*.txt` | 同上，c=16 | §5 |
| `ctrl-samehost-two-lo-detail.txt` | **同机双跳对照实验** | §6 |
| `clock-skew-check.txt` | 偏斜注入前后比对 + 实测钟差 | §2.4 |
| `*-table-sample.csv` | 逐条 CSV 的表头 + 聚合行 + 前 25 条 | — |

**`table` 全量 CSV 未入库**（每组 4~9 MB），留在 suzhou950 的
`~/ladder/<topo>-<load>-table-full.csv`。它一行一条 trace、一列一个时间段（单位 µs），
开头四行是全量样本的 `__avg__`/`__p50__`/`__p90__`/`__p99__`：

```python
import pandas as pd
df  = pd.read_csv("two-lo-table-full.csv", comment="#")
agg = df[ df.trace_id.str.startswith("__")]   # 聚合行
raw = df[~df.trace_id.str.startswith("__")]   # 逐条数据
```

`detail` 只给分位数，看不到单条请求内部各段的相关性 ——
比如「尾延迟那些请求到底卡在排队还是网络」。`table` 保留逐条数据就是为了回答这类问题。
