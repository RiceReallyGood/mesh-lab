# Kitex Echo × Envoy L7 Sidecar 端到端链路打点 —— 代码修改方案设计

- 日期:2026-08-06
- 状态:设计已确认,待实施
- 目标读者:实施者本人;假定读者熟悉 C++/Go,但不假定熟悉 Envoy 扩展机制与 Thrift 线格式

---

## 1. 目标与范围

### 1.1 要做成什么

用 **Kitex 原生 TTHeader + Thrift Binary** 协议,跑通一条 **双跳 sidecar** 的 echo 链路,并在**全链路关键点插入时间戳打点**,最终产出单个请求的 waterfall 时序图,回答这些问题:

- 一次 RPC 的时间到底花在哪一段?
- 双跳 sidecar 相比直连,多付出了多少?这些开销分别属于协议栈、Envoy 自身处理、还是连接池?
- 同机通信走 UDS 相比 loopback TCP,省了多少?

### 1.2 明确不做

- 不做 xDS 动态配置(用静态 bootstrap,减少变量)
- 不做 mTLS(会引入 TLS 握手噪声,干扰时序分析)
- 不做 iptables 透明拦截(用显式代理;差异写在附录 A)
- 不做多机部署(时钟问题见 §8.2;差异写在附录 B)
- 不做 streaming / 多路复用(只做 unary,`transport.TTHeader`)

### 1.3 为什么这条路值得走

Envoy 的 `thrift_proxy` 支持 Apache THeader,但**不支持 Kitex 的 TTHeader**(§3.1 给出逐字段证明)。这意味着现成的 Envoy 无法在 L7 层理解 Kitex 的默认协议 —— 只能降级到 `Framed`(丢掉全部 header KV),或者退化成 TCP 透传(丢掉全部 L7 能力)。

本方案通过给 Envoy 增加一个 TTHeader transport,让 Envoy **原生理解 Kitex 协议**,从而拿到:基于 header 的 L7 路由、per-method 统计、以及在代理内部按请求打点的能力。

---

## 2. 环境与约束(全部实测)

### 2.1 开发机(WSL2)

| 项 | 值 |
|---|---|
| 平台 | WSL2, kernel 6.6.87.2-microsoft-standard, x86_64 |
| CPU / 内存 | 16 核 / 7 GB |
| 工具链 | **无 go、无 docker、无 bazel** |
| sudo | 被 `.claude/settings.json` deny |
| 网络 | GitHub 直连可用(0.82s);另有 Windows 侧代理 `127.0.0.1:10808` 可用 |

**结论:开发机只能做代码编辑与阅读,7 GB 内存编不动 Envoy。** 所有构建与运行在 suzhou950。

### 2.2 构建/运行机(suzhou950)

| 项 | 值 |
|---|---|
| 平台 | openEuler 24.03 SP3, kernel 6.6.0, **aarch64** |
| CPU / 内存 | **384 核 / 1379 GB** |
| 磁盘 | /home 可用 6.0 T;**/tmp 是 690 G tmpfs** |
| 编译器 | gcc 12.3.1, clang 17.0.6, glibc 2.38 |
| 构建依赖 | make/unzip/zip/patch/libtool/autoconf/automake/pkg-config/pip3/java/javac/curl/wget/rsync/tar/xz 齐全;zlib、openssl 头文件在 |
| 观测工具 | strace / ltrace / tcpdump / perf / bpftrace / ss / lsof / nc / jq **全部就位** |
| 内核旋钮 | `perf_event_paranoid=-1`(perf 非 root 全功能,已实测);`kptr_restrict=0`;eBPF 已由用户开启 |
| ulimit | nofile 软 1024 / **硬 524288** → 会话内 `ulimit -n 65536`,无需 sudo |
| 缺失 | **go 未装、bazel 未装** —— 均为用户态解压安装,不需要 sudo |
| 网络 | GitHub 直连 200 / 0.89s;goproxy.cn 可用;releases.bazel.build 可用 |

### 2.3 由环境推导出的三条设计约束

1. **aarch64 不是 Envoy 的 CI 一等公民**(上游 CI 覆盖 Ubuntu x86_64/arm64,不覆盖 openEuler)。首要退路是 `--config=gcc`(`.bazelrc:134-149` 备有整组 gcc 兼容开关,说明上游认真支持 gcc)。此风险在 §11 展开。
2. **bazel output_base 放 `/tmp`**(690 G tmpfs),避免机械盘 IO 成为 384 核的瓶颈。
3. **跨机开发**:代码在 WSL2 编辑 → rsync 到 suzhou950 构建运行。需要一个稳定的同步脚本(§10.1)。

---

## 3. 关键技术判定

本节的每一条都是从源码逐行核对得出的,是后续所有设计决策的依据。

### 3.1 判定一:Kitex TTHeader 与 Apache THeader 不兼容

**Envoy 侧**(`source/extensions/filters/network/thrift_proxy/header_transport_impl.h:39`):
```cpp
static constexpr uint16_t Magic = 0x0FFF;
```

**Kitex 侧**(`gopkg/protocol/ttheader/encode.go:66`):
```go
TTHeaderMagic uint32 = 0x10000000   // 高 16 位即 0x1000
```

魔数不同只是表象。逐字段比对后,**差异是"整套 varint 换定宽整数"**:

| 字段 | 偏移 | Apache THeader(Envoy 现有) | Kitex TTHeader |
|---|---|---|---|
| LENGTH | 0 | uint32 BE | uint32 BE |
| MAGIC | 4 | `0x0FFF` | **`0x1000`** |
| FLAGS | 6 | uint16 BE | uint16 BE |
| SEQ ID | 8 | int32 BE | int32 BE |
| HEADER SIZE | 12 | uint16 BE(值 ×4) | uint16 BE(值 ×4) |
| PROTOCOL ID | 14 | **varint i32** | **uint8** |
| NUM TRANSFORMS | — | **varint i32** | **uint8** |
| TRANSFORM ID | — | varint i32 每个 | **uint8 每个** |
| INFO ID | — | **varint i32**,仅 `1` = KV | **uint8**:`0x00` padding / `0x01` StrKV / `0x10` IntKV / `0x11` ACLToken |
| KV 条数 | — | varint i32 | **uint16 BE** |
| Key 长度 | — | varint i32(限 i16) | **uint16 BE** |
| Value 长度 | — | varint i32(限 i16) | **uint16 BE** |
| IntKV 的 key | — | 不存在此概念 | **uint16 BE** |
| padding | — | `4 - size%4`(总是补) | `(4 - size%4) % 4`(整除时不补) |

