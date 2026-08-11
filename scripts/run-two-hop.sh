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

# base-id 曾经写死，但在**共享开发机**上会撞别的用户：base-id N 对应
# /dev/shm/envoy_shared_memory_<N*10>，该文件 0600 且属于创建者。
# 2026-08-11 实测 _110/_120 被 root 占着，Envoy 直接 abort：
#   panic: cannot open shared memory region /envoy_shared_memory_110
# 改用 --use-dynamic-base-id：每个实例自己挑没被占的，既不撞别人也不互踢。
# 本脚本从不按 base-id 找进程（停止走 pkill -x envoy-static），丢掉已知值无代价。
BASE_FLAG="--use-dynamic-base-id"

start() {
  mkdir -p "$RUN"
  stop >/dev/null 2>&1
  rm -f "$RUN"/*.sock "$RUN"/*.ndjson* "$RUN"/*.log   # .ndjson* 而非 .ndjson：Envoy 探针按线程分文件，实际名字是 trace-xxx.ndjson.<tid>

  tmux new-session -d -s "$SESSION" 2>/dev/null || true

  echo "[1/3] kitex-server (UDS $RUN/app.sock)"
  tmux new-window -t "$SESSION" -n server \
    "$ULIMIT_CMD; $DEMO/bin/server -addr $RUN/app.sock -trace $RUN/trace-server.ndjson 2>&1 | tee $RUN/server.log"
  sleep 2

  echo "[2/3] envoy-in (TCP 127.0.0.1:15006)"
  # 探针输出路径经环境变量传入（不扩展 bootstrap schema，见 probe.cc 注释）。
  # 不设这两个变量即为「无探针」模式，用于 §8.6 的对照组。
  tmux new-window -t "$SESSION" -n envoy-in \
    "$ULIMIT_CMD; KITEX_PROBE_PATH=$RUN/trace-envoy-in.ndjson KITEX_PROBE_NODE=envoy-in $ENVOY -c $CONF/two-hop-in.yaml $BASE_FLAG --log-level info 2>&1 | tee $RUN/envoy-in.log"
  sleep 3

  echo "[3/3] envoy-out (UDS $RUN/out.sock)"
  tmux new-window -t "$SESSION" -n envoy-out \
    "$ULIMIT_CMD; KITEX_PROBE_PATH=$RUN/trace-envoy-out.ndjson KITEX_PROBE_NODE=envoy-out $ENVOY -c $CONF/two-hop-out.yaml $BASE_FLAG --log-level info 2>&1 | tee $RUN/envoy-out.log"
  sleep 3

  status
}

stop() {
  # 先等 Ticker 刷盘 → 再 SIGTERM → 最后拆 tmux。三步顺序都要紧，
  # 少一步会让小批量验证轮的打点数据静默丢失，详见 run-direct.sh 的同名函数注释。
  sleep 2
  pkill -u "$USER" -x envoy-static 2>/dev/null
  pkill -u "$USER" -x server 2>/dev/null
  sleep 2
  tmux kill-session -t "$SESSION" 2>/dev/null
  sleep 1
  echo "已停止"
}

status() {
  local ok=0 uds tcp
  # socket 文件存在不等于有人 listen，必须用 ss 确认。
  #
  # 先把 ss 的输出收进变量，再用 here-string 交给 grep —— 不要写成
  # `ss ... | grep -q ...`。本脚本开了 set -o pipefail，而 grep -q 命中第一行
  # 就退出，ss 还在往管道里写就会吃到 SIGPIPE 死掉（rc=141），
  # pipefail 取管道里最后一个非零码，于是整条管道返回 141，
  # 明明在监听却被判成「未监听」。
  # Envoy 的每个 worker 各自 SO_REUSEPORT bind 同一端口，384 worker 就是 384 行，
  # 必然触发；而 worker 数少（如 ENVOY_CONCURRENCY=2）时 ss 写完就退了，碰不到。
  # 这也是它藏了很久才被发现的原因。
  uds=$(ss -xln 2>/dev/null)
  tcp=$(ss -tln 2>/dev/null)
  grep -qF -- "$RUN/app.sock" <<<"$uds" && echo "  app.sock      : 监听中" || { echo "  app.sock      : 未监听"; ok=1; }
  grep -qF -- ":15006"        <<<"$tcp" && echo "  tcp :15006    : 监听中" || { echo "  tcp :15006    : 未监听"; ok=1; }
  grep -qF -- "$RUN/out.sock" <<<"$uds" && echo "  out.sock      : 监听中" || { echo "  out.sock      : 未监听"; ok=1; }
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
