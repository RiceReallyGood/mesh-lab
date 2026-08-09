#!/bin/bash
# 直连基线：client --UDS--> server，中间不经任何 Envoy。
#
#   client ──UDS(app.sock)──▶ kitex-server
#
# 用法：./run-direct.sh [start|stop|status]
#
# 这是归因实验的**基线组**：单跳/双跳的数字减去这一组，才是「sidecar 的净代价」。
# 没有它的话，「双跳比直连贵多少」只能靠猜，因为 Kitex 自身的编解码、
# netpoll 的读写路径、goroutine 调度这些成本在三种拓扑里都存在。
#
# 与 run-single-hop.sh 的差别只有一条：不起 Envoy。其余（tmux 托管、ulimit、
# 用 ss 而不是文件存在性判断监听）保持一致，便于三组横向对比时排除脚本差异。
set -uo pipefail

ROOT="$HOME/envoy_kitex"
DEMO="$ROOT/mesh-lab/demo"
RUN=/tmp/kitex-demo
SESSION=meshdirect

# 与其它拓扑保持一致：Envoy 需要它，Go 侧其实用不到这么多，
# 但三组必须同参数，否则 fd 分配路径不同会引入不可比的差异。
ULIMIT_CMD="ulimit -n 65536"

start() {
  mkdir -p "$RUN"
  stop >/dev/null 2>&1
  # .ndjson* 而非 .ndjson：与其它脚本一致（Envoy 按线程分文件带 .<tid> 后缀）。
  # 直连没有 Envoy，但保持同样的清理规则，避免上一次跑双跳的残留文件污染本次分析。
  rm -f "$RUN"/*.sock "$RUN"/*.ndjson* "$RUN"/*.log

  tmux new-session -d -s "$SESSION" 2>/dev/null || true

  echo "[1/1] kitex-server (UDS $RUN/app.sock)"
  tmux new-window -t "$SESSION" -n server \
    "$ULIMIT_CMD; KITEX_PROBE_HOST=${KITEX_PROBE_HOST:-suzhou950} $DEMO/bin/server -addr $RUN/app.sock -trace $RUN/trace-server.ndjson 2>&1 | tee $RUN/server.log"
  sleep 2

  echo "[检查]"
  status
}

stop() {
  # 三步顺序都要紧，少一步就会**静默丢数据**（2026-08-09 实测定位）：
  #
  # 打点落盘由 probe/tracer.go 的 `bufio(1MB) + 每秒 Ticker` 驱动。
  # 300 请求的验证轮 18 毫秒就跑完、只产生约 380 KB —— 两个刷盘条件一个都没触发，
  # 事件全在缓冲里。此时：
  #   · 先 tmux kill-session → SIGHUP（server 不处理它）→ 进程当场死 → 实测落盘 0 条
  #   · 直接 SIGTERM 也不保险 → tr.Close() 与退出竞争 → 实测只剩 346 / 1087 条
  #   · 先等 2 秒让 Ticker 触发一次 → 实测 6300/6300 条，丢弃=0 ✅
  #
  # 大压测感觉不到这个问题，因为 1MB 缓冲被反复写满、一直在落盘。
  sleep 2                                    # ① 让每秒 Ticker 至少触发一次
  pkill -u "$USER" -x server 2>/dev/null     # ② SIGTERM，走优雅退出 + tr.Close()
  sleep 2
  tmux kill-session -t "$SESSION" 2>/dev/null # ③ 最后才拆 tmux
  sleep 1
  echo "已停止"
  # 完整性判据：server.log 里应有 `[probe] ... 丢弃=0`。**没有这一行就说明 Close() 没跑完，数据不可信。**
}

status() {
  local ok=0 uds
  # 先收进变量再 grep：本脚本开了 set -o pipefail，`ss | grep -q` 会因
  # grep 提前退出让 ss 吃 SIGPIPE（rc=141）而误报未监听。
  # 详见 run-two-hop.sh 里 status() 的注释。
  uds=$(ss -xln 2>/dev/null)
  if grep -qF -- "$RUN/app.sock" <<<"$uds"; then
    echo "  app.sock : 正在监听"
  else
    echo "  app.sock : 未监听"; ok=1
  fi
  if [ $ok -ne 0 ]; then
    echo "--- server.log 尾 ---"; tail -5 "$RUN/server.log" 2>/dev/null | cut -c1-150
  fi
  return $ok
}

case "${1:-start}" in
  start)  start ;;
  stop)   stop ;;
  status) status ;;
  *) echo "用法: $0 [start|stop|status]"; exit 1 ;;
esac
