#!/bin/bash
# 真实跨机链路（在 suzhou950 上执行，自动操作 suzhou920B）。
#
# 支持三级「归因阶梯」，由 TOPO 环境变量选择（默认 two，向后兼容）：
#
#   TOPO=direct   client(950) ─────────────────TCP──▶ server(920B:15008)
#   TOPO=single   client(950) ─UDS─▶ envoy-out ─TCP──▶ server(920B:15008)
#   TOPO=two      client(950) ─UDS─▶ envoy-out ─TCP──▶ envoy-in(920B:15006) ─UDS─▶ server
#
# 三级都跨机，跨机 TCP 那一跳始终存在，每级只多一个 sidecar ——
# **所以级差就是「加一个 sidecar 的净代价」，不掺别的**。
#
# 为什么不在同一台机器上做这个阶梯：那样每加一跳就多一组 Envoy 进程
# （默认每个 384 个 worker 线程）挤同一批 CPU，三级的资源竞争程度根本不同，
# 级差里混着 CPU 争抢，不能归给「多一跳」。要么跨机，要么固定线程数/绑核。
#
# 用法：TOPO=single ./run-cross-machine.sh [start|stop|status|collect|target]
#   target 打印本级拓扑下 client 应该打的地址，压测脚本直接取用，不要自己拼。
#
# 两端必须显式设置 KITEX_PROBE_HOST：本实验两台机器的 hostname 都是
# localhost.localdomain，靠 gethostname 会把跨机误判成同机，
# 从而让「禁止跨机相减」这条纪律形同虚设（§8.2）。
#
# collect 会把 920B 上的 trace 拉回 950 —— 走两机间的局域网（实测 112MB/s），
# 不经开发机（那条路只有 ~150KB/s）。
set -uo pipefail

TOPO=${TOPO:-two}
case "$TOPO" in
  direct|single|two) ;;
  *) echo "TOPO 必须是 direct|single|two，当前是 '$TOPO'"; exit 1 ;;
esac

PEER=suzhou920B
PEER_IP=192.168.25.51

LOCAL_ROOT="$HOME/envoy_kitex"
ENVOY="$LOCAL_ROOT/envoy/bazel-bin/source/exe/envoy-static"

# Envoy 默认按 CPU 核数开 worker（本机 384 个）。核数远多于连接数时，
# 每个 worker 一次 epoll 只拿到零星事件，「事件循环排队」这个点位恒为 ~290ns，
# 测不出任何东西。要观察排队必须把 worker 压下来。
#
# 做阶梯对照时**务必显式设定并对 single/two 取同一个值**，否则两级的
# Envoy 线程数不同，级差里会混进线程规模的影响。
ENVOY_CONCURRENCY=${ENVOY_CONCURRENCY:-}
CONC_FLAG=""
[ -n "$ENVOY_CONCURRENCY" ] && CONC_FLAG="--concurrency $ENVOY_CONCURRENCY"
DEMO="$LOCAL_ROOT/mesh-lab/demo"
CONF="$LOCAL_ROOT/mesh-lab/envoy-conf"

# 服务端二进制。
#
# 默认用 **kitex-benchmark 派生的 reciever**（改动见 kitex-benchmark 仓的
# thrift/meshlab.go，`git diff` 即差异清单）—— 归因数字要站得住，加压/被压
# 两端都得是公认实现，自己写的那份别人无从核对。
#
# 它比旧的 demo/server 多两个 flag：-proto（默认 framed，过 Envoy 必须给
# ttheader）与 -node。设 MESHLAB_LEGACY=1 回到旧的 demo/server。
if [ "${MESHLAB_LEGACY:-0}" = "1" ]; then
  SRV_BIN="server"
  SRV_ARGS=""
else
  SRV_BIN="reciever"
  SRV_ARGS="-proto ttheader -node kitex-server"
fi

# 运行目录**按用户隔离**。
#
# 曾经两台机器都写死 /tmp/kitex-demo —— 这两台是共享开发机，2026-08-10 就撞上了：
# 另一个用户在同一时间跑同一套实验，两边的 /tmp/kitex-demo 全变成他的文件，
# 我方进程写不进去（sticky 位挡着），而 collect 照样把**对方的** 588MB trace
# 拉了回来。数据看着是「有」的，只是不是自己的 —— 这种污染没有任何报错。
#
# 用 id -un 而不是 $USER：后者在非交互 ssh 下可能为空。
RUN=${MESHLAB_RUN:-/tmp/kitex-demo-$(id -un)}
PEER_RUN=${MESHLAB_PEER_RUN:-/tmp/kitex-demo-$(id -un)}