**PROTOCOL ID 取值也不同:**

| 值 | Apache THeader | Kitex TTHeader |
|---|---|---|
| 0 | Binary | ThriftBinary |
| 1 | JSON | — |
| 2 | Compact | ThriftCompact(Kitex 不支持) |
| 3 | — | ThriftCompactV2(Kitex 不支持) |
| 4 | — | **KitexProtobuf** |
| 0x10 / 0x11 | — | TTHeader Streaming 专用 |

> **注意 padding 那一行。** Apache 实现"总是补"(`header_transport_impl.cc:253` `const int padding = 4 - (header_size % 4);` —— 当 `header_size % 4 == 0` 时补 4 字节),Kitex 是 `(4 - writeSize%4) % 4` —— 整除时补 0 字节。这个差异若在 encode 侧抄错,会导致帧长计算偏移 4 字节,表现为"偶发解析失败"(只在 header 长度恰为 4 的倍数时触发),极难排查。

**结论:必须新写一个 transport,不能改造现有的。**

### 3.2 判定二:Envoy 的 Transport 是可注册扩展点

`transport.h:85-105`:
```cpp
class NamedTransportConfigFactory : public Envoy::Config::UntypedFactory {
  virtual TransportPtr createTransport() PURE;
  std::string category() const override { return "envoy.thrift_proxy.transports"; }
  static NamedTransportConfigFactory& getFactory(TransportType type) { ... }
};
template <class TransportImpl> class TransportFactoryBase : ... // :111
```

新增 transport 的标准路径是:实现 `Transport` 接口(3 个虚函数:`decodeFrameStart` / `decodeFrameEnd` / `encodeFrame`)+ `REGISTER_FACTORY`。参照 `header_transport_impl.cc:346-354`。

**这意味着协议扩展是"加文件"而非"改文件"**,对上游 rebase 极友好。

### 3.3 判定三:trace 上下文可以天然贯穿

这条链路已逐段验证:

```
业务代码 metainfo.WithValue(ctx, k, v)
  → transmeta/metainfo.go WriteMeta:  sendMsg.TransInfo().PutTransStrInfo(kvs)
  → TTHeader InfoIDKeyValue (0x01) 段
  → 【新】Envoy TTHeaderTransportImpl::decodeFrameStart
  → metadata.requestHeaders()            (Http::RequestHeaderMap, metadata.h:124)
  → RouteMatch.headers 的 HeaderMatcher  (route.proto:101)
  → 【新】encodeFrame 写回 TTHeader
  → 下一跳 / Kitex server
```

**推论:一旦 transport 写好,基于 trace header 的 L7 路由是白送的**,不需要额外写 filter。这也是"真 L7 而非 TCP 透传"最直接的证据(验证阶梯第 3 级,§10.2)。

### 3.4 判定四:Kitex 已有细粒度事件框架,不必重造

`pkg/stats/event.go:89-105` 已预定义(`LevelDetailed`):

| 事件 | 实际记录位置 |
|---|---|
| `RPCStart` / `RPCFinish` | client/server 顶层 |
| `ClientConnStart` / `ClientConnFinish` | `remote/remotecli/conn_wrapper.go:121,139` |
| `WriteStart` / `WriteFinish` | `remote/trans/default_client_handler.go:49,52`;`default_server_handler.go:67,70` |
| `ReadStart` / `ReadFinish` | `default_client_handler.go:67,70`;`default_server_handler.go:98,100` |
| `WaitReadStart` / `WaitReadFinish` | `remote/codec/thrift/thrift.go:211,220` |
| `ServerHandleStart` / `ServerHandleFinish` | `server/server.go:373,368` |
| `ChecksumGenerate/Validate Start/Finish` | `remote/codec/validate.go:66-93` |

接口极简(`pkg/stats/tracer.go`):
```go
type Tracer interface {
    Start(ctx context.Context) context.Context
    Finish(ctx context.Context)
}
```
`Finish` 时从 `rpcinfo.GetRPCInfo(ctx).Stats()` 取全部事件(`pkg/rpcinfo/interface.go:41-51` 的 `RPCStats.GetEvent`)。

并且 `event.go:136` 提供 `DefineNewEvent(name, level)`,允许在 init 阶段注册自定义事件,不占用预定义槽位。

**推论:Kitex 侧 11 个点零改动即可获得,只需补 4 个自定义事件(§6.2)。**

### 3.5 判定五:Kitex 早已为 mesh 代理预留了打点头

`pkg/remote/transmeta/metakey.go`:

```go
// IntKV keys（uint16，iota 顺序）
MeshVersion, TransportType, LogID, FromService, FromCluster, FromIDC,
ToService, ToCluster, ToIDC, ToMethod, Env, DestAddress, RPCTimeout,
ReadTimeout, RingHashKey, DDPTag, WithMeshHeader, ConnectTimeout,
SpanContext, ShortConnection, FromMethod, StressTag, MsgType,
HTTPContentType, RawRingHashKey, LBType

// StrKV：给 mesh proxy 回填时间戳用的约定 key
HeaderTransPerfTConnStart = "pcs"
HeaderTransPerfTConnEnd   = "pce"
HeaderTransPerfTSendStart = "pss"
HeaderTransPerfTRecvStart = "prs"
HeaderTransPerfTRecvEnd   = "pre"
```

