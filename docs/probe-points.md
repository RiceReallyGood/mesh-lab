# 打点位置清单

本文逐点说明每个打点的**含义**、**代码位置**、以及**它使哪个区间可测**。
用于讨论增删点位 —— 判断一个点该不该存在,标准是"去掉它之后,哪个区间就算不出来了"。

> **与代码的对应关系核对于 2026-08-11**，四个仓库的分支见根目录 `README.md`
> 「代码分布」。核对方式是把源码里所有点位字面量抓出来，与 1000 条实测 trace
> 里去重后的点位名逐一对照，两边必须完全相等。当前口径：
>
> | 节点 | 每条 trace 的去重点位名 | 每条 trace 的实际事件数 |
> |---|---:|---:|
> | Envoy（每跳） | **24** | ~30（`onWriteReady` 重复触发，见 §一末） |
> | kitex-client | **29** | 29 |
> | kitex-server | **30** | 30 |
> | 合计（跨机双跳） | **107** | **~119** |

---

## 一、Envoy 侧(每跳 24 个点位名)

请求经过一个 Envoy sidecar 的完整路径。**带 ★ 的是 socket 系统调用边界**：

```
 ⓪dn_epoll_wake            下游 epoll 就绪
        │  ← 下游事件循环排队
 ★dn_readv_start / dn_readv_done      下游收包（readv）
        │  ← buffer 管理 + filter chain 派发
 ①dn_first_byte            下游数据到达（filter 的 onData 入口）
        │  ← TTHeader 解析
 ②hdr_decoded              trace 可知（此刻才知道是否采样）
        │  ← 协议层解消息头
 ③msg_begin                method name 可见
        │  ← 路由匹配
 ④route_resolved           cluster 已选定
        │  ← 从连接池取上游连接
   up_conn_new / up_conn_reused       二选一，新建还是复用
        │  ← 帧编码
   up_encode_done
        │  ← ConnectionImpl::write()，**只入队**
 ⑤up_write_done / up_socket_write_done
        │  ← 等事件循环轮到写
 ★up_writev_start / up_writev_done    上游真正发包（writev）
        │  ← ★★ 纯等待：对端处理 + 网络在途 + 内核到 epoll 就绪
   up_epoll_wake           上游 epoll 就绪
        │  ← 上游事件循环排队
 ★up_readv_start / up_readv_done      上游收包（readv）
        │  ← buffer 管理 + filter chain 派发
 ⑥up_first_byte            上游响应首字节
        │  ← 响应解码
 ⑦resp_decoded             响应解析完成
        │  ← 下游响应编码
   dn_encode_done
        │  ← connection().write()，**只入队**
   dn_socket_write_done
        │
 ⑧req_done / rpc_done      RPC 侧结束
        │  ← 等事件循环轮到写（**这一段已经在 rpc_done 之后**）
 ★dn_writev_start / dn_writev_done    下游真正发包（writev）
```

> **⑤ 与 `up_socket_write_done` 是同一时刻的两个名字**：`up_write_done` 记在
> `router_impl.cc messageEnd` 里 `encodeAndWrite()` 返回之后，
> `up_socket_write_done` 记在 `upstream_request.cc` 的 `connection().write()`
> 之后，中间只隔一个函数返回。前者是骨架点（分层分解用），后者是细分点。

> **四个 socket 边界，每个都拆到了系统调用**。每跳 Envoy 横跨两条连接
> （下游 UDS、上游 TCP），各有收发。2026-08-10 之前只有上游那条被插桩，
> 而且发送侧只记到「入队」为止：
>
> | | 接收 | 发送（入队） | 发送（真正的系统调用） |
> |---|---|---|---|
> | 上游 | `up_readv_start/done` | `up_encode_done`→`up_socket_write_done` | `up_writev_start/done` |
> | 下游 | `dn_readv_start/done` | `dn_encode_done`→`dn_socket_write_done` | `dn_writev_start/done` |
>
> 补齐前有两个后果，都不是「少两行数据」那么轻：
> 一是「上一跳的纯等待」里混着下一跳 `dn_first_byte` 之前的全部时间，
> 而那一段占端到端约 79 %；二是拿 Envoy 的**入队时间**去比 Go 的**系统调用时间**，
> 得出了「Go 写 socket 比 Envoy 贵 7–10 倍」这个错误结论（详见
> `probe-coverage-audit.md` §二）。

> **①–⑧ 是点位的权威编号，本文是它的出处。** merge 的 detail 输出
> 2026-08-10 起不再把编号印在行首 —— 四个节点里只有 Envoy 有这套编号，
> 混排反而难读，改用统一的树形缩进表达包含关系。查编号对应关系看本表。

| # | 点位 | 代码位置 | 含义 | 去掉它就算不出什么 |
|---|---|---|---|---|
| ① | `dn_first_byte` | `conn_manager.cc` `onData` 入口 | 下游字节到达 Envoy 的最早时刻 | **"数据到达"到"开始解析"的间隔**;这是唯一早于 TTHeader 解析的点,也是与上一跳衔接的锚点 |
| ② | `hdr_decoded` | `transportBegin`(即 `decodeFrameStart` 之后) | TTHeader 解析完成,trace 与采样标志在此刻才可知 | **TTHeader 解析耗时**(①→②)。这个点还承担回填职责:①记录时 trace 未知 |
| ③ | `msg_begin` | `conn_manager.cc` `messageBegin` | 协议层解出消息头,method name 可见 | **协议层解析耗时**(②→③) |
| ④ | `route_resolved` | `router_impl.cc` `messageBegin`,`route_entry_` 取到后 | 路由匹配完成,cluster 已选定 | **路由匹配耗时**(③→④) |
| ⑤ | `up_write_done` | `router_impl.cc` `messageEnd`,`encodeAndWrite` 之后 | 请求已编码并写往上游 | **编码+取连接+写出的耗时**(④→⑤) |
| ⑥ | `up_first_byte` | `router_impl.cc` `onUpstreamData` | 上游响应首字节到达 | **★ 等待上游的时间**(⑤→⑥)—— 这是本跳之后所有环节的总和,是分层归因的关键 |
| ⑦ | `resp_decoded` | `conn_manager.cc` `ResponseDecoder::finalizeResponse` | 响应解析完成 | **响应解码耗时**(⑥→⑦) |
| ⑧ | `req_done` / `rpc_done` | `finalizeRequest` / `doDeferredRpcDestroy` | 请求侧处理完成 / RPC 真正结束 | ⑦→⑧ 这一段现已细分为「响应编码 / 写 socket(仅入队) / 入队→真正写出 / writev」四段,见 §「下游写」 |

