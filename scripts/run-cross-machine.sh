#!/bin/bash
# 真实跨机双跳链路（在 suzhou950 上执行，自动操作 suzhou920B）：
#
#   suzhou950                              suzhou920B
#   ┌─────────────────────────┐            ┌──────────────────────────┐
#   │ kitex-client            │            │  envoy-in ──UDS──▶ server│
#   │      │ UDS              │            │      ▲                   │
#   │      ▼                  │  真实网络   │      │                   │
#   │ envoy-out ──────────────┼───TCP──────┼──────┘                   │
#   └─────────────────────────┘  :15006    └──────────────────────────┘
#
# 用法：./run-cross-machine.sh [start|stop|status|collect]
#
# 两端必须显式设置 KITEX_PROBE_HOST：本实验两台机器的 hostname 都是
# localhost.localdomain，靠 gethostname 会把跨机误判成同机，
# 从而让「禁止跨机相减」这条纪律形同虚设（§8.2）。
#
# collect 会把 920B 上的 trace 拉回 950 —— 走两机间的局域网（实测 112MB/s），
# 不经开发机（那条路只有 ~150KB/s）。
set -uo pipefail

PEER=suzhou920B
PEER_IP=192.168.25.51

LOCAL_ROOT="$HOME/envoy_kitex"
ENVOY="$LOCAL_ROOT/envoy/bazel-bin/source/exe/envoy-static"
DEMO="$LOCAL_ROOT/mesh-lab/demo"
CONF="$LOCAL_ROOT/mesh-lab/envoy-conf"
RUN=/tmp/kitex-demo

PEER_HOME='$HOME/meshlab'
PEER_RUN=/tmp/kitex-demo

SSH="ssh -o ConnectTimeout=10 -o BatchMode=yes"
ULIMIT_CMD="ulimit -n 65536"
SESSION=meshxm

# 两个 Envoy 在不同机器上，base-id 冲突本不会发生，
# 但保持与单机模式一致的取值，便于对照排查。
BASE_OUT=11
BASE_IN=12

# KITEX_PROBE_DISABLE=1 时不给 Envoy 传探针环境变量，
# 探针代码虽在二进制里但完全不激活 —— 这是 §8.6 的基线组。
if [ "${KITEX_PROBE_DISABLE:-0}" = "1" ]; then
  PROBE_OUT=""
  PROBE_IN=""
else
  PROBE_OUT="KITEX_PROBE_HOST=suzhou950 KITEX_PROBE_PATH=$RUN/trace-envoy-out.ndjson KITEX_PROBE_NODE=envoy-out"
  PROBE_IN="KITEX_PROBE_HOST=suzhou920B KITEX_PROBE_PATH=$PEER_RUN/trace-envoy-in.ndjson KITEX_PROBE_NODE=envoy-in"
fi

sync_peer() {
  echo "[同步] 推送二进制与配置到 $PEER"
  $SSH "$PEER" "mkdir -p ~/meshlab/bin ~/meshlab/conf $PEER_RUN" 2>/dev/null
  rsync -a "$ENVOY" "$PEER:~/meshlab/bin/envoy-static"
  rsync -a "$DEMO/bin/server" "$PEER:~/meshlab/bin/server"
  rsync -a "$CONF/two-hop-in-remote.yaml" "$PEER:~/meshlab/conf/"
}

start() {
  stop >/dev/null 2>&1
  sync_peer

  echo "[$PEER] 启动 kitex-server + envoy-in"
  # 远端也用 tmux：Envoy 会响应 ssh 断开时的 SIGTERM，nohup setsid 保不住它。
  $SSH "$PEER" "
    tmux kill-session -t $SESSION 2>/dev/null
    rm -f $PEER_RUN/*.sock $PEER_RUN/*.ndjson* $PEER_RUN/*.log
    mkdir -p $PEER_RUN
    tmux new-session -d -s $SESSION
    tmux new-window -t $SESSION -n server \
      '$ULIMIT_CMD; KITEX_PROBE_HOST=suzhou920B ~/meshlab/bin/server -addr $PEER_RUN/app.sock -trace $PEER_RUN/trace-server.ndjson 2>&1 | tee $PEER_RUN/server.log'
    sleep 2
    tmux new-window -t $SESSION -n envoy-in \
      '$ULIMIT_CMD; $PROBE_IN ~/meshlab/bin/envoy-static -c ~/meshlab/conf/two-hop-in-remote.yaml --base-id $BASE_IN --log-level info 2>&1 | tee $PEER_RUN/envoy-in.log'
  " 2>/dev/null
  sleep 4

  echo "[本机] 启动 envoy-out"
  mkdir -p "$RUN"
  rm -f "$RUN"/*.sock "$RUN"/*.ndjson* "$RUN"/*.log
  tmux kill-session -t "$SESSION" 2>/dev/null
  tmux new-session -d -s "$SESSION"
  tmux new-window -t "$SESSION" -n envoy-out \
    "$ULIMIT_CMD; $PROBE_OUT $ENVOY -c $CONF/two-hop-out-remote.yaml --base-id $BASE_OUT --log-level info 2>&1 | tee $RUN/envoy-out.log"
  sleep 3

  status
}

stop() {
  tmux kill-session -t "$SESSION" 2>/dev/null
  pkill -u "$USER" -x envoy-static 2>/dev/null
  $SSH "$PEER" "tmux kill-session -t $SESSION 2>/dev/null; pkill -u \$USER -x envoy-static 2>/dev/null; pkill -u \$USER -x server 2>/dev/null" 2>/dev/null
  sleep 1
  echo "已停止两端"
}

status() {
  local ok=0
  ss -xln 2>/dev/null | grep -q "$RUN/out.sock" && echo "  [950] out.sock   : 监听中" || { echo "  [950] out.sock   : 未监听"; ok=1; }
  # 从本机探测对端端口，顺带验证网络连通
  timeout 3 bash -c "echo > /dev/tcp/$PEER_IP/15006" 2>/dev/null \
    && echo "  [920B] :15006    : 可达" || { echo "  [920B] :15006    : 不可达"; ok=1; }
  $SSH "$PEER" "ss -xln 2>/dev/null | grep -q $PEER_RUN/app.sock" 2>/dev/null \
    && echo "  [920B] app.sock  : 监听中" || { echo "  [920B] app.sock  : 未监听"; ok=1; }
  if [ $ok -ne 0 ]; then
    echo "--- 本机 envoy-out.log ---"; tail -4 "$RUN/envoy-out.log" 2>/dev/null | cut -c1-140
    echo "--- 920B envoy-in.log ---"; $SSH "$PEER" "tail -4 $PEER_RUN/envoy-in.log" 2>/dev/null | cut -c1-140
  fi
  return $ok
}

collect() {
  echo "[收集] 从 $PEER 拉取 trace（走局域网，不经开发机）"
  rsync -a "$PEER:$PEER_RUN/trace-server.ndjson" "$RUN/" 2>/dev/null
  rsync -a "$PEER:$PEER_RUN/trace-envoy-in.ndjson."* "$RUN/" 2>/dev/null
  ls -lh "$RUN"/trace-* 2>/dev/null | awk '{print "  "$5, $9}'
}

case "${1:-start}" in
  start)   start ;;
  stop)    stop ;;
  status)  status ;;
  collect) collect ;;
  *) echo "用法: $0 [start|stop|status|collect]"; exit 1 ;;
esac
