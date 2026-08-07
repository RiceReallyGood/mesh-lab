# 2026-08-07 归因阶梯 —— merge 原始输出

[`../../test-report.md`](../../test-report.md) 里每个数字都能在这里回查到出处。
这些是 `demo/bin/merge` 的**未经加工的输出**，直接 `cat` 即可。

## 实验参数

三级拓扑**全部跨机**（client 在 suzhou950，server 在 suzhou920B），
每级只多一个 sidecar，跨机 TCP 那一跳始终存在：

```
direct   client(950) ────────────────TCP:15006──────────────▶ server(920B)
single   client(950) ─UDS─▶ envoy-out(950) ──TCP:15006──────▶ server(920B)
two      client(950) ─UDS─▶ envoy-out(950) ──TCP:15006──▶ envoy-in(920B) ─UDS─▶ server
```

| | 值 |
|---|---|
| 轮次 | 3 轮，**按 r1(direct→single→two) → r2 → r3 交错**，三轮合并分析 |
| 每轮时长 | 20 秒 |
| payload | 64 B |
| `ENVOY_CONCURRENCY` | **4**（三级取同值，否则线程规模差异会混进级差） |
| `lo` 负载 | `-c 1  -sample 0.05` —— 无排队，用于归因 |
| `hi` 负载 | `-c 16 -sample 0.01` —— 有排队，用于观察排队点位 |
| 失败数 | 18 组全部 **0** |

## 文件清单

| 文件 | 是什么 | 怎么读 |
|---|---|---|
| `{direct,single,two}-lo-summary.txt` | **粗略分段** —— 只有各节点总时长与分位数 | 先看这个，建立量级感 |
| `{direct,single,two}-lo-detail.txt` | **详细分段** —— 每个节点拆到十几个阶段，含 8 个新点位 | 主力。「等待对端」「等待上游」在这里被拆开 |
| `{direct,single,two}-lo-waterfall.txt` | **单请求瀑布** —— 2 条 trace 的逐点时刻 | 排查个案；看时间断层落在哪两个点之间 |
| `{direct,single,two}-hi-*.txt` | 同上三种，`-c 16` | 看排队点位在有负载时的读数 |
| `ctrl-samehost-two-lo-detail.txt` | **对照实验**：两个 Envoy 都放在 950 | 判定「入向更贵」是路径还是机器 |
| `clock-skew-check.txt` | 注入 +50 s 人工偏移前后的输出比对 | 证明分析对时钟偏斜免疫 |
| `*-table-sample.csv` | 逐条 CSV 的表头 + 4 行聚合 + 前 25 条 | 看列名与数据形状 |

每个 `.txt` 开头的 `#` 注释行记录了该组的参数和三轮各自的 client 自报值。

## 建议的阅读顺序

**第一次看，就按这个顺序：**

1. `direct-lo-summary.txt` —— 没有 mesh 时是什么样，只有两个节点
2. `two-lo-summary.txt` —— 完整 mesh，四个节点，对比总时长
3. `two-lo-detail.txt` —— 展开看时间具体花在哪；重点看这两行下面的缩进块：
   - `★等待对端(纯网络)` → 被拆成 *纯等待 / poller 事件排队 / readv / LinkBuffer 入队 / **goroutine 调度延迟***
   - `★等待上游` → 被拆成 *纯等待 / **事件循环排队** / readv / buffer+filter 派发*
4. `two-lo-waterfall.txt` —— 一条真实请求长什么样
5. `two-hi-detail.txt` —— 对比第 3 步，看 `事件循环排队` 的 p90/p99 怎么从几百纳秒涨到 15~24 µs

## 读数据的三条纪律

**① 分位数不可加。** 各阶段 p50 之和 ≠ 总时长 p50。这些表只能判断**量级与占比**，
不能做加减。差几个 µs 属正常。

**② 跨机只能得往返。** `envoy-out − envoy-in` 那一项是往返总和，拆单向需要
PTP + 硬件时间戳网卡，是物理限制不是插桩能力问题。

**③ 低负载下「事件循环排队」只有一两百纳秒是正确读数，不是探针坏了。**
`-c 1` 时压根没有并发连接可排队。要看它得读 `-hi` 那组的 p90/p99。

## 全量 CSV 不在这里

`table` 格式的全量输出每组 4~9 MB，未入库，留在 suzhou950 的
`~/ladder/<topo>-<load>-table-full.csv`。一行一条 trace、一列一个时间段（单位 µs），
开头四行是全量样本的聚合值：

```python
import pandas as pd
df  = pd.read_csv("two-lo-table-full.csv", comment="#")
agg = df[ df.trace_id.str.startswith("__")]   # __avg__ / __p50__ / __p90__ / __p99__
raw = df[~df.trace_id.str.startswith("__")]   # 逐条数据
```

`detail` 只给分位数，看不到单条请求内部各段的相关性 ——
比如「尾延迟那些请求到底卡在排队还是网络」。`table` 保留逐条数据就是为了回答这类问题。
