# 打点完整性审计

按四类核对覆盖度：**各阶段处理时间**、**socket 接口时间**、**排队时间**、**链路上的时间**。
每一类先说该类里"一条请求会在哪些地方花时间"，再对照现有点位，最后列出缺口。

判定标准仍是那一条：**去掉它之后，哪个区间就算不出来了。**

> 详细点位清单见 [probe-points.md](probe-points.md)，本文只做覆盖度评估。
> **与代码的对应关系核对于 2026-08-11。**

---

## 结论速览

| 类别 | 覆盖 | 最大缺口 |
|---|---|---|
| 各阶段处理时间 | 🟢 **好** | 中间件/filter 链内部不可分 |
| socket 接口时间 | 🟢 **六个边界全部拆到系统调用**（Envoy 上下游各收发 + Go 两侧收发） | 只剩 netpoll 的**写慢路径**（EPOLLOUT 之后由 poller 续写那段）没拆。所谓"Go 比 Envoy 贵 7–10 倍"**已作废** |
| 排队时间 | 🟡 **从零到部分覆盖** | 内核 accept 队列、TCP 发送缓冲区阻塞、服务端 goroutine 池 |
| 链路上的时间 | 🔴 **只能得往返，且看不见 socket 以下的一切** | **网卡、驱动、协议栈、softirq 全部不可见** |

**最重要的一条：整套插桩的下界是 socket 系统调用。** 网卡、驱动、中断、协议栈的时间统统被算进"纯等待"这一坨里。
平时这不要紧——那部分本来就小且稳定。但**一旦要评测网卡本身，这恰好是唯一该看的部分，而现在完全看不见。**
补法见 §五。

### 点位总数（2026-08-11 口径）

| 位置 | 每条 trace 的去重点位名 |
|---|---:|
| Envoy（每跳） | **24** |
| kitex-client（框架 10 + 自定义 10 + netpoll 9） | **29** |
| kitex-server（框架 10 + 自定义 11 + netpoll 9） | **30** |
| **跨机双跳合计** | **107**（实际事件 ~119，见下） |

> **Envoy 侧不能数事件数。** `onWriteReady()` 每条 trace 触发多次（第 1 次是真正
> 的发送，之后是缓冲区已排空的空写），实际事件 24~30 不等，**去重后的点位名恒为
> 24**。merge 对同名点取最早一次，恰好取到对的那个。
> **完整性判据是「记录==落盘 且 丢弃=0 且 下游写未归属=0」，不是数行数。**

---

## 一、各阶段处理时间 🟢

一条请求在"纯计算"上花的时间：解析、路由、编解码、业务逻辑。

### 已覆盖

实测取自 2026-08-07 跨机双跳、c=1、payload 64 B（`results/2026-08-07/two-lo-detail.txt`），
Envoy 两个数字为 envoy-out(950) / envoy-in(920B)：

| 阶段 | 由哪两点之差得出 | 实测 p50 |
|---|---|---|
| TTHeader 解析（Envoy） | `dn_first_byte` → `hdr_decoded` | 2.9 / 4.7 µs |
| 协议层解消息头 | `hdr_decoded` → `msg_begin` | 490 / 910 ns |
| 路由匹配 | `msg_begin` → `route_resolved` | 470 / 860 ns |
| 取上游连接 | `route_resolved` → `up_conn_*` | 1.6 / 3.5 µs |
| 帧编码 | `up_conn_*` → `up_encode_done` | 4.8 / 7.5 µs |
| 响应解码 | `up_first_byte` → `resp_decoded` | 4.2 / 7.0 µs |
| TTHeader 解码（Kitex） | `mesh_hdr_decode_*` | 710 ns（client）/ 1.7 µs（server） |
| TTHeader 编码（Kitex） | `mesh_hdr_encode_*` | 90 / 100 ns |
| payload 序列化 | `mesh_payload_encode_*`（曾名 `mesh_payload_codec_*`） | 300 / 470 ns |
| payload 反序列化 | `wait_read_*`（名字骗人，实为解码） | 400 ns（client）/ 550 ns（server） |
| 业务 handler | `server_handle_start/finish` | **220 ns** |

