# mesh-lab

Kitex RPC 经 Envoy 双跳 sidecar 的**端到端时延归因**实验。

在 Kitex、netpoll、Envoy 三处源码插桩，按请求打点，把一次 RPC 的时间拆到微秒级：
每一段花在哪个进程、是在算还是在等、是在排队还是在网络上。

```
[机器 A]  kitex-client ──UDS──> envoy-out ─┐
                                          │ TCP 跨机
[机器 B]                    ┌── envoy-in <─┘
                            └──UDS──> kitex-server
```

## 这套东西能回答什么

**「上了 mesh 要多付多少钱？」** 直连 / 只加出向 sidecar / 完整双跳，三级跨机实测（p50，payload 64 B）：

| | 端到端 | 比直连多 |
|---|---:|---:|
| 直连（无 mesh） | 147.3 µs | — |
| 单跳（出向 sidecar） | 163.2 µs | **+15.9 µs**（+10.8 %） |
| 双跳（完整 mesh） | 228.2 µs | **+80.9 µs**（**+54.9 %**） |

几条不靠猜的结论：

- **两跳严重不对称，入向比出向贵 4.1 倍**（65.0 vs 15.9 µs）。
  但这**是机器不是路径** —— 把两个 Envoy 放同一台机器上，各阶段耗时逐项相等
  （编码 5.0 vs 5.0 µs）。做了对照实验才敢这么说。
- **加出向 sidecar 之所以便宜，是因为它替客户端省了钱**：客户端不再直接读跨机 TCP，
  改读本机 UDS，**goroutine 调度延迟从 11.4 µs 降到 2.8 µs**，写 socket 从 7.8 µs 降到 1.8 µs。
- 业务 handler 只有 **240 ns** —— 测的几乎全是框架与传输开销。
- 「等待对端」不再是黑盒：拆成 *真等待 / poller 排队 / readv / 缓冲入队 / goroutine 调度* 五段。

完整结论见 [docs/test-report.md](docs/test-report.md)，
原始 merge 输出在 [docs/results/2026-08-07/](docs/results/2026-08-07/)。

## 从哪读起

| 你想 | 看 |
|---|---|
| **第一次复现，从零到出数据** | [docs/runbook-reproduce.md](docs/runbook-reproduce.md) —— 假定网络已通，从下载 Go 一路到分析出图。每步都带 2026-08-07 全量复现的真实耗时与输出 |
| 已跑通过，日常跑一轮实验 | [docs/runbook-operations.md](docs/runbook-operations.md) —— 构建→运行→采集→分析 |
| 搭环境（镜像、代理、bazel 取舍）、网络排障 | [docs/runbook-build-environment.md](docs/runbook-build-environment.md) |
| 看实测结论 | [docs/test-report.md](docs/test-report.md) |
| 了解每个点位的含义 | [docs/probe-points.md](docs/probe-points.md) |
| 知道**还有什么测不到** | [docs/probe-coverage-audit.md](docs/probe-coverage-audit.md) |
| 专门评测网卡 | [docs/runbook-nic-benchmark.md](docs/runbook-nic-benchmark.md) |
| 看设计与取舍 | [docs/specs/](docs/specs/) |

## 代码分布

本仓库只含文档、demo、工具与脚本。插桩本身在另外三个仓库：

| 仓库 | 分支 | 内容 |
|---|---|---|
| [envoy](https://github.com/RiceReallyGood/envoy) | `feat/thrift-ttheader-transport` | **可上游** —— 让 `thrift_proxy` 原生识别 Kitex 的 TTHeader（magic `0x1000`），含 47 个单测 |
| [envoy](https://github.com/RiceReallyGood/envoy) | `wip/kitex-e2e-probe` | 基于上一分支追加侵入式打点，**不上游** |
| [kitex](https://github.com/RiceReallyGood/kitex) | `feat/detailed-trace-events` | 12 个细粒度事件 + 承接 netpoll 时间戳的槽位 |
| [netpoll](https://github.com/RiceReallyGood/netpoll) | `feat/meshlab-read-probe` | 读路径 5 个点位（epoll 唤醒 / readv / 唤醒通知） |

`kitex-benchmark` 未修改，只借用它已生成的 echo `kitex_gen` 代码。

## 本仓库结构

```
demo/         Kitex 客户端与服务端，含 probe 包（Tracer + 落盘）
tools/merge/  把各节点 NDJSON 合并成时序视图并做归因分析
tools/        fixturegen（造 TTHeader 测试向量）、udsdump
envoy-conf/   六份 Envoy 配置：单跳（同机/跨机）、双跳（同机/跨机）
scripts/      拓扑拉起、压测、构建环境辅助
docs/         见上表
```

**四种拓扑怎么拉起**：

| 拓扑 | 命令 | 用途 |
|---|---|---|
| 直连（跨机） | `TOPO=direct ./scripts/run-cross-machine.sh start` | 归因基线：没有 mesh 时是多少 |
| 单跳（跨机） | `TOPO=single ./scripts/run-cross-machine.sh start` | 只有出向 sidecar |
| 双跳（跨机） | `./scripts/run-cross-machine.sh start` | 完整 mesh，真实拓扑 |
| 同机降级 | `./scripts/run-single-hop.sh` / `run-two-hop.sh` / `run-direct.sh` | 只有一台机器时验证链路通不通 |

**做归因阶梯必须用跨机的那三级**。同机跑双跳的话，每加一跳就多一组 Envoy 进程挤同一批
CPU，三级的资源竞争程度不同，级差里混着 CPU 争抢，不能归给「多一跳」。

## 几个值得单独一提的设计点

**TTHeader 不是 Apache THeader。** magic 是 `0x1000` 而非 `0x0FFF`，且 THeader 里的每个
varint 在 TTHeader 里都是定长整数 —— 两者线格式不兼容。所以是新写一个 transport，
不是改现有的。

**时钟纪律写进了工具而不是文档。** 两台机器实测差 16.34 秒，靠人自觉"不跨机相减"不现实。
`merge` 强制：同机内才允许相减，跨机只能用差值法导出往返，并提供 `--inject-skew`
证明分析对偏斜免疫（注入任意偏移后各段时长逐位不变）。

**未采样路径必须近乎零开销**，否则打点本身会主导压测结果。实测 1% 采样时开销低于
3% 的噪声下限；100% 采样有 6.6% 开销。

**整套插桩的下界是 socket 系统调用** —— 网卡、驱动、中断、协议栈全在盲区。
平时无所谓，但要评测网卡时这恰好是唯一该看的部分，补法见
[probe-coverage-audit.md](docs/probe-coverage-audit.md) §5。