> **⑧ 不是本跳的最后一个事件。** `dn_writev_*` 发生在 `rpc_done` **之后**
> （write 只入队，真正的 writev 由事件循环稍后执行）。所以 merge 里
> 「节点总时长」的终点是 `dn_writev_done` 而不是 `rpc_done` —— 这一点直接影响
> 端到端分解：终点收在 `rpc_done` 的话，本跳异步写出的那几微秒会被算进
> 「往返传输」，同时低估本跳自身。

### 由这些点导出的关键量

| 量 | 算法 | 意义 |
|---|---|---|
| **本跳自身处理** | 节点总时长 − 纯等待 | 去掉等待上游的部分,才是这一跳真正消耗的 CPU 时间 |
| **纯等待** | `up_writev_done` → `up_epoll_wake` | 请求真正离开本机、到响应在本机 epoll 就绪为止 |
| 解析开销 | ①→③ | TTHeader + 协议层 |
| 路由开销 | ③→④ | |

> **「纯等待」的两端 2026-08-10 都收紧过，收紧的理由是同一条：把节点自己干的活
> 从「往返」里踢出去。** 起点原为 ⑤（`up_write_done`），那只是入队，请求还没离开
> 本机，中间那段等事件循环的时间被算成了网络；终点原为 ⑥（`up_first_byte`），
> 那已经在 readv、buffer、filter 派发之后，本机取数据的几微秒也被算成了网络。
> 现在两端都卡在「本机与内核交接」的那一刻上。实现见 `tools/merge/main.go` 的
> `waitOf()`。

**「纯等待」是整个设计里最关键的一个量。** 没有它就无法把"本跳处理"与"等待下游"分开,归因会退化成只知道总时长。

### ④→⑤ 的细分(已实现)

原本 `route_resolved → up_write_done` 一段把三件事混在一起(实测 7～16 µs),
现已拆开:

| 点位 | 位置 | 含义 |
|---|---|---|
| `up_conn_new` / `up_conn_reused` | `UpstreamRequest::onPoolReady` | **上游连接是新建/排队得来,还是直接复用**。这让建连成本可以按请求归因,而不只是从 `cluster.*.upstream_cx_total` 看聚合值 |
| `up_encode_done` | `encodeAndWrite` 中 `encodeFrame` 之后 | 帧编码完成 |
| `up_socket_write_done` | `connection().write()` 之后 | **入队**完成（不是系统调用，见下） |

为此给 `RequestOwner` 加了 `downstreamConnectionId()`,**带默认实现返回 0** ——
`ShadowRouterImpl`(影子流量,不需要打点)因此不受影响。

> **`up_conn_new` / `up_conn_reused` 二选一，这让依赖它的两段区间样本数少一个。**
> merge 的「取上游连接」「帧编码」两段以 `up_conn_reused` 为界，遇到新建连接的
> 请求就取不到 —— 实测 1000 条里 999 条复用、1 条新建（首请求），于是那两行显示
> `n=999` 而其余是 `n=1000`。**这是正常的，不是丢数据。**
> 要把新建那条也算进来，得让 merge 支持「两个点位取其一」的区间定义。

### ⑤→⑥ 的细分(已实现)

原本 `up_write_done → up_first_byte` 这段「等待上游」把三件事混在一起:真正的等待、
readv 系统调用、事件循环调度。现已拆开:

| 点位 | 位置 | 含义 |
|---|---|---|
| `up_epoll_wake` | `connection_impl.cc` `onFileEvent`,取 **`dispatcher.approximateMonotonicTime()`** | **事件后端返回、尚未派发任何回调的那一瞬**。⑤→这里才是真正的等待 |
| `up_readv_start` / `up_readv_done` | `connection_impl.cc` `transport_socket_->doRead()` 前后 | socket 收包。注意 `doRead` 内部是循环,会反复 readv 直到 EAGAIN,所以是「N 次 readv + N 次 append」的总和 |
| `up_writev_start` / `up_writev_done` | `connection_impl.cc` `onWriteReady()` 里 `doWrite()` 前后 | **真正的 writev**。`up_socket_write_done` 记的是 `write()` 入队,不是系统调用 —— 这个区别曾让「Go 写 socket 比 Envoy 贵 7–10 倍」的错误结论挂了很久。`onWriteReady` 每条 trace 触发多次,第 1 次才是请求发送,其余是空写;merge 取最早一次 |

> **`up_epoll_wake` 取的是缓存值,不是「现在」——这一点很容易改错,改错了还看不出来。**
>
> 初版在 `onFileEvent` 入口取当前时间,是**错的**:那里已经在 libevent 派发到本连接
> **之后**,于是 `up_epoll_wake → up_readv_start` 只剩几百纳秒的分支开销,恒定不变。
>
> **发现方式**:把 Envoy worker 从 384 压到 2、端到端劣化 5.1 倍,
> 这一段**反而**从 290 ns 降到 180 ns —— 排队明明变严重了读数却变小,说明真正的排队
> 全发生在这个点之前,完全没被覆盖。
>
> 改取 `approximateMonotonicTime`(由 `DispatcherImpl` 注册的 libevent check 回调
> `updateApproximateMonotonicTime` 更新)之后,同样条件下 envoy-out 21.0 µs /
> envoy-in 91.9 µs,**差两三个数量级**。顺带还省一次 `clock_gettime` ——
> 这个值本来就是缓存好的。
>
> 修正提交 `envoy@448c7f0f`(2026-08-07)。

