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
  tmux kill-session -t "$SESSION" 2>/dev/null
  pkill -u "$USER" -x server 2>/dev/null
  sleep 1
  echo "已停止"
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
