# 加压器与 kitex-benchmark 的关系

**2026-08-11 起，本项目的加压器就是 kitex-benchmark 的 `kitex-echo`**，
不再是自己写的 client。差异是**对 3 个上游文件的 62 行改动 + 1 个新文件**，
`git diff` 就是完整清单。

改动在 [`RiceReallyGood/kitex-benchmark`](https://github.com/RiceReallyGood/kitex-benchmark)
的 **`feat/meshlab-probe`** 分支（fork 自 `cloudwego/kitex-benchmark`）：

```bash
git clone -b feat/meshlab-probe git@github.com:RiceReallyGood/kitex-benchmark.git
git diff main..feat/meshlab-probe          # ← 完整改动清单
```

之所以要这样，是因为归因结论的可信度有一半压在加压器上。自己写一份意味着
并发模型、限速、预热、计数、分位数算法都是我们的实现，别人无从核对，
「我们测出 147 µs」这句话就没有分量。

> 历史：2026-08-11 之前用的是 `demo/client` + `demo/server`（自研）。
> 那份代码仍在仓库里，`MESHLAB_LEGACY=1` 可切回，但**不应再用于出数据**。

---

## 一、改了什么：完整清单

```
 go.mod                      | 17 +++++++++++++++++     ← 14 行是注释，真正的指令 4 条
 thrift/client.go            |  4 ++--                  ← 2 处 ctx 包一层
 thrift/kitex/client/main.go | 13 ++++++++-----
 thrift/kitex/main.go        | 38 +++++++++++++++++++++++++++++++++++---
 thrift/meshlab.go           | 新增（本项目的适配层，全部逻辑集中在这里）
```

**被删掉的上游逻辑只有三处硬编码**：`transport.Framed`、`"test.echo.kitex"`、
`:8001`。三者都改成了 flag，**默认值就是原来的硬编码值**。

### 没有动的东西 —— 这才是重点

| 目录 | 内容 | 改动 |
|---|---|---|
| `runner/` | 并发模型、令牌桶限速、预热、Counter、分位数、TPS/TP99 输出 | **0** |
| `perf/` | CPU/MEM 采集、pprof、begin/end/report 协议 | **0** |
| `codec/` | echo.thrift 与生成代码 | **0** |
| `scripts/` | 官方压测脚本 | **0** |

**整套加压与测量机器一行没改。** 我们只换了协议、地址和挂了个 tracer。

---

## 二、三条硬约束：为什么每一处非改不可

### 1. Framed 没有 header KV 段

上游用 `transport.Framed`，线格式是「4 字节长度 + thrift 消息体」，**没有 KV 段**。
于是：

- **traceparent 无处安放** → 四个节点的事件无法关联到同一条 trace
- **`ToService` 无处安放** → Envoy 没法做 L7 路由

这不是嫌它慢，是物理上没有位置。过 sidecar 必须换 TTHeader，并且要注册
`ClientTTHeaderHandler` —— Kitex 默认只挂 `MetainfoClientHandler`（写 StrKV），
不注册的话 TTHeader 里没有 IntKV 段，Envoy 的 `x-tt-to-service` 匹配不到东西，
**而现象只是「路由不上」，不会告诉你少了什么**。

### 2. 服务名要能配

Envoy 按 `x-tt-to-service: echo-server` 精确匹配（见 `envoy-conf/*.yaml`），
上游写死的是 `test.echo.kitex`。

### 3. 监听地址要能配

机内 sidecar 通信走 UDS，上游写死 `:8001` TCP。
客户端侧**零改动** —— `client.WithHostPorts` 本来就内建 UDS 支持
（`kitex/client/option.go:152`，先试 `ResolveTCPAddr` 再试 `ResolveUnixAddr`）。

---

## 三、默认值 = 上游行为，而且这一条是验证过的

所有开关默认关闭：不给 `-proto` 就是 Framed，不给 `-trace` 就不挂探针，
不给 `-svc`/`-addr` 就是上游写死的那两个值。

**同一个二进制既是我们的加压器，也是干净的公认基准。** 做校准跑
（「我们这两台机器跑标准基准是多少」）不需要另编一份二进制。

2026-08-11 实测：

```
$ reciever -addr :8001 &
$ bencher -addr 127.0.0.1:8001 -b 1024 -c 4 -qps 2000 -t 3 -warmup 1
Info: [KITEX]: requests total: 6007, failed: 0
Info: [KITEX]: TPS: 2001.10, AVG: 0.05ms, TP99: 0.10ms, TP999: 0.71ms (b=1024 Byte, c=4, qps=2000, t=3s)

本次新生成的 .ndjson 文件：一个都没有      ← 默认确实不挂探针
server 日志里的 [probe] 行数：0
```

---

## 四、一个必须知道的细节：预热流量不采样

`runner.Main` 的时序是 **预热 → 发 `begin` 控制消息 → 正式压测 → 发 `end`**。
预热默认 2 秒、满速率发压。如果照采不误，采样池里会混进约 17 % 的预热流量，
而那批请求跑在**冷连接池**上（单请求 waterfall 里见过 2.3 ms 的建连），
足以把 p99 整体抬起来。

解法是直接借 runner 自己的 begin/end 协议卡采样窗口 —— 它们的发生时刻
恰好就是计量窗口的两端，不需要另加时序假设（`thrift/meshlab.go` 的 `mlArmed`）。

冒烟测试印证了这一点：

```
Info: [KITEX]: finish benching, took 3000 ms for 1501 requests
[probe] node=kitex-client 总请求=2004 采样=1501 丢弃=0
                          ^^^^ 含预热        ^^^^ 恰好等于计量窗口内的请求数
```

而且 client trace 恰好 **29 × 1501 = 43529** 行、server 恰好 **30 × 1501 = 45030** 行，
与 [probe-points.md](probe-points.md) 记的每节点点位数逐条吻合。

---

## 五、代码已经一样了，运行参数仍然不同

代码差异消掉之后，剩下的差异全在**怎么跑**上，而这些是刻意的：

| | kitex-benchmark 官方脚本 | 我们的归因矩阵 |
|---|---|---|
| 目的 | 吞吐基准（找饱和点） | **单请求时延归因**（必须待在非饱和区） |
| 并发 | `c=100` | `c ∈ {1, 16}` |
| 速率 | `qps=0`（不限速，闭环打满） | **`qps = c×1000`**（开环定速） |
| payload | `b=1024` | `1K / 4K / 8K / 16K / 64K` |
| 拓扑 | client → server 直连 | client → envoy-out → envoy-in → server，**跨机** |
| 采样 | 无 | 逐格调，目标每格约 3000 条 trace |
| CPU 绑核 | `taskset` / `numactl` 分核分 NUMA | **没有绑** —— 见下 |

> **绑核这一条是我们的已知偏差。** 官方脚本把 client 与 server 绑到不同的
> NUMA 节点，我们的跨机拓扑下两端本来就在不同机器上，但 **950 上 client 与
> envoy-out 仍在抢同一批 CPU**。384 核、c≤16 时争抢概率很低，所以没加；
> 要做高并发对照时必须补上，否则级差里会混进 CPU 争抢。

---

## 六、于是哪些数字可比、哪些不可比

**可比**：跑 `-proto framed`（默认）、直连、官方参数时，我们的数字与公开的
kitex-benchmark 结果口径一致 —— 这正是「校准跑」的用途。

**不可比**：归因矩阵的绝对数字。因为拓扑、协议、并发、payload 全都不同。
但**级差是可比的** —— 直连 / 单跳 / 双跳三级用同一个加压器、同一套参数，
只有 sidecar 数量在变，所以级差就是「加一个 sidecar 的净代价」。

> 一条不要犯的错：**别拿 Framed 直连做基线、TTHeader 双跳做终点**去算 mesh 开销。
> 那样级差里混进了协议变化（TTHeader 多出 header 编解码：编码 100 ns、
> 解码 0.7~1.7 µs），说不清哪部分是 mesh 的钱。三级必须全用 TTHeader。

---

## 七、怎么跑

```bash
# 归因矩阵（c ∈ {1,16} × 5 种包大小，10 格）
cd ~/envoy_kitex/mesh-lab && ./scripts/bench-matrix.sh

# 单格
CONCS=1 SIZES=4096 DURATION=10 ./scripts/bench-matrix.sh

# 校准跑：干净的上游行为，不挂探针
reciever -addr :8001 &
bencher -addr 127.0.0.1:8001 -b 1024 -c 100 -qps 0 -t 30
```

新增的 flag（两个二进制通用，全部默认关闭）：

| flag | 默认 | 说明 |
|---|---|---|
| `-proto` | `framed` | `ttheader` 才能过 Envoy |
| `-svc` | `test.echo.kitex` | 要与 Envoy 路由规则的 `x-tt-to-service` 一致 |
| `-trace` | 空 | **留空则完全不挂探针**（= 上游行为） |
| `-sample` | `1.0` | 采样率；c=16 高 qps 下务必调低 |
| `-node` | 按角色 | trace 里的节点标识 |
| `-addr`（server） | `:8001` | 绝对路径视为 UDS |

---

## 相关文档

- [bench-matrix-2026-08-11.md](bench-matrix-2026-08-11.md) —— 本次归因矩阵的实测结果
- [probe-points.md](probe-points.md) —— 107 个点位逐个说明
- [probe-coverage-audit.md](probe-coverage-audit.md) —— 还有什么测不到
