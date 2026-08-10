# 打点位置清单

本文逐点说明每个打点的**含义**、**代码位置**、以及**它使哪个区间可测**。
用于讨论增删点位 —— 判断一个点该不该存在,标准是"去掉它之后,哪个区间就算不出来了"。

---

## 一、Envoy 侧(骨架 8 个点 + 细分 11 个,每跳实际发射 20 个点位名)

请求经过一个 Envoy sidecar 的完整路径:

```
 ⓪dn_epoll_wake      下游 epoll 就绪（2026-08-10 新增）
        │  ← 事件循环排队
   dn_readv_start / dn_readv_done   下游 socket 收包
        │  ← buffer 管理 + filter chain 派发
 ①dn_first_byte      下游数据到达
        │  ← TTHeader 解析
 ②hdr_decoded        trace 可知（此刻才知道是否采样）
        │  ← 协议层解消息头
 ③msg_begin          method name 可见
        │  ← 路由匹配
 ④route_resolved     cluster 已选定
        │  ← 编码 + 取连接 + 写出
 ⑤up_write_done      请求已发往上游
        │  ← ★ 等待上游（含下一跳的全部时间）
 ⑥up_first_byte      上游响应首字节
        │  ← 响应解码
 ⑦resp_decoded       响应解析完成
        │  ← 下游响应编码（2026-08-10 新增）
   dn_encode_done
        │  ← 写下游 socket（2026-08-10 新增）
   dn_socket_write_done
        │
 ⑧req_done / rpc_done
```

> **四个 socket 边界**。每跳 Envoy 横跨两条连接（下游 UDS、上游 TCP），
> 各有收发。2026-08-10 之前只有上游那条被插桩，下游整条不可见 ——
> 于是「上一跳的纯等待」里一直混着下一跳 `dn_first_byte` 之前的全部时间。
> 补齐后四个边界对称：`dn_readv_*` / `up_encode_done`+`up_socket_write_done` /
> `up_readv_*` / `dn_encode_done`+`dn_socket_write_done`。

| # | 点位 | 代码位置 | 含义 | 去掉它就算不出什么 |
|---|---|---|---|---|
| ① | `dn_first_byte` | `conn_manager.cc` `onData` 入口 | 下游字节到达 Envoy 的最早时刻 | **"数据到达"到"开始解析"的间隔**;这是唯一早于 TTHeader 解析的点,也是与上一跳衔接的锚点 |
| ② | `hdr_decoded` | `transportBegin`(即 `decodeFrameStart` 之后) | TTHeader 解析完成,trace 与采样标志在此刻才可知 | **TTHeader 解析耗时**(①→②)。这个点还承担回填职责:①记录时 trace 未知 |
| ③ | `msg_begin` | `conn_manager.cc` `messageBegin` | 协议层解出消息头,method name 可见 | **协议层解析耗时**(②→③) |
| ④ | `route_resolved` | `router_impl.cc` `messageBegin`,`route_entry_` 取到后 | 路由匹配完成,cluster 已选定 | **路由匹配耗时**(③→④) |
| ⑤ | `up_write_done` | `router_impl.cc` `messageEnd`,`encodeAndWrite` 之后 | 请求已编码并写往上游 | **编码+取连接+写出的耗时**(④→⑤) |
| ⑥ | `up_first_byte` | `router_impl.cc` `onUpstreamData` | 上游响应首字节到达 | **★ 等待上游的时间**(⑤→⑥)—— 这是本跳之后所有环节的总和,是分层归因的关键 |
| ⑦ | `resp_decoded` | `conn_manager.cc` `ResponseDecoder::finalizeResponse` | 响应解析完成 | **响应解码耗时**(⑥→⑦) |
| ⑧ | `req_done` / `rpc_done` | `finalizeRequest` / `doDeferredRpcDestroy` | 请求侧处理完成 / RPC 真正结束 | **响应回写耗时**(⑦→⑧) |

### 由这些点导出的关键量

| 量 | 算法 | 意义 |
|---|---|---|
| **本跳自身处理** | (①→⑤) + (⑥→⑧) | 去掉等待上游的部分,才是这一跳真正消耗的 CPU 时间 |
| **等待下游全部环节** | ⑤→⑥ | 包含网络往返 + 下一跳的全部处理 |
| 解析开销 | ①→③ | TTHeader + 协议层 |
| 路由开销 | ③→④ | |

**⑥ 是整个设计里最关键的一个点。** 没有它就无法把"本跳处理"与"等待下游"分开,归因会退化成只知道总时长。

### ④→⑤ 的细分(已实现)

原本 `route_resolved → up_write_done` 一段把三件事混在一起(实测 7～16 µs),
现已拆开:

| 点位 | 位置 | 含义 |
|---|---|---|
| `up_conn_new` / `up_conn_reused` | `UpstreamRequest::onPoolReady` | **上游连接是新建/排队得来,还是直接复用**。这让建连成本可以按请求归因,而不只是从 `cluster.*.upstream_cx_total` 看聚合值 |
| `up_encode_done` | `encodeAndWrite` 中 `encodeFrame` 之后 | 帧编码完成 |
| `up_socket_write_done` | `connection().write()` 之后 | 写 socket 完成 |

为此给 `RequestOwner` 加了 `downstreamConnectionId()`,**带默认实现返回 0** ——
`ShadowRouterImpl`(影子流量,不需要打点)因此不受影响。

### ⑤→⑥ 的细分(已实现)

原本 `up_write_done → up_first_byte` 这段「等待上游」把三件事混在一起:真正的等待、
readv 系统调用、事件循环调度。现已拆开:

| 点位 | 位置 | 含义 |
|---|---|---|
| `up_epoll_wake` | `connection_impl.cc` `onFileEvent`,取 **`dispatcher.approximateMonotonicTime()`** | **事件后端返回、尚未派发任何回调的那一瞬**。⑤→这里才是真正的等待 |
| `up_readv_start` / `up_readv_done` | `connection_impl.cc` `transport_socket_->doRead()` 前后 | socket 收包。注意 `doRead` 内部是循环,会反复 readv 直到 EAGAIN,所以是「N 次 readv + N 次 append」的总和 |

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
| ⑤ → `up_epoll_wake` | **纯等待**:对端处理 + 网络在途 + 内核协议栈到 epoll 就绪 | **177.4 µs**(占「等待上游」的 98 %) |
| `up_epoll_wake` → `up_readv_start` | **事件循环内排队** | c=1 时 **130 ns**;c=16 时 p50 420 ns、**p90 15.5 µs、p99 41.5 µs** |
| `up_readv_start` → `up_readv_done` | readv 系统调用 | 3.5 µs |
| `up_readv_done` → ⑥ | buffer 管理 + filter chain 派发 | 150 ns |

**「事件循环内排队」这一刀区分「Envoy 处理慢」与「Envoy 排不过来」。**
读它必须看 p90/p99 —— **排队是尾延迟现象不是中位数现象**,低负载或只看 p50
都会得出「没有排队」的错误结论。数据来源 `results/2026-08-07/two-{lo,hi}-detail.txt`。

**绑定机制**:给 `Network::Connection` 加了带默认实现的
`setKitexProbeDownstreamId` / `kitexProbeDownstreamId`,由 `UpstreamRequest::onPoolReady`
在采样命中时设为下游 conn_id,`releaseConnection` 与 `onResponseComplete` 两条释放路径上都清零。

不用旁路 map 的原因:通用读路径服务全进程所有连接,压测下每个 epoll 事件做一次哈希查找不可接受。
裸成员让未采样路径退化成一次读加一次分支。

**裸标志只挂上游连接**。下游侧的读发生在 `bindTrace` 之前,那时还不知道采样与否,
所以下游走的是另一套机制 —— 见下。

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

### 已知缺口

| 想测但目前测不了 | 为什么还没做 |
|---|---|
| **listener accept** | 属于连接级而非请求级;新建连接的成本已体现在 client 侧的 `client_conn_start/finish` 与上游侧的 `up_conn_new` |
| **filter chain 内各 filter 耗时** | 本实验只挂 thrift_proxy 一个 filter,测不出东西;真实部署挂十几个 filter 时必须补 |

---

## 二、Kitex 侧(12 个点,全部为框架自带)

**这些点零源码改动即可获得** —— 都是 Kitex `pkg/stats` 预定义的事件,只需 `WithStatsLevel(LevelDetailed)` 加一个 Tracer。

| 点位 | Kitex 记录位置 | 含义 | 使哪个区间可测 |
|---|---|---|---|
| `rpc_start` / `rpc_finish` | 顶层 | RPC 起止 | **端到端总时长**,差值法的输入 |
| `client_conn_start` / `client_conn_finish` | `remotecli/conn_wrapper.go:121,139` | 从连接池取连接 | **建连成本**。实测冷启动可达 2.3 ms,是首请求延迟的主因 |
| `write_start` / `write_finish` | `default_client_handler.go:49,52`<br>`default_server_handler.go:67,70` | 编码并写入 socket | **发送耗时**(含编码) |
| `read_start` / `read_finish` | 同上 `:67,70` / `:98,100` | 读取并解码 | **接收耗时** |
| `wait_read_start` / `wait_read_finish` | `codec/thrift/thrift.go:211,220` | 等待 body 剩余字节到齐 | 见下面「两个『等待』的关系」 |
| `server_handle_start` / `server_handle_finish` | `server/server.go:373,368` | 业务 handler 执行 | **业务逻辑耗时**。echo 场景实测仅 **220~240 ns**(三种拓扑下逐项一致),便于把框架开销与业务开销分开 |