由此得到:

| 段 | 含义 | 实测(双跳 envoy-out,p50) |
|---|---|---|
| `up_writev_done` → `up_epoll_wake` | **纯等待**:对端处理 + 网络在途 + 内核协议栈到 epoll 就绪 | **161.4 µs**(占本跳观测区间的 81 %) |
| `up_epoll_wake` → `up_readv_start` | **事件循环内排队** | c=1 时 **190 ns**;c=16 时 p50 420 ns、**p90 15.5 µs、p99 41.5 µs** |
| `up_readv_start` → `up_readv_done` | readv 系统调用 | 1.9 µs |
| `up_readv_done` → ⑥ | buffer 管理 + filter chain 派发 | 170 ns |

> p50 取自 `results/2026-08-10/two-cross-detail.txt`（c=1）；c=16 那一列仍来自
> `results/2026-08-07/two-hi-detail.txt`，因为 2026-08-10 那轮只跑了 c=1。
> 「纯等待」的起点 2026-08-07 时还是 ⑤（入队），当时读数 177.4 µs，
> **两个数不可直接相比** —— 换了起点，也换了机器负载。

**「事件循环内排队」这一刀区分「Envoy 处理慢」与「Envoy 排不过来」。**
读它必须看 p90/p99 —— **排队是尾延迟现象不是中位数现象**,低负载或只看 p50
都会得出「没有排队」的错误结论。数据来源 `results/2026-08-07/two-{lo,hi}-detail.txt`。

**绑定机制**:给 `Network::Connection` 加了四个带默认实现的虚函数
`enableKitexProbe(uint64_t)` / `disableKitexProbe()` / `kitexProbeEnabled()` /
`kitexProbeDownstreamId()`（`envoy/network/connection.h`）。上游连接由
`UpstreamRequest::onPoolReady` 在采样命中时 `enableKitexProbe(下游 conn_id)`,
`releaseConnection` 与 `onResponseComplete` 两条释放路径上都 `disableKitexProbe()`。

> **开关与取值必须是两个字段，不能拿「id==0」当未采样的哨兵。**
> `ConnectionImpl::next_global_id_` 从 0 开始，进程里第一条连接的 id 就是 0，
> 于是「采样命中且下游 conn_id 为 0」与「未采样」无法区分。繁忙的 Envoy 上这只
> 影响第一条连接；**本实验只有一条下游连接，结果是 100 % 丢数据**，而且不报错，
> 表现为「点位一条都没有」。修正提交 `envoy@6af429ba`。

不用旁路 map 的原因:通用读路径服务全进程所有连接,压测下每个 epoll 事件做一次哈希查找不可接受。
裸成员让未采样路径退化成一次读加一次分支。

**上下游共用这套开关，但语义不同**。上游连接在 `onPoolReady` 里**查过采样**才开；
下游连接在 `ConnectionManager::initializeReadFilterCallbacks` 里**无条件开**
（只判 `KitexProbe::enabled()`），因为下游读发生在 `bindTrace` 之前,
那时还不知道采样与否 —— 所以下游走的是另一套存储机制,见下。

### 下游读:时间戳槽位(2026-08-10 新增)

下游读的采样状态**在物理上不可知**:traceparent 还在没解析的字节流里。
既不能像上游那样「查一次采样、挂裸标志」,也不该像 `dn_first_byte` 那样往
`pending` vector 里塞完整 Event —— 3 个点 × 99% 未采样请求 = 白做的 `push_back`
与可能的扩容,违反「未采样近乎零开销」。

改用**每连接固定三个 int64 槽位,覆盖式写入**:

```cpp
struct ConnSlots { int64_t epoll_wake, readv_start, readv_done; };   // probe.cc
void connSlot(uint64_t conn_id, Slot which, MonotonicTime mono);      // probe.h
```

- 写:读路径上一次哈希查找 + 一次 store,零分配。**这就是未采样请求付的全部代价。**
- 取:`bindTrace` 确认采样后才兑现成事件,wall 由基准点推算(不回头读 CLOCK_REALTIME)。
- 清:`endRpc` 里连同 `bindings`/`pending` 一起 erase,防止 map 无界增长。

与 Kitex 侧 netpoll 探针的「时间戳槽位」是同一个模式。

**side 的判定不改 `envoy/network/connection.h`**:`ClientConnectionImpl : public
ConnectionImpl` —— 主动发起的连接天然是派生类,被 accept 的不是。构造函数里置
`kitex_probe_upstream_` 即可。改那个核心接口头会触发 1134 个动作的大范围重编,
而且多一处 rebase 冲突点;走派生类判定是 26 秒的增量。

### 下游写：RPC 结束之后才发生的点位

`dn_writev_*` 有个特殊之处：**它发生在 `rpc_done` 之后**。conn_manager 里
`write()` 只入队，真正的 writev 由事件循环稍后执行，那时普通 `rpcEvent`
已经查不到绑定了（实测 `dn_writev_*` 0 条）。

解法不是「不擦绑定」—— 那在流水线下会出错：请求 N 的 writev 还没跑，
N+1 的 `bindTrace` 就把 `bindings[conn]` 覆盖了，N 的写会被记到 N+1 头上。

