# 打点位置清单

本文逐点说明每个打点的**含义**、**代码位置**、以及**它使哪个区间可测**。
用于讨论增删点位 —— 判断一个点该不该存在,标准是"去掉它之后,哪个区间就算不出来了"。

---

## 一、Envoy 侧(每跳 8 个点)

请求经过一个 Envoy sidecar 的完整路径:

```
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
        │
 ⑧req_done / rpc_done
```

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
| `up_epoll_wake` | `connection_impl.cc` `onFileEvent` 入口(仅读就绪) | **epoll 唤醒后用户态第一个可记录时刻**。⑤→这里才是真正的等待 |
| `up_readv_start` / `up_readv_done` | `connection_impl.cc` `transport_socket_->doRead()` 前后 | socket 收包。注意 `doRead` 内部是循环,会反复 readv 直到 EAGAIN,所以是「N 次 readv + N 次 append」的总和 |

由此得到:

| 段 | 含义 | 价值 |
|---|---|---|
| ⑤ → `up_epoll_wake` | **纯等待**:对端处理 + 网络在途 + 内核协议栈到 epoll 就绪 | 156 µs 的绝大部分 |
| `up_epoll_wake` → `up_readv_start` | **事件循环内排队** | ★ 低负载≈0,压测下随排队深度增长。**这一刀区分「Envoy 处理慢」与「Envoy 排不过来」** |
| `up_readv_start` → `up_readv_done` | readv 系统调用 | |
| `up_readv_done` → ⑥ | buffer 管理 + filter chain 派发 | |

**绑定机制**:给 `Network::Connection` 加了带默认实现的
`setKitexProbeDownstreamId` / `kitexProbeDownstreamId`,由 `UpstreamRequest::onPoolReady`
在采样命中时设为下游 conn_id,`releaseConnection` 与 `onResponseComplete` 两条释放路径上都清零。

不用旁路 map 的原因:通用读路径服务全进程所有连接,压测下每个 epoll 事件做一次哈希查找不可接受。
裸成员让未采样路径退化成一次读加一次分支。

**只挂上游连接**。下游侧做不到——下游的读发生在 `bindTrace` 之前,那时还不知道采样与否。

### 已知缺口

| 想测但目前测不了 | 为什么还没做 |
|---|---|
| **listener accept** | 属于连接级而非请求级;新建连接的成本已体现在 client 侧的 `client_conn_start/finish` 与上游侧的 `up_conn_new` |
| **下游侧 epoll/readv** | 见上,采样状态在下游读发生时尚不可知 |

---

## 二、Kitex 侧(12 个点,全部为框架自带)

**这些点零源码改动即可获得** —— 都是 Kitex `pkg/stats` 预定义的事件,只需 `WithStatsLevel(LevelDetailed)` 加一个 Tracer。

| 点位 | Kitex 记录位置 | 含义 | 使哪个区间可测 |
|---|---|---|---|
| `rpc_start` / `rpc_finish` | 顶层 | RPC 起止 | **端到端总时长**,差值法的输入 |
| `client_conn_start` / `client_conn_finish` | `remotecli/conn_wrapper.go:121,139` | 从连接池取连接 | **建连成本**。实测冷启动可达 2.3 ms,是首请求延迟的主因 |
| `write_start` / `write_finish` | `default_client_handler.go:49,52`<br>`default_server_handler.go:67,70` | 编码并写入 socket | **发送耗时**(含编码) |
| `read_start` / `read_finish` | 同上 `:67,70` / `:98,100` | 读取并解码 | **接收耗时** |
| `wait_read_start` / `wait_read_finish` | `codec/thrift/thrift.go:211,220` | 等待 payload 到齐 | **★ 实际等待对端的时间**;`read_start→wait_read_start` 是读 header,`wait_read` 区间才是等 body |
| `server_handle_start` / `server_handle_finish` | `server/server.go:373,368` | 业务 handler 执行 | **业务逻辑耗时**。echo 场景实测仅 1 µs,便于把框架开销与业务开销分开 |

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
| `mesh_payload_codec_start/finish` | `encodePayload` 前后 | thrift 序列化耗时 |

---

## 二·五、netpoll 内部(5 个点,仅客户端侧)

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
| `mesh_np_trigger` | `inputAck` 里 `triggerRead` 之前 | 数据已进 LinkBuffer,即将唤醒等待方 |

由此得到的关键量:

| 段 | 含义 |
|---|---|
| `mesh_socket_read_start` → `mesh_np_epoll_wake` | **纯等待** |
| `mesh_np_epoll_wake` → `mesh_np_dispatch` | poller 事件循环内排队 |
| `mesh_np_readv_start` → `_done` | socket 收包 |
| **`mesh_np_trigger` → `mesh_first_byte`** | ★ **goroutine 调度延迟**。整套插桩里唯一能量出「数据到了但没被调度起来」的地方 |

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

**每一项都是"两个各自在本机测得的时长"相减**,因此对时钟偏斜免疫(§8.2.3)。本环境两机实测偏差 16.34 秒,该方法仍然成立。

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

1. **连接池命中标志** —— 建连是首请求延迟的主因(实测 2.3 ms),但目前只能从聚合 stats 看,不能按请求归因
2. **`NetpollOnReadEnter`** —— 唯一能量出"内核收到数据 → Go 被唤醒"这一段的点
3. TTHeader 编解码的独立区间 —— mesh 场景下这是核心成本项之一