覆盖是充分的——两跳 sidecar 自身处理只占端到端 **19.1 %**（43.5 / 228.2 µs），
而这 19.1 % 已被拆到微秒级。

> envoy-in 每一项都比 envoy-out 慢 1.6~2.4 倍。**那是机器不是路径** ——
> 把两个 Envoy 放同一台机器跑同样参数，各阶段逐项相等（编码 5.0 vs 5.0 µs）。
> 见 `test-report.md` §6。**看这张表时不要把它读成「入向解析更贵」。**

### 缺口

| 缺什么 | 影响 | 建议 |
|---|---|---|
| **Envoy filter chain 内各 filter 耗时** | 本实验只挂了 thrift_proxy 一个 filter，无所谓；真实部署挂十几个 filter 时，`dn_first_byte → hdr_decoded` 会变成一个新黑盒 | 真实部署前必须补 |
| **Kitex 中间件链** | 同上。demo 没挂中间件，生产环境的鉴权/限流/熔断都在这里 | 加 `MWChainEnter/Exit` |

这两项在当前 demo 里都测不出东西（值为 0），**所以本轮没加**——加了只会增加噪声。
但迁到真实业务前必须补，否则会把中间件的开销误算成"框架开销"。

---

## 二、socket 接口时间 🟢

read/write 系统调用本身的耗时，以及围绕它的缓冲区管理。

### 已覆盖：六个边界，每个都拆到了系统调用

一次双跳 RPC 一共穿过 **6 个 socket 边界**（每跳 Envoy 两条连接各收发一次 = 4，
Go 两端各收发一次 = 2，共 12 次系统调用；按「代码里的插桩位置」算是 6 处）。
实测取自 2026-08-10 跨机双跳、c=1、payload 128 B（`results/2026-08-10/two-cross-detail.txt`），
**avg**，两个数字分别是 950 侧 / 920B 侧：

| 位置 | 点位 | 传输 | 实测 avg |
|---|---|---|---|
| Envoy 下游**接收** | `dn_readv_start` → `dn_readv_done` | UDS / TCP | 2.2 / 4.4 µs |
| Envoy 上游**接收** | `up_readv_start` → `up_readv_done` | TCP / UDS | 3.0 / 4.7 µs |
| Envoy 上游**发送** | `up_writev_start` → `up_writev_done` | TCP / UDS | 5.1 / **3.4** µs |
| Envoy 下游**发送** | `dn_writev_start` → `dn_writev_done` | UDS / TCP | **1.9** / 6.4 µs |
| netpoll **接收** | `mesh_np_readv_start` → `mesh_np_readv_done` | UDS / UDS | 2.3 / 4.4 µs |
| netpoll **发送** | `mesh_np_writev_start` → `mesh_np_writev_done` | UDS / UDS | **1.8** / **2.9** µs |

> **「入队」不是「发送」，两者都留了点位，别混用。**
> `up_encode_done` → `up_socket_write_done`（297 / 751 ns）括住的是
> `ConnectionImpl::write()`，只做 `write_buffer_->move()` + `activateFileEvents`；
> 同理 Kitex 的 `mesh_socket_write_start/finish`（2.1 / 3.3 µs）括住整个
> `Flush()`，里面才是真正的 sendmsg。**做跨语言对比只能用 `*_writev_*` 那一行。**

> **接收侧 Envoy 比 Go 贵，是因为 `doRead` 内部是循环**，反复 readv 直到 EAGAIN，
> 所以是「N 次 readv + N 次 append」的总和；netpoll 的 `ioread()` 是单次。
>
> 950 与 920B 的普遍差距（1.5~2.9 倍）是**机器差异**，不是路径差异 ——
> 见 `test-report.md` §6 的同机对照实验。**看这张表时不要读成「入向更贵」。**

### ~~缺口：发送侧的不对称~~ —— 2026-08-10 拆开了，**顺带推翻了一个结论**

