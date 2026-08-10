# Envoy 下游 socket 点位补全 + waterfall 同机对齐 —— 设计方案

- 日期：2026-08-10
- 状态：**设计已确认，待实施**
- 前置：`2026-08-06-kitex-envoy-e2e-trace-design.md`（本文沿用其 §8 的探针约定与术语）
- 目标读者：实施者本人；假定熟悉 Envoy 的 `ConnectionImpl` 读路径与 thrift_proxy 的
  `ConnectionManager`，不假定记得本项目现有点位的分布

---

## 1. 问题

每跳 Envoy 实际横跨**两条连接**：下游（client → envoy，本实验走 UDS）与上游
（envoy → 对端，跨机走 TCP）。两条连接各有收、发，一共**四个 socket 边界**。

现有 15 个点位里，socket 级的只覆盖了上游那一条：

| 边界 | 方向 | 现有点位 | 状态 |
|---|---|---|---|
| 下游读（UDS 收包） | 请求进 | 无 | 🔴 **完全不可见** |
| 上游写（TCP 发包） | 请求出 | `up_encode_done` → `up_socket_write_done` | 🟢 |
| 上游读（TCP 收包） | 响应进 | `up_epoll_wake` / `up_readv_start` / `up_readv_done` | 🟢 |
| 下游写（UDS 发包） | 响应出 | 无 | 🔴 **完全不可见** |

`dn_first_byte` 记在 `conn_manager.cc onData` 入口 —— 那已经在 socket 读、buffer
append、filter chain 派发**之后**，不是 socket 边界。

### 1.1 为什么这不只是「少两行数据」

上一跳的 `⑤→⑥ 等待上游` 被拆成了「纯等待 / 事件循环排队 / readv / 派发」四段，
其中**「纯等待」这一段（envoy-out 实测 174.9 µs，占端到端 79 %）里，
混着下一跳 `dn_first_byte` 之前的全部时间**：对端的 epoll 唤醒、readv、
buffer 管理、filter 派发，统统算在「纯等待」里。

也就是说，归因阶梯到这里就断了 —— **占比最大的那一段至今没有被分解过一次**。
补上下游读点位之后，这段才第一次能往下拆。

这也是 `probe-coverage-audit.md` §2 早就记下的那句「接收侧已补齐，发送侧不对称」
的完整版：不只是发送侧不对称，是整条下游连接都没有 socket 级点位。

---

## 2. 新增点位：每跳 15 → 20

### 2.1 下游读（3 个，走槽位机制）

| 点位 | 位置 | 取值 |
|---|---|---|
| `dn_epoll_wake` | `connection_impl.cc` `onFileEvent` | `dispatcher_.approximateMonotonicTime()` |
| `dn_readv_start` | `connection_impl.cc` `doRead`，`transport_socket_->doRead()` 之前 | `dispatcher_.timeSource()` |
| `dn_readv_done` | 同上，之后 | 同上 |

`dn_epoll_wake` **必须**取 `approximateMonotonicTime()` 而不是「现在」，理由与
`up_epoll_wake` 完全相同（见 `connection_impl.cc` 该处注释）：`onFileEvent` 入口
已经在 libevent 派发到本连接**之后**，取「现在」会让这一段退化成几百纳秒的分支开销，
把真正的事件循环排队全漏掉。

### 2.2 下游写（2 个，直接 `rpcEvent`）

| 点位 | 位置 |
|---|---|
| `dn_encode_done` | `conn_manager.cc` `ResponseDecoder::finalizeResponse`，帧编码完成、`write()` 之前 |
| `dn_socket_write_done` | 同处 `cm.read_callbacks_->connection().write(buffer, false)` 之后 |

此处 trace 已绑定（`KITEX_PROBE_END` 要到 `rpc_done` 才解绑），所以走普通
`rpcEvent` 即可，未采样时是一次哈希查找加一次布尔判断，无需任何新机制。

### 2.3 明确不加

- **`up_socket_write_start`**：`up_encode_done` 紧邻 `write()` 调用，已经充当 start，
  再加一个纯冗余。
- **下游 accept / listener 队列**：属连接级而非请求级，且本实验是长连接，
  每条连接只 accept 一次。理由同 `probe-points.md` 的既有结论。

---

## 3. 槽位机制（下游读的采样难题）

### 3.1 为什么不能照抄上游的做法

上游侧的做法是：`onPoolReady` 时查一次 `isSampled(dn_id)`，把结果作为裸标志
挂到上游连接上（`enableKitexProbe`），此后热路径只判零。

**下游侧查不了** —— 下游读发生时 TTHeader 还没解析，traceparent 还在字节流里，
采样状态在物理上不可知。这正是 `dn_first_byte` 要走「先记后绑」的原因。

### 3.2 为什么不复用 `pending`

