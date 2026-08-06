#!/bin/bash
# Envoy 构建看门狗（在 suzhou950 上运行）
#
# 背景：该机到 GitHub 的连接间歇性抽风 —— 连接能建立，但对端停止发数据，
# 表现为 ESTABLISHED 且收发队列为 0，只有读超时能打破。
#
# 策略：内层快速放弃 + 外层激进重启。
#   反面教材：retries=10 × timeout_scaling=2.0 → 单个依赖最坏 20 分钟才放弃，
#   内层把外层重试机制饿死了。
#   每次重启都复用 /tmp/eob 已缓存的依赖，所以整体是单调前进的。
exec >> ~/watchdog.log 2>&1

STALL_SECS=240      # 4 分钟无新依赖且日志无增长 => 判定卡死
MAX_ROUNDS=40
TARGET=//source/exe:envoy-static

cd ~/envoy_kitex/envoy || exit 1
echo "===== WATCHDOG START $(date) ====="

deps() { ls /tmp/eob/external 2>/dev/null | wc -l; }

killbazel() {
  ~/bin/bazel --output_base=/tmp/eob shutdown >/dev/null 2>&1
  sleep 2
  pkill -9 -u "$USER" -x java 2>/dev/null
  sleep 2
}

for round in $(seq 1 $MAX_ROUNDS); do
  echo "########## ROUND $round  $(date +%H:%M:%S)  deps=$(deps) ##########"
  rm -f ~/.build_done
  (
    ulimit -n 65536
    ~/bin/bazel --output_base=/tmp/eob build -c opt \
        --curses=no --color=no --show_progress_rate_limit=15 \
        --experimental_repository_downloader_retries=2 \
        --http_timeout_scaling=1.0 \
        "$TARGET" > ~/build_envoy.log 2>&1
    echo "BUILD_RC=$?" >> ~/build_envoy.log
    touch ~/.build_done
  ) &
  BPID=$!

  last_deps=$(deps); last_size=0; stall=0
  while kill -0 $BPID 2>/dev/null; do
    sleep 30
    d=$(deps)
    s=$(stat -c %s ~/build_envoy.log 2>/dev/null || echo 0)
    if [ "$d" = "$last_deps" ] && [ "$s" = "$last_size" ]; then
      stall=$((stall+30))
    else
      stall=0; last_deps=$d; last_size=$s
    fi
    if [ $stall -ge $STALL_SECS ]; then
      echo ">>> 卡死 ${stall}s (deps=$d)，强制重启本轮 <<<"
      killbazel
      kill -9 $BPID 2>/dev/null
      break
    fi
  done
  wait $BPID 2>/dev/null

  if grep -q "BUILD_RC=0" ~/build_envoy.log 2>/dev/null; then
    echo "===== 构建成功 $(date) ====="
    ls -lh bazel-bin/source/exe/envoy-static 2>/dev/null
    touch ~/.watchdog_success
    break
  fi

  # 区分网络错误与编译错误：编译错误重试无意义，立即停止并把错误摘出来
  if grep -qE "^ERROR" ~/build_envoy.log 2>/dev/null \
     && ! grep -qiE "Connect timed out|Error downloading|fetch of repository|Read timed out|connection reset" ~/build_envoy.log 2>/dev/null; then
    echo ">>> 疑似编译/配置错误，停止重试 <<<"
    grep -E "^ERROR|error:" ~/build_envoy.log | head -30
    touch ~/.watchdog_compile_error
    break
  fi

  killbazel
  sleep 10
done

echo "===== WATCHDOG END $(date) deps=$(deps) ====="
touch ~/.watchdog_done