本节原先写着「**Go 侧写 socket 比 Envoy 贵 7–10 倍**（1.8–2.9 µs vs 270–690 ns），
至今无法归因」。现在 netpoll 的写路径拆开了，结论是：

**那个比较从一开始就不成立 —— 两边测的不是同一件事。**

`up_encode_done → up_socket_write_done` 括住的是 Envoy 的
`ConnectionImpl::write()`，而它**只做两件事**：

```cpp
write_buffer_->move(data);                                  // 把数据搬进写缓冲区
ioHandle().activateFileEvents(Event::FileReadyType::Write); // 激活写事件
```

**真正的 writev 发生在 `onWriteReady()` 里，由事件循环稍后调用** ——
那 260 ns 是「入队 + 激活事件」的耗时，不是系统调用。
而 Go 侧的 `mesh_socket_write_start/finish` 括住的是同步的 `Flush()`，
里面**真的**执行了 sendmsg。拿一个入队时间去比一个系统调用时间，
差 7–10 倍毫不奇怪。

### Go 侧写路径的真实分解（2026-08-10 新增）

| 段 | client(950) | server(920B) |
|---|---:|---:|
| LinkBuffer 整理 + 拼 iovec | 99 ns | 152 ns |
| **★ writev 系统调用** | **1.9 µs (86%)** | **3.2 µs (89%)** |
| 缓冲区回收 | 57 ns | 92 ns |
| 合计（写socket） | 2.2 µs | 3.6 µs |

**Go 侧写 socket 的开销几乎全在系统调用本身**，LinkBuffer 管理只占 4~5%。
此前怀疑的「链表整理 + 拼 iovec + 可能的 epoll 注册混在一起」被证伪：
小报文一次写完（实测 `writes=1`、`waited=false`，1000/1000），
根本走不到注册 EPOLLOUT 那条路。

点位：`mesh_np_flush_start` / `mesh_np_writev_start` / `mesh_np_writev_done` /
`mesh_np_flush_done`，两侧都有。慢路径（部分写）**没有拆开**，
由 attrs 里的 `waited=true` 标出，分析时按需过滤。

### ~~现在真正的发送侧缺口：Envoy 的 writev 没测~~ —— 同日补齐

在 `ConnectionImpl::onWriteReady()` 的 `transport_socket_->doWrite()` 前后加了
`up_writev_start/done`（按 side 选 `up_`/`dn_`，与读路径同构）。

**注意 `onWriteReady` 每条 trace 会触发多次**：第 1 次是真正的请求发送，
之后是缓冲区已排空后的空写（实测 0.04~0.09 µs）。merge 取同名点最早一次，
恰好取到对的那个；直接数事件数会被误导。

### 结论：两边 writev 是一个量级，旧结论彻底作废

下游 writev 补齐之后（见下一节），**同机同传输的干净对比有两对**，
两台机器各一对，avg：

| 机器 | Go 侧写 UDS | Envoy 侧写 UDS | 比值 |
|---|---:|---:|---:|
| 950 | kitex-client **1.8 µs** | envoy-out 下游 **1.9 µs** | 1.06 |
| 920B | kitex-server **2.9 µs** | envoy-in 上游 **3.4 µs** | 1.17 |

**两对都落在同一量级。** 只有一对时还能说「样本太少」，两台机器、两个方向都复现，
这个结论就站得住了。

所谓「Go 侧写 socket 比 Envoy 贵 7–10 倍」是拿 Go 的**系统调用时间**
去比 Envoy 的**入队时间**（300~750 ns），两边量的不是一回事。
现在 Envoy 侧三段分开了（avg）：

| 段 | envoy-out 上游 | envoy-in 上游 | envoy-out 下游 | envoy-in 下游 |
|---|---:|---:|---:|---:|
| 写socket(仅入队) | 297 ns | 751 ns | 731 ns | 1.1 µs |
| 入队 → 真正写出（事件循环调度） | 2.3 µs | 3.8 µs | **7.9 µs** | **11.7 µs** |
| **★ writev 系统调用** | 5.1 µs | 3.4 µs | 1.9 µs | 6.4 µs |