`pending` 是 `flat_hash_map<uint64_t, std::vector<Event>>`，`connEvent` 往里
`push_back` 完整的 `Event`（含 point 名、两个时间戳、seq_id）。给 `dn_first_byte`
一个点用没问题，但下游读有 3 个点，**且未采样的请求也要照付** ——
在 1 % 采样、40k QPS 下，99 % 的请求会白白做 3 次可能触发扩容的 `push_back`。

这违反探针的设计约束 1：「未采样请求必须近乎零开销」。

### 3.3 槽位

```cpp
// 每连接固定 3 个时间戳，覆盖式写入，不分配、不增长。
struct ConnSlots {
  int64_t epoll_wake{0};
  int64_t readv_start{0};
  int64_t readv_done{0};
};
// ThreadState 新增成员：
absl::flat_hash_map<uint64_t, ConnSlots> slots;

enum class Slot { DnEpollWake, DnReadvStart, DnReadvDone };
void connSlot(uint64_t conn_id, Slot which, MonotonicTime mono);
```

- **写**：读路径上直接 `st.slots[conn_id].<field> = mono`。未采样代价 =
  一次哈希查找 + 一次 store，零分配。
- **取**：`bindTrace` 时若 `sampled`，把三个非零槽位转成事件，与 `pending`
  一起绑定到 trace；不采样则完全忽略，槽位留着被下一个请求覆盖。
- **清**：`endRpc` 里连同 `bindings` / `pending` 一起 `erase(conn_id)`，
  防止 map 随连接数无界增长。`endRpc` 对采样与否都会调用（`doDeferredRpcDestroy`
  里无条件执行），所以这条清理是可靠的。

与 Kitex 侧 netpoll 探针的「时间戳槽位」是同一个模式，两侧做法一致。

### 3.4 `ConnectionImpl` 要区分自己在哪一侧

点位名现在硬编码成 `up_*`，需要按 side 选 `up_*` / `dn_*`。

**不改 `envoy/network/connection.h`** —— `enableKitexProbe(uint64_t)` 的签名保持
原样，side 由 `ConnectionImpl` 自己判断：

```cpp
// connection_impl.h，ConnectionImpl 的 protected 成员
bool kitex_probe_upstream_{false};        // 默认 = 下游
// ClientConnectionImpl 构造函数里置 true
```

依据是 `ClientConnectionImpl : public ConnectionImpl` —— **上游（主动发起的）
连接天然是派生类，被 accept 的下游连接不是**，这个区分是现成的，不需要
调用方传参。

这么做有两个实打实的好处，值得为它多写一个成员变量：

1. **构建时间**：`connection.h` 是核心接口头，改它实测触发 1134 个动作的大范围
   重编（十几分钟）；只改 `connection_impl.{h,cc}` 是 26 秒的增量。
2. **可摘除性**：`probe.h` 开宗明义说插桩要「保持一行式，便于 rebase 时定位与摘除」。
   少污染一个上游接口，就少一处 rebase 冲突点。

开启位置：

- **上游侧**：`upstream_request.cc onPoolReady`，维持现状 —— 查采样后才开启，
  调用点一个字不用改。
- **下游侧**：`conn_manager.cc initializeReadFilterCallbacks`（该 filter 的
  每条下游连接），**无条件开启**（那时查不了采样，这正是要走槽位的原因）。

连接池复用时上游标志必须清零这条约束不变（见 `onPoolReady` 现有注释）。

> 注意 `ClientConnectionImpl` 覆盖全进程所有主动发起的连接（xDS、健康检查等），
> 不止 thrift 上游。但 side 只在 `kitex_probe_on_` 为真时才被读到，
> 而那个标志只有上面两处会显式打开，所以不会误伤。

---

## 4. 新增可测区间

`merge` 的 `detail` / `table` 格式新增五行：

| 区间 | 含义 |
|---|---|
| `dn_epoll_wake → dn_readv_start` | 下游事件循环排队 |
| `dn_readv_start → dn_readv_done` | 下游 socket 收包（UDS readv） |
| `dn_readv_done → dn_first_byte` | buffer 管理 + filter chain 派发 |
| `resp_decoded → dn_encode_done` | 下游响应编码 |
| `dn_encode_done → dn_socket_write_done` | 下游 socket 写（UDS write） |

**读这五行时的纪律**：与既有的 `net.*` 列一样，它们描述的是**本跳自己**的
时间，可以在同节点内自由相减；但「上一跳的纯等待 − 本跳的这几段」是跨节点相减，
同机才合法，跨机只能得往返（§8.2.4 不变）。

---

## 5. waterfall 改造：同机共用原点

### 5.1 现状

`printWaterfall` 按节点分块，每块的偏移相对**该节点自己**的最早事件。
于是同一台机器上 client 与 envoy-out 的衔接处（UDS 一来一回）看不出来 ——
两个 0 点各说各话。

### 5.2 目标形态

