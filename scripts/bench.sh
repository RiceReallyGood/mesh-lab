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

# 四组对照，**按轮次交错**执行。
#
# 为什么必须交错：把一组跑完再跑下一组（基线→A→B→C）时，
# 任何随时间推进的系统性变化 —— 连接池预热、页缓存、CPU 频率、
# 内核缓冲区状态 —— 都会系统性地偏袒靠后的组，这是顺序偏倚而非随机噪声。
# 实测中曾出现「探针组比基线快 4.8%」这种物理上不可能的结果，
# 正是顺序偏倚的表现。
#
# 交错后每组均匀分布在整个测试时段上，漂移被摊平到所有组。
matrix() {
  local c=${1:-16}
  echo "=== 打点开销四组对照（并发 c=$c，$REPEAT 轮交错）==="
  echo "每轮内依次跑四组，共 $REPEAT 轮；交错以消除顺序偏倚。"
  echo

  local groups=("基线|1|0" "A_探针开_采样0|0|0" "B_探针开_采样1%|0|0.01" "C_探针开_采样100%|0|1.0")
  declare -A samples

  for round in $(seq 1 "$REPEAT"); do
    printf "  轮 %s/%s: " "$round" "$REPEAT"
    for grp in "${groups[@]}"; do
      local label=${grp%%|*}; local rest=${grp#*|}
      local disable=${rest%%|*}; local rate=${rest#*|}

      # 每组开跑前重启，保证探针开关状态正确；
      # 重启带来的进程实例差异也因交错而被摊平。
      if [ "$disable" = "1" ]; then
        KITEX_PROBE_DISABLE=1 "$XM" start >/dev/null 2>&1
      else
        "$XM" start >/dev/null 2>&1
      fi
      sleep 2
      local q
      q=$(run_qps "$c" "$rate")
      samples[$label]="${samples[$label]:-}$q"$'\n'
      printf "%s " "${label%%_*}"
      sleep "$SETTLE"
    done
    echo
  done

  echo
  local base_mid=""
  local base_lo="" base_hi=""
  for grp in "${groups[@]}"; do
    local label=${grp%%|*}
    read -r mid lo hi <<< "$(echo "${samples[$label]}" | grep -v '^$' | stat_of)"
    if [ -z "$base_mid" ]; then
      base_mid=$mid; base_lo=$lo; base_hi=$hi
    fi
    local delta overlap
    delta=$(awk -v m="$mid" -v b="$base_mid" 'BEGIN{printf "%+.1f%%", (m-b)/b*100}')
    # 区间与基线重叠即判为「落在噪声内」
    overlap=$(awk -v l="$lo" -v h="$hi" -v bl="$base_lo" -v bh="$base_hi" \
      'BEGIN{print (l<=bh && bl<=h) ? "区间与基线重叠→落在噪声内" : "区间分离→效应可辨"}')
    printf "%-20s 中位=%-8s [%s, %s]  %-7s %s\n" "$label" "$mid" "$lo" "$hi" "$delta" "$overlap"
  done
}

case "${1:-sweep}" in
  sweep)  sweep ;;
  matrix) matrix "${2:-16}" ;;
  *) echo "用法: $0 [sweep|matrix [并发数]]"; exit 1 ;;
esac
