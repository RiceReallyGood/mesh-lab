# 打点完整性审计

按四类核对覆盖度：**各阶段处理时间**、**socket 接口时间**、**排队时间**、**链路上的时间**。
每一类先说该类里"一条请求会在哪些地方花时间"，再对照现有点位，最后列出缺口。

判定标准仍是那一条：**去掉它之后，哪个区间就算不出来了。**

> 本轮新增：Envoy 上游接收路径 3 点、netpoll 读路径 5 点。
> 详细点位清单见 [probe-points.md](probe-points.md)，本文只做覆盖度评估。

---

## 结论速览

| 类别 | 覆盖 | 最大缺口 |
|---|---|---|
| 各阶段处理时间 | 🟢 **好** | 中间件/filter 链内部不可分 |
| socket 接口时间 | 🟡 **Envoy 四个边界已对称（2026-08-10）；Go 侧发送仍不对称** | **netpoll 的 writev 边界没拆** —— 这正是"Go 侧写 socket 贵 7–10 倍"查不出原因的地方 |
| 排队时间 | 🟡 **本轮从零到部分覆盖** | 内核 accept 队列、TCP 发送缓冲区阻塞、服务端 goroutine 池 |
| 链路上的时间 | 🔴 **只能得往返，且看不见 socket 以下的一切** | **网卡、驱动、协议栈、softirq 全部不可见** |

**最重要的一条：整套插桩的下界是 socket 系统调用。** 网卡、驱动、中断、协议栈的时间统统被算进"纯等待"这一坨里。
平时这不要紧——那部分本来就小且稳定。但**一旦要评测网卡本身，这恰好是唯一该看的部分，而现在完全看不见。**
补法见 §5。

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
| payload 序列化 | `mesh_payload_codec_*` | 300 / 470 ns |
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

## 二、socket 接口时间 🟡

read/write 系统调用本身的耗时，以及围绕它的缓冲区管理。

### 已覆盖

实测值取自 2026-08-07 跨机双跳、c=1、payload 64 B（`results/2026-08-07/two-lo-detail.txt`），
两个数字分别是 950 侧 / 920B 侧：

| 位置 | 点位 | 实测 p50 |
|---|---|---|
| Envoy 上游**接收** | `up_readv_start` → `up_readv_done` | **3.5 / 6.3 µs** |
| Envoy 上游**发送** | `up_encode_done` → `up_socket_write_done` | 290 / 710 ns |
| netpoll **接收** | `mesh_np_readv_start` → `mesh_np_readv_done` | **2.5 µs**（仅客户端侧） |
| Kitex **发送** | `mesh_socket_write_start` → `mesh_socket_write_finish` | 1.8 / 5.7 µs |

> **接收比发送贵一个数量级**（Envoy 3.5 µs vs 290 ns）。`doRead` 内部是循环，
> 反复 readv 直到 EAGAIN，所以是「N 次 readv + N 次 append」的总和，
> 而发送侧一次 `writev` 就完事。这不是异常。
>
> 950 与 920B 的差距（3.5 vs 6.3 µs，2.9 倍）是**机器差异**，不是路径差异 ——
> 见 `test-report.md` §6 的同机对照实验。

### 缺口：发送侧的不对称 ⚠️

**这是本轮留下的最大技术债。**

之前测出一个没解释的现象：**Go 侧写 socket 比 Envoy 贵 7–10 倍**（1.8–2.9 µs vs 270–690 ns）。
本轮把**接收**侧拆开了，但**发送**侧没有：

| | 接收 | 发送 |
|---|---|---|
| Envoy | `doRead` 前后 ✓ | `write()` 后（只有终点）△ |
| Go | netpoll `readv` 前后 ✓ | **Flush 前后，里面混着 LinkBuffer 管理 + writev + 可能的 epoll 注册** ✗ |

`mesh_socket_write_start/finish` 括住的是 `bufWriter.Flush()`，而 netpoll 的 Flush 内部要做：
LinkBuffer 链表整理 → `outputs()` 拼 iovec → `writev` → 写不完则注册 EPOLLOUT 等下次。
**这四件事混在一起，所以那 7–10 倍差距至今无法归因。**

要拆开需在 netpoll 的 `connection_reactor.go` 的 `outputs`/`outputAck` 与 `sys_exec.go` 的 `writev` 上再加一组点位，
结构与本轮做的读路径完全对称。**建议下一轮就做**——读路径的框架（`ReadProbe` 槽位、原子快照、`Consistent` 校验）可以直接复用。

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

现在四个 socket 边界对称了：

