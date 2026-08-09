#!/bin/bash
# 阶段 2:拉起单跳链路 client --UDS--> envoy --UDS--> server
#
# 用法：./run-single-hop.sh [start|stop|status]
#
# 为什么用 tmux 而不是 nohup setsid：
#   Envoy 注册了 SIGTERM 处理器，ssh 会话结束时它会收到 SIGTERM 并优雅退出
#   （日志里是 "caught ENVOY_SIGTERM"）。Go 进程和 xray 在同样条件下能存活，
#   但 Envoy 不行。tmux 把进程挂在一个长驻的 session 下，彻底脱离 ssh 生命周期。
set -uo pipefail

ROOT="$HOME/envoy_kitex"
ENVOY="$ROOT/envoy/bazel-bin/source/exe/envoy-static"
DEMO="$ROOT/mesh-lab/demo"
CONF="$ROOT/mesh-lab/envoy-conf/single-hop.yaml"
RUN=/tmp/kitex-demo
SESSION=meshlab

# Envoy 默认 fd 需求远超 1024，不设会在启动时报 "Too many open files"，
# 而且只是 warn —— 进程看似起来了实则不可用。硬限 524288，无需 sudo。
ULIMIT_CMD="ulimit -n 65536"

# Envoy 的热重启机制：同 base-id 的新实例启动时会通过共享域套接字
# 通知旧实例退出。同机跑多个 Envoy（双跳降级模式）必须给不同 base-id，
# 否则它们会互相杀死。
ENVOY_BASE_ID=${ENVOY_BASE_ID:-7}

start() {
  mkdir -p "$RUN"
  stop >/dev/null 2>&1
  rm -f "$RUN"/*.sock "$RUN"/*.ndjson* "$RUN"/*.log   # .ndjson* 而非 .ndjson：Envoy 探针按线程分文件，实际名字是 trace-xxx.ndjson.<tid>

  tmux new-session -d -s "$SESSION" 2>/dev/null || true

  echo "[1/3] kitex-server (UDS $RUN/app.sock)"
  tmux new-window -t "$SESSION" -n server \
    "$ULIMIT_CMD; $DEMO/bin/server -addr $RUN/app.sock -trace $RUN/trace-server.ndjson 2>&1 | tee $RUN/server.log"
  sleep 2

  echo "[2/3] envoy (监听 $RUN/out.sock, base-id=$ENVOY_BASE_ID)"
  # 探针靠环境变量初始化，不设就完全不落盘 —— 而且不报错，
  # 表现为「Envoy 一个点位都没有」，很容易误判成打点代码坏了。
  # 双跳的两个脚本一直有这段，单跳漏了，2026-08-07 补上。
  tmux new-window -t "$SESSION" -n envoy \
    "$ULIMIT_CMD; KITEX_PROBE_HOST=${KITEX_PROBE_HOST:-suzhou950} KITEX_PROBE_PATH=$RUN/trace-envoy.ndjson KITEX_PROBE_NODE=envoy $ENVOY -c $CONF --log-level info --base-id $ENVOY_BASE_ID 2>&1 | tee $RUN/envoy.log"
  sleep 4

  echo "[3/3] 检查"
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
  local ok=0 uds
  # 只看进程在不在是不够的 —— socket 文件存在也不代表有人 listen。
  # 用 ss -xln 确认真的在监听。
  #
  # 先收进变量再 grep，不要写成 `ss ... | grep -q ...`：本脚本开了
  # set -o pipefail，grep -q 命中即退出会让 ss 吃 SIGPIPE（rc=141），
  # pipefail 把 141 当成整条管道的退出码，于是在监听也报「未监听」。
  # 详见 run-two-hop.sh 里 status() 的注释。
  uds=$(ss -xln 2>/dev/null)
  for s in app.sock out.sock; do
    if grep -qF -- "$RUN/$s" <<<"$uds"; then
      echo "  $s : 正在监听"
    else
      echo "  $s : 未监听"; ok=1
    fi
  done
  if [ $ok -ne 0 ]; then
    echo "--- envoy.log 尾 ---"; tail -5 "$RUN/envoy.log" 2>/dev/null | cut -c1-150
    echo "--- server.log 尾 ---"; tail -3 "$RUN/server.log" 2>/dev/null | cut -c1-150
  fi
  return $ok
}

case "${1:-start}" in
  start)  start ;;
  stop)   stop ;;
  status) status ;;
  *) echo "用法: $0 [start|stop|status]"; exit 1 ;;
esac