**推论两条:**
1. 路由用的字段(`ToService` / `ToMethod` / `ToCluster`)是 IntKV,Envoy 必须能解 IntKV 才能做 L7 路由 —— 这坐实了 §5.2 静态映射表的必要性。
2. `pcs/pce/pss/prs/pre` 这套 perf key 让我们的打点走 Kitex 原生语义,而不是外挂一套私有约定。

### 3.6 判定六:UDS 三段皆可,但语义不同

| 端 | 证据 | 结论 |
|---|---|---|
| Kitex client | `client/option.go:152-153`:`WithHostPorts` 会 `net.ResolveUnixAddr` 探测,成功则建 `discovery.NewInstance("unix", hp, ...)` | 直接传 sock 路径即可 |
| Kitex server | `remote/trans/netpoll/trans_server.go:69`:`if addr.Network() == "unix"` 有专门分支(会 unlink 残留 sock) | 支持 |
| Envoy | `api/envoy/config/core/v3/address.proto:23` `message Pipe`;`:197` `Address` oneof | listener 与 cluster endpoint 均支持 |

**采用的切法**(见 §4.1):第 1、3 段 UDS,第 2 段 TCP。理由:第 2 段代表跨主机网络,换 UDS 会丢掉 `ss -ti` 的 RTT/cwnd/重传、`tcpdump` 包级证据,以及 Envoy 的 TCP/TLS 相关 access log formatter(如 commit `24c0256a6f` 新增的 downstream handshake 起止时间点与 RTT)。

---

## 4. 总体架构

### 4.1 拓扑

```
┌──────────────┐   UDS    ┌──────────────┐  TCP loopback  ┌──────────────┐   UDS    ┌──────────────┐
│ kitex-client │ ───────▶ │  envoy-out   │ ─────────────▶ │   envoy-in   │ ───────▶ │ kitex-server │
│   (进程 A)   │ /run/    │  (进程 B)    │  127.0.0.1:    │  (进程 C)    │ /run/    │  (进程 D)    │
│              │ out.sock │              │     15006      │              │ app.sock │              │
└──────────────┘          └──────────────┘                └──────────────┘          └──────────────┘
     ▲                          ▲                                ▲                        ▲
  本机 sidecar 通信        ── 模拟跨主机网络 ──              本机 sidecar 通信
  (绕开 TCP/IP 栈)         (保留 TCP 可观测性)               (绕开 TCP/IP 栈)
```

**全程 TTHeader + Thrift Binary,不降级。**

三段地址全部做成配置项,可一键切回全 TCP —— 这本身构成一组 A/B 实验(同机 UDS vs loopback TCP 的开销差),是报告的一个数据点。

**两个 Envoy 是独立进程,不是一个进程开两个 listener。** 原因:只有独立进程才能把每一跳的连接池、worker 线程、内存占用单独量出来;合并进程会让双跳开销无法拆分。

### 4.2 路由设计(L7 证据)

| 节点 | 路由依据 | Envoy 配置形态 |
|---|---|---|
| envoy-out | TTHeader IntKV `ToService`(id=6) | `headers: [{name: x-tt-to-service, string_match: {exact: echo-server}}]` |
| envoy-in | Thrift `method_name` | `match: {method_name: "Echo"}` |

两者分别验证 **IntKV 解析**与**协议层字段解析**都正确落到了路由决策上。不使用 `ORIGINAL_DST`,不使用 TCP 透传。

### 4.3 客户端如何进入 outbound sidecar

采用**显式代理**:`client.WithHostPorts("/tmp/kitex-demo/out.sock")`,真实目标服务名写入 TTHeader `ToService`。

不采用 iptables 透明拦截,原因:需要 root、WSL2 上 netfilter 行为不稳定、且它不影响任何打点结论(只影响流量如何被捕获)。生产形态差异见附录 A。

---

## 5. Envoy 侧代码修改

分**协议支持**与**打点探针**两块,刻意解耦 —— 前者是可上游化的通用能力,后者是本项目专用的侵入式改动。

### 5.1 新增 TTHeader transport

#### 5.1.1 文件清单

| 文件 | 动作 | 说明 |
|---|---|---|
| `source/extensions/filters/network/thrift_proxy/ttheader_transport_impl.h` | **新建** | 类声明 + `Magic = 0x1000` + 静态表声明 |
| `source/extensions/filters/network/thrift_proxy/ttheader_transport_impl.cc` | **新建** | 三个虚函数实现 + `REGISTER_FACTORY` |
| `source/extensions/filters/network/thrift_proxy/thrift.h` | 改 | `TransportType` 加 `TTHeader`;`LastTransportType` 顺延;`TransportNameValues` 加 `TTHEADER = "ttheader"` 及 `fromType` 分支;`ProtoUtils::getTransportType` 加分支 |
| `source/extensions/filters/network/thrift_proxy/auto_transport_impl.cc` | 改 | 在 `:34` 的 Header 探测**之前**插入 TTHeader 魔数探测 |
| `api/envoy/extensions/filters/network/thrift_proxy/v3/thrift_proxy.proto` | 改 | `TransportType` 枚举加 `TTHEADER = 4` |
| `source/extensions/filters/network/thrift_proxy/BUILD` | 改 | 新 target,并加入 `config.cc` 的依赖 |

`extensions_build_config.bzl` **无需改动** —— transport 随 `envoy.filters.network.thrift_proxy`(:275)一并注册。

#### 5.1.2 `decodeFrameStart` 实现要点

以 `header_transport_impl.cc:48-180` 为骨架,替换字段读取方式:

```
1. 长度检查：buffer.length() < 14 → return false（等更多数据）
2. frame_size = peekBEInt<int32_t>(0)；校验范围 [MinFrameStartSize, 0x3FFFFFFF]
3. magic = peekBEInt<uint16_t>(4)；!= 0x1000 → throw EnvoyException
4. flags = peekBEInt<uint16_t>(6)     → metadata.setHeaderFlags()
5. seq_id = peekBEInt<int32_t>(8)     → metadata.setSequenceId()
6. header_size = peekBEInt<uint16_t>(12) * 4；校验 [2, MaxHeaderSize]
7. buffer.length() < header_size + 14 → return false
8. buffer.drain(14)
9. metadata.setFrameSize(frame_size - header_size - 10)
10. protocol_id = drainUint8()   → 0→Binary, 4→KitexProtobuf, 其余 throw
11. num_transforms = drainUint8()；非 0 → setAppException(MissingResult)
12. 循环读 info block（uint8 info_id）：
      0x00 → padding，continue
      0x01 → StrKV：uint16 count，然后 count 组 (u16 len + bytes) × 2
      0x10 → IntKV：uint16 count，然后 count 组 (u16 key + u16 len + bytes)
      0x11 → ACLToken：u16 len + bytes
      其余 → throw
13. 剩余字节 drain（padding）
```

`decodeFrameEnd` 与 `encodeFrame` 按 §3.1 的差异表反向实现。**`encodeFrame` 的 padding 必须用 `(4 - size % 4) % 4`,不能照抄 Apache 的 `4 - size % 4`。**

### 5.2 零分配的 header 映射(性能关键)

#### 5.2.1 设计

IntKV 在线上是 `(uint16 key, string value)`。Envoy 内部需要一个字符串 header 名才能被 `HeaderMatcher` 匹配。采用**编译期静态常量表**,照搬 Envoy 自己的惯用法(`source/common/http/headers.h:53,392`):

```cpp
// ttheader_transport_impl.h
class TTHeaderIntKeyNameValues {
public:
  const Http::LowerCaseString MeshVersion{"x-tt-mesh-version"};
  const Http::LowerCaseString TransportType{"x-tt-transport-type"};
  const Http::LowerCaseString LogID{"x-tt-log-id"};
  const Http::LowerCaseString FromService{"x-tt-from-service"};
  // ... 共 26 项，与 kitex/pkg/remote/transmeta/metakey.go 的 iota 顺序一一对应
  const Http::LowerCaseString LBType{"x-tt-lb-type"};

  // 按 id 取名；越界返回 nullptr，调用方走 fallback
  const Http::LowerCaseString* fromId(uint16_t id) const;
};
using TTHeaderIntKeyNames = ConstSingleton<TTHeaderIntKeyNameValues>;
```

解码热路径:
```cpp
if (const auto* name = TTHeaderIntKeyNames::get().fromId(key_id); name != nullptr) {
    headers.addReferenceKey(*name, value);     // key 按引用，零拷贝零分配
} else {
    // 冷路径：未知 id
    headers.addCopy(Http::LowerCaseString(absl::StrCat("x-ttheader-int-", key_id)), value);
}
```

#### 5.2.2 为什么这比"数字透传"快

这是本设计中最反直觉的一点,展开说明。

**语义化静态表(采用)**,每条 IntKV 每请求:
1. `fromId(id)` —— 数组下标 + 边界检查
2. `addReferenceKey(*name, value)`(`envoy/http/header_map.h:376`)—— **key 按 `const LowerCaseString&` 传引用,不拷贝**;只拷贝 value

→ **key 侧 0 次分配、0 次转换。** 静态表在进程 init 时构造一次。

**数字透传(不采用)**,每条 IntKV 每请求:
1. `absl::StrCat("x-ttheader-int-", id)` —— 整数转字符串 + `std::string` 构造。`"x-ttheader-int-6"` 是 16 字节,**超过 libstdc++ SSO 的 15 字节阈值,必然堆分配**
2. `Http::LowerCaseString(那个串)` —— `header_map.h:71-74`,再拷一次 + 逐字符 `absl::ascii_tolower`
3. `addCopy(key, value)` —— key 第三次拷贝进 header map

→ **每条 ~3 次堆分配 + 1 次 itoa + 1 次 tolower 遍历。**

典型 Kitex 请求携带 5–8 条 IntKV,数字方案每请求多出 **15–24 次堆分配**。语义化方案不仅可读性更好,而且**严格更快**。

代价:二进制中多一张几 KB 的静态表,以及与 `metakey.go` 同步的维护成本(该文件的 iota 列表多年未变动)。未知 id 走 fallback,是冷路径。

#### 5.2.3 StrKV 侧的同类优化

StrKV 的 key 来自线上,是任意字符串,不能全部静态化。但可以:

1. **热点 key 静态化**:`traceparent`、`pcs`/`pce`/`pss`/`prs`/`pre` 建小表,命中走 `addReferenceKey`。
2. **消除无条件的 `StrReplaceAll`**。现有实现(`header_transport_impl.cc:160-161`)对每个 key 无条件构造新串:
   ```cpp
   key_string = absl::StrReplaceAll(key_string, {{std::string(1,'\0'), ""}, {"\n",""}, {"\r",""}});
   ```
   改为先检测:
   ```cpp
   if (absl::string_view(key_string).find_first_of(absl::string_view("\0\n\r", 3)) != absl::string_view::npos) {
       key_string = absl::StrReplaceAll(key_string, {...});   // 冷路径
   }
   ```
   `find_first_of` 无分配、分支高度可预测,绝大多数请求走零分配路径。

### 5.3 打点探针

#### 5.3.1 为什么不用 ThriftFilter

`thrift_proxy/filters/` 目录下只有 `router` / `header_to_metadata` / `payload_to_metadata` / `ratelimit`,且 thrift_proxy **没有 Lua/Wasm 挂载点**(对比:HTTP 侧有 `envoy.filters.http.lua`、`envoy.filters.http.wasm`)。

即便写一个新 ThriftFilter,它只能观察到 `transportBegin` / `messageBegin` / `messageEnd` / `transportEnd` 四个事件 —— **看不到 listener accept、连接池 ready、upstream write、响应首字节**这些真正需要量的点。

**结论:走源码插桩。**

#### 5.3.2 探针库设计

