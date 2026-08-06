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
- 不做 streaming / 多路复用(只做 unary,`transport.TTHeader`)
- **不支持 KitexProtobuf 载荷**(`ProtocolID=0x04`)—— 显式拒绝而非静默降级,理由见 §3.7

### 1.3 部署形态:真实多机

本方案**按多机部署设计并实测**。两台机器:

| 角色 | 机器 | 承载进程 |
|---|---|---|
| 主调侧 | suzhou950(`192.168.25.145`) | kitex-client + envoy-out |
| 被调侧 | suzhou920B(`192.168.25.51`) | envoy-in + kitex-server |

这不是"单机模拟多机",是真的跨主机。由此带来的**时钟问题是本方案最需要严肃对待的部分**,见 §8.2。

### 1.4 为什么这条路值得走

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
| 已装 | **bazel 8.7.0**(华为云镜像,与 `.bazelversion` 一致,自检通过);**Go 1.26.5 linux-arm64**(aliyun 镜像)。均安装在 `~/`,未使用 sudo |
| 源码 | `~/envoy_kitex/envoy`(244 M,`--depth 1` clone);`~/envoy_kitex/kitex`(8.7 M) |
| sudo | **需要密码,不可用** |

### 2.3 被调侧机器(suzhou920B)

| 项 | 值 |
|---|---|
| 地址 | `192.168.25.51` —— **与 suzhou950 同网段** |
| 平台 | openEuler, **aarch64** |
| CPU / 内存 | 160 核 / 502 GB |
| /home 可用 | 112 G(已用 85%,需留意) |

**两台同架构(aarch64)是好事** —— 排除了跨架构带来的编译差异与内存序差异,让测出的时序差异只归因于网络与代理本身。

### 2.4 带宽地形(实测,决定了整个工作流)

各链路速度相差**两个数量级**,这直接决定了"什么东西该从哪里下载":

| 链路 | 实测速度 |
|---|---|
| suzhou950 → `mirrors.huaweicloud.com` | **11.5 MB/s** |
| WSL2 → 互联网 | 5.9 MB/s |
| suzhou950 → `codeload.github.com`(bazel 拉依赖的实际域名) | 1.5 MB/s |
| **WSL2 → suzhou950** | **~150 KB/s** ← 最慢的一环 |
| suzhou950 → `releases.bazel.build` | 75 KB/s |
| suzhou950 → `raw.githubusercontent.com` | 超时不可用 |

**由此确立的工作流原则:大数据绝不经开发机中转。**

- 源码 → 在目标机上直接 `git clone`(244 M 走 codeload,几分钟)
- 工具二进制 → 优先国内镜像(bazel 走华为云,62.8 MB 仅 5.5 秒)
- 开发机只传 **KB 级的代码 diff**,慢链路无所谓
- 机器间传大文件(编译产物)→ 走 950 ↔ 920B 的同网段直连,不经开发机

### 2.5 由环境推导出的设计约束

1. **aarch64 不是 Envoy 的 CI 一等公民**(上游 CI 覆盖 Ubuntu x86_64/arm64,不覆盖 openEuler)。此风险在 §11 展开。
2. **编译器选择**(此处修正了早期的一个错误判断):

   - **bazel 会自行下载 LLVM 工具链**(`external/llvm_toolchain`)并用它编译,**不依赖系统 gcc/clang**。编译错误信息里出现 `external/llvm_toolchain/bin/cc_wrapper.sh --target=aarch64-unknown-linux-gnu` 即为证据。因此"系统缺 `libstdc++.a` / 缺 libc++"这类担心是不成立的。
   - **`--config=clang` 是存在的**,定义在 `.bazelrc:124` 的 `common:clang`(前缀是 `common:` 而非 `build:`,故 `grep '^build:clang'` 查不到 —— 早期据此误判为"无 clang config")。它连带 `clang-common` 与 `libc++`。
   - **`--config=gcc` 确实不可用**:它连带 `--config=libstdc++`,要求静态库 `libstdc++.a`(`.bazelrc:165`),而该机只有 `libstdc++.so.6`。
   - **当前采用:默认工具链 + 单条警告豁免**(见 §11.5),而非整套 `--config=clang`。理由是后者会改变 `host_platform` 与 `-stdlib`,使已完成的上万个编译动作缓存全部失效。
3. **bazel output_base 放 `/tmp`**(690 G tmpfs),避免磁盘 IO 成为 384 核的瓶颈。注意 tmpfs 占用的是内存,Envoy opt 构建产物约数十 GB,相对 1379 G 内存安全。
4. **到 suzhou950 的连接不稳定**(实测多次 `Timeout, server not responding`)。**所有长任务必须 `nohup setsid` 在远端后台运行 + 轮询标记文件**,不可在前台 ssh 里等待;所有传输必须可续传。

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

**采用的切法**(见 §4.1):机内两段 UDS,跨机一段 TCP。多机部署下这个切分是天然的 —— UDS 本来就只能在同一台机器上用。

### 3.7 判定七:载荷协议的支持边界

TTHeader 的 `PROTOCOL ID` 字段(偏移 14,uint8)声明了载荷用什么编码。我们的 transport 读它并调 `metadata.setProtocol()`,Envoy 据此挑选对应的 `Protocol` 实现来解载荷。**transport 本身一个载荷字节都不碰,所以分层上是解耦的。**

但端到端能否工作,取决于 Envoy 是否有对应的 `Protocol` 实现。Envoy 只有四个:`binary` / `compact` / `twitter` / `auto`。对照 Kitex 的 ProtocolID:

| Kitex ProtocolID | Envoy 对应 | 本方案处理 |
|---|---|---|
| `0x00` ThriftBinary | `ProtocolType::Binary` | ✅ 放行(主路径) |
| `0x02` ThriftCompact | `ProtocolType::Compact` | ✅ 映射放行(Envoy 有实现;Kitex 自身标注不支持,实际不会出现,但映射保留以求正确) |
| `0x03` ThriftCompactV2 | 无精确对应 | ❌ 显式拒绝 |
| `0x04` **KitexProtobuf** | **无** | ❌ **显式拒绝** |
| `0x10`/`0x11` TTHeader Streaming | 无 | ❌ 显式拒绝(streaming 在 §1.2 已排除) |