改为**单独一个 `finishing` 槽**：`endRpc` 把 binding 移交过去，`bindings`
照常擦（`isSampled` 等语义不变）；下游 writev 走 `rpcEventTail()` 先查这个槽，
查不到再回落到 `bindings`（覆盖「事件循环先于 deferred delete」的顺序），
`dn_writev_done` 记完即释放（否则 `onWriteReady` 后续的空写会继续挂在它名下）。

> **归档数据 `results/2026-08-10/` 早于这一版实现，读那批 `dn_writev_*` 要留神。**
> 那轮跑于 20:19，用的是中间版本 `envoy@cad62df`（「干脆不擦绑定」），
> `finishing` 槽是 20:31 的 `envoy@981475a` 才落地的。表现上的差别：
>
> | | 那批数据（不擦绑定） | 现在（`finishing` 槽） |
> |---|---:|---:|
> | 每条 trace 的 `dn_writev_start` | envoy-out **3 次**、envoy-in 2 次 | **1 次** |
>
> 多出来的是 `onWriteReady` 的空写（实测 40 ns 量级），因为绑定没被释放而继续
> 挂在同一条 trace 名下。**但归档的分位数不受影响** —— merge 对同名点取最早一次，
> 取到的恰好是真正那次 writev。要按事件数核对完整性的话，得用当前口径重跑。

**一条连接同时最多容纳一个「待写出」的 RPC。** 前一个还没写出就又结束一个时，
计入 `g_write_lost` 并丢弃 —— **宁可丢也不能误记**。这也是诚实的：流水线下
一次 writev 可能同时写出多个响应，「某个 RPC 的 writev」本就不可拆。

该计数出现在收尾行里，**是完整性判据的一部分**：

```
[probe] node=envoy-out host=suzhou950 记录=832004 落盘=832004 丢弃=0 下游写未归属=0
```

实测 c=1 / 16 / 64 三档均为 0（Kitex 用连接池，每条连接串行，因果上不会出现
流水线）。c=64、每节点 32000 条 trace 的因果校验：`dn_writev_start` 早于本条
`dn_socket_write_done` 的**倒挂 0 条**，距入队超 50ms 的**异常 0 条**，
入队→writev 的 p50 为 7.0 / 10.5 µs。

### 已知缺口

| 想测但目前测不了 | 为什么还没做 |
|---|---|
| **listener accept** | 属于连接级而非请求级;新建连接的成本已体现在 client 侧的 `client_conn_start/finish` 与上游侧的 `up_conn_new` |
| **filter chain 内各 filter 耗时** | 本实验只挂 thrift_proxy 一个 filter,测不出东西;真实部署挂十几个 filter 时必须补 |

---

## 二、Kitex 侧(框架自带 12 个事件名 + 自定义 11 个)

按边算：**client 侧 10 + 10 = 20 个，server 侧 10 + 11 = 21 个**，
再加 §二·五 的 netpoll 9 个，就是实测的 client 29 / server 30。

### 框架自带（12 个事件名，client 用 10、server 用 10）

**这些点零源码改动即可获得** —— 都是 Kitex `pkg/stats` 预定义的事件,只需 `WithStatsLevel(LevelDetailed)` 加一个 Tracer。

| 点位 | Kitex 记录位置 | 哪一侧 | 含义 | 使哪个区间可测 |
|---|---|---|---|---|
| `rpc_start` / `rpc_finish` | 顶层 | 两侧 | RPC 起止 | **端到端总时长**,差值法的输入 |
| `client_conn_start` / `client_conn_finish` | `remotecli/conn_wrapper.go:121,139` | **仅 client** | 从连接池取连接 | **建连成本**。实测冷启动可达 2.3 ms,是首请求延迟的主因；池命中时只有 240 ns |
| `write_start` / `write_finish` | `default_client_handler.go:51,54`<br>`default_server_handler.go:82,85` | 两侧 | 编码并写入 socket | **发送耗时**(含编码) |
| `read_start` / `read_finish` | `default_client_handler.go:93,96`<br>`default_server_handler.go:139,137` | 两侧 | 读取并解码 | **接收耗时** |
| `wait_read_start` / `wait_read_finish` | `codec/thrift/thrift.go:215,224` | 两侧 | 名为「等待 body」实为**反序列化** | 见下面「三个名字骗人的区间」① |
| `server_handle_start` / `server_handle_finish` | `server/server.go:373,368` | **仅 server** | 业务 handler 执行 | **业务逻辑耗时**。echo 场景实测 **220 ns（2026-08-07）/ 260 ns（2026-08-10）**,便于把框架开销与业务开销分开 |

> `read_start` 在服务端的行号（139）大于 `read_finish`（137），不是笔误 ——
> 那一处是 `defer` 与顺序执行的关系，`Record(ReadFinish)` 写在 defer 里、
> 位置在上，实际执行在后。

### 自定义（11 个，`kitex/pkg/stats/meshlab_events.go`）

全部经 `DefineNewEvent` 注册,不占预定义槽位,不影响既有消费方。

