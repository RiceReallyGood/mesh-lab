#!/bin/bash
# 阶段6 压测:QPS 扫描 + 打点开销四组对照
#
# 用法：
#   ./bench.sh sweep      # 第一步：逐级加压找饱和点（trace 关闭）
#   ./bench.sh matrix     # 第二步：四组对照，量化打点开销
#
# 方法论前提（设计文档 §8.5）：
#   归因测量必须在非饱和区进行。饱和后排队延迟主导端到端时间，
#   waterfall 上「时间花在哪」会退化成「在队列里等」，对归因毫无价值。
#   所以先 sweep 找拐点，再在 ~50% 处做归因。
set -uo pipefail

ROOT="$HOME/envoy_kitex"
DEMO="$ROOT/mesh-lab/demo"
RUN=/tmp/kitex-demo
XM="$ROOT/mesh-lab/scripts/run-cross-machine.sh"

WARMUP=3s
DURATION=10s
SIZE=64

# 每组之间等一会儿，让上一轮的连接与内核缓冲状态散掉，
# 否则前一组的残留会污染后一组的冷启动。
SETTLE=3

run_once() {   # $1=并发 $2=采样率 $3=标签
  local c=$1 rate=$2 label=$3
  # 预热：跳过首次建连等冷启动成本（单请求 waterfall 里见过 2.3ms 的建连）
  KITEX_PROBE_HOST=suzhou950 "$DEMO/bin/client" -target "$RUN/out.sock" -service echo-server \
      -c "$c" -d "$WARMUP" -size "$SIZE" -sample 0 \
      -trace /dev/null >/dev/null 2>&1

  local out
  out=$(KITEX_PROBE_HOST=suzhou950 "$DEMO/bin/client" -target "$RUN/out.sock" -service echo-server \
      -c "$c" -d "$DURATION" -size "$SIZE" -sample "$rate" \
      -trace "$RUN/trace-client.ndjson" 2>&1)
  local qps p50 p99 fail
  qps=$(echo "$out"  | grep -oP 'QPS=\K[0-9.]+')
  fail=$(echo "$out" | grep -oP '失败=\K[0-9]+')
  p50=$(echo "$out"  | grep -oP 'p50=\K[^ ]+')
  p99=$(echo "$out"  | grep -oP 'p99=\K[^ ]+')
  printf "%-22s c=%-4s 采样=%-5s QPS=%-9s p50=%-10s p99=%-10s 失败=%s\n" \
         "$label" "$c" "$rate" "$qps" "$p50" "$p99" "$fail"
}

sweep() {
  echo "=== QPS 扫描（trace 关闭，仅测吞吐与延迟拐点）==="
  echo "目的：找到饱和点，归因测量要在 ~50% 处做（§8.5）"
  echo
  for c in 1 2 4 8 16 32 64 128; do
    run_once "$c" 0 "sweep"
    sleep "$SETTLE"
  done
  echo
  echo "读法：QPS 不再随并发线性增长、且 p99 开始非线性上升处，即为拐点。"
}

# run_qps 只回显 QPS 数字，供多次重复后做统计
run_qps() {   # $1=并发 $2=采样率
  local c=$1 rate=$2
  KITEX_PROBE_HOST=suzhou950 "$DEMO/bin/client" -target "$RUN/out.sock" -service echo-server \
      -c "$c" -d "$WARMUP" -size "$SIZE" -sample 0 -trace /dev/null >/dev/null 2>&1
  KITEX_PROBE_HOST=suzhou950 "$DEMO/bin/client" -target "$RUN/out.sock" -service echo-server \
      -c "$c" -d "$DURATION" -size "$SIZE" -sample "$rate" \
      -trace "$RUN/trace-client.ndjson" 2>&1 | grep -oP 'QPS=\K[0-9.]+'
}

# 多次重复后报告 中位数/最小/最大，并给出相对基线的变化。
#
# 为什么必须重复：单次运行的噪声可以达到 5%，而「插桩存在但不采样」
# 的固定成本很可能小于这个量级。首轮实测中 A 组(探针开、采样0)
# 甚至比基线快 4.9% —— 加插桩不可能变快，那只说明噪声吞掉了效应量。
# 不做重复就下结论，等于把噪声当数据。
REPEAT=${REPEAT:-5}

stat_of() {  # stdin: 一行一个数
  sort -n | awk -v n="$REPEAT" '
    {v[NR]=$1}
    END{
      if(NR==0){print "NA NA NA"; exit}
      mid=(NR%2)?v[(NR+1)/2]:(v[NR/2]+v[NR/2+1])/2
      printf "%.0f %.0f %.0f", mid, v[1], v[NR]
    }'
}

matrix() {
  local c=${1:-16}
  echo "=== 打点开销四组对照（并发 c=$c，每组重复 $REPEAT 次）==="
  echo "基线→A 隔离「插桩存在但不采样」的固定成本；"
  echo "A→B 是实际归因配置的成本；B→C 显示采样率的影响。"
  echo

  local base_mid=""
  for grp in "基线|1|0" "A_探针开_采样0|0|0" "B_探针开_采样1%|0|0.01" "C_探针开_采样100%|0|1.0"; do
    local label=${grp%%|*}; local rest=${grp#*|}
    local disable=${rest%%|*}; local rate=${rest#*|}

    if [ "$disable" = "1" ]; then
      KITEX_PROBE_DISABLE=1 "$XM" start >/dev/null 2>&1
    else
      "$XM" start >/dev/null 2>&1
    fi
    sleep 3

    local samples=""
    for _ in $(seq 1 "$REPEAT"); do
      samples="$samples$(run_qps "$c" "$rate")"$'\n'
      sleep "$SETTLE"
    done
    read -r mid lo hi <<< "$(echo "$samples" | grep -v '^$' | stat_of)"
    [ -z "$base_mid" ] && base_mid=$mid
    local delta
    delta=$(awk -v m="$mid" -v b="$base_mid" 'BEGIN{printf "%+.1f%%", (m-b)/b*100}')
    printf "%-20s QPS 中位=%-8s 最小=%-8s 最大=%-8s 相对基线=%s\n" \
           "$label" "$mid" "$lo" "$hi" "$delta"
  done
  echo
  echo "判读：若某组的 [最小,最大] 区间与基线的区间重叠，"
  echo "      则其差异落在噪声内，不能宣称存在开销。"
}

case "${1:-sweep}" in
  sweep)  sweep ;;
  matrix) matrix "${2:-16}" ;;
  *) echo "用法: $0 [sweep|matrix [并发数]]"; exit 1 ;;
esac
