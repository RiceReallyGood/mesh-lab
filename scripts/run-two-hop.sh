#!/bin/bash
# 双跳链路（单机降级模式）：
#   client --UDS--> envoy-out --TCP:15006--> envoy-in --UDS--> server
#
# 用法：./run-two-hop.sh [start|stop|status]
set -uo pipefail

ROOT="$HOME/envoy_kitex"
ENVOY="$ROOT/envoy/bazel-bin/source/exe/envoy-static"
DEMO="$ROOT/mesh-lab/demo"
CONF="$ROOT/mesh-lab/envoy-conf"
RUN=/tmp/kitex-demo
SESSION=meshlab2

ULIMIT_CMD="ulimit -n 65536"

# 两个 Envoy 必须用不同 base-id。同 base-id 时，后启动的会通过共享域套接字
# 通知先启动的退出（热重启机制），表现为「起了第二个，第一个就死了」。
BASE_OUT=11
BASE_IN=12

start() {
  mkdir -p "$RUN"
  stop >/dev/null 2>&1
  rm -f "$RUN"/*.sock "$RUN"/*.ndjson "$RUN"/*.log

  tmux new-session -d -s "$SESSION" 2>/dev/null || true

  echo "[1/3] kitex-server (UDS $RUN/app.sock)"
  tmux new-window -t "$SESSION" -n server \
    "$ULIMIT_CMD; $DEMO/bin/server -addr $RUN/app.sock -trace $RUN/trace-server.ndjson 2>&1 | tee $RUN/server.log"
  sleep 2

  echo "[2/3] envoy-in (TCP 127.0.0.1:15006, base-id=$BASE_IN)"
  tmux new-window -t "$SESSION" -n envoy-in \
    "$ULIMIT_CMD; $ENVOY -c $CONF/two-hop-in.yaml --base-id $BASE_IN --log-level info 2>&1 | tee $RUN/envoy-in.log"
  sleep 3

  echo "[3/3] envoy-out (UDS $RUN/out.sock, base-id=$BASE_OUT)"
  tmux new-window -t "$SESSION" -n envoy-out \
    "$ULIMIT_CMD; $ENVOY -c $CONF/two-hop-out.yaml --base-id $BASE_OUT --log-level info 2>&1 | tee $RUN/envoy-out.log"
  sleep 3

  status
}

stop() {
  tmux kill-session -t "$SESSION" 2>/dev/null
  pkill -u "$USER" -x envoy-static 2>/dev/null
  pkill -u "$USER" -x server 2>/dev/null
  sleep 1
  echo "已停止"
}

status() {
  local ok=0
  # socket 文件存在不等于有人 listen，必须用 ss 确认
  ss -xln 2>/dev/null | grep -q "$RUN/app.sock" && echo "  app.sock      : 监听中" || { echo "  app.sock      : 未监听"; ok=1; }
  ss -tln 2>/dev/null | grep -q ":15006"        && echo "  tcp :15006    : 监听中" || { echo "  tcp :15006    : 未监听"; ok=1; }
  ss -xln 2>/dev/null | grep -q "$RUN/out.sock" && echo "  out.sock      : 监听中" || { echo "  out.sock      : 未监听"; ok=1; }
  local n
  n=$(pgrep -u "$USER" -xc envoy-static 2>/dev/null || echo 0)
  echo "  envoy 进程数  : $n (应为 2)"
  [ "$n" != "2" ] && ok=1
  if [ $ok -ne 0 ]; then
    for f in envoy-out envoy-in; do
      echo "--- $f.log 尾 ---"; tail -4 "$RUN/$f.log" 2>/dev/null | cut -c1-150
    done
  fi
  return $ok
}

case "${1:-start}" in
  start)  start ;;
  stop)   stop ;;
  status) status ;;
  *) echo "用法: $0 [start|stop|status]"; exit 1 ;;
esac