「入队 → 真正写出」这一段是 Envoy 异步写模型的固有延迟，Go 的同步 Flush 没有
对应物 —— 这才是两种模型的真实差别，而不是系统调用快慢。

> **下游那一列的「入队 → 真正写出」明显更长（7.9 / 11.7 µs）。**
> 因为下游写发生在 `rpc_done` **之后**，中间隔着 `deferredDelete` 与
> 一整轮事件循环；上游写则是在处理请求的同一轮里被激活的。
> 这不是「下游 socket 更慢」，是**排到写的时机不同**。

### ~~剩余缺口：下游 writev 采不到~~ —— 2026-08-10 当晚补齐

原文写着「`dn_writev_*` 实测 **0 条**，要采到得把绑定活到下游写完为止，
涉及绑定生命周期改造，有泄漏风险，**本轮没做**」。当晚就做了。

难点确实在生命周期，但解法不是「不擦绑定」—— 那在流水线下会错：请求 N 的 writev
还没跑，N+1 的 `bindTrace` 就把 `bindings[conn]` 覆盖了，N 的写会记到 N+1 头上。
改为**单独一个 `finishing` 槽**：`endRpc` 把 binding 移交过去，`bindings` 照常擦
（`isSampled` 语义不变）；下游 writev 走 `rpcEventTail()` 查这个槽，
`dn_writev_done` 记完即释放。

**一条连接同时最多容纳一个「待写出」的 RPC**，前一个还没写出就又结束一个时，
计入 `g_write_lost` 并丢弃 —— 宁可丢也不误记。这也是诚实的：流水线下一次 writev
可能同时写出多个响应，「某个 RPC 的 writev」本就不可拆。该计数进了收尾行，
**是完整性判据的一部分**：

```
[probe] node=envoy-out host=suzhou950 记录=832004 落盘=832004 丢弃=0 下游写未归属=0
```

实测 c=1 / 16 / 64 三档均为 0（Kitex 用连接池，每条连接串行，因果上不出现流水线）。

> **归档的 `results/2026-08-10/` 早于这一版实现**（那轮用的是中间版本
> 「干脆不擦绑定」），所以那批数据里每条 trace 有 2~3 次 `dn_writev_*`，
> 多出来的是空写。**分位数不受影响**（merge 取最早一次），但要按事件数核对
> 完整性的话得用当前口径重跑。详见 `probe-points.md` §「下游写」。

### ~~缺口：服务端的 readv 完全没测~~ —— 2026-08-10 已补齐

原文说 netpoll 读路径的 5 个点位「只插在客户端」，因为采样判定必须早于阻塞读，
而服务端此刻读不到 traceparent。**前提对，结论错** —— 和下面 Envoy 下游读那条
犯的是同一个错：采样未知只否定了「按 RPC 开关探针」这一种做法，不否定打点本身。

服务端改为**连接级常开**（`default_server_handler.OnActive`）、
**`OnRead` 入口取快照**（那时读早已完成，槽位是完整的一轮），
采样与否留到 `Tracer.Finish` 再判。开销由 `KITEX_PROBE_NETPOLL_SERVER=0` 可关。

补上之后服务端多出 **12.9 µs** 此前完全不可见的时间
（avg，跨机双跳 c=1，`results/2026-08-10/two-cross-detail.txt`）：

| 段 | 服务端(920B) | 客户端(950) |
|---|---:|---:|
| poller 事件排队 | 345 ns | 198 ns |
| **readv 系统调用** | **4.4 µs** | 2.3 µs |
| LinkBuffer 入队 | 199 ns | 103 ns |
| **goroutine 调度延迟** | **7.3 µs** | 3.1 µs |
| 合计 | **12.9 µs** | 5.9 µs |

服务端每项都贵 2~3 倍，与 `test-report.md` §6 记的机器差异一致，不是路径差异。

> **这张表的数字在 2026-08-10 那天改过三次**（18.0 → 14.8 → 12.9 µs），
> 不是测量不稳，是**同一天里 merge 的区间口径连改了三版**（服务端 netpoll 五段
> 落地 → 「谁在干活」重新划分 → 节点区间按点位去重）。
> **看到别处引用 18.0 / 14.8 的，按本表为准**；口径变动的完整记录见
> `test-report.md` §2.6。