| 点位 | 位置 | 哪一侧 | 为什么现有事件不够 |
|---|---|---|---|
| **`mesh_first_byte`** | `default_codec.go:204`，`DecodeMeta` 首次 `Peek` 返回后 | 两侧 | **最关键的一个**。`ReadStart` 记录于 `Read()` 入口,那时还没开始读,真正的阻塞等待埋在 `codec.Decode` 内部 —— 于是 `read_start→read_finish` 吞掉了整个网络等待(实测占 client 侧 96%),等于没有分解 |
| `mesh_socket_read_start` | `default_codec.go:197`，阻塞 `Peek` 之前 | 两侧 | 与上一个配对,得到**纯粹的等待对端时间**（仅 client 侧成立，见 ②） |
| **`mesh_netpoll_onread`** | `default_server_handler.go:187`，`OnRead` 入口 | **仅 server** | **epoll 唤醒后用户态的第一个可记录时刻**。服务端的 goroutine 调度延迟只能量到这里为止 |
| **`mesh_socket_write_start/finish`** | `default_client_handler.go:84,86`<br>`default_server_handler.go:118,120`，`bufWriter.Flush()` 前后 | 两侧 | `WriteStart/Finish` 括住"编码+写"整段,**分不清是序列化慢还是 socket 慢**。Flush 才是真正触发 write 系统调用的地方 |
| `mesh_hdr_decode_start/finish` | `default_codec.go:209,212` | 两侧 | TTHeader 解析耗时 |
| `mesh_hdr_encode_start/finish` | `default_codec.go:120,130` | 两侧 | TTHeader 编码耗时;mesh 场景的核心成本项 |
| `mesh_payload_encode_start/finish` | `default_codec.go:131,133`，`encodePayload` 前后 | 两侧 | thrift **序列化**耗时。**只在发送路径上** —— 见下面「payload 只有编码没有解码」 |

> **`mesh_payload_encode_*` 曾叫 `mesh_payload_codec_*`。** 名字里的 codec 暗示
> 两个方向都覆盖，实际只插在 `encodePayload` 前后，分析侧据此把它标成
> 「payload 解码」，错了整整一轮。**反序列化方向不另设点位** ——
> 上游预定义的 `wait_read_*` 已经精确括住 `unmarshalThriftData`，
> 重复打点只会多一份开销和一份要维护的真相。

### 三个名字骗人的区间(2026-08-10 全部改名)

判断一个区间是什么,**要看它在源码里包住了哪段代码**,不能看名字。
下面三个都因名字被长期误读过。

#### ① `wait_read_*` 不是「等待」,是**反序列化**

`kitex/pkg/remote/codec/thrift/thrift.go`:

```go
rpcinfo.Record(ctx, ri, stats.WaitReadStart, nil)
err = c.unmarshalThriftData(ctx, in, data, dataLen)   // ← thrift 反序列化在这里面
rpcinfo.Record(ctx, ri, stats.WaitReadFinish, err)
```

包住的是 `unmarshalThriftData`(按需读取缺失字节 + 反序列化)。
实测 server 550ns / client 400ns,**与编码同量级**(server 500ns / client 320ns),
正说明它是解码而不是等待。旧名「等待请求体 / 等待响应体」会让人以为开销在等字节。

**推论:接收方向的反序列化一直有点位,只是名字骗人。** 不需要另加。

#### ② 服务端的 `mesh_socket_read_start → mesh_first_byte` 不是「等待对端」

包住的是 `default_codec.go` 的 `in.Peek(2*Size32)`:

- **client 侧**:这次 Peek 真的阻塞等响应,实测 213µs —— 叫「等待对端」没问题
- **server 侧**:netpoll 的 poller **已经把数据放进 LinkBuffer** 才会触发 `OnRead`,
  此处 Peek 立即返回,实测 **310ns**。**那里没有任何网络等待**,已改名「取首字节(Peek)」

服务端真正的「等对端」发生在两个请求之间的空闲期,**不属于这条 RPC**,也不该被计入。

> **它也不是 readv 时间。** 服务端的 readv 发生在 poller goroutine 里、
> `mesh_netpoll_onread` **之前**。2026-08-10 之前 netpoll 读探针只插在客户端，
> 所以那一段确实完全没测；现在服务端也有了（连接级常开、`OnRead` 入口取快照，
> 见 §二·五），实测 readv 4.4 µs、goroutine 调度延迟 7.3 µs。
> **要看服务端的收包耗时，看 `mesh_np_*` 那五段，不要看这里。**

#### ③ payload 那对点位只有编码,没有解码

插在 `encodePayload` 前后,**只覆盖发送路径**:

- client 侧实测落在 3.17µs —— 在 `mesh_socket_write_start`(3.59µs) **之前**,是编请求
- server 侧实测落在 11.13µs —— 在 `server_handle_finish`(9.50µs) **之后**,是编响应

这一轮**连事件名带区间名一起改了**：事件名由 `mesh_payload_codec_*` 改成
`mesh_payload_encode_*`（`kitex/pkg/stats/meshlab_events.go`），merge 的区间名由
「payload解码」改成「payload编码」。解码那一半由上面 ① 覆盖。

> **改事件名而不只是改区间名，是因为名字骗人的根源在事件名上。**
> 只改分析侧的话，下一个读 `meshlab_events.go` 的人还会再错一次。

> 早期报告里出现「payload解码」「等待请求体」「等待响应体」字样的,
> 按本节重新理解。

### client 侧两段的先后关系

`★等待对端(到epoll就绪)` 与 `payload反序列化` 是**前后两段,不是包含关系**。
1000 条 trace 相对 `rpc_start` 的中位偏移(client 侧):

```
  5.91µs  mesh_socket_read_start   ┐ ★等待对端 = 207.8µs（到 epoll 就绪为止）
213.69µs  mesh_np_epoll_wake       ┘
219.08µs  mesh_first_byte            ← 取数据（poller 派发/readv/调度）到此结束
219.91µs  mesh_hdr_decode_finish
220.41µs  wait_read_start          ┐ payload 反序列化 = 430ns
220.85µs  wait_read_finish         ┘
222.18µs  read_finish
```

**等待在 `mesh_np_epoll_wake` 就结束了**，`→ mesh_first_byte` 那 5.4 µs 是本机
把数据取上来（poller 排队 + readv + LinkBuffer + goroutine 调度），
之后全是解码。

> **这一段区间名 2026-08-10 改过。** 旧名是「★等待对端(纯网络)」且一直括到
> `mesh_first_byte` —— **把上面那 5.4 µs 本地干活也算进了「纯网络」**，名不副实。
> 现在终点收在 `mesh_np_epoll_wake`，与 Envoy 侧「纯等待」的口径一致
> （两边都卡在「本机与内核交接」那一刻）。