**"显式拒绝"是刻意的设计决策,不是偷懒。** 若对 `0x04` 放任不管,Envoy 会拿 thrift binary 的解析器去啃 protobuf 字节流,解出的 method name、字段类型全是垃圾,表现为随机的路由错误或崩溃 —— 排查成本极高。抛一个 `EnvoyException("ttheader: unsupported protocol id 0x04 (KitexProtobuf)")`,一眼就知道发生了什么。

**结论:本方案对载荷协议"在 transport 层无关,在支持列表内有关"。** 说"用户用什么序列化都无所谓"是不准确的;准确表述是"支持 Thrift Binary(及 Compact),其余显式拒绝"。

---

## 4. 总体架构

### 4.1 拓扑

```
        机器 A: suzhou950 (192.168.25.145)          机器 B: suzhou920B (192.168.25.51)
   ┌──────────────────────────────────────┐   ┌──────────────────────────────────────┐
   │                                      │   │                                      │
   │  ┌────────────┐  UDS  ┌───────────┐  │   │  ┌───────────┐  UDS  ┌────────────┐  │
   │  │kitex-client│──────▶│ envoy-out │──┼───┼─▶│ envoy-in  │──────▶│kitex-server│  │
   │  │  (进程 A)  │ out.  │  (进程 B) │  │   │  │ (进程 C)  │ app.  │  (进程 D)  │  │
   │  └────────────┘ sock  └───────────┘  │   │  └───────────┘ sock  └────────────┘  │
   │                                      │   │       ▲                              │
   └──────────────────────────────────────┘   └───────┼──────────────────────────────┘
                    │                                 │
                    └────── 真实跨主机 TCP ───────────┘
                            :15006  同网段
```

**全程 TTHeader + Thrift Binary,不降级。**

**这个切分在多机下是天然的**:UDS 本来就只能同机使用,所以"机内走 UDS、跨机走 TCP"不是人为设计,而是部署形态自己决定的。好处正如 §3.6 所说 —— 协议栈开销只出现在真正的网络段上,与 sidecar 自身处理开销天然分离。

**机内那两段仍做成可切 TCP 的配置项** —— 切回 loopback TCP 就得到一组 A/B(同机 UDS vs loopback TCP 的开销差),是报告的一个数据点。

**两个 Envoy 天然是独立进程**(本来就在不同机器上),每一跳的连接池、worker 线程、内存占用都能单独量。

### 4.1.1 单机降级模式(调试用)

多机链路出问题时不好定位,因此保留一个**四进程全在 suzhou950** 的降级模式,中间段走 `127.0.0.1:15006`。用途仅限于:
- 排查"是代码问题还是网络问题"
- 阶段 2/3 的功能性验证(见 §10.2)

**性能数据一律以多机模式为准**,单机模式的数字不进报告结论。

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

> **这 42 个点只在被采样的请求上记录。** 全量记录在压测下不成立(100k QPS 下约 840 MB/s),且会让打点本身主导测量。采样机制见 §8.4;唯一的例外是 E1,它早于采样判断,需无条件写一个预分配槽位(§8.4.2)。

其中 6 次传输的内核路径并不相同:
- 第 1、3 段(UDS)× 双向 = **4 次走 unix socket**,不经过 TCP/IP 协议栈
- 第 2 段(TCP loopback)× 双向 = **2 次走完整 TCP/IP 栈**

这个不对称正是 §4.1 选择混合切法的目的 —— 让"协议栈开销"只出现在代表跨主机网络的那一段上,从而与"sidecar 自身处理开销"分离。

### 交叉验证手段

打点数据不能自证正确,用外部工具交叉校验(两台机上工具均已就位):

| 校验对象 | 工具 | 校验什么 | 跨机可用? |
|---|---|---|---|
| `WriteFinish` → `dn_first_byte`(机内 UDS 段) | `strace -T -e trace=write,sendto,epoll_wait` | 打点间隔是否与 syscall 耗时吻合 | 同机,✅ |
| `NetpollOnReadEnter` 的准确性 | `bpftrace` uprobe + `tracepoint:syscalls:sys_exit_epoll_wait` | Go 侧唤醒时刻 vs 内核 epoll 返回时刻 | 同机,✅ |
| 跨机 TCP 段 | **两端同时** `tcpdump -i <NIC> --time-stamp-precision=nano` | 包级时序;**注意两端抓包时间戳同样受 16.34 s 偏斜影响**,只能各自与本机打点比对 | ⚠️ 见下 |
| Envoy 内部 CPU 分布 | `perf record -g`(`perf_event_paranoid=-1`,非 root 可用) | 热点是否落在预期函数 | 同机,✅ |
| 跨机往返总时长 | §8.2.3 差值法 | 与 `ping` 的 RTT(实测 0.057 ms)量级是否自洽 | ✅ |

**跨机 tcpdump 的正确用法**:两端抓到的包时间戳分属两台机器的时钟,**不可直接相减**(同 §8.2)。正确做法是各端把"本机 tcpdump 时间戳"与"本机打点时间戳"比对,验证**本机内**的打点准确性;跨机的部分仍由差值法给出。

**基线参考**:两机 `ping` 实测 RTT `min/avg/max/mdev = 0.047/0.057/0.081/0.013 ms`,0% 丢包。差值法算出的跨机往返应当 ≥ 这个量级;若小于它,说明分析有误。这是一条廉价但有效的自洽性检查。

---

## 8. 数据模型、时钟与汇聚

### 8.1 统一事件格式

四进程写同一种 NDJSON,一行一点:

```json
{"host":"suzhou950","node":"envoy-out","trace":"4bf92f3577b34da6a3ce929d0e0e4736",
 "span":"00f067aa0ba902b7","parent":"0000000000000001","point":"pool_ready",
 "wall_ns":1754438400123456789,"mono_ns":98234512345,
 "attrs":{"new_conn":true,"cluster":"echo-in"}}
```