**归因收益**：端到端分解里 `UDS往返 envoy-in↔kitex-server` 从 34.7 µs 降到
**5.4 µs**，`kitex-server 自身` 从 19.2 µs 升到 **32.1 µs** ——
原先记在「不可解释的 UDS 往返」里的时间，一部分挪进了服务端自己的收包路径，
另一部分被 Envoy 侧同期补上的下游写点位吸收（往返的两端同时收紧了）。

> 补的过程中发现 `mesh_np_trigger` 在服务端**根本没被记过**：`inputAck` 里
> 服务端走 `onRequest()` 分支并直接返回，压根到不了记 trigger 的地方，
> 于是五点校验必然判为不一致，1000 条只有 1 条通过。详见 `probe-points.md` §二·五。

### ~~缺口：Envoy 下游侧~~ —— 2026-08-10 已补齐

**这一节原来的结论「做不到」是错的**，留在这里作为记录。

原文说：下游的读发生在 `bindTrace` **之前**，那时还没解析 TTHeader，不知道 trace
也不知道采样，无差别打点在压测下不可接受 —— 前半句对，后半句的推论错了。
「采样未知」逼着放弃的只是**事件队列**这一种存储方式，不是打点本身：
改用**每连接固定时间戳槽位、覆盖式写入**，未采样请求的代价就是一次哈希查找加一次
store，零分配，`bindTrace` 时再决定兑现还是丢弃（机制见 `probe-points.md` §一）。

而且原文说「影响有限」也低估了：下游读那一段虽然发生在本跳，却被**上一跳的
「纯等待」**整段吸收 —— 上一跳那段占端到端 79 %，是全链路最大的一坨，
却因为下界卡在 `dn_first_byte` 而从未被分解过。

现在四个 socket 边界对称了，**而且每个方向都同时有「入队」与「真正的系统调用」**：

| | 接收 | 发送（入队） | 发送（系统调用） |
|---|---|---|---|
| Envoy 上游 | `up_readv_start/done` ✓ | `up_encode_done` → `up_socket_write_done` ✓ | `up_writev_start/done` ✓ |
| Envoy 下游 | `dn_readv_start/done` ✓ | `dn_encode_done` → `dn_socket_write_done` ✓ | `dn_writev_start/done` ✓ |

响应回写也从 `resp_decoded → rpc_done` 一个粗粒度区间拆成了
「响应编码 / 写socket(仅入队) / 入队→真正写出 / writev」四段。

---

## 三、排队时间 🟡

请求"已经就绪但还没被处理"的时间。**这一类 2026-08-07 之前几乎完全空白。**

### 已覆盖

| 排队发生在哪 | 点位 | 为什么重要 |
|---|---|---|
| **Envoy 上游事件循环内** | `up_epoll_wake` → `up_readv_start` | c=1 时 190 ns；**c=16 时 p90 15.5 µs / p99 41.5 µs**。**这一刀区分「Envoy 处理慢」和「Envoy 排不过来」** |
| **Envoy 下游事件循环内** | `dn_epoll_wake` → `dn_readv_start` | 与上一行同构，2026-08-10 补上。avg 779 ns（envoy-out）/ 1.2 µs（envoy-in） |
| **Envoy 的异步写排队** | `*_socket_write_done` → `*_writev_start` | 「已入队但事件循环还没轮到写」。下游侧 avg 7.9 / 11.7 µs，是**这一类里最大的一项** |
| **netpoll 同批 epoll 事件内** | `mesh_np_epoll_wake` → `mesh_np_dispatch` | 198 ns（client）/ 345 ns（server），Go 侧的对应物 |
| **goroutine 调度延迟** | `mesh_np_trigger` → `mesh_first_byte`（client）<br>`mesh_np_trigger` → `mesh_netpoll_onread`（server） | **跨机 TCP 11.4 µs / 本机 UDS 2.8 µs（4.1 倍）**。数据已进 LinkBuffer 但 RPC goroutine 还没被调度起来。2026-08-10 起服务端也有，实测 7.3 µs |