三段都被 `read_start → read_finish`(「读取+解码(整段)」)**包含**,
merge 的 detail 用缩进表达这层嵌套。

---

## 二·五、netpoll 内部(读 5 点 + 写 4 点,**两侧都有**)

Kitex 客户端阻塞在 `Peek` 上等响应,这一段占端到端 95% 以上,但它是个黑盒:
里面混着「对端真没回」「回了但 epoll 没轮到」「读完了但 goroutine 没被调度」三件事,
优化方向截然相反却无法区分。

时间戳由 **poller goroutine** 采集,经挂在 context 上的槽位带回 RPC goroutine,
由 Tracer 在 `Finish` 时输出。**不走 `stats.Record`** —— 那记的是调用那一刻的时间,
等 RPC goroutine 被调度起来再记,恰好抹掉了要测的调度延迟。

| 点位 | netpoll 位置 | 含义 |
|---|---|---|
| `mesh_np_epoll_wake` | `poll_default_linux.go` `EpollWait` 返回后 | epoll 唤醒 |
| `mesh_np_dispatch` | `handler()` 里轮到本连接时 | **同批事件里排在前面的连接占用的时间** |
| `mesh_np_readv_start` / `_done` | `ioread()` 前后 | readv 系统调用(单次,不像 Envoy 是循环) |
| `mesh_np_trigger` | `inputAck` 里交接给等待方之前 | 数据已进 LinkBuffer,即将交接 |

**`mesh_np_trigger` 在两侧记在不同分支**,这是 2026-08-10 补服务端时踩的坑。
`inputAck` 的结构是:

```go
needTrigger := true
if length == n { needTrigger = c.onRequest() }   // 服务端在这里就把请求派发出去了
if needTrigger && ... { markTrigger(); c.triggerRead(nil) }   // 客户端走这条
```

服务端装了 `onRequest` 回调,交接发生在 `onRequest()` **内部**,处理成功后它返回
`false`,下面整块跳过 —— 只在下面记的话**服务端的 trigger 恒为 0**,
五点校验必然判为不一致。实测 1000 条只有 1 条通过。现已在两条分支各记一次
(用局部标志防止都命中时把 `rounds` 多加一遍)。

由此得到的关键量:

| 段 | 含义 |
|---|---|
| `mesh_socket_read_start` → `mesh_np_epoll_wake` | **纯等待** |
| `mesh_np_epoll_wake` → `mesh_np_dispatch` | poller 事件循环内排队 |
| `mesh_np_readv_start` → `_done` | socket 收包 |
| **`mesh_np_trigger` → `mesh_first_byte`**(client)<br>**`mesh_np_trigger` → `mesh_netpoll_onread`**(server) | ★ **goroutine 调度延迟**。整套插桩里唯一能量出「数据到了但没被调度起来」的地方。服务端取 `mesh_netpoll_onread` 是因为那才是它用户态的第一个时刻 |

### 写路径(4 个点,2026-08-10 新增)

| 点位 | netpoll 位置 | 含义 |
|---|---|---|
| `mesh_np_flush_start` | `connection.Flush()` 入口 | netpoll 写路径起点 |
| `mesh_np_writev_start` / `_done` | `flush()` 里 `sendmsg` 前后 | **真正的系统调用** |
| `mesh_np_flush_done` | `flush()` 返回前 | 缓冲区回收完成 |

由此得到:

| 段 | 含义 | 实测 avg(client/server) |
|---|---|---|
| `flush_start` → `writev_start` | LinkBuffer 链表整理 + 拼 iovec | 99 ns / 152 ns |
| **`writev_start` → `writev_done`** | ★ **sendmsg 系统调用** | **1.9 µs / 3.2 µs** |
| `writev_done` → `flush_done` | Skip/Release 缓冲区回收 | 57 ns / 92 ns |

**写路径比读路径简单得多**,因为它全程在 RPC goroutine 上同步执行,
既不跨 goroutine(不需要一致性校验),采样状态也早已确定(trace 已绑定),
所以两侧都能精确按 RPC 开关,未采样零开销。

attrs 带出两个标志:`writes` 是 sendmsg 调用次数(>1 表示部分写),
`waited` 表示走了 EPOLLOUT 慢路径。**慢路径没有拆开** —— 后续的写发生在
poller 的 `outputs`/`outputAck` 上。小报文实测 `writes=1`、`waited=false`,
1000/1000 都走快路径。

> **不要拿这三段去和 Envoy 的「写socket(仅入队)」比。** Envoy 的
> `up_encode_done → up_socket_write_done` 括住的是 `ConnectionImpl::write()`,
> 而那只做「`write_buffer_->move(data)` + `activateFileEvents`」,
> **真正的 writev 在 `onWriteReady()` 里由事件循环稍后执行**。
> 拿 Go 的系统调用时间去比 Envoy 的入队时间，就是「Go 写 socket 比 Envoy 贵
> 7–10 倍」这个错误结论的来源。
>
> **要比就比 `mesh_np_writev_*` 与 `up_writev_*` / `dn_writev_*`** ——
> 两边都是真正的系统调用。下游 writev 补齐后，**同机同传输的干净对比有两对**
> （avg，`results/2026-08-10`）：
>
> | 机器 | Go 侧写 UDS | Envoy 侧写 UDS |
> |---|---:|---:|
> | 950 | kitex-client **1.8 µs** | envoy-out 下游 **1.9 µs** |
> | 920B | kitex-server **2.9 µs** | envoy-in 上游 **3.4 µs** |
>
> 两对都落在同一量级，**7–10 倍彻底不存在**。详见 `probe-coverage-audit.md` §二。

### 两侧的开关方式完全不同

