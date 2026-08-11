#!/bin/bash
# 跨机双跳归因矩阵：c ∈ {1,16} × payload ∈ {1K,4K,8K,16K,64K}
#
# 加压器是 **kitex-benchmark 自己的 runner**（见 kitex-benchmark 仓的
# thrift/meshlab.go），不是我们手写的 client —— 并发模型、限速、预热、
# 计数、分位数算法全部沿用它的实现，`git diff` 就是差异清单。
#
# 参数口径（2026-08-11 与需求方确认）：
#   qps               = c × 1000，**严格按此发压，跑不满如实记录实际值**
#   ENVOY_CONCURRENCY = c（两跳取同一个值，否则级差里混进线程规模的影响）
#   采样              = 逐格调整，目标每格约 3000 条 trace
#
# ⚠️ **两机链路是 1 GbE（实测 ethtool 1000Mb/s，MTU 1500）**，每方向 125 MB/s。
# echo 场景每方向负载 = qps × payload，于是：
#
#     c=16 × 8K  → 131 MB/s（105 %）  ← 打不上去
#     c=16 × 16K → 262 MB/s（210 %）  ← 打不上去
#     c=16 × 64K → 1049 MB/s（839 %） ← 打不上去
#     c=1  × 64K → 单请求收发 128 KB，光序列化就 ≥1.05 ms，c=1 到不了 1000 qps
#
# 这几格**测的是链路带宽而不是 sidecar 开销**，归因数字无意义（项目方法论
# §8.5：归因必须在非饱和区）。脚本会把「目标 qps vs 实际 qps」一并记下来，
# 达成率低于 90 % 的格子在报告里必须标注。
set -uo pipefail

ROOT="$HOME/envoy_kitex"
BIN="$ROOT/mesh-lab/demo/bin"
XM="$ROOT/mesh-lab/scripts/run-cross-machine.sh"
RUN=${MESHLAB_RUN:-/tmp/kitex-demo-$(id -un)}
OUT=${OUT:-$RUN/matrix-$(date +%m%d-%H%M)}

# ⚠️ **控制台日志要写进 $OUT/ 里，不能写在 $RUN/ 下。**
# run-cross-machine.sh 的 start() 每格都会 `rm -f "$RUN"/*.log` 清场，
# 放在 $RUN 下的 `matrix-xxx.run.log` 会被它一并删掉 —— 2026-08-11 首跑就这么
# 丢了控制台输出（好在 $OUT 是子目录，逐格结果和 summary 都还在）。
RUNLOG="$OUT/console.log"

DURATION=${DURATION:-10}      # 计量窗口（秒）
WARMUP=${WARMUP:-3}           # 预热（秒）；预热流量不会被采样，见 meshlab.go 的 mlArmed
SIZES=${SIZES:-"1024 4096 8192 16384 65536"}
CONCS=${CONCS:-"1 16"}

# 每格的采样率，目标约 3000 条 trace。
# 大包格子发不满，请求数少，所以采样率要调高。
sample_for() {   # $1=c $2=size
  case "$1-$2" in
    1-1024)   echo 0.30 ;;  1-4096)   echo 0.30 ;;  1-8192)   echo 0.30 ;;
    1-16384)  echo 0.30 ;;  1-65536)  echo 0.40 ;;
    16-1024)  echo 0.02 ;;  16-4096)  echo 0.02 ;;  16-8192)  echo 0.025 ;;
    16-16384) echo 0.05 ;;  16-65536) echo 0.20 ;;
    *) echo 0.05 ;;
  esac
}

human() { case $1 in 1024) echo 1K;; 4096) echo 4K;; 8192) echo 8K;; 16384) echo 16K;; 65536) echo 64K;; *) echo "$1B";; esac; }

mkdir -p "$OUT"
SUMMARY="$OUT/matrix-summary.tsv"
printf "c\tpayload\t目标qps\t实际qps\t达成率\t链路占用\tp50\tp99\t请求数\t采样数\t失败\n" > "$SUMMARY"