这几项是最有价值的产出——它们是"负载升高时时延为什么变差"的直接答案，而此前只能看到总时延变大。

> **「Envoy 的异步写排队」这一项是补齐 writev 点位后才冒出来的。**
> 在此之前它整段藏在「等待上游」里（上游写）或干脆不可见（下游写）。
> 下游侧 7.9~11.7 µs 的量级已经和 readv 系统调用相当 ——
> **Envoy 异步写模型的固有成本，Go 的同步 Flush 没有对应物。**

**2026-08-07 实测补充**，两条读法上的教训：

**① 排队必须看 p90/p99，不能看 p50。** c=16 时 Envoy 事件循环排队的 p50 仍只有 420 ns，
但 p90 已到 15.5 µs、p99 到 41.5 µs。**排队是尾延迟现象不是中位数现象**，
只看 p50 会得出「没有排队」的错误结论。

**② `up_epoll_wake` 的取时位置一度是错的，且症状具有欺骗性。** 初版在 `onFileEvent`
入口取当前时间，那已经在 libevent 派发之后，排队全没被覆盖。发现方式是
**worker 从 384 压到 2、端到端劣化 5.1 倍时，这一段反而从 290 ns 降到 180 ns** ——
读数与物理直觉反向才暴露问题。改取 `dispatcher.approximateMonotonicTime()`
后同条件下是 21.0 / 91.9 µs。修正提交 `envoy@448c7f0f`。

> **教训可迁移**：验证一个「排队」类点位是否真的埋对了位置，
> 不能只看它有没有读数，要**主动制造排队然后看它是否单调变大**。

### 仍然缺的

| 缺什么 | 后果 | 难度 |
|---|---|---|
| **内核 accept 队列 / SYN backlog** | 新连接在内核队列里等多久完全不可见。连接风暴时这是主因 | 需 eBPF 或 `ss -lti` 采样 |
| **TCP 发送缓冲区阻塞** | 对端慢时 write 部分写或阻塞，现在会被算成"编码慢" | 记录 `write()` 返回值与请求长度之差即可，成本低 |
| **连接池等待时长** | 现在只有 `up_conn_new`/`up_conn_reused` 二元标志。`route_resolved → up_conn_*` 勉强可算，但混着 DNS/健康检查 | 低，可加显式点位 |
| **Envoy worker 线程负载不均** | 某个 worker 过载时，其上所有连接一起变慢，但归因会指向"Envoy 处理慢" | 打点时记录 worker/thread id 即可 |

**建议优先补 TCP 发送缓冲区阻塞**——成本最低（记一个返回值），而它现在会伪装成"编码慢"，是主动误导。

---

## 四、链路上的时间 🔴

### 已覆盖：往返

分层差值法（见 probe-points.md §三）能得到三段往返。**统一的式子只有一个**：

```
A↔B 往返 = 「A 花在等 B 身上的时间」 − 「B 的观测总时长」
```

「A 等」两端都卡在**本机与内核交接**的那一刻（`tools/merge/main.go` 的 `waitOf()`）：

| A | 「A 等」取哪两个点 | 实测 avg（2026-08-10） |
|---|---|---:|
| kitex-client | `mesh_socket_read_start` → `mesh_np_epoll_wake` | UDS 往返 **4.7 µs** |
| envoy-out | `up_writev_done` → `up_epoll_wake` | 跨机往返 **68.8 µs** |
| envoy-in | 同上 | UDS 往返 **5.4 µs** |

> **跨机往返 68.8 µs 对上了 `ping` 实测的 RTT 0.068 ms** —— 这是整套口径最强的
> 一个验证信号。口径收紧之前这一列是 100 µs 量级，里面混着两端各自的本地开销。

每一项都是两个各自在本机测得的时长相减，**对时钟偏斜免疫**（实测两机差 15.5 秒左右仍成立，且注入额外 50 秒偏移后输出逐字节不变）。