### 补充的自定义点(已实现,`kitex/pkg/stats/meshlab_events.go`)

全部经 `DefineNewEvent` 注册,不占预定义槽位,不影响既有消费方。

| 点位 | 位置 | 为什么现有事件不够 |
|---|---|---|
| **`mesh_first_byte`** | `DecodeMeta` 首次 `Peek` 返回后 | **最关键的一个**。`ReadStart` 记录于 `Read()` 入口,那时还没开始读,真正的阻塞等待埋在 `codec.Decode` 内部 —— 于是 `read_start→read_finish` 吞掉了整个网络等待(实测占 client 侧 96%),等于没有分解 |
| `mesh_socket_read_start` | 阻塞 `Peek` 之前 | 与上一个配对,得到**纯粹的等待对端时间** |
| **`mesh_netpoll_onread`** | `default_server_handler.go` `OnRead` 入口 | **epoll 唤醒后用户态的第一个可记录时刻**。与 `mesh_socket_read_start` 一起,把"唤醒→开始读"这一段单独剥出 |
| **`mesh_socket_write_start/finish`** | `bufWriter.Flush()` 前后 | `WriteStart/Finish` 括住"编码+写"整段,**分不清是序列化慢还是 socket 慢**。Flush 才是真正触发 write 系统调用的地方 |
| `mesh_hdr_decode_start/finish` | `ttHeaderCodec.decode` 前后 | TTHeader 解析耗时 |
| `mesh_hdr_encode_start/finish` | Encode 的 header 段 | TTHeader 编码耗时;mesh 场景的核心成本项 |
| `mesh_payload_codec_start/finish` | `encodePayload` 前后 | thrift **序列化**耗时。**只在发送路径上** —— 见下面「payload 只有编码没有解码」 |

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
> `mesh_netpoll_onread` **之前**,而 netpoll 读探针只插在客户端(§2.5:服务端此刻
> 读不到 TTHeader 里的 traceparent,采样状态不可知)。
> **所以服务端的 readv 时间目前根本没测** —— 这是现存缺口,记在
> `probe-coverage-audit.md`。

#### ③ `mesh_payload_codec_*` 只有编码,没有解码

插在 `encodePayload` 前后,**只覆盖发送路径**:

- client 侧实测落在 3.17µs —— 在 `mesh_socket_write_start`(3.59µs) **之前**,是编请求
- server 侧实测落在 11.13µs —— 在 `server_handle_finish`(9.50µs) **之后**,是编响应

已由「payload解码」改名为「payload编码」。解码那一半由上面 ① 覆盖。

> 早期报告里出现「payload解码」「等待请求体」「等待响应体」字样的,
> 按本节重新理解。

### client 侧两段的先后关系

`★等待对端(纯网络)` 与 `payload反序列化` 是**前后两段,不是包含关系**。
1000 条 trace 相对 `rpc_start` 的中位偏移(client 侧):

```
  5.91µs  mesh_socket_read_start   ┐
213.69µs  mesh_np_epoll_wake       │ ★等待对端 = 213µs,大头全在这
219.08µs  mesh_first_byte          ┘
219.91µs  mesh_hdr_decode_finish
220.41µs  wait_read_start          ┐ payload 反序列化 = 430ns
220.85µs  wait_read_finish         ┘
222.18µs  read_finish
```

**等待在 `mesh_first_byte` 就结束了**,后面全是解码。
两段都被 `read_start → read_finish`(「读取+解码(整段)」)**包含**,
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

> **不要拿这三段去和 Envoy 的「写socket」比。** Envoy 的
> `up_encode_done → up_socket_write_done` 括住的是 `ConnectionImpl::write()`,
> 而那只做「`write_buffer_->move(data)` + `activateFileEvents`」——
> **真正的 writev 在 `onWriteReady()` 里,由事件循环稍后执行,目前没有点位**。
> 详见 `probe-coverage-audit.md` §二。

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

### 两个必须知道的限制

**1. 只覆盖客户端。** 探针必须在阻塞读**之前**打开,事后无法补采,所以采样判定只能在
`Tracer.Start` 做。客户端的 traceparent 是自己塞进 ctx 的,此刻可读;服务端的还在
对端发来的 TTHeader 里,此刻读不到。服务端侧的唤醒由 `mesh_netpoll_onread` 覆盖。

**2. 快照可能不一致,必须看 `Consistent` 标志。**