字段语义(**`wall_ns` 与 `mono_ns` 的分工是本设计的关键**):

| 字段 | 含义 | 允许怎么用 |
|---|---|---|
| `host` | 物理机标识 | 判断两个点是否同机 —— **决定能否直接相减** |
| `node` | 逻辑角色 ∈ `{kitex-client, envoy-out, envoy-in, kitex-server}` | 泳道归属 |
| `trace` / `span` / `parent` | W3C trace-id(32 hex)+ span 树 | 因果结构 |
| `wall_ns` | 该机 `CLOCK_REALTIME` 纳秒 | **仅粗排序与人眼可读;跨 host 严禁相减** |
| `mono_ns` | 该机 `CLOCK_MONOTONIC` 纳秒 | **同 host 内精确相减;跨 host 严禁相减** |

merge 工具必须**强制检查 `host` 字段**:任何跨 host 的时间戳相减都应当在工具层面被拒绝,而不是靠人自觉。这是把 §8.2 的纪律固化进代码。

### 8.2 时钟 —— 多机部署下最容易出错的地方

这是整个方案技术上最需要小心的部分。**跨机器的绝对时间戳不可直接相减。**

#### 8.2.0 实测:本环境两台机的时钟差 16.34 秒

不是假设,是量出来的。用夹逼法(`t1 = 本机时间;tb = 远端时间;t2 = 本机时间`,取 `tb − (t1+t2)/2`)测 5 次:

| 次数 | 920B 相对 950 的偏移 | 测量不确定度 |
|---|---|---|
| 1 | **−16331 ms** | ±57 ms |
| 2 | −16340 ms | ±48 ms |
| 3 | −16345 ms | ±44 ms |
| 4 | −16339 ms | ±49 ms |
| 5 | −16341 ms | ±47 ms |

五次结果离散度仅 14 ms,远小于单次测量不确定度 —— **这是一个稳定的 16.34 秒固定偏移,不是抖动**。

成因已查明:

| | suzhou950 | suzhou920B |
|---|---|---|
| `timedatectl NTPSynchronized` | **no** | yes |
| chrony 状态 | 无 | 偏离 NTP 200 µs |

**这个数字的意义:**

1. 若按单机思路把两台机的绝对时间戳排在同一条轴上,**每条 trace 都会显示 server 在 client 发出请求的 16 秒之前返回了响应** —— 结果是纯粹的垃圾。
2. 但 16 秒这么离谱**反而是幸运的**,因为一眼能看出错。**真正致命的是几毫秒级的偏斜** —— 那会产出"看起来完全合理、但每个数都是错的"结论,而被测量的段延迟本身就是微秒级。
3. §8.2.3 的差值法对任意大小的偏移完全免疫,因为它只在同一台机器内部做减法。

**运维建议(不影响分析正确性,但影响可视化)**:建议在 suzhou950 上启用 NTP(需 root)。16 秒偏移会让合并后的 waterfall 在**视觉上**呈现"响应早于请求"的错乱 —— 导出的时长数字仍然正确,但图没法看。毫秒级同步即可,见 §8.2.6。

#### 8.2.1 问题的量级

要测的段延迟是**微秒级**,而:

| 同步手段 | 典型精度 | 够用吗 |
|---|---|---|
| 无同步 | 秒~分钟级漂移 | ❌ |
| NTP / chrony(公网) | **毫秒级** | ❌ 比被测量大 3 个数量级 |
| NTP(局域网、低抖动) | 亚毫秒~百微秒 | ❌ 仍与被测量同量级 |
| PTP(IEEE 1588,需硬件时间戳网卡+交换机支持) | 微秒~亚微秒 | ✅ 但需专门硬件 |

**结论:不能把"两台机器的时钟对齐到足够精度"当作方案的前提。** 必须设计成对偏斜免疫。

#### 8.2.2 采用的模型:span 树 + 本地时长

抛弃"把所有时间戳排在一条全局绝对时间轴上"的做法,改用 **OpenTelemetry / Zipkin 的 span 模型**:

每个 span 记录三样东西:
- **本地 start**:该机 `CLOCK_REALTIME`,**只用于粗排序和人眼可读**,不参与精确计算
- **duration**:该机 `CLOCK_MONOTONIC` 测量,**精确,不受 NTP 阶跃影响**
- **parent 链接**:构成因果树

分布式 tracing 系统之所以都是这个模型,原因正是时钟偏斜。我们不是在发明轮子,是在避免重造一个已知会塌的轮子。

#### 8.2.3 跨机延迟怎么算:差值法,偏斜自动抵消

核心技巧 —— **网络时间由两个"各自在本机测量的时长"相减得到**:

```
网络往返时间 = client 侧观测的 RPC 总时长 − server 侧观测的处理时长
             └─ 机器 A 的单调钟测 ─┘      └─ 机器 B 的单调钟测 ─┘
```

两项各自在本机测量,**相减时两台机器的时钟偏斜完全抵消**,不需要任何同步。同理可逐层剥出每一跳的开销:

```
envoy-out 转发开销 = (client 观测总时长) − (envoy-out 观测的 upstream 往返时长) − (client 本地编解码)
跨机网络往返     = (envoy-out 观测的 upstream 往返) − (envoy-in 观测的总处理时长)
envoy-in 转发开销 = (envoy-in 观测总时长) − (server 观测的处理时长)
```

#### 8.2.4 必须承认的根本限制

**差值法只能得到往返总和,无法拆成"去程"和"回程"。**

要拆,数学上必须有同步时钟 —— 这是信息论层面的限制,不是本方案的缺陷。**所有分布式 tracing 系统都受此约束**,Jaeger/Zipkin 的 UI 里那些看起来分得清去回程的图,要么依赖了时钟同步(并因此不准),要么其实也只是在画往返。

报告中凡是涉及跨机的数字,一律标注为"往返",不伪造单向数字。若确实需要单向拆分,唯一正解是 PTP + 硬件时间戳网卡,列为后续工作。