新建自包含的探针库,把对 Envoy 源码的侵入压到"每处一行":

```
source/common/kitex_probe/
    probe.h      // KITEX_PROBE(ctx, point, ...) 宏 + 编译期开关
    probe.cc     // thread_local ring buffer → NDJSON，无锁
    BUILD
```

设计要点:
- **thread_local ring buffer**:Envoy 是 per-worker 单线程事件循环,thread_local 天然无竞争,不引入任何锁。
- **编译期总开关**:`--define kitex_probe=disabled` 时宏展开为空,零残留,便于做"打点开销"对照实验。
- **时钟**:复用 `ActiveRpc` 已有的 `time_source_`(`conn_manager.h:401`),不引入第二个时钟源。
- **落盘**:worker 线程定期(或 buffer 满时)批量 flush,避免在请求路径上做 IO。

#### 5.3.3 插桩点(每处 1 行)

| # | 位置 | 打点名 | 为什么需要 |
|---|---|---|---|
| E1 | `conn_manager.cc:27` `onData` 入口 | `dn_first_byte` | 下游数据到达 Envoy 的最早时刻 |
| E2 | `ttheader_transport_impl.cc` `decodeFrameStart` 末尾 | `hdr_decoded` | **trace_id 在此刻才可知**(见 §8.3) |
| E3 | `conn_manager.cc:853` `messageBegin` | `msg_begin` | method name 可见 |
| E4 | `router_impl.cc:281` `Router::messageBegin` | `route_resolved` | 路由匹配完成,cluster 已选定 |
| E5 | `router/upstream_request.cc` `onPoolReady` | `pool_ready` | 携带"是否新建连接"属性 —— 连接池命中与否是双跳开销的主要变量 |
| E6 | `router/upstream_request.cc` 写出后 | `up_write_done` | |
| E7 | `router_impl.cc:502` `onUpstreamData` | `up_first_byte` | 上游响应首字节 |
| E8 | `conn_manager.cc:267` `ResponseDecoder::finalizeResponse` | `resp_decoded` | |
| E9 | `conn_manager.cc:748` `finalizeRequest` | `rpc_done` | |

`ActiveRpc` 本就持有 `stream_info_`(`conn_manager.h:357`),探针数据挂在其上,随 RPC 生命周期自然回收。

---

## 6. Kitex 侧代码修改

原则:**优先使用 Kitex 自带机制,只在确实缺口处改源码。**

### 6.1 零改动获得的 11 个点

`client.WithStatsLevel(stats.LevelDetailed)` + 注册自定义 `stats.Tracer`,即可拿到 §3.4 表中全部预定义事件。**不改任何 Kitex 源码。**

### 6.2 需要新增的 4 组自定义事件

用 `stats.DefineNewEvent`(`event.go:136`)在 init 阶段注册,不占预定义槽位:

| 事件 | 插入位置 | 为什么现有事件不够 |
|---|---|---|
| `TTHeaderEncodeStart/Finish` | `pkg/remote/codec/header_codec.go` | 现有只有 `WriteStart/Finish`(整个写路径),看不到 header 编码本身的成本 —— 而这正是 mesh 场景要评估的 |
| `PayloadCodecStart/Finish` | `pkg/remote/codec/default_codec.go` | 把 thrift 序列化从 Write 中剥离 |
| `NetpollOnReadEnter` | `pkg/remote/trans/netpoll/trans_server.go:170` 之前 | **epoll 唤醒的真实时刻**。现有 `ReadStart` 在 handler 内部记录,已经晚了;只有这个点能量出"数据到达内核 → Go 侧被唤醒"的延迟 |
| `MWChainEnter/Exit` | client/server 中间件链首尾 | 量中间件自身开销 |

### 6.3 trace 上下文注入

新增一个 `remote.MetaHandler`,**不改现有代码**,注册即可:

```go
type traceMetaHandler struct{}

func (h *traceMetaHandler) WriteMeta(ctx context.Context, msg remote.Message) (context.Context, error) {
    // 1. 注入 W3C traceparent
    // 2. 回填 Kitex 原生 perf key：pcs/pce/pss/prs/pre
    msg.TransInfo().PutTransStrInfo(map[string]string{...})
    return ctx, nil
}
func (h *traceMetaHandler) ReadMeta(ctx context.Context, msg remote.Message) (context.Context, error) { ... }
```

走的是 §3.3 已验证的链路。使用 Kitex 原生的 perf key 而非私有约定。

### 6.4 Echo 用例

新建独立 module,**不污染两个上游仓库**:

```
demo/
  go.mod                    // replace 指向本地 kitex 源码树
  idl/echo.thrift           // Echo(Request) → Response，含可变长 payload 字段
  server/main.go            // 监听 UDS；注册 Tracer + MetaHandler
  client/main.go            // 拨 UDS 到 envoy-out；可配并发/QPS/包体大小
  probe/tracer.go           // stats.Tracer 实现 → NDJSON
  probe/events.go           // DefineNewEvent 注册
```

`kitex-benchmark/thrift/` 下已有可复用的 IDL 与 client/server 骨架。

---

## 7. 全链路打点清单

单次请求,四进程,去程三段 + 回程三段。按因果顺序编号:

### 去程

| # | 节点 | 打点 | 来源 |
|---|---|---|---|
| 1 | kitex-client | `RPCStart` | 预定义 |
| 2 | kitex-client | `MWChainEnter` | **新增** |
| 3 | kitex-client | `ClientConnStart` / `ClientConnFinish` | 预定义(连接池) |
| 4 | kitex-client | `TTHeaderEncodeStart` / `Finish` | **新增** |
| 5 | kitex-client | `PayloadCodecStart` / `Finish` | **新增** |
| 6 | kitex-client | `WriteStart` / `WriteFinish` | 预定义 |
| 7 | envoy-out | `dn_first_byte` (E1) | **新增** |
| 8 | envoy-out | `hdr_decoded` (E2) | **新增** |
| 9 | envoy-out | `msg_begin` (E3) | **新增** |
| 10 | envoy-out | `route_resolved` (E4) | **新增** |
| 11 | envoy-out | `pool_ready` (E5) | **新增** |
| 12 | envoy-out | `up_write_done` (E6) | **新增** |
| 13 | envoy-in | `dn_first_byte` … `up_write_done` | 同 7–12 |
| 14 | kitex-server | `NetpollOnReadEnter` | **新增** |
| 15 | kitex-server | `ReadStart` / `WaitReadStart` / `WaitReadFinish` / `ReadFinish` | 预定义 |
| 16 | kitex-server | `MWChainEnter` | **新增** |
| 17 | kitex-server | `ServerHandleStart` | 预定义 |