初版设计押在「channel 收发构成同步边,普通字段读写即可」上,被 `-race` 直接证伪:
`waitRead` 在数据已入缓冲区时**直接返回,根本不碰 `c.readTrigger`**,快路径上没有任何同步边。

改成全字段原子访问后无竞争,但原子只保证不撕裂、**不保证同一轮**。
故由 netpoll 侧校验五个时刻单调不减,不过则置 `Consistent=false`,消费方须**整条丢弃**。

实测良率:请求—响应模式 **100%**;而 writer 全力灌数据的场景只有 2.8%(reader 几乎总走快路径,
没有唤醒可测)。真实 RPC 属于前者。

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

于是:

| 段 | 算法 | 跨机? |
|---|---|---|
| client 本地 + UDS 往返① | client总 − envoy-out总 | 同机(950),精确 |
| envoy-out 处理 + 跨机网络往返 | envoy-out总 − envoy-in总 | **跨机**,但两项各自在本机测,偏斜抵消 |
| envoy-in 处理 + UDS 往返② | envoy-in总 − server总 | 同机(920B),精确 |

**每一项都是"两个各自在本机测得的时长"相减**,因此对时钟偏斜免疫(§8.2.3)。
本环境两机墙钟实测偏差 **15.5 秒左右**(950 未启用 NTP,数值随时间漂移),该方法仍然成立 ——
2026-08-07 用 `--inject-skew kitex-server=+50` 注入额外 50 秒偏移,
`detail` 输出与不注入时**逐字节相同**(`results/2026-08-07/clock-skew-check.txt`)。

---

## 四、可以讨论的增删

**建议保留的核心点**(去掉会直接损失分析能力):

- Envoy ①②⑤⑥ —— 分别是"到达/可知trace/发出/收到响应",构成分层分解的骨架
- Kitex `rpc_start/finish`、`server_handle_*` —— 差值法的输入与业务开销基线

**可以考虑去掉的**:

- Envoy ③ `msg_begin`:与 ② 通常只差 0.5–1 µs,信息量低
- Envoy ⑦ `resp_decoded`:与 ⑧ 接近
- Kitex `read_start/read_finish`:与 `wait_read_*` 区间高度重叠

**建议补上的**(按价值排序):

| # | 点位 | 状态 |
|---|---|---|
| 1 | **netpoll 写路径(writev 边界)** | **未实现,当前唯一的 P0**。见下 |
| 2 | 连接池命中标志(客户端侧) | **部分完成** —— Envoy 侧已由 `up_conn_new`/`up_conn_reused` 覆盖;Kitex 客户端仍只有 `client_conn_start/finish`,看不出这次是新建还是复用 |
| 3 | 中间件链开销 | 未实现。预计 1 µs 量级,demo 里测不出东西,迁真实业务前再做 |
| 4 | 协议栈内部(网卡 → 内核 → socket) | 未实现。需 SO_TIMESTAMPING / eBPF,评测网卡时是唯一该看的部分,见 `probe-coverage-audit.md` §5 |

> **以下两条在 2026-08-07 已完成,从"建议补上"移出:**
>
> - ~~`NetpollOnReadEnter` —— 唯一能量出"内核收到数据 → Go 被唤醒"这一段的点~~
>   → 已实现。客户端由 §二·五 的 `mesh_np_*` 五点覆盖(并且拆得更细),
>   服务端由 `mesh_netpoll_onread` 覆盖。实测客户端 goroutine 调度延迟
>   跨机 TCP 11.4 µs / 本机 UDS 2.8 µs。
> - ~~TTHeader 编解码的独立区间~~
>   → 已实现,`mesh_hdr_encode_start/finish` 与 `mesh_hdr_decode_start/finish`。

### 唯一的 P0:netpoll 写路径

读路径已经拆开了,**写路径仍是一个黑盒**。实测各处「写 socket」的耗时:

| 写 socket | p50(双跳,c=1) |
|---|---:|
| envoy-out(C++,本机 UDS) | **290 ns** |
| kitex-client(Go,本机 UDS) | 1.8 µs |
| kitex-server(Go,本机 UDS) | 5.7 µs |
| kitex-client(Go,**跨机 TCP**,直连) | **7.8 µs** |

已能解释的部分:**传输方式**(同为 Go client,跨机 TCP 是本机 UDS 的 4.3 倍)与
**机器差异**(920B 比 950 慢 1.5~2.9 倍,见 test-report §6.1)。

**仍无法回答的**:Go 的写路径里,`writev` 系统调用本身与 netpoll 的缓冲管理各占多少。
这需要在 `ioread()` 的对偶位置插桩。**框架可以直接复用读路径那套**
(`ReadProbe` 的原子槽位 + 单调不减的因果校验 + `Consistent` 标志),估计一天工作量。