|  | 客户端 | 服务端 |
|---|---|---|
| 采样何时可知 | `Tracer.Start` 时就知道(traceparent 是自己生成的) | **读发生时不可知**,traceparent 还在没解析的字节流里 |
| 探针开关粒度 | **每次 RPC**:读之前开、读之后关(`default_client_handler`) | **每条连接常开**(`default_server_handler.OnActive`) |
| 何时取快照 | 阻塞读返回后的 defer | **`OnRead` 入口** —— 那时读早已完成,槽位是完整的一轮 |
| 未采样开销 | 零(压根不开) | poller 每次 epoll 唤醒一次 `time.Now`,每次读几次原子写 |

服务端做不到「按 RPC 开关」的根本原因是**时序**:`OnRead` 被回调时读**已经做完了**,
那一刻 RPC 还不存在,根本没有「读之前」可供开启。所以只能连接级常开、事后取。

因为常开有持续开销,留了开关:`KITEX_PROBE_NETPOLL_SERVER=0` 可关掉服务端这一侧,
让 §8.6 的对照组仍能拿到干净基线。默认开。

### 三个必须知道的限制

**1. 慢路径（部分写）没有拆开。** `flush()` 一次 `sendmsg` 没写完时会注册 EPOLLOUT
并阻塞在 `waitFlush` 上，后续的写发生在 poller 的 `outputs`/`outputAck` 上，
那条路**没有点位**。只由 attrs 里的 `waited=true` 标出来，读数据时按需过滤。
小报文实测 1000/1000 都是 `writes=1`、`waited=false`，走不到这条路。

**2. 快照可能不一致,必须看 `Consistent` 标志。**

初版设计押在「channel 收发构成同步边,普通字段读写即可」上,被 `-race` 直接证伪:
`waitRead` 在数据已入缓冲区时**直接返回,根本不碰 `c.readTrigger`**,快路径上没有任何同步边。

改成全字段原子访问后无竞争,但原子只保证不撕裂、**不保证同一轮**。
故由 netpoll 侧校验五个时刻单调不减,不过则置 `Consistent=false`,消费方须**整条丢弃**。

实测良率:请求—响应模式 **100%**;而 writer 全力灌数据的场景只有 2.8%(reader 几乎总走快路径,
没有唤醒可测)。真实 RPC 属于前者。

**3. `Consistent` 在读、写两侧的处置方式不同 —— 别照抄。**

| | 读探针 | 写探针 |
|---|---|---|
| 跨 goroutine | 是（poller 写、RPC goroutine 读） | 否（全程在 RPC goroutine 上） |
| `Consistent=false` 的含义 | 快照横跨了两轮读，差值无意义 | 只是某一段没走到（如 Flush 提前返回） |
| 消费方该怎么做 | **整条丢弃** | **逐个判零**，能取几段取几段 |

写侧若照读侧那样整体门控，会把「其余几段本来是好的」也一并丢掉。
`emitNetpollWrite` 因此**不能挂在 `emitNetpoll` 末尾** —— 读探针无效时那边会提前
`return`，写路径的数据会被连坐。两者在 `Tracer.Finish` 里是并列调用的。

---

## 三、跨节点的分层分解

四个节点的"本地总时长"构成嵌套关系,由此可逐层剥出各段:

```
client 总
  = client 本地处理 + [client↔envoy-out UDS 往返] + envoy-out 总
envoy-out 总
  = envoy-out 处理  + [跨机网络往返]              + envoy-in 总
envoy-in 总
  = envoy-in 处理   + [envoy-in↔server UDS 往返]  + server 总
```

merge 的 `decompose()` 用**两个式子交替套用**，把上面的嵌套拆成一个可相加的划分：

```
「N 自身」  = N 总   −  N 等          （N 真正占 CPU 的时间）
「A↔B 往返」= A 等   −  B 总          （数据走了一个来回）
```

其中「N 等」= 节点 N 花在等下一跳身上的时间，**两端都卡在「本机与内核交接」那一刻**：

| 节点 | 「N 等」取哪两个点 |
|---|---|
| Envoy 各跳 | `up_writev_done` → `up_epoll_wake` |
| kitex-client / kitex-server | `mesh_socket_read_start` → `mesh_np_epoll_wake` |

从 client 一路推到 server，各段相加**恰好**等于端到端 —— 望远镜求和，
中间每个「N 总」都被加一次减一次，误差恒为 0。

> **起点为什么不取 `up_write_done`（入队）、终点为什么不取 `up_first_byte`：**
> 前者时请求还没离开本机，中间等事件循环那段会被算成网络；后者已经在 readv、
> buffer、filter 派发之后，本机取数据的几微秒也会被算成网络。
> **两头的本地开销算进「往返」，会同时高估链路、低估节点自身。**
> 收紧口径后跨机往返 68.8 µs，与两机 `ping` 实测 RTT 0.068 ms 几乎完全吻合。

于是:

| 段 | 算法 | 跨机? |
|---|---|---|
| client 自身 + UDS 往返① | 由上面两式导出 | 同机(950),精确 |
| envoy-out 自身 + 跨机网络往返 | 同上 | **跨机**,但两项各自在本机测,偏斜抵消 |
| envoy-in 自身 + UDS 往返② | 同上 | 同机(920B),精确 |

**每一项都是"两个各自在本机测得的时长"相减**,因此对时钟偏斜免疫(§8.2.3)。
本环境两机墙钟实测偏差 **15.5 秒左右**(950 未启用 NTP,数值随时间漂移),该方法仍然成立 ——
2026-08-07 用 `--inject-skew kitex-server=+50` 注入额外 50 秒偏移,
`detail` 输出与不注入时**逐字节相同**(`results/2026-08-07/clock-skew-check.txt`)。

### 「往返」这一列有个大小上限（2026-08-11 新发现）

