# 2026-08-10 socket 边界补齐（Envoy 下游 + Go 收发两侧）—— merge 原始输出

这些是 `demo/bin/merge` 的**未经加工的输出**，直接 `cat` 即可。
本轮的改动见 [`../../specs/2026-08-10-downstream-socket-probes-design.md`](../../specs/2026-08-10-downstream-socket-probes-design.md)。

## 本轮做了什么

每跳 Envoy 横跨两条连接（下游 UDS、上游 TCP），四个 socket 边界过去只测了上游那条。
本轮补上下游读（3 点）与下游写（2 点），**每跳点位数 15 → 20**；
同时把 netpoll 读路径的 5 个点位从「仅客户端」扩到**服务端也有**（server 21 → 26），
并新增写路径 4 个点位（client 25 → 29、server 26 → 30），Go 侧收发终于对称。

最大的收益不在这五段本身：`dn_first_byte` 记在 filter 的 `onData` 入口，
已经在 socket 读、buffer append、filter 派发**之后** —— 那一整段过去被
**上一跳的「纯等待」**吸收，而那段占端到端约 79%，是全链路最大的一坨。

## 实验参数

```
two   client(950) ─UDS─▶ envoy-out(950) ──TCP:15006──▶ envoy-in(920B) ─UDS─▶ server(920B)
```

| | 值 |
|---|---|
| 拓扑 | 跨机双跳（`TOPO=two`） |
| 请求数 | 1000，`-c 1`，`-d 0` |
| payload | 128 B |
| 采样率 | **1.0**（验证轮，要每条都有） |
| `ENVOY_CONCURRENCY` | 2 |
| 端到端 p50 | 240 µs |

## 数据完整性

四个节点的收尾行全部齐全：

```
[probe] node=kitex-client 总请求=1000 采样=1000 丢弃=0
[probe] node=envoy-out    host=suzhou950  记录=20000 落盘=20000 丢弃=0
[probe] node=envoy-in     host=suzhou920B 记录=20000 落盘=20000 丢弃=0
[probe] node=kitex-server 总请求=1000 采样=1000 丢弃=0
```

逐条核对：client **1000×29**、envoy-out 1000×20、envoy-in 1000×20、**server 1000×30** 点位，
共 **99000** 个事件，一条不缺。**判据是「记录==落盘 且 丢弃=0」，不是数行数。**

服务端 21 → 30、客户端 25 → 29，来自 netpoll 读路径 5 点（仅服务端新增）与写路径 4 点（两侧新增）。

## 文件

| 文件 | 内容 |
|---|---|
| `two-cross-summary.txt` | **端到端分解**：各节点自身 + 各段往返，相加等于 100%（下面有表） |
| `two-cross-detail.txt` | **主力**：各阶段分位数。节点按请求流向排，节点内按真实时序排，父子用缩进 |
| `two-cross-table-sample.csv` | 逐条 CSV 的前 25 条（前四行是全量聚合，不受 limit 影响） |
| `two-cross-waterfall.txt` | 3 条个案，**按物理机分块、同机节点交错成一条时间线** |

全量 CSV（`two-cross-table.csv`）与原始 `raw/*.ndjson` 不入库
（`.ndjson` 已在 `.gitignore` 里），需要时按 runbook 重新生成。

## 端到端分解

`summary` 现在给的是一个**划分**而不是嵌套区间 —— 各段相加恰好等于端到端：

| 段 | avg | 占比 |
|---|---:|---:|
| kitex-client 自身 | 16.7 µs | 6.5% |
| UDS 往返 client↔envoy-out | 20.9 µs | 8.2% |
| envoy-out 自身 | 25.9 µs | 10.1% |
| **跨机往返 envoy-out↔envoy-in** | **100.6 µs** | **39.4%** |
| envoy-in 自身 | 43.1 µs | 16.9% |
| UDS 往返 envoy-in↔server | 13.6 µs | 5.3% |
| kitex-server 自身 | 34.5 µs | 13.5% |
| 合计 | 255.3 µs | 100.0% |

推导只用两个式子交替套用：**节点总时长 − 它等下一跳的时间 = 节点自身处理**，
**它等下一跳的时间 − 下一跳的总时长 = 两者之间的往返**。望远镜求和，误差恒为 0。

三条必读的限制：

- **只有 avg 能这么加**，分位数不可加，所以这张表天生只给 avg
- **「往返」不可拆单向**：跨机是物理限制（需 PTP + 硬件时间戳），同机 UDS 同理
- **「往返」不是纯线路时间**：内层节点起算点之前的排队落在测量区间外，全被算进这一列

旧的「各节点观测区间」保留在 summary 下半部分 —— 那些数是差值法的原始输入，
也是既有报告引用的口径，但它们**嵌套且不可相加**。

## 服务端 netpoll 补齐带来的归因转移