> **「往返」不是纯线路时间。** B 起算点之前的排队（listener/worker 队列、
> goroutine 调度）落在 B 的测量区间之外，全被这个差值吸收。实测 Envoy worker
> 从 384 压到 2 时，同机 UDS「往返」从 21 µs 涨到 128 µs —— 多出来的一百微秒
> 是排队不是传输。**低负载无排队时才近似真实传输时间。**

### 根本限制一：拆不出单向

差值法只能得往返。拆单向需要两端时钟精确同步到亚微秒——**PTP + 硬件时间戳网卡**。
这不是插桩能力问题，是物理限制。NTP（毫秒级）远远不够。

### 根本限制一·五：报文一大，「往返」这一列就失效 ⚠️（2026-08-11 新发现）

`A↔B 往返 = A 等 − B 总` 隐含「B 的区间完全嵌套在 A 的等待里」。
**单条消息大到要多轮 socket 读写时，对端在发送方还没写完就已经开始读**，
B 总里混进了「A 还在写」的时间，差值可能为负 ——
2026-08-11 的 64K 格实测 avg **−1.00 µs**，p50 −1.8 µs。

同轮 1K/4K/8K/16K 全部正常。**判据：报文接近一次 socket 读写能吞下的量时，
不要引用「往返」那几列**；节点自身与各 socket 分段不受影响。
机制见 `probe-points.md` §三。

### 根本限制二：看不见 socket 以下的一切 ⚠️

**这是整套插桩最重要的盲区。**

一个响应包从对端网卡发出到 Envoy 的 `up_readv_done`，实际经过：

```
对端网卡发出
   │  ← 线缆 / 交换机 / 路由
本机网卡收到           ← 不可见
   │  ← DMA 到 ring buffer，触发硬中断
硬中断处理             ← 不可见
   │  ← 唤起 softirq (NET_RX)
softirq 协议栈处理     ← 不可见（GRO 合并、IP 重组、TCP 乱序队列……）
   │
数据入 socket 接收队列 ← 不可见
   │  ← 唤醒 epoll
epoll_wait 返回        ← ★ up_epoll_wake / mesh_np_epoll_wake 从这里开始
   │
readv()                ← ★ up_readv_start/done
```

**箭头上方那五段，全部被算进"纯等待"（`up_write_done → up_epoll_wake`）。**

平时无所谓——它们小而稳定，混在网络往返里不影响归因结论。
**但要评测网卡，这恰恰是唯一该看的部分。** 换网卡影响的是：中断合并策略、GRO/LRO、
多队列与 RSS 分布、DMA 延迟、驱动 NAPI 轮询——**没有一项落在现有点位的可见范围内**。

---

## 五、要测网卡，必须补的三样

按性价比排序。这三样不改任何业务代码。

### 5.1 SO_TIMESTAMPING —— 最直接，优先做

内核支持给每个包打时间戳，包括**网卡硬件打的**：

```c
int flags = SOF_TIMESTAMPING_RX_HARDWARE | SOF_TIMESTAMPING_RX_SOFTWARE |
            SOF_TIMESTAMPING_TX_HARDWARE | SOF_TIMESTAMPING_TX_SOFTWARE |
            SOF_TIMESTAMPING_RAW_HARDWARE | SOF_TIMESTAMPING_SOFTWARE;
setsockopt(fd, SOL_SOCKET, SO_TIMESTAMPING, &flags, sizeof(flags));
```

时间戳经 `recvmsg` 的辅助数据（`SCM_TIMESTAMPING`）取回。于是可得：

| 段 | 含义 | 换网卡会不会变 |
|---|---|---|
| 硬件 RX 时间戳 → 软件 RX 时间戳 | **网卡收到 → 内核协议栈处理完**（含中断合并、GRO、驱动 NAPI） | **会，而且这就是主要变化** |
| 软件 RX 时间戳 → `up_epoll_wake` | 内核唤醒到用户态被调度 | 基本不变 |

**先确认网卡是否支持硬件时间戳：**

```bash
ethtool -T eth0
```

看 `hardware-receive` / `hardware-transmit` 是否在 Capabilities 里。不支持就只能拿软件时间戳
（打在 softirq 里），仍然比现在强——至少能把协议栈时间和用户态时间分开。

