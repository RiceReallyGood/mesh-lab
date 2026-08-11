# 2026-08-11 归因矩阵 —— merge 原始输出

`demo/bin/merge` 的**未经加工的输出**，直接 `cat` 即可。
分析与结论见 [`../../bench-matrix-2026-08-11.md`](../../bench-matrix-2026-08-11.md)。

**本轮是第一次用 kitex-benchmark 自己的加压器**（改动清单见
[`../../demo-vs-kitex-benchmark.md`](../../demo-vs-kitex-benchmark.md)），
不再是自研的 `demo/client`。

## 参数

```
拓扑     跨机双跳（TOPO=two）
矩阵     c ∈ {1,16} × payload ∈ {1K,4K,8K,16K,64K}
速率     qps = c × 1000（严格按此发压，跑不满如实记录实际值）
Envoy    --concurrency = c
每格     预热 3s（不采样）+ 计量 10s
采样     逐格调整，目标每格约 3000 条；实测落在 2446~3459
失败     10 格全部 0
```

## 文件

| 文件 | 内容 |
|---|---|
| `matrix-summary.tsv` | 逐格的目标/实际 qps、达成率、链路占用、p50/p99 |
| `matrix-report.md` | **主力**：跨 10 格的对照表（端到端分解 / 占比 / 关键分段） |
| `<格>.summary.txt` | 该格的端到端分解 |
| `<格>.detail.txt` | 该格各阶段分位数 |
| `<格>.waterfall.txt` | 该格 2 条个案 |

原始 `.ndjson` 不入库（`.gitignore` 已排除），需要时按
[`../../runbook-operations.md`](../../runbook-operations.md) 重新生成。

## 一句话结论

**10 格里有 4 格没打上去**（`1×64K` 达成 60 %、`16×8K` 83 %、`16×16K` 43 %、
`16×64K` 9 %）—— 两机链路是 1 GbE，实用上限 87~91 %。那四格测的是链路或并发
上限，**归因数字不代表 sidecar 成本**。

饱和时队列**排在 Envoy 里而不是线上**：`16×64K` 链路只占 78 %，
但两个 sidecar 自身合计 7.37 ms、占端到端 69 %。

本轮还暴露了差值法的一个新限制：**单条消息大到需要多轮 socket 读写时，
`A↔B 往返` 这一列不再可靠**（64K 两格出现负值）。详见分析文档 §三。