#### 8.2.5 `pcs/pce/pss/prs/pre` 的正确用法

§3.5 提到 Kitex 预留了这套 perf key。**多机下它们的正确语义是"每跳自报本跳内部区间",而不是"各跳往同一条时间轴上盖戳"。**

消费方只把它们当**区间**用(区间的两端都在同一台机器上测,所以有效),**绝不跨机相减**。这个区别很容易搞错,是本方案里最需要 code review 盯住的一点。

#### 8.2.6 仍然要跑 NTP —— 但目的不同

两台机仍需 chrony/NTP 保持毫秒级同步。**目的不是精度,而是让因果序在可视化时不出现"响应早于请求"这种视觉错乱。** 毫秒级足够。

#### 8.2.7 偏斜鲁棒性必须被验证,不能只是声明

本环境恰好有 **16.34 秒**的天然偏斜(§8.2.0),这是难得的测试素材 —— 一旦启用 NTP 就消失了。因此**刻意保持 950 不同步,直到三重验证的前两步完成**。

三重验证的完整定义见 §10.2 第 5 级。核心一条:

> **在 merge 工具中注入人工偏移**:给某一节点的全部时间戳统一加 ±50 ms,断言导出的**每一跳时长一个数都不变**。

不做这一步,"支持多机"就只是一句声明。

### 8.3 trace_id 的"先占位后回填"

**问题**:Envoy 收到第一个字节时,TTHeader 尚未解析,trace_id 未知。但"首字节到达时刻"(E1)恰恰是必须量的点。

**方案**:
1. E1 时以 `(connection_id, 帧序号)` 作为临时 key 暂存事件
2. E2(`decodeFrameStart` 完成)解出 StrKV 中的 `traceparent`
3. 将该临时 key 下的事件**回填**到真实 trace_id
4. flush 时统一归并

不做这一步,`dn_first_byte` 将永远无法与其余点关联。

### 8.4 采样:让打点在压测下成立

**问题**:§7 那 42 个打点若每请求全量记录,在压测下不成立:

```
100k QPS × 42 点 × ~200 字节/条 ≈ 840 MB/s
```

写不动是其次,**打点本身会主导测量结果** —— 测出来的是观察者效应而非 mesh 开销。

#### 8.4.1 头部采样,标志随 traceparent 传播

W3C traceparent 的最后一个字节是 flags,bit0 即 sampled:

```
00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
                                                     ^^ bit0 = sampled
```

Kitex client 决定采样并置位,经 TTHeader StrKV 传播;**每一跳进探针先查该位,未采样立即返回**,开销是一次分支判断。

选头部采样而非尾部采样的理由:尾部采样要求每一跳先无条件记录、事后再决定丢弃,那正是我们要避免的开销模型。

#### 8.4.2 一个必须解决的矛盾:E1 早于采样判断

E1(`dn_first_byte`,下游首字节到达)发生在**解析 TTHeader 之前**,那时 sampled 标志尚不可知。而这个点恰恰必须量 —— 它是"数据到达 Envoy"的唯一时刻。

**方案**:E1 无条件写**一个时间戳**到每连接的预分配槽位(无堆分配、无锁),待 E2 解出标志后,未采样则直接丢弃。

代价约 20–30 ns/请求。不这么做,采样请求将永远拿不到首字节时刻 —— 这是为极小的固定成本换取不可替代的观测点。

#### 8.4.3 三种运行模式

| 模式 | 采样率 | 用途 |
|---|---|---|
| 调试 | 100% | 验证打点正确性、看单请求 waterfall |
| **归因(本方案主用)** | 1/100 ~ 1/10 | 中等 QPS 下做开销归因,样本充足 |
| 极限吞吐 | 0(或编译期关闭) | 测 QPS 上限,不受打点干扰 |

#### 8.4.4 两层可观测性,缺一不可

| 层 | 覆盖 | 回答什么 |
|---|---|---|
| 采样 trace | 采样率 | "**某一个**请求的时间花在哪" |
| 全量直方图 | 100% | "**分布**是什么" —— Envoy stats + Kitex stats,常数开销 |

只有采样 trace 会漏掉长尾:p999 事件在 1/1000 采样下大概率采不到。两层结合才能既有分布又有归因。

### 8.5 归因测量的方法论前提

**归因测量必须在非饱和区进行。** 这是本方案能否给出有意义结论的前提。

**理由**:一旦压到饱和,请求在队列中等待的时间会主导端到端延迟,waterfall 上表现为某一段异常膨胀 —— 但那反映的是排队,不是该组件的处理成本。此时"时间花在哪"的答案退化为"在队列里等",对归因毫无价值。

**因此测量分两步:**

| 步骤 | 做什么 | 输出 |
|---|---|---|
| **1. QPS 扫描** | 逐级加压直到延迟拐点(p99 开始非线性上升),trace 关闭 | 饱和点 QPS_max |
| **2. 归因测量** | 固定在 **~50% × QPS_max**,开启采样 trace | waterfall + 各段 p50/p99 |

报告中的每一组归因数据都必须标注其运行 QPS 与当时的 QPS_max,否则读者无法判断该数据是否落在非饱和区。

### 8.6 打点开销的量化

四组对照,把固定成本与记录成本分开:

| 组 | 配置 | 隔离出什么 |
|---|---|---|
| 基线 | `--define kitex_probe=disabled` | 无插桩的真实性能 |
| A | 插桩编译进去,采样率 0 | **编译期插桩的固定成本**(分支、代码膨胀、寄存器压力) |
| B | 采样率 1/100 | 实际归因配置的成本 |
| C | 采样率 100% | 成本上界 |

`基线 → A` 的差值是"哪怕不采样也要付出的代价",这个数字决定了插桩能否长期留在生产构建里。

### 8.7 汇聚与呈现

**数据收集**:两台机各写各的 NDJSON。收集走 950 ↔ 920B 直连(同网段,实测 200 MB 传输正常),**不经开发机**。

离线 Go 工具 `cmd/merge` 的处理流程:

```
读多机 NDJSON
  → 按 trace 分组
  → 按 host 分区                      ← 关键：同 host 才允许时间戳运算
  → 各 host 内用 mono_ns 算 span duration
  → 按 parent 链接拼 span 树
  → 跨 host 段用差值法导出（§8.2.3）
  → 输出
```

**工具层必须强制的三条纪律**(把 §8.2 的规则固化进代码,而非靠人自觉):

1. **跨 host 的时间戳相减 → 直接报错**,不允许静默出数
2. **跨 host 的段一律标注"往返"**,不输出单向数字(§8.2.4)
3. **提供 `--inject-skew <node>=<±ms>` 开关** —— 用于 §10.2 第 5 级的鲁棒性验证;正是因为这个开关的存在,"偏斜无关"才是可证伪的

**三种输出形态**:

1. **终端 waterfall**(ASCII,每段标注 Δµs;跨 host 段标注 `[往返]`)
2. **Chrome Trace Event JSON** —— 可直接拖入 `chrome://tracing` 或 Perfetto,四进程显示为四条泳道,**按 host 分组**
3. **汇总统计** —— 各段 p50/p99,回答"双跳 sidecar 的开销分布"

> **可视化的诚实性**:Perfetto 需要一条统一时间轴,而跨机时间戳不可比。做法是**以 client 的 `RPCStart` 为原点做逻辑对齐**,并在图上显式标注"跨机段为往返估计,非绝对时刻"。绝不把偏斜过的绝对时间戳直接画上去 —— 那会画出"响应早于请求"的图。

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

**已实证(不再是推测)**:用 Kitex 官方编码器生成含 `RPC_PERSIST_TENANT` 等大写 key 的真实帧,做 decode → encode 往返:

| `preserve_keys` | 往返结果 |
|---|---|
| `true`(即配置了 `header_keys_preserve_case`) | **逐字节保真** |
| `false`(默认) | **失真** —— 大写前缀被改成小写 |

机制上的原因:`MessageMetadata` 只在 `preserve_keys=true` 时才安装 `ThriftCaseHeaderFormatter`(`metadata.h:63`),没有 formatter 就没有原始大小写可还原。

该行为已由单测 `TTHeaderMetainfoCaseTest.CaseIsLostWithoutPreserveKeys` **双向锁定**:既断言开启后保真,也断言关闭后必然失真。后者的意义在于 —— 如果哪天 Envoy 改了 header 大小写行为使得不开也能保真,这条测试会失败,提醒我们重新评估本节结论,而不是让一个过时的配置要求悄悄留在文档里。

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

| 阶段 | 内容 | 完成判据 | 对应验证级 | 状态 |
|---|---|---|---|---|
| **0a** | 工具链就位:bazel 8.7.0 + Go + 两仓源码 | `bazel --version` 正常 | — | **✅ 已完成** |
| **0b** | **验证 Envoy 在 openEuler aarch64 上编得过** | `envoy-static` 产出且 `--version` 正常 | — | **🔄 进行中,尚未进入编译阶段** |
| **1** | TTHeader transport(§5.1 + §5.2) | round-trip 单测全绿 | 第 1 级 | 待开始 |
| **2** | 静态 bootstrap + 单跳打通(单机降级模式) | echo 通 | 第 2 级 | 待开始 |
| **3** | 路由配置(IntKV header + method_name) | 改 header 能改变 cluster 选择;metainfo 往返完整 | 第 3 级 | 待开始 |
| **4** | 探针库(§5.3)+ Kitex 打点(§6)+ 双跳(单机) | 42 个点齐全,能 merge 出单请求 waterfall | 第 4 级 | 待开始 |
| **5** | 真实跨机部署 + merge 工具 + 时钟鲁棒性验证 | §10.3 交付物齐备 | 第 5 级 | 待开始 |

**阶段 0b 必须最先做且不可跳过** —— 它验证的是 §11 中唯一可能推翻整体方案的风险。若失败,后续所有 C++ 工作都无处落地,应立即转入退路(§11 第一行)。

> **诚实声明**:至本文修订时,阶段 0b **尚未完成**。构建两次卡在依赖拉取阶段(§11.1),**一行 Envoy 代码都还没有真正编译过**,因此 "aarch64 能编过" 既未被证实也未被证伪。本报告的所有 C++ 设计以此风险未消除为前提。

### 10.1 构建路径(全部在 suzhou950,无需 sudo)

**已完成的步骤(实测数据)**:

| 步骤 | 实际做法 | 实测耗时 |
|---|---|---|
| 装 bazel | **不用 bazelisk**(其 GitHub release 从该机下不动)。直接从**华为云镜像**取 8.7.0 二进制 → `~/bin/bazel` | **5.5 s @ 11.5 MB/s** |
| 装 Go | `mirrors.aliyun.com` 的 go1.26.5 linux-arm64 → `~/sdk/go` | 秒级 |
| 拉源码 | **在 suzhou950 上直接 `git clone --depth 1`**,不从开发机传 | envoy 244 M,分钟级 |

**关键经验**:开发机 → suzhou950 只有 ~150 KB/s(§2.4),因此

> **凡是大文件,一律在目标机上直接从国内镜像/codeload 获取,绝不经开发机中转。**

同一个 bazel 二进制:开发机下载 10.6 s,但传到 suzhou950 需 ~7 min;而 suzhou950 直接从华为云取只要 5.5 s。**差 76 倍。**

**构建命令**:

```bash
ulimit -n 65536
~/bin/bazel --output_base=/tmp/eob build -c opt \
    --curses=no --color=no \
    --experimental_repository_downloader_retries=10 \
    --http_timeout_scaling=2.0 \
    //source/exe:envoy-static
```

要点说明:

| 选项 | 为什么 |
|---|---|
| **`--experimental_downloader_config`** | **最关键的一条**。不做 URL 重写,构建会卡在 Rust 工具链下载 5–10 小时。详见 §11.2 |
| **不加 `--config=gcc` / `--config=clang`** | 前者要 `libstdc++.a`(该机没有),后者要 libc++(也没有)且本版本已无 `setup_clang.sh`。用默认自动探测工具链 |
| `--output_base=/tmp/eob` | `/tmp` 是 690 G tmpfs,避免磁盘 IO 拖累 384 核 |
| `--curses=no --color=no` | 否则日志被 `Computing main repo mapping` 反复覆盖,无法判断卡在哪个依赖 |
| `--http_timeout_scaling=2.0` | **低于**默认 6.0。面对间歇性故障要快速失败,不能拉长等待(§11.1) |
| 外层 10 次重试循环 | 仅在识别为网络错误时重试;编译错误立即停止 |
| `ulimit -n 65536` | 默认 1024 不够;硬限 524288,无需 sudo |

**远程执行纪律**(因连接不稳,§2.5 第 4 条):

- 长任务一律 `nohup setsid <script> </dev/null &` + 轮询标记文件,**绝不在前台 ssh 里等待**
- 不使用 ssh ControlMaster 复用 —— 连接僵死后 socket 残留会让后续所有复用它的 ssh 挂起
- 重启构建前先确认无残留 bazel 客户端/服务端进程占锁(`Another command is running` 即为此症状)

### 10.2 验证阶梯

五级,每级可独立证伪,避免"全连上了但不知道哪坏了":

**第 1 级 · 协议正确性(不需要跑 Envoy)**

C++ 单测:用 Kitex 真实产生的 TTHeader 字节流(从 demo 抓取并固化为测试 fixture)喂给 `decodeFrameStart`,断言 StrKV / IntKV / protocol_id / seq_id 全部解析正确;再 `encodeFrame` 回去,断言**与原始字节逐字节相等**(round-trip)。

必须覆盖:header 长度为 4 倍数(§9.2)、含 ACLToken、含未知 IntKV id(fallback 路径)、空 KV。

**第 2 级 · 单跳连通**

`client → envoy-out → server`,echo 通。此级验证 transport 在真实数据流下可用。

**第 3 级 · L7 能力证据**

配置两条路由:一条基于 `x-tt-to-service`(IntKV 来源),一条基于 `method_name`(协议层来源)。验证**修改 header 能改变 cluster 选择**。

这是"真 L7 而非 TCP 透传"的硬证据 —— TCP 透传做不到这件事。

**第 4 级 · 双跳全链路打点(单机降级模式)**

四进程全在 suzhou950,merge 出 waterfall;用 §7 的交叉验证手段校验关键 Δ。

此级用单机模式,目的是**在引入跨机变量之前**先确认打点逻辑本身正确。

**第 5 级 · 真实多机 + 时钟鲁棒性**

这一级才是本方案的真正验收,三条断言缺一不可:

| # | 断言 | 怎么验 |
|---|---|---|
| 5a | 跨机链路端到端打通,42 个点齐全 | 950 跑 client+envoy-out,920B 跑 envoy-in+server |
| 5b | **merge 工具拒绝任何跨 host 的时间戳相减** | 构造一条跨 host 直减的调用,断言工具报错而非静默出数 |
| 5c | **偏斜鲁棒性** | 见下 |

**5c 的具体做法** —— 三重验证,层层加码:

1. **真实偏斜**:本环境两机天然有 **16.34 秒**偏移(§8.2.0 实测)。直接跑,断言导出的每跳时长合理(而非出现负数或 16 秒量级的荒谬值)。
2. **人工注入**:在 merge 工具中给某一节点全部时间戳统一加 ±50 ms,断言导出的**每一跳时长逐位不变**。
3. **消除偏斜后复测**:待 950 启用 NTP、两机偏差降到毫秒级后重跑,断言导出时长与步骤 1 的结果**在测量误差内一致**。

第 3 步是最有说服力的一条:**同一套分析,在 16 秒偏斜和毫秒偏斜两种条件下给出相同答案**,才算真正证明了偏斜无关性。只做第 2 步(纯人工注入)有可能因为注入方式与真实偏斜的作用路径不同而漏掉问题。

**第 6 级 · 压测下的开销归因(最终目标)**

前五级都是功能性验证,这一级才产出方案要回答的问题。按 §8.5 的两步法:

| 步骤 | 内容 | 判据 |
|---|---|---|
| 6a | QPS 扫描定位饱和点(trace 关闭) | 得到 QPS_max 与延迟拐点 |
| 6b | 在 ~50% × QPS_max 下开采样 trace | 各段 p50/p99 归因数据 |
| 6c | §8.6 四组对照 | 量化打点自身开销 |

**每组归因数据必须标注运行 QPS 与当时的 QPS_max**,否则无法判断是否落在非饱和区(§8.5)。

### 10.3 交付物

1. 本设计报告
2. 可复现的构建 / 运行脚本(含两机分发)
3. 一次真实跨机请求的 waterfall(终端 + Perfetto 两种形态)
4. UDS vs loopback TCP 的 A/B 数据
5. **打点开销的四组对照**(§8.6)
6. **时钟鲁棒性验证报告**(§10.2 第 5 级三重验证的结果)
7. **压测下的开销归因报告**(§10.2 第 6 级)—— 本方案的最终产出:双跳 sidecar 的时间具体花在哪几段,以及每段的 p50/p99

---

## 11. 风险与退路

| 风险 | 概率 | 影响 | 状态 / 退路 |
|---|---|---|---|
| **openEuler aarch64 编不过 Envoy** | 中 | 高(阻塞全部) | **仍未验证** —— 至本文修订时构建尚未进入编译阶段(卡在依赖拉取),**不能声称此风险已消除**。退路:① 用官方 Envoy 构建容器(docker 权限已开通);② 最坏情况用预编译 envoy 二进制验证 §10.2 第 2–3 级,仅插桩部分受阻。**注意 `--config=gcc` 不可用**(见 §2.5 第 2 条) |
| **bazel 依赖拉取失败** | **高(已发生)** | 中 | 已实际发生两次。见 §11.1 |
| TTHeader 解析边界情况遗漏 | 中 | 中 | 第 1 级单测用真实字节流 fixture,不用手工构造 |
| 打点本身扰动时序 | 低 | 中 | thread_local 无锁 + 批量 flush;并用编译期开关做对照实验量化 |
| metainfo 大小写问题漏配 | 中 | **高(静默)** | 第 3 级验证中显式断言 metainfo 往返完整 |
| **跨机时钟偏斜导致结论错误** | **高(环境实测 16.34 s)** | **高(静默)** | §8.2 全套设计针对此;§10.2 第 5 级三重验证把关 |
| 920B 磁盘紧张 | 中 | 低 | /home 已用 85%,仅余 112 G。920B 只放二进制与 trace 数据,不放构建产物 |