`A↔B 往返 = A 等 − B 总` 隐含一个前提：**B 的观测区间完全嵌套在 A 的等待区间
里面**。小报文时成立；**单条消息大到需要多轮 socket 读写时就不成立了**：

```
envoy-in:  ...──── up_writev（写 64KB 到 UDS，要多轮）────▶│ 才开始等
kitex-server:        │◀── 第一次 epoll 唤醒在这里，早于对端写完
                     └────────── B 总 ──────────▶
```

对端**在发送方还没写完时就已经开始读**，于是 `B 总` 把一段「A 还在写」的时间
也算了进去，`A 等 − B 总` 可能为负。2026-08-11 的 64K 格实测
`UDS往返 envoy-in↔kitex-server` = **−1.00 µs（avg）**，p50 −1.8 µs ——
**平均值为负，不是噪声**。

**影响范围仅限「往返」那几列。** 节点自身、纯等待、各 socket 分段都是
同一节点内两点相减，不涉及跨节点嵌套假设，不受影响。

判据：本轮 1K/4K/8K/16K 全部正常，只有 64K 出问题。**报文接近或超过一次
socket 读写能吞下的量时，就不要引用「往返」那一列。**
详见 `bench-matrix-2026-08-11.md` §三。

---

## 四、可以讨论的增删

**建议保留的核心点**(去掉会直接损失分析能力):

- Envoy ①② + `up_writev_done` + `up_epoll_wake` —— 分别是"到达/可知trace/真正发出/响应就绪",构成分层分解的骨架
- Kitex `rpc_start/finish`、`server_handle_*` —— 差值法的输入与业务开销基线
- 两侧的 `*_writev_*` —— 唯一能做「Go vs C++ 写 socket」这类跨语言对比的点位

**可以考虑去掉的**:

- Envoy ③ `msg_begin`:与 ② 通常只差 0.5–1 µs,信息量低
- Envoy ⑦ `resp_decoded`:与 ⑧ 接近
- Kitex `read_start/read_finish`:与 `wait_read_*` 区间高度重叠

**建议补上的**(按价值排序):

| # | 点位 | 状态 |
|---|---|---|
| 1 | 协议栈内部(网卡 → 内核 → socket) | 未实现,**当前的 P0**。需 SO_TIMESTAMPING / eBPF,评测网卡时是唯一该看的部分,见 `probe-coverage-audit.md` §五 |
| 2 | TCP 发送缓冲区阻塞（记 `write()` 返回值与请求长度之差） | 未实现。成本极低，而它现在会伪装成「编码慢」，属主动误导 |
| 3 | 连接池命中标志(客户端侧) | **部分完成** —— Envoy 侧已由 `up_conn_new`/`up_conn_reused` 覆盖;Kitex 客户端仍只有 `client_conn_start/finish`,看不出这次是新建还是复用 |
| 4 | 中间件链 / filter 链开销 | 未实现。预计 1 µs 量级,demo 里测不出东西,迁真实业务前再做 |
| 5 | netpoll 写慢路径（EPOLLOUT 之后由 poller 续写的那段） | 未实现。小报文走不到，只有大报文/对端慢时才需要 |

> **以下四条已完成,从"建议补上"移出:**
>
> - ~~`NetpollOnReadEnter` —— 唯一能量出"内核收到数据 → Go 被唤醒"这一段的点~~
>   → 2026-08-07 实现。客户端由 §二·五 的 `mesh_np_*` 五点覆盖(并且拆得更细),
>   服务端由 `mesh_netpoll_onread` 覆盖。实测客户端 goroutine 调度延迟
>   跨机 TCP 11.4 µs / 本机 UDS 2.8 µs。
> - ~~TTHeader 编解码的独立区间~~
>   → 2026-08-07 实现,`mesh_hdr_encode_start/finish` 与 `mesh_hdr_decode_start/finish`。
> - ~~**netpoll 写路径(writev 边界)** —— 曾是"唯一的 P0"~~
>   → 2026-08-10 实现，`mesh_np_flush_start` / `mesh_np_writev_start/done` /
>   `mesh_np_flush_done`，**两侧都有**。结论见下。
> - ~~Envoy 的真正 writev~~
>   → 2026-08-10 实现，`up_writev_*` / `dn_writev_*`（`onWriteReady()` 里
>   `doWrite()` 前后）。四个 socket 边界至此全部拆到系统调用。

### 曾经的 P0：写路径黑盒 —— 已拆开，并推翻了一个结论

立这个 P0 时的观察是「各处写 socket 的耗时差得离谱」：

| 写 socket（旧口径） | p50(双跳,c=1) |
|---|---:|
| envoy-out(C++,本机 UDS) | **290 ns** |
| kitex-client(Go,本机 UDS) | 1.8 µs |
| kitex-server(Go,本机 UDS) | 5.7 µs |
| kitex-client(Go,**跨机 TCP**,直连) | **7.8 µs** |

拆开之后才发现，**这张表的第一行与其余行量的不是同一件事**：Envoy 那 290 ns 是
`ConnectionImpl::write()` 的**入队**耗时，Go 那几行是同步 `Flush()` 里**真正的
sendmsg**。两边补齐系统调用点位后，同机同传输的对比是 1.8 vs 1.9 µs（950）、
2.9 vs 3.4 µs（920B）—— **一个量级**。

顺带回答了立项时的问题「Go 的写路径里系统调用与缓冲管理各占多少」：
**系统调用占 86~89 %**，LinkBuffer 管理只占 4~5 %，此前怀疑的
「链表整理 + epoll 注册混在一起」被证伪。

两种模型真正的差别不在系统调用快慢，而在 Envoy 多一段
「入队 → 事件循环轮到它 → 才写出」（envoy-out 下游 7.9 µs、envoy-in 下游 11.7 µs），
**Go 的同步 Flush 没有对应物**。