服务端此前最早只能看到 `mesh_netpoll_onread`（OnRead 入口），
epoll 唤醒→readv→LinkBuffer→调度这一整段不可见，被算进了 UDS 往返里。
补上 5 个点位后：

| 段 | avg |
|---|---:|
| ⓪ netpoll 收包（到 OnRead） | **14.8 µs** |
| 　├ poller 事件排队 | 451 ns |
| 　├ **readv 系统调用** | **6.0 µs** |
| 　├ LinkBuffer 入队 | 145 ns |
| 　└ **goroutine 调度延迟** | **7.8 µs** |

于是 `UDS 往返 envoy-in↔server` 从 34.7 µs 降到 **13.6 µs**，
`kitex-server 自身` 从 19.2 µs 升到 **34.5 µs** ——
约 15 µs 从「不可解释的往返」挪进了服务端自己的收包路径。

**开销**：服务端探针只能连接级常开，所以专门测了一轮（`KITEX_PROBE_NETPOLL_SERVER`
0/1 交错 3 轮、`-sample 0`）：均值 241.3 vs 239.0 µs，**开着比关着还「快」1.0%**，
差异全在噪声内。

## writev 拆开：一个长期结论被推翻

`mesh_socket_write_start/finish` 此前括住整个 `Flush()`，是个黑盒。拆开后：

| 段 | client(950) | server(920B) |
|---|---:|---:|
| LinkBuffer 整理 + 拼 iovec | 99 ns | 152 ns |
| **★ writev 系统调用** | **1.9 µs (86%)** | **3.2 µs (89%)** |
| 缓冲区回收 | 57 ns | 92 ns |
| 合计 | 2.2 µs | 3.6 µs |

Go 侧写 socket 的开销**几乎全在系统调用本身**，LinkBuffer 管理只占 4~5%，
此前怀疑的「链表整理 + epoll 注册混在一起」被证伪（实测 `writes=1`、
`waited=false`，1000/1000 都一次写完）。

**但这同时推翻了「Go 侧写 socket 比 Envoy 贵 7–10 倍」这个结论** ——
那个比较根本不成立。Envoy 的 `up_encode_done → up_socket_write_done` 括住的是
`ConnectionImpl::write()`，而它只做 `write_buffer_->move(data)` +
`activateFileEvents`；**真正的 writev 在 `onWriteReady()` 里、由事件循环稍后执行，
目前没有点位**。拿入队时间去比系统调用时间，差 7–10 倍毫不奇怪。

在给 Envoy 的 `doWrite()` 补上点位之前，**任何「Go 写 socket 比 Envoy 慢」的说法
都不要再提**。

## 归因闭合

本轮的核心验证 —— 上一跳的「纯等待」减去下一跳新口径的处理时间，剩余应为正且量级合理：

| | avg |
|---|---|
| envoy-out「纯等待」(`up_write_done` → `up_epoll_wake`) | 206.1 µs |
| − envoy-in 在这段时间里做的事（新口径，`dn_epoll_wake` 起） | 97.4 µs |
| **= 剩余：跨机网络往返 + 内核到 epoll 就绪** | **108.7 µs** |

对照旧口径（只能扣到 `dn_first_byte`）：内层 93.2 µs、剩余 112.9 µs。
**本轮把 4.2 µs 从「不可归因的网络往返」里挖出来，归给了 envoy-in 的下游收包**，
占原不可归因部分的 3.7%。

两项各自在本机用单调钟测量，相减时时钟偏斜完全抵消（§8.2.3）。

## 开销对照

三种配置**按轮次交错**各 3 轮（不能一组跑完再跑另一组），每轮 5000 请求：

| 配置 | 三轮 p50 | 均值 | 相对 A |
|---|---|---|---|
| A 探针完全不激活（`KITEX_PROBE_DISABLE=1`） | 243 / 225 / 234 µs | 234.0 µs | — |
| B 探针启用但请求未采样（`-sample 0`） | 247 / 237 / 224 µs | 236.0 µs | +0.9% |
| C 探针启用且全采样（`-sample 1.0`） | 235 / 229 / 231 µs | 231.7 µs | −1.0% |

**C 比 A「更快」—— 插桩不可能让系统变快，所以这 1% 是纯噪声。**
同一配置内部的轮间波动最大到 9.7%，远大于配置之间的差异。

> **这只证明「低于本底噪声」，不等于零开销。** 本测量是 `c=1` 的端到端 p50，
> 被 165 µs 量级的跨机等待主导，对几微秒的插桩开销本就不敏感。
> 要更灵敏得用高并发吞吐，或直接看 Envoy 内部各段。

## 时钟偏斜自检

`--inject-skew kitex-server=+50` 注入 50 ms 人工偏移后，`detail` 输出**逐字节不变**，
证明分析对时钟偏斜免疫。（waterfall 的摆位会变，那是预期 —— 它用 wall 摆位。）