### 回程

| # | 节点 | 打点 |
|---|---|---|
| 18 | kitex-server | `ServerHandleFinish` |
| 19 | kitex-server | `WriteStart` / `WriteFinish` |
| 20 | envoy-in | `up_first_byte` (E7) / `resp_decoded` (E8) / `rpc_done` (E9) |
| 21 | envoy-out | 同 20 |
| 22 | kitex-client | `ReadStart` / `ReadFinish` |
| 23 | kitex-client | `MWChainExit` / `RPCFinish` |

**合计 42 个时间戳**(去程 29 + 回程 13),分布在 4 个进程、6 次跨进程传输上。

其中 6 次传输的内核路径并不相同:
- 第 1、3 段(UDS)× 双向 = **4 次走 unix socket**,不经过 TCP/IP 协议栈
- 第 2 段(TCP loopback)× 双向 = **2 次走完整 TCP/IP 栈**

这个不对称正是 §4.1 选择混合切法的目的 —— 让"协议栈开销"只出现在代表跨主机网络的那一段上,从而与"sidecar 自身处理开销"分离。

### 交叉验证手段

打点数据不能自证正确,用外部工具交叉校验(suzhou950 上全部可用):

| 校验对象 | 工具 | 校验什么 |
|---|---|---|
| `WriteFinish` → `dn_first_byte` | `strace -T -e trace=write,sendto,epoll_wait` | 打点间隔是否与 syscall 耗时吻合 |
| `NetpollOnReadEnter` 的准确性 | `bpftrace` uprobe + `tracepoint:syscalls:sys_exit_epoll_wait` | Go 侧唤醒时刻 vs 内核 epoll 返回时刻 |
| 第 2 段(TCP)传输延迟 | `tcpdump -i lo --time-stamp-precision=nano` | 包级时序 vs 打点时序 |
| Envoy 内部 CPU 分布 | `perf record -g`(非 root 可用) | 热点是否落在预期函数 |

---

## 8. 数据模型、时钟与汇聚

### 8.1 统一事件格式

四进程写同一种 NDJSON,一行一点:

```json
{"ts":1754438400123456789,"trace":"4bf92f3577b34da6a3ce929d0e0e4736","span":"00f067aa0ba902b7","node":"envoy-out","point":"pool_ready","attrs":{"new_conn":true,"cluster":"echo-in"}}
```

- `ts`:**纳秒 epoch**
- `node` ∈ `{kitex-client, envoy-out, envoy-in, kitex-server}`
- `trace`:W3C trace-id(32 hex)

### 8.2 时钟(最容易出错的地方)

四进程在**同一台机器**,因此:

**必须用 `CLOCK_REALTIME`** —— Go 的 `time.Now()`、Envoy 的 `TimeSource::systemTime()`。vDSO 读取,同一硬件时钟源,**跨进程可直接比较,无需任何对齐**。

**绝不能用 `CLOCK_MONOTONIC` 做跨进程比较** —— 它的零点是每进程、每次启动各不相同的,跨进程相减毫无意义。按直觉选 monotonic 是此类工作最常见的错误。

**两种时钟各司其职**:
- 跨进程时序 → `CLOCK_REALTIME`
- 单点内部时长(如"编码耗时") → `CLOCK_MONOTONIC`,避免 NTP 阶跃污染

**多机场景下本方案失效** —— 详见附录 B。

### 8.3 trace_id 的"先占位后回填"

**问题**:Envoy 收到第一个字节时,TTHeader 尚未解析,trace_id 未知。但"首字节到达时刻"(E1)恰恰是必须量的点。

**方案**:
1. E1 时以 `(connection_id, 帧序号)` 作为临时 key 暂存事件
2. E2(`decodeFrameStart` 完成)解出 StrKV 中的 `traceparent`
3. 将该临时 key 下的事件**回填**到真实 trace_id
4. flush 时统一归并

不做这一步,`dn_first_byte` 将永远无法与其余点关联。

### 8.4 汇聚与呈现

离线 Go 工具 `cmd/merge`:读四份 NDJSON → 按 trace 分组 → 按 ts 排序 → 输出三种形态:

1. **终端 waterfall**(ASCII,每段标注 Δµs)
2. **Chrome Trace Event JSON** —— 可直接拖入 `chrome://tracing` 或 Perfetto,四进程显示为四条泳道
3. **汇总统计** —— 各段 p50/p99,回答"双跳 sidecar 的开销分布"

---

## 9. 必踩的坑

按"若不处理会发生什么"排序,越靠前越隐蔽。

### 9.1 metainfo 大小写(静默数据丢失)

metainfo 前缀是**大写**(`bytedance/gopkg` `cloud/metainfo/kv.go:23-27`):
```
RPC_PERSIST_ / RPC_TRANSIT_ / RPC_TRANSIT_UPSTREAM_ / RPC_BACKWARD_ / RPC_BACKWARD_DOWNSTREAM_
```

而 `Http::LowerCaseString` 构造时**强制 `lower()`**(`header_map.h:73`)。请求经过 Envoy 后 `RPC_PERSIST_foo` 变为 `rpc_persist_foo`;Kitex 按大写前缀做匹配,**metainfo 会静默消失 —— 不报错、不告警、不掉包**。