### 5.2 eBPF 挂协议栈关键点

你的 eBPF 环境已配好。挂这几个点即可补上盲区：

| 挂载点 | 得到什么 |
|---|---|
| tracepoint `net:netif_receive_skb` | 包进入协议栈的时刻 |
| kprobe `tcp_v4_rcv` / `tcp_rcv_established` | TCP 层处理时刻 |
| kprobe `sock_def_readable` | 唤醒 epoll 的时刻 |
| tracepoint `irq:softirq_entry/exit`（vec=NET_RX） | **softirq 占用时间**，换网卡后中断合并策略变化直接体现在这里 |

与用户态点位按 `(fd, 时间)` 对齐即可拼出完整链路。

### 5.3 网卡侧的对照指标

时延之外，这几项必须同时采，否则时延数字无法解释：

```bash
ethtool -S eth0 | grep -iE 'drop|error|miss|discard'   # 丢包（时延突刺的头号成因）
ethtool -c eth0                                         # 中断合并参数
ethtool -l eth0                                         # 队列数（影响多核扩展性）
cat /proc/interrupts | grep eth0                        # 中断在哪些 CPU 上（NUMA 亲和）
ss -ti                                                  # 重传、RTT、cwnd
```

**特别提醒重传**：一次重传就是几百毫秒的 RTO，能把 p99 拉到天上，
而现有点位只会显示"等待对端变长了"，完全指不出原因。

---

## 六、点位总账（2026-08-11 核对）

| 位置 | 点位名数 | 相对 2026-08-06 |
|---|---:|---:|
| Envoy（每跳） | **24** | +11（下游读 3、下游写 2+2、上游 writev 2、上游 epoll/readv 3、连接池标志 1 …） |
| Kitex 框架自带 | 12（client 用 10、server 用 10） | 0 |
| Kitex 自定义 | **11** | 0（`mesh_payload_codec_*` 更名为 `mesh_payload_encode_*`） |
| netpoll | **9**（读 5 + 写 4），**两侧都有** | +9 |

两跳拓扑下，一条被采样的请求产生 **107 个去重点位名 / 约 119 个事件**
（实测 1000 条 trace 逐条相同：client 29、envoy-out 24、envoy-in 24、server 30）。

> 上一版这里写着「Envoy 13 / netpoll 5 / 约 60 个事件」，那是 2026-08-10 补下游
> 与写路径之前的账。**核对方式**：把源码里的点位字面量抓出来，与实测 trace 里
> 去重后的点位名逐一对照，两边必须完全相等 —— 光看代码会漏掉「写了但从没被
> 执行到」的点位（`dn_writev_*` 就当过一阵这样的点）。

---

## 七、优先级建议

| 优先级 | 补什么 | 理由 |
|---|---|---|
| **P0** | `ethtool -T` 确认硬件时间戳 + SO_TIMESTAMPING | 网卡测试的前提。不做的话换网卡只能看到"总时延变了"，说不出为什么 |
| P1 | TCP 发送缓冲区阻塞（记 write 返回值） | 成本极低，且它现在会伪装成"编码慢"，属主动误导 |
| P1 | eBPF 补协议栈盲区 | 环境已就绪 |
| P2 | 中间件链 / filter 链 | demo 里测不出东西，迁到真实业务前再做 |
| P2 | netpoll 写慢路径（EPOLLOUT 之后由 poller 续写） | 小报文一次写完，走不到；大报文或对端慢时才需要 |
| P2 | 内核 accept 队列 | 只在连接风暴场景才重要 |

> **~~P0：netpoll 写路径（writev 边界）~~ 已于 2026-08-10 完成**，
> 连带 Envoy 的 `up_writev_*` / `dn_writev_*` 一起补齐。
> 它当初被列为 P0 的理由是「唯一一个已知有异常但查不出原因的问题
> （Go 写 socket 贵 7–10 倍）」—— 拆开之后发现**那个异常根本不存在**，
> 是拿入队时间比系统调用时间比出来的。见 §二。