# Envoy 的 bootstrap 里 UDS 路径是写死的，且 Envoy 不做环境变量替换，
# 所以每轮把配置按本轮的运行目录生成一份临时副本。
GEN_CONF="$RUN/conf"
gen_conf() {   # $1=源文件名 $2=目标路径前缀里用的运行目录
  mkdir -p "$GEN_CONF"
  sed "s#/tmp/kitex-demo#$2#g" "$CONF/$1" > "$GEN_CONF/$1"
}
# direct/single 下 server 直接对外提供 TCP；two 下它躲在 envoy-in 后面走 UDS。
#
# 端口刻意与 two 的 envoy-in 用同一个 15006，两个理由：
#
#   1. **920B 上 firewalld 是 active 的，且没有 root 看不了规则**。实测同时在
#      15006 和 15008 起监听，只有 15006 能从 950 连上，15008 报
#      `No route to host`（ICMP host-prohibited，firewalld REJECT 的典型症状）。
#      15006 是之前就放行的，别的端口要开得找管理员。
#   2. 三级拓扑走同一个端口，防火墙规则、路由、conntrack 行为完全一致，
#      不会有额外差异混进级差。
#
# direct/single 不跑 envoy-in，这个端口本来就是空的，不会冲突。
SRV_PORT=15006

SSH="ssh -o ConnectTimeout=10 -o BatchMode=yes"
ULIMIT_CMD="ulimit -n 65536"
SESSION=meshxm

# 每个 Envoy 实例的 base-id 必须唯一：同 base-id 的新实例启动时会通过
# 共享域套接字通知旧实例退出（热重启机制），表现为「起了新的，旧的就死了」。
#
# **但固定值在共享开发机上会撞车，而且撞的是别的用户。** base-id N 对应
# /dev/shm/envoy_shared_memory_<N*10>，那个文件是 0600 且属于创建者：
# 2026-08-11 实测 _110 与 _120 都被 root 占着（另有 wsy、l50061311 两个用户
# 各占了一批），我方 Envoy 直接 abort：
#
#   panic: cannot open shared memory region /envoy_shared_memory_110
#          check user permissions.
#
# 这与 2026-08-10 那次 /tmp/kitex-demo 撞车是同一类问题 —— 共享机上任何
# 全局命名空间都得假定别人也在用。运行目录当时按用户隔离了，base-id 漏了。
#
# 改用 --use-dynamic-base-id：Envoy 自己挑一个没被占的，彻底不需要我们协调。
# 本脚本从不按 base-id 找进程（停止走 pkill -x envoy-static），所以丢掉这个
# 已知值没有代价；反过来每个实例天然拿到独立 id，热重启互踢也不会发生。
BASE_FLAG="--use-dynamic-base-id"

# KITEX_PROBE_DISABLE=1 时不给 Envoy 传探针环境变量，
# 探针代码虽在二进制里但完全不激活 —— 这是 §8.6 的基线组。
if [ "${KITEX_PROBE_DISABLE:-0}" = "1" ]; then
  PROBE_OUT=""
  PROBE_IN=""
else
  PROBE_OUT="KITEX_PROBE_HOST=suzhou950 KITEX_PROBE_PATH=$RUN/trace-envoy-out.ndjson KITEX_PROBE_NODE=envoy-out"
  PROBE_IN="KITEX_PROBE_HOST=suzhou920B KITEX_PROBE_PATH=$PEER_RUN/trace-envoy-in.ndjson KITEX_PROBE_NODE=envoy-in"
fi

# client 该打哪里，由拓扑决定，不要在调用方重复推导。
target() {
  case "$TOPO" in
    direct) echo "$PEER_IP:$SRV_PORT" ;;
    *)      echo "$RUN/out.sock" ;;
  esac
}

