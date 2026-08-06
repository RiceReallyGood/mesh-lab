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

### 已知缺口

| 想测但目前测不了 | 需要什么 | 为什么还没做 |
|---|---|---|
| **连接池命中/新建** | `UpstreamRequest::onPoolReady`,需携带"是否新建连接" | `RequestOwner` 接口不暴露下游连接 id,插桩需要额外接口改动,比"一行插桩"侵入得多。当前可从 `cluster.*.upstream_cx_total` 聚合看到,只是不能按请求归因 |
| **listener accept** | `Network::ConnectionImpl` 层 | 与 thrift_proxy 无关,属于连接级而非请求级;新建连接的成本已体现在 client 侧的 `client_conn_start/finish` |

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

### 已补充的自定义点(设计文档 §6.2,尚未实现)

| 点位 | 为什么现有事件不够 |
|---|---|
| `TTHeaderEncodeStart/Finish` | 现有只有 `WriteStart/Finish`(整个写路径),看不到 header 编码本身的成本 —— 而这正是 mesh 场景要评估的 |
| `PayloadCodecStart/Finish` | 把 thrift 序列化从 Write 中剥离 |
| **`NetpollOnReadEnter`** | **epoll 唤醒的真实时刻**。现有 `ReadStart` 在 handler 内部记录,已经晚了;只有这个点能量出"数据到达内核 → Go 侧被唤醒"的延迟 |
| `MWChainEnter/Exit` | 量中间件自身开销 |

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