```
┌─ host=suzhou950   原点 = 本机最早事件
│      0ns  kitex-client  rpc_start
│    6.9µs  kitex-client  mesh_socket_write_finish
│    7.4µs  envoy-out     dn_epoll_wake          ◀ UDS 衔接 500ns
│    9.1µs  envoy-out     dn_readv_done
│    9.3µs  envoy-out     dn_first_byte
│      …
│  213.6µs  envoy-out     dn_socket_write_done
│  216.0µs  kitex-client  mesh_np_epoll_wake     ◀ UDS 衔接 2.4µs
└─
```

同一台机器上所有节点的事件**按时刻交错成一条流**，节点名单独一列；
相邻两行跨节点时标注 `◀ UDS 衔接 Xµs`。

### 5.3 原点只能取 wall，但精度仍由 mono 保证

同机两个进程共享 `CLOCK_REALTIME`，所以 wall 在**同机内**可比。
而 mono 不可比：Go 侧的 `monoBase = time.Now()` 是**进程相对**的，
Envoy 侧的 `MonotonicTime` 是**开机相对**的，两者不同基准。

因此摆位公式为：

```
事件位置 = (节点起始wall − 本机原点wall) + (事件mono − 节点起始mono)
           └────── 跨节点摆位，用 wall ──────┘   └─ 节点内偏移，用 mono ─┘
```

节点**内部**相邻点的精度完全不受影响（仍是 mono 相减）；只有节点之间的
相对位置用到 wall。这是同机唯一可用的基准，也是 wall 字段本来的用途
（§8.2「wall 仅供粗排序」的合法延伸）。

### 5.4 跨机不对齐

两台机器之间画分隔，明写「时间轴独立，不可对齐」。**不做**任何形式的跨机
原点估算（例如按「上一跳 `up_write_done` + 半个往返」去摆对端）——
那等于把估算值画成事实，正是 §8.2 要防的操作。跨机结论仍只由差值法给出，
维持现有输出不变。

---

## 6. 边界与错误处理

| 情形 | 处理 |
|---|---|
| 槽位缺失（响应与请求在同一次 readv 里到达，或连接复用后没有新的 epoll wake） | 该点位不输出。`detail` 里体现为 `n=` 变小，**不补零** —— 补零会把「没发生」画成「耗时 0」 |
| 同机跨节点 wall 出现负偏移（两进程 wall 读数抖动） | 钳到 0，行尾标注。单条 trace 上出现属正常，与 `net.*` 列可能为负同理 |
| `--inject-skew` 自检 | 必须仍然通过：注入偏移后**各段时长逐位不变**。waterfall 的摆位会变，这是预期行为，不是回归 |
| 只有一台 host 的数据（同机双跳） | 四个节点全部并进一条时间线，这是该改造收益最大的场景 |

---

## 7. 验证

1. **点位齐全**：e2e 跑 1000 请求、`sample=1.0`，四节点点位数应为
   client 25 / **envoy-out 20** / **envoy-in 20** / server 21，
   且四条 `[probe]` 收尾行均为「记录==落盘 且 丢弃=0」。
2. **开销对照**：`sample=1.0` 下新旧二进制**按轮次交错**各跑 3 轮
   （不能一组跑完再跑另一组 —— 后跑的会白占缓存预热与 CPU 频率爬升的便宜，
   这个坑踩过，当时得出过「加了插桩反而快 4.9 %」的结论）。
   判据：p50 变化落在噪声内。
3. **归因闭合**：上一跳的「纯等待」减去下一跳新拆出的三段，
   剩余应为正且量级合理 —— 这是本次改造的核心收益，必须实测确认。
4. **merge 回归**：`--inject-skew` 自检；同机双跳下 waterfall 应把 4 个节点
   并进一条时间线。

---

## 8. 影响面

| 文件 | 改动 |
|---|---|
| `envoy/source/common/kitex_probe/probe.{h,cc}` | 新增 `ConnSlots` / `Slot` / `connSlot()`；`bindTrace` 取槽位；`endRpc` 清槽位 |
| `envoy/source/common/network/connection_impl.{h,cc}` | 新增 `kitex_probe_upstream_`（`ClientConnectionImpl` 构造置真）；读路径按 side 选 `up_*`/`dn_*`，下游走 `connSlot` |
| `envoy/source/extensions/filters/network/thrift_proxy/conn_manager.cc` | `initializeReadFilterCallbacks` 开启下游探针；`finalizeResponse` 加 2 个写侧点位 |
| `mesh-lab/tools/merge/main.go` | `phases()` 增五行；`printWaterfall` 改为按 host 分块 + 同机交错 |
| `mesh-lab/docs/probe-points.md` / `probe-coverage-audit.md` | 补新点位说明，更新覆盖度评估 |

**`envoy/network/connection.h` 与 `upstream_request.cc` 都不动**（见 §3.4）。
因此 Envoy 侧是 26 秒的增量重编，不是改核心头文件那种十几分钟的大范围重编。