### 11.1 已发生的风险:bazel 依赖拉取

**现象**:两次构建均卡在依赖拉取阶段,一次报 `Connect timed out`(googleapis),一次在 `build_bazel_rules_apple` 之后静默停滞 7 分钟且无任何网络连接。

**根因分析**:

1. suzhou950 到 GitHub 的连接是**间歇性抽风**,不是持续不可达。同一 URL 前后两次实测差异极大:`github.com` 时而 0.89 s / 200,时而 20 s 超时;`raw.githubusercontent.com` 时而超时,时而 0.61 s / 200。该机**无全局 IPv6**,全部走 IPv4。
2. **第二次卡死是配置失误自招的**:为"应对不稳网络"把 `--http_timeout_scaling` 设成 `10.0`,结果 bazel 陷进超长超时等待,**外层重试机制根本没机会触发**。

**教训(已固化进构建脚本)**:

> 面对**间歇性**故障,正确策略是**快速失败 + 多次重试**,而不是延长单次等待。拉长超时只会把"快速失败后重试即可成功"变成"长时间挂死"。

**当前配置**:`--http_timeout_scaling=2.0`(低于 `.bazelrc:52` 默认的 6.0)+ `--experimental_repository_downloader_retries=10` + 外层 10 次重试循环,且**仅在识别为网络错误时才重试**(编译错误立即停止,避免浪费)。

**仍未用上的退路**:`--distdir` 预下载(依赖清单在 `bazel/repository_locations.bzl`)。因开发机到 suzhou950 仅 ~150 KB/s,传输 GB 级 distdir 不现实,故此退路实际不可行;真正的退路是官方构建容器。

### 11.2 真正的根因:两个特定域名极慢,必须做 URL 重写

排查过程中反复出现"静默停滞":连接处于 `ESTABLISHED` 且收发队列均为 0,TCP 层看不出异常,只有读超时能打破。逐一实测各源后定位到**两个致命慢源**:

| 域名 | 用途 | 从 suzhou950 实测 |
|---|---|---|
| `static.rust-lang.org` | Rust 工具链(Envoy 有 Rust 扩展,`rules_rust` 在 analysis 阶段就要解析工具链,绕不过) | **29.6 KB/s** |
| `raw.githubusercontent.com` / `185.199.x` CDN | 多个 repository rule 拉取 | **完全超时** |

Rust 工具链体积数百 MB,按 29.6 KB/s **需 5–10 小时**,这就是前三次构建"卡在依赖拉取"的真相 —— 不是 GitHub 不稳,是这一个包在拖。

**解法:bazel `--experimental_downloader_config` 做 URL 重写。**

`~/.bazelrc`:
```
build --experimental_downloader_config=/home/<user>/downloader.cfg
```

`downloader.cfg`:
```
rewrite static\.rust-lang\.org/(.*) rsproxy.cn/$1
rewrite raw\.githubusercontent\.com/(.*) gh-proxy.com/https://raw.githubusercontent.com/$1
rewrite objects\.githubusercontent\.com/(.*) gh-proxy.com/https://objects.githubusercontent.com/$1
```

镜像实测对比:

| 原始源 | 速度 | 镜像 | 速度 | 倍数 |
|---|---|---|---|---|
| `static.rust-lang.org` | 29.6 KB/s | `rsproxy.cn` | **22 MB/s** | **750×** |
| `raw.githubusercontent.com` | 超时 | `gh-proxy.com` | 0.93 s / 200 | — |

加上 Rust 重写后立竿见影:依赖数一分钟内从卡了半小时的 86 冲到 **357**,首次进入 analysis 阶段。

**安全性说明**:bazel 对每个外部依赖强制校验 sha256(取自 `repository_locations.bzl`),因此即便第三方镜像返回被篡改的内容,也会在校验阶段立刻失败,不会静默引入。这使得使用非官方镜像在此场景下是可接受的。

**`rsproxy.cn` 的一个特性**:首次请求某文件可能返回 `504`(回源冷启动),重试即 `200`。因此 `--experimental_repository_downloader_retries` 必须 ≥ 2。

### 11.3 排障方法论:如何识别"静默停滞"

这类故障最难的是**症状与卡点不对应** —— 日志只显示 `Analyzing: ...`,不告诉你在等谁。有效的诊断序列:

1. `ls /tmp/eob/external | wc -l` 定期采样 —— 依赖数不涨说明卡在拉取
2. `ls -lt /tmp/eob/external | head` —— 最后落盘的是谁,下一个就是嫌疑人
3. `ss -tnp state established | grep :443` —— **关键**。看是否有连接、连到哪个 IP
   - 有 `ESTABLISHED` 且队列为 0 → 对端不发数据,是慢源
   - 无任何连接 → 在重试退避里干等
4. 反查 IP 归属(`185.199.x` = GitHub 静态 CDN,`140.82.x` = github.com 主站)
5. 用 `curl -r 0-2000000 -w "%{speed_download}"` 分别实测原始源与候选镜像

**只看 `ss ... state established` 会漏掉 `SYN_SENT`** —— 排查早期我就漏了一次,应当用 `ss -tan` 看全部状态。

**验证重写是否真的生效**:看连接的对端 IP 归属。走 `gh-proxy.com` 时对端会是 Cloudflare 段(`172.64.x`);若仍是 `185.199.x`(GitHub 静态 CDN),说明规则没匹配上。这比看日志可靠 —— bazel 不会打印它实际请求的 URL。

**两个容易误判的监控口径**:

| 错误口径 | 为什么误导 | 正确口径 |
|---|---|---|
| 盯单个文件大小 | 该文件下完后转下一个,大小不再变,看起来像卡死 | `du -sm /tmp/eob/external` 看总量 |
| 盯 `external` 下的**目录数** | 一个 repository rule 可能要下多个文件(如 `rules_buf_toolchains` 下有 buf、protoc-gen-buf-lint、protoc-gen-buf-breaking),期间目录数不变 | 总量 + `lsof` 看当前写入 fd |

### 11.4 已发生:tmpfs 的 inode 耗尽

**症状**:编译阶段大量报 `Could not copy inputs into sandbox: ... (No space left on device)`,**但 `df -h` 显示空间只用了 1%**。

**根因**:

```
df -h /tmp   →  690G 总量，用了 5.6G，可用 685G      （1%）
df -i /tmp   →  1,048,576 inode，用了 843,548        （失败时 100%）
```

tmpfs 挂载参数写死 `nr_inodes=1048576`,**与容量无关**。而 bazel 用 383 路并行,每个 sandbox 都要为 LLVM 头文件创建成千上万个符号链接 —— **inode 消耗与并行度成正比,与数据量无关**。

**处理**:`--output_base` 从 `/tmp` 迁到 `/home`(ext4 on NVMe,2.32 亿 inode,已用 10%)。

**关键**:切换 output_base **不会导致重新下载** —— bazel 的 repository cache(`~/.cache/bazel/.../cache/repos`,按 sha256 索引)独立于 output_base,已有内容会被复用。

**排障要点**:`No space left on device` 未必是空间不足,**必须同时看 `df -h` 与 `df -i`**。

### 11.5 已发生:cel-cpp 触发 `-Wnullability-completeness`

**症状**:

```
external/cel-cpp/common/internal/reference_count.h:179:36:
  error: pointer is missing a nullability type specifier
         [-Werror,-Wnullability-completeness]
```

**性质:这不是 aarch64 特有问题**,而是 Clang 版本敏感的告警 —— 一旦某翻译单元用到 `_Nonnull`/`_Nullable`,Clang 就要求同文件内所有指针都标注。Envoy 开了 `-Werror`,告警即错误。

**Envoy 上游已知此问题**,在两处做了豁免:

```
.bazelrc:108   build:macos          --cxxopt=-Wno-nullability-completeness
.bazelrc:119   common:clang-common  --cxxopt=-Wno-nullability-completeness
```

我们未启用 `--config=clang`,故未继承该豁免。

**处理**:直接加 `--cxxopt=-Wno-nullability-completeness`,而非切到整套 `--config=clang` —— 后者会改变 `host_platform` 与 `-stdlib=libc++`,使已完成的上万个编译动作缓存失效。

### 11.6 已排除的退路:官方构建容器

曾考虑用 `envoyproxy/envoy-build-ubuntu` 官方构建镜像绕开环境问题,**经查证不可行**:

1. **`registry-1.docker.io` 从 suzhou950 超时不可达**(实测 code=000),镜像根本拉不下来
2. 该机 docker 版本为 **18.09.0**(2018 年),无 `buildx`、多架构 manifest 支持
3. **更根本的是:该镜像只提供工具链,不提供依赖。** Envoy 的几百个外部依赖仍由 bazel 在构建时从网络拉取 —— 也就是说 §11.2 的三个慢源一个都躲不掉。Envoy 官方 CI 之所以快,靠的是 RBE 远程缓存,而非镜像本身

而工具链问题我们已用"默认自动探测 + 不加 `--config`"绕过(§2.5 第 2 条),恰恰是容器唯一能帮上忙的部分。**结论:容器路线成本高、收益为零。**

**关于第一条**:这是唯一可能推翻整体方案的风险,因此在实施第一步就验证,不拖到后期。

---

## 附录 A:与生产形态的差异(显式代理 vs 透明拦截)

本方案用显式代理(client 直接拨 sidecar 的 UDS)。生产环境的 Istio 类方案用 iptables `REDIRECT` + `ORIGINAL_DST` cluster 做透明拦截。

差异仅在于**流量如何进入 sidecar**:
- 显式代理:应用知道 sidecar 存在,目标服务名走 TTHeader `ToService`
- 透明拦截:应用无感知,目标地址由内核 `SO_ORIGINAL_DST` 还原

**对本方案的全部打点结论无影响** —— 进入 sidecar 之后的路径完全一致。透明拦截额外引入的是 netfilter 的 conntrack 开销与一次 `getsockopt(SO_ORIGINAL_DST)`,可作为后续扩展实验。

## 附录 B:若要拆分单向延迟,需要什么

§8.2.4 说明了差值法只能得到**往返**总和,无法拆成去程/回程。若后续确实需要单向数字,以下是完整的技术路径:

**必要条件(缺一不可):**

1. **PTP(IEEE 1588)** 而非 NTP —— NTP 毫秒级,与被测量同量级甚至更大
2. **网卡硬件时间戳**(`SO_TIMESTAMPING` + `HWTSTAMP_TX_ON`/`RX_FILTER_ALL`)—— 软件时间戳会把内核调度延迟计入
3. **交换机支持 PTP transparent clock 或 boundary clock** —— 否则交换机排队延迟会污染同步
4. `ethtool -T <iface>` 确认网卡的时间戳能力

**本环境的现状**:两机分别是 `enp33s0` 与 `eno1`,未验证是否支持硬件时间戳;且 suzhou950 当前连 NTP 都未开启。**在补齐上述条件之前,任何"单向延迟"数字都是编造的。**

**折中方案**:用 `tcpdump --time-stamp-precision=nano` 在**两端同时抓包**,配合 TCP 序列号做包级配对。这仍受时钟偏斜影响,但可以用"同一台机器上看到的发包与收包时刻"约束住往返的组成部分,从而给出单向延迟的**区间估计**而非点估计。

**本方案的立场**:报告中所有跨机数字一律标注"往返",不给单向点估计。这不是能力不足,是拒绝给出无法支撑的精度。

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