**处理**:配置开启 `header_keys_preserve_case`,并在新 transport 中接上 formatter 的 `processKey()` / `format()`(Envoy 已有此机制,参见 `header_transport_impl.cc:136-137,156-158,238`)。

### 9.2 padding 计算差异(偶发解析失败)

见 §3.1 末尾的注。Apache 是 `4 - size%4`(整除时补 4 字节),Kitex 是 `(4 - size%4) % 4`(整除时补 0)。抄错只在 header 长度恰为 4 的倍数时触发,表现为低频偶发失败。

**处理**:round-trip 单测必须覆盖 header 长度为 4 倍数的用例。

### 9.3 UDS 残留 socket 文件

Kitex server 端有 unlink 处理(`trans_server.go:69` 分支),Envoy 侧需确认。异常退出后残留的 sock 文件会导致下次 bind 失败。

**处理**:启动脚本先清理 `/tmp/kitex-demo/*.sock`。

### 9.4 UDS 路径长度上限 108 字节

`sockaddr_un.sun_path` 在 Linux 上固定为 **108 字节**(含结尾 `\0`)。超长路径会在 `bind()`/`connect()` 时失败,且报错信息往往指向别处,不易联想到长度。

本方案的 `/tmp/kitex-demo/out.sock` 只有 25 字节,安全。但若把 sock 放进带会话 ID 的临时目录(例如 CI 的工作目录)就极易超限。

**处理**:sock 一律放 `/tmp/kitex-demo/` 下的短路径,不使用带长随机后缀的目录。

### 9.5 `ulimit -n` 默认 1024

对 bazel 构建和 Envoy 高并发运行都偏低。

**处理**:所有脚本开头 `ulimit -n 65536`(硬限 524288,无需 sudo)。

---

## 10. 构建与验证

### 10.0 实施阶段划分

本方案体量较大(新 Envoy transport + 探针库 + Kitex 改动 + demo + 汇聚工具)。按下表切成 6 个阶段,**每个阶段都有独立的完成判据**,避免长时间处于"写了很多但什么都没验证"的状态。

| 阶段 | 内容 | 完成判据 | 对应验证级 |
|---|---|---|---|
| **0** | 装 bazelisk + Go;**验证 Envoy 在 openEuler aarch64 上编得过** | `envoy-static` 产出且 `--version` 正常 | — |
| **1** | TTHeader transport(§5.1 + §5.2) | round-trip 单测全绿 | 第 1 级 |
| **2** | 静态 bootstrap + 单跳打通 | echo 通 | 第 2 级 |
| **3** | 路由配置(IntKV header + method_name) | 改 header 能改变 cluster 选择;metainfo 往返完整 | 第 3 级 |
| **4** | 探针库(§5.3)+ Kitex 打点(§6)+ 双跳 | 42 个点齐全,能 merge 出单请求 waterfall | 第 4 级 |
| **5** | merge 工具 + A/B 实验(UDS vs TCP、打点开销) | 交付物 §10.3 齐备 | — |

**阶段 0 必须最先做且不可跳过** —— 它验证的是 §11 中唯一可能推翻整体方案的风险。若阶段 0 失败,后续所有 C++ 工作都无处落地,应立即转入退路(§11 第一行)。

### 10.1 构建路径(全部在 suzhou950,无需 sudo)

| 步骤 | 要点 | 预估 |
|---|---|---|
| 装 bazelisk | 解压到 `~/bin`;自动拉取 `.bazelversion` 指定的 **bazel 8.7.0**(aarch64) | 分钟级 |
| 装 Go | linux-arm64 tarball → `~/sdk/go`;`GOPROXY=https://goproxy.cn` | 分钟级 |
| 拉源码 | 从 GitHub 直连 clone 最新 envoy / kitex | 分钟级 |
| 编 Envoy | `bazel build --config=clang --output_base=/tmp/eob //source/exe:envoy-static` | **首次 30–60 min**,增量分钟级 |
| 编 demo | `go build`,arm64 原生 | 秒级 |
| 同步 | WSL2 编辑 → `rsync` 到 suzhou950 | 秒级 |

### 10.2 验证阶梯

四级,每级可独立证伪,避免"全连上了但不知道哪坏了":

**第 1 级 · 协议正确性(不需要跑 Envoy)**

C++ 单测:用 Kitex 真实产生的 TTHeader 字节流(从 demo 抓取并固化为测试 fixture)喂给 `decodeFrameStart`,断言 StrKV / IntKV / protocol_id / seq_id 全部解析正确;再 `encodeFrame` 回去,断言**与原始字节逐字节相等**(round-trip)。

必须覆盖:header 长度为 4 倍数(§9.2)、含 ACLToken、含未知 IntKV id(fallback 路径)、空 KV。

**第 2 级 · 单跳连通**

`client → envoy-out → server`,echo 通。此级验证 transport 在真实数据流下可用。

**第 3 级 · L7 能力证据**

配置两条路由:一条基于 `x-tt-to-service`(IntKV 来源),一条基于 `method_name`(协议层来源)。验证**修改 header 能改变 cluster 选择**。

这是"真 L7 而非 TCP 透传"的硬证据 —— TCP 透传做不到这件事。

**第 4 级 · 双跳全链路打点**

四进程齐发,merge 出 waterfall;用 §7 的交叉验证手段校验关键 Δ。

### 10.3 交付物

1. 本设计报告
2. 可复现的构建 / 运行脚本
3. 一次真实请求的 waterfall(终端 + Perfetto 两种形态)
4. UDS vs loopback TCP 的 A/B 数据
5. 打点开销自身的量化(`--define kitex_probe=disabled` 对照组)

---

## 11. 风险与退路