sync_peer() {
  echo "[同步] 推送到 $PEER（TOPO=$TOPO）"
  $SSH "$PEER" "mkdir -p ~/meshlab/bin ~/meshlab/conf $PEER_RUN" 2>/dev/null
  rsync -a "$DEMO/bin/$SRV_BIN" "$PEER:~/meshlab/bin/$SRV_BIN"
  # 810MB 的 envoy-static 只有 two 用得上。rsync 在文件未变时几乎零开销，
  # 但 direct/single 根本不需要它，没必要每轮都比对。
  if [ "$TOPO" = "two" ]; then
    rsync -a "$ENVOY" "$PEER:~/meshlab/bin/envoy-static"
    gen_conf two-hop-in-remote.yaml "$PEER_RUN"
    rsync -a "$GEN_CONF/two-hop-in-remote.yaml" "$PEER:~/meshlab/conf/"
  fi
}

start() {
  stop >/dev/null 2>&1
  sync_peer

  # ── 对端 ──
  if [ "$TOPO" = "two" ]; then
    echo "[$PEER] 启动 kitex-server(UDS) + envoy-in"
    $SSH "$PEER" "
      tmux kill-session -t $SESSION 2>/dev/null
      mkdir -p $PEER_RUN
      rm -f $PEER_RUN/*.sock $PEER_RUN/*.ndjson* $PEER_RUN/*.log
      tmux new-session -d -s $SESSION
      tmux new-window -t $SESSION -n server \
        '$ULIMIT_CMD; KITEX_PROBE_HOST=suzhou920B ~/meshlab/bin/$SRV_BIN -addr $PEER_RUN/app.sock $SRV_ARGS -trace $PEER_RUN/trace-server.ndjson 2>&1 | tee $PEER_RUN/server.log'
      sleep 2
      tmux new-window -t $SESSION -n envoy-in \
        '$ULIMIT_CMD; $PROBE_IN ~/meshlab/bin/envoy-static -c ~/meshlab/conf/two-hop-in-remote.yaml $BASE_FLAG $CONC_FLAG --log-level info 2>&1 | tee $PEER_RUN/envoy-in.log'
    " 2>/dev/null
    sleep 4
  else
    echo "[$PEER] 启动 kitex-server（TCP :$SRV_PORT）"
    $SSH "$PEER" "
      tmux kill-session -t $SESSION 2>/dev/null
      mkdir -p $PEER_RUN
      rm -f $PEER_RUN/*.sock $PEER_RUN/*.ndjson* $PEER_RUN/*.log
      tmux new-session -d -s $SESSION
      tmux new-window -t $SESSION -n server \
        '$ULIMIT_CMD; KITEX_PROBE_HOST=suzhou920B ~/meshlab/bin/$SRV_BIN -addr :$SRV_PORT $SRV_ARGS -trace $PEER_RUN/trace-server.ndjson 2>&1 | tee $PEER_RUN/server.log'
    " 2>/dev/null
    sleep 3
  fi

  # ── 本机 ──
  mkdir -p "$RUN"
  rm -f "$RUN"/*.sock "$RUN"/*.ndjson* "$RUN"/*.log
  tmux kill-session -t "$SESSION" 2>/dev/null
  case "$TOPO" in
    direct)
      echo "[本机] 无 sidecar，client 直连 $PEER_IP:$SRV_PORT"
      ;;
    single)
      echo "[本机] 启动 envoy-out（single-hop-remote，动态 base-id）"
      gen_conf single-hop-remote.yaml "$RUN"
      tmux new-session -d -s "$SESSION"
      tmux new-window -t "$SESSION" -n envoy-out \
        "$ULIMIT_CMD; $PROBE_OUT $ENVOY -c $GEN_CONF/single-hop-remote.yaml $BASE_FLAG $CONC_FLAG --log-level info 2>&1 | tee $RUN/envoy-out.log"
      sleep 3
      ;;
    two)
      echo "[本机] 启动 envoy-out（two-hop-out-remote，动态 base-id）"
      gen_conf two-hop-out-remote.yaml "$RUN"
      tmux new-session -d -s "$SESSION"
      tmux new-window -t "$SESSION" -n envoy-out \
        "$ULIMIT_CMD; $PROBE_OUT $ENVOY -c $GEN_CONF/two-hop-out-remote.yaml $BASE_FLAG $CONC_FLAG --log-level info 2>&1 | tee $RUN/envoy-out.log"
      sleep 3
      ;;
  esac

  status
}

stop() {
  # 先等 Ticker 刷盘 → 再 SIGTERM → 最后拆 tmux。三步顺序都要紧，两端同样处理，
  # 少一步会让小批量验证轮的打点数据静默丢失，详见 run-direct.sh 的同名函数注释。
  sleep 2
  pkill -u "$USER" -x envoy-static 2>/dev/null
  $SSH "$PEER" "pkill -u \$USER -x envoy-static 2>/dev/null; pkill -u \$USER -x server 2>/dev/null; pkill -u \$USER -x reciever 2>/dev/null" 2>/dev/null
  sleep 2
  tmux kill-session -t "$SESSION" 2>/dev/null
  $SSH "$PEER" "tmux kill-session -t $SESSION 2>/dev/null" 2>/dev/null
  sleep 1
  echo "已停止两端"
}

status() {
  local ok=0 uds
  # 先收进变量再 grep，不要写成 `ss ... | grep -q ...`：本脚本开了 set -o pipefail，
  # grep -q 命中即退出会让 ss 吃 SIGPIPE（rc=141），pipefail 把 141 当成整条管道的
  # 退出码，于是在监听也报「未监听」。详见 run-two-hop.sh 里 status() 的注释。
  uds=$(ss -xln 2>/dev/null)

  if [ "$TOPO" != "direct" ]; then
    grep -qF -- "$RUN/out.sock" <<<"$uds" \
      && echo "  [950] out.sock   : 监听中" || { echo "  [950] out.sock   : 未监听"; ok=1; }
  fi

  # 从本机探测对端端口，顺带验证跨机连通。
  # 三级都是 15006：two 下是 envoy-in 在听，direct/single 下是 server 在听。
  timeout 3 bash -c "echo > /dev/tcp/$PEER_IP/$SRV_PORT" 2>/dev/null \
    && echo "  [920B] :$SRV_PORT     : 可达" || { echo "  [920B] :$SRV_PORT     : 不可达（若报 No route to host 见脚本顶部 SRV_PORT 注释：firewalld）"; ok=1; }

  if [ "$TOPO" = "two" ]; then
    # 对端这条 grep -q 走的是远端 ssh 的非交互 shell，不继承本地的 pipefail，
    # 但保持写法一致更不容易再踩坑，同样收进变量。
    $SSH "$PEER" "uds=\$(ss -xln 2>/dev/null); grep -qF -- '$PEER_RUN/app.sock' <<<\"\$uds\"" 2>/dev/null \
      && echo "  [920B] app.sock  : 监听中" || { echo "  [920B] app.sock  : 未监听"; ok=1; }
  fi

  if [ $ok -ne 0 ]; then
    [ "$TOPO" != "direct" ] && { echo "--- 本机 envoy-out.log ---"; tail -4 "$RUN/envoy-out.log" 2>/dev/null | cut -c1-140; }
    echo "--- 920B server.log ---"; $SSH "$PEER" "tail -4 $PEER_RUN/server.log" 2>/dev/null | cut -c1-140
    [ "$TOPO" = "two" ] && { echo "--- 920B envoy-in.log ---"; $SSH "$PEER" "tail -4 $PEER_RUN/envoy-in.log" 2>/dev/null | cut -c1-140; }
  fi
  return $ok
}

collect() {
  echo "[收集] 从 $PEER 拉取 trace（走局域网，不经开发机）"
  rsync -a "$PEER:$PEER_RUN/trace-server.ndjson" "$RUN/" 2>/dev/null
  # envoy-in 只在 two 下存在；direct/single 没有这些文件，rsync 会静默失败，
  # 但显式跳过更清楚，也避免把上一轮 two 的残留当成本轮结果。
  if [ "$TOPO" = "two" ]; then
    rsync -a "$PEER:$PEER_RUN/trace-envoy-in.ndjson."* "$RUN/" 2>/dev/null
  fi
  ls -lh "$RUN"/trace-* 2>/dev/null | awk '{print "  "$5, $9}'
}

case "${1:-start}" in
  start)   start ;;
  stop)    stop ;;
  status)  status ;;
  collect) collect ;;
  target)  target ;;
  *) echo "用法: [TOPO=direct|single|two] $0 [start|stop|status|collect|target]"; exit 1 ;;
esac
