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

一条 40 µs 的 echo 请求端到端要 206 µs，多出来的 166 µs 在哪？实测拆解：

- **两跳 sidecar 自身处理只占 20.1%**，进程间传输占 65.3%（是前者的 3.2 倍）
- 业务 handler 只有 **240 ns** —— 测的几乎全是框架与传输开销
- Envoy 的 readv 系统调用 3.1 µs、Go 侧 goroutine 调度延迟 2.5 µs（p99 21 µs）
  —— 这些此前都被笼统算作"等待对端"

完整结论见 [docs/test-report.md](docs/test-report.md)。

## 从哪读起

| 你想 | 看 |
|---|---|
| **动手跑起来** | [docs/runbook-operations.md](docs/runbook-operations.md) —— 构建→运行→采集→分析 |
| 搭环境（镜像、代理、bazel 取舍） | [docs/runbook-build-environment.md](docs/runbook-build-environment.md) |
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
envoy-conf/   五份 Envoy 配置：单跳、双跳（同机/跨机）
scripts/      拓扑拉起、压测、构建环境辅助
docs/         见上表
```

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