| 风险 | 概率 | 影响 | 退路 |
|---|---|---|---|
| **openEuler aarch64 编不过 Envoy** | 中 | 高(阻塞全部) | ① `--config=gcc`(`.bazelrc:134-149` 备有整组兼容开关);② 用官方 Envoy 构建容器(docker 权限用户已开通);③ 最坏情况用预编译 envoy 二进制验证 §10.2 第 2–3 级,只有插桩部分受阻 |
| bazel 拉依赖慢/失败 | 中 | 中 | GitHub 直连实测 0.89s;失败时用 `--distdir` 预下载(依赖清单在 `bazel/repository_locations.bzl`) |
| TTHeader 解析边界情况遗漏 | 中 | 中 | 第 1 级单测用真实字节流 fixture,不用手工构造 |
| 打点本身扰动时序 | 低 | 中 | thread_local 无锁 + 批量 flush;并用编译期开关做对照实验量化 |
| metainfo 大小写问题漏配 | 中 | **高(静默)** | 第 3 级验证中显式断言 metainfo 往返完整 |

**关于第一条**:这是唯一可能推翻整体方案的风险,因此在实施第一步就验证,不拖到后期。

---

## 附录 A:与生产形态的差异(显式代理 vs 透明拦截)

本方案用显式代理(client 直接拨 sidecar 的 UDS)。生产环境的 Istio 类方案用 iptables `REDIRECT` + `ORIGINAL_DST` cluster 做透明拦截。

差异仅在于**流量如何进入 sidecar**:
- 显式代理:应用知道 sidecar 存在,目标服务名走 TTHeader `ToService`
- 透明拦截:应用无感知,目标地址由内核 `SO_ORIGINAL_DST` 还原

**对本方案的全部打点结论无影响** —— 进入 sidecar 之后的路径完全一致。透明拦截额外引入的是 netfilter 的 conntrack 开销与一次 `getsockopt(SO_ORIGINAL_DST)`,可作为后续扩展实验。

## 附录 B:多机部署时的时钟问题

§8.2 的方案依赖"四进程同机、共享硬件时钟"。一旦拆到多台机器:

- NTP 的典型精度是毫秒级,而本方案要测的段延迟是微秒级 —— **NTP 完全不够用**
- PTP(IEEE 1588)可到微秒/亚微秒,但需要硬件时间戳支持的网卡与交换机
- 退而求其次:只信任**同进程内的差值**与**跨进程的因果序**(happens-before),不信任跨机的绝对时间差

实用做法:在每跳的 TTHeader 中回填该跳的本地时间戳(即 Kitex 原生 `pcs/pce/pss/prs/pre` 的设计意图),用"每跳自报的内部耗时"拼装链路,而非用绝对时间相减。

## 附录 C:关键源码位置索引

**Envoy**

| 内容 | 位置 |
|---|---|
| THeader 魔数 | `source/extensions/filters/network/thrift_proxy/header_transport_impl.h:39` |
| THeader 解码实现(骨架参考) | `header_transport_impl.cc:48-180` |
| THeader 编码实现 | `header_transport_impl.cc:189-284` |
| Transport 接口 / 注册工厂 | `transport.h:25,85,111` |
| TransportType 枚举 / 名称表 | `thrift.h:15,28,42,111` |
| transport 自动探测 | `auto_transport_impl.cc:34-47` |
| ConnectionManager 请求路径 | `conn_manager.cc:27,65,267,719,748,853` |
| ActiveRpc 的 StreamInfo / TimeSource | `conn_manager.h:357,401` |
| Router 上游路径 | `router/router_impl.cc:266,271,281,502,510` |
| MessageMetadata header map | `metadata.h:120-134,266-267` |
| HeaderMap add API | `envoy/http/header_map.h:348,362,376,388,400` |
| LowerCaseString(含强制 lower) | `envoy/http/header_map.h:53,71-74` |
| 静态 header 名单例惯用法 | `source/common/http/headers.h:53,392` |
| thrift 路由匹配能力 | `api/.../thrift_proxy/v3/route.proto:65,74,79,101` |
| thrift_proxy 配置(access_log 等) | `api/.../thrift_proxy/v3/thrift_proxy.proto:103,112,119` |
| UDS(Pipe)地址 | `api/envoy/config/core/v3/address.proto:23,197` |
| 默认构建包含的扩展 | `source/extensions/extensions_build_config.bzl:275,326-329` |
| gcc 兼容开关 | `.bazelrc:134-149` |

**Kitex**

| 内容 | 位置 |
|---|---|
| 预定义事件 / 自定义事件注册 | `pkg/stats/event.go:89-105,136` |
| Tracer 接口 | `pkg/stats/tracer.go` |
| RPCStats 接口 | `pkg/rpcinfo/interface.go:41-51` |
| 事件记录入口 | `pkg/rpcinfo/stats_util.go:29` |
| 连接池事件 | `pkg/remote/remotecli/conn_wrapper.go:121,139` |
| 客户端读写事件 | `pkg/remote/trans/default_client_handler.go:49,52,67,70` |
| 服务端读写事件 | `pkg/remote/trans/default_server_handler.go:67,70,98,100` |
| WaitRead 事件 | `pkg/remote/codec/thrift/thrift.go:211,220` |
| ServerHandle 事件 | `server/server.go:368,373` |
| checksum 事件 | `pkg/remote/codec/validate.go:66-93` |
| netpoll OnRead(插桩点) | `pkg/remote/trans/netpoll/trans_server.go:170` |
| netpoll UDS 支持 | `pkg/remote/trans/netpoll/trans_server.go:69` |
| client UDS 支持 | `client/option.go:152-153` |
| TTHeader IntKV / perf key 定义 | `pkg/remote/transmeta/metakey.go` |
| metainfo → TTHeader StrKV | `pkg/transmeta/metainfo.go` |
| 传输协议常量 | `transport/keys.go` |

**gopkg**

| 内容 | 位置 |
|---|---|
| TTHeader 魔数与常量 | `protocol/ttheader/encode.go:66` |
| TTHeader 编码(含 writeKVInfo) | `protocol/ttheader/encode.go` |
| TTHeader 解码 | `protocol/ttheader/decode.go` |
| metainfo 前缀常量 | `cloud/metainfo/kv.go:23-27` |