run_cell() {   # $1=c $2=size
  local c=$1 size=$2 qps=$(( $1 * 1000 )) rate tag
  rate=$(sample_for "$c" "$size")
  tag="c${c}-$(human "$size")"
  echo
  echo "══════════════════════════════════════════════════════════════"
  echo "  ▶ $tag   qps=$qps  sample=$rate  ENVOY_CONCURRENCY=$c"
  echo "══════════════════════════════════════════════════════════════"

  # 每格重启拓扑：Envoy 探针的文件句柄是常开的，不重启就没法干净地清空上一格的
  # trace（rm 掉只是 unlink，进程照旧往那个 inode 写）。顺带每格拿到干净的
  # Envoy 连接状态，不带上一格的残留。
  ENVOY_CONCURRENCY=$c "$XM" start >"$OUT/$tag.topo.log" 2>&1
  if ! grep -q "监听中" "$OUT/$tag.topo.log"; then
    echo "  ✗ 拓扑没起来，跳过本格。详见 $OUT/$tag.topo.log"; return 1
  fi

  local target; target=$("$XM" target)
  KITEX_PROBE_HOST=suzhou950 "$BIN/bencher" \
      -addr "$target" -proto ttheader -svc echo-server \
      -trace "$RUN/trace-client.ndjson" -node kitex-client -sample "$rate" \
      -b "$size" -c "$c" -qps "$qps" -t "$DURATION" -warmup "$WARMUP" \
      >"$OUT/$tag.bench.log" 2>&1

  "$XM" stop    >>"$OUT/$tag.topo.log" 2>&1
  "$XM" collect >>"$OUT/$tag.topo.log" 2>&1

  # ── 从 bencher 输出里取实际值 ──
  local actual p50 p99 total failed sampled
  actual=$(grep -oP 'TPS: \K[0-9.]+'  "$OUT/$tag.bench.log" | tail -1)
  p99=$(grep -oP 'TP99: \K[0-9.]+[a-z]+' "$OUT/$tag.bench.log" | tail -1)
  total=$(grep -oP 'requests total: \K[0-9]+' "$OUT/$tag.bench.log" | tail -1)
  failed=$(grep -oP 'failed: \K[0-9]+'  "$OUT/$tag.bench.log" | tail -1)
  sampled=$(grep -oP '采样=\K[0-9]+'    "$OUT/$tag.bench.log" | tail -1)

  # ── merge 出归因 ──
  "$BIN/merge" -format detail    "$RUN"/trace-*.ndjson* >"$OUT/$tag.detail.txt"    2>&1
  "$BIN/merge" -format summary   "$RUN"/trace-*.ndjson* >"$OUT/$tag.summary.txt"   2>&1
  "$BIN/merge" -format waterfall -limit 2 "$RUN"/trace-*.ndjson* >"$OUT/$tag.waterfall.txt" 2>&1
  p50=$(grep -oP '端到端（client 总时长）\s+\S+\s+\K\S+' "$OUT/$tag.summary.txt" | head -1)

  # 原始数据留档（大包格子会比较大）
  mkdir -p "$OUT/$tag.raw" && cp "$RUN"/trace-*.ndjson* "$OUT/$tag.raw/" 2>/dev/null

  local pct link
  pct=$(awk -v a="${actual:-0}" -v q="$qps" 'BEGIN{printf "%.0f%%", a*100/q}')
  link=$(awk -v a="${actual:-0}" -v s="$size" 'BEGIN{printf "%.0f%%", a*s*100/125000000}')
  printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n" \
     "$c" "$(human "$size")" "$qps" "${actual:-—}" "$pct" "$link" \
     "${p50:-—}" "${p99:-—}" "${total:-—}" "${sampled:-—}" "${failed:-—}" >> "$SUMMARY"
  echo "  ✔ 实际 QPS ${actual:-?}（目标 $qps，达成 $pct，链路 $link） 采样 ${sampled:-?} 条"
}

{
echo "输出目录: $OUT"
for c in $CONCS; do
  for size in $SIZES; do
    run_cell "$c" "$size"
  done
done
} 2>&1 | tee -a "$RUNLOG"

echo
echo "══════════ 矩阵完成 ══════════"
column -t -s $'\t' "$SUMMARY"
echo
echo "全部输出在 $OUT"