| | 接收 | 发送 |
|---|---|---|
| Envoy 上游 | `up_readv_start/done` ✓ | `up_encode_done` → `up_socket_write_done` ✓ |
| Envoy 下游 | `dn_readv_start/done` ✓ | `dn_encode_done` → `dn_socket_write_done` ✓ |

响应回写也从 `resp_decoded → rpc_done` 一个粗粒度区间拆成了「响应编码 / 写 socket」两段。

---

## 三、排队时间 🟡

请求"已经就绪但还没被处理"的时间。**这一类本轮之前几乎完全空白。**

### 本轮新增覆盖

| 排队发生在哪 | 点位 | 为什么重要 |
|---|---|---|
| **Envoy 事件循环内** | `up_epoll_wake` → `up_readv_start` | c=1 时 130 ns；**c=16 时 p90 15.5 µs / p99 41.5 µs**。**这一刀区分「Envoy 处理慢」和「Envoy 排不过来」** |
| **netpoll 同批 epoll 事件内** | `mesh_np_epoll_wake` → `mesh_np_dispatch` | 190 ns ~ 1.1 µs，Go 侧的对应物 |
| **goroutine 调度延迟** | `mesh_np_trigger` → `mesh_first_byte` | **跨机 TCP 11.4 µs / 本机 UDS 2.8 µs（4.1 倍）**。数据已进 LinkBuffer 但 RPC goroutine 还没被调度起来 |

这三项是本轮最有价值的产出——它们是"负载升高时时延为什么变差"的直接答案，而此前只能看到总时延变大。

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
| **服务端 goroutine 池排队** | Kitex server 侧 `OnRead` → handler 之间的调度。客户端侧已有 `mesh_np_trigger`，服务端侧没有对应物 | 中等，`mesh_netpoll_onread` 已是半个 |
| **连接池等待时长** | 现在只有 `up_conn_new`/`up_conn_reused` 二元标志。`route_resolved → up_conn_*` 勉强可算，但混着 DNS/健康检查 | 低，可加显式点位 |
| **Envoy worker 线程负载不均** | 某个 worker 过载时，其上所有连接一起变慢，但归因会指向"Envoy 处理慢" | 打点时记录 worker/thread id 即可 |

**建议优先补 TCP 发送缓冲区阻塞**——成本最低（记一个返回值），而它现在会伪装成"编码慢"，是主动误导。

---

## 四、链路上的时间 🔴

### 已覆盖：往返

分层差值法（见 probe-points.md §3）能得到三段往返：

| 段 | 算法 |
|---|---|
| client↔envoy-out UDS 往返 | client总 − envoy-out总 − client本地处理 |
| 跨机网络往返 | envoy-out总 − envoy-in总 − envoy-out处理 |
| envoy-in↔server UDS 往返 | envoy-in总 − server总 − envoy-in处理 |

每一项都是两个各自在本机测得的时长相减，**对时钟偏斜免疫**（实测两机差 15.5 秒左右仍成立，且注入额外 50 秒偏移后输出逐字节不变）。

### 根本限制一：拆不出单向

差值法只能得往返。拆单向需要两端时钟精确同步到亚微秒——**PTP + 硬件时间戳网卡**。
这不是插桩能力问题，是物理限制。NTP（毫秒级）远远不够。

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

## 六、本轮之后的点位总账

| 位置 | 点位数 | 本轮新增 |
|---|---:|---:|
| Envoy（每跳） | 13 | **+3** |
| Kitex 框架自带 | 12 | 0 |
| Kitex 自定义 | 12 | 0 |
| netpoll | 5 | **+5** |

两跳拓扑下，一条被采样的请求产生约 **60 个事件**。

---

## 七、优先级建议

| 优先级 | 补什么 | 理由 |
|---|---|---|
| **P0** | netpoll **写**路径（writev 边界） | 唯一一个"已知有异常但查不出原因"的问题（Go 写 socket 贵 7–10 倍）。框架可复用读路径的 |
| **P0** | `ethtool -T` 确认硬件时间戳 + SO_TIMESTAMPING | 网卡测试的前提。不做的话换网卡只能看到"总时延变了"，说不出为什么 |
| P1 | TCP 发送缓冲区阻塞（记 write 返回值） | 成本极低，且它现在会伪装成"编码慢"，属主动误导 |
| P1 | eBPF 补协议栈盲区 | 环境已就绪 |
| P2 | 中间件链 / filter 链 | demo 里测不出东西，迁到真实业务前再做 |
| P2 | 内核 accept 队列 | 只在连接风暴场景才重要 |
