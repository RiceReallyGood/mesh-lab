# 网卡性能测试操作手册

**用途**：在无外网、无法联系我的公司内网服务器上，手动评测一块新网卡的性能。

本手册自包含，每一步都可直接复制执行。遇到问题看 §9 故障排查。

---

## 0. 先读这一页：分三级，别一上来就编 Envoy

评测网卡时，**最该看的东西恰好是这套插桩看不见的部分**（网卡→驱动→中断→协议栈，详见
[probe-coverage-audit.md](probe-coverage-audit.md) §4）。所以不要默认"跑全链路 = 测得准"。

| 级别 | 测什么 | 需要构建什么 | 耗时 | 对网卡评测的价值 |
|---|---|---|---|---|
| **L0 裸网络基线** | 网卡本身：带宽、时延、丢包、中断 | 无（系统自带工具） | **10 分钟** | ⭐⭐⭐⭐⭐ **最高** |
| **L1 Kitex 直连** | RPC 框架视角的端到端 | Go + demo（约 5 分钟） | 30 分钟 | ⭐⭐⭐⭐ 高 |
| **L2 双跳 + Envoy** | 完整 mesh 拓扑 | 上面全部 + Envoy（**数小时**） | 半天 | ⭐⭐ 低（Envoy 处理会淹没网卡差异） |

**建议：先做 L0，再做 L1。L2 只在你确实要评估"这块网卡对 mesh 场景的影响"时才做。**

理由（2026-08-07 跨机三级阶梯实测，c=1、64 B、p50）：

| 拓扑 | 端到端 | 相对 L0 多出来的东西 |
|---|---:|---|
| 直连（≈L1） | **147.3 µs** | Kitex 框架 + 一次跨机往返 |
| 双跳（L2） | **228.2 µs** | 再加两个 Envoy（自身 43.5 µs）+ 两段 UDS 往返 |

**L2 比 L1 多出 80.9 µs（+55 %），而真正属于网卡的部分是个位数微秒** ——
这 80.9 µs 会把网卡差异彻底淹没。用 `TOPO=direct` 跑 L1 能把 sidecar 影响完全去掉。

---

## 1. L0：裸网络基线（先做这个）

### 1.1 网卡能力自检

在**两台机器**上都执行，先确认这块网卡到底支持什么：

```bash
IF=eth0          # ← 改成你的网卡名，用 `ip -br link` 查

echo "===== 驱动与固件 ====="
ethtool -i $IF

echo "===== 链路速率 ====="
ethtool $IF | grep -E 'Speed|Duplex|Link detected'

echo "===== 硬件时间戳能力（关键！） ====="
ethtool -T $IF

echo "===== 中断合并 ====="
ethtool -c $IF

echo "===== 队列数 ====="
ethtool -l $IF

echo "===== offload 开关 ====="
ethtool -k $IF | grep -E 'tcp-segmentation|generic-receive|large-receive|scatter-gather|rx-checksumming|tx-checksumming'

echo "===== ring buffer ====="
ethtool -g $IF

echo "===== 中断分布在哪些 CPU ====="
grep "$IF" /proc/interrupts | head -20

echo "===== NUMA 归属 ====="
cat /sys/class/net/$IF/device/numa_node 2>/dev/null || echo "(不可用)"
```

**`ethtool -T` 的输出决定后面能做到什么精度**：

- 输出里有 `hardware-receive` / `hardware-transmit` → 能拿网卡硬件时间戳，可以精确测出"网卡收到 → 协议栈处理完"
- 只有 `software-receive` → 只能拿 softirq 里打的时间戳，仍然有用（能分开协议栈与用户态），但测不出网卡内部
- 什么都没有 → 只能靠 §1.3 的黑盒对比

**把这份输出保存下来**，换网卡前后各存一份，diff 一下往往直接看出差异：

```bash
mkdir -p ~/nic-bench && bash -c "$(declare -f 2>/dev/null); true" 
# 把上面那段存成脚本再执行，输出重定向：
#   bash nic-probe.sh > ~/nic-bench/nic-info-$(date +%F-%H%M).txt 2>&1
```

### 1.2 基础指标

```bash
# 丢包与错误（时延突刺的头号成因，必看）
ethtool -S $IF | grep -iE 'drop|error|miss|discard|fifo|crc' | grep -v ': 0$'

# 若上面为空，说明没有非零计数 —— 那是好事
```

### 1.3 带宽与时延

安装（若无外网，多数发行版的基础源里都有）：

```bash
# CentOS/RHEL:  yum install -y iperf3
# Ubuntu/Debian: apt install -y iperf3
```

**带宽**（服务端一台，客户端另一台）：

```bash
# 服务端
iperf3 -s -p 5201

# 客户端：单流 + 多流各测一次
iperf3 -c <服务端IP> -p 5201 -t 30 -i 5            # 单流
iperf3 -c <服务端IP> -p 5201 -t 30 -i 5 -P 8       # 8 流并发
```

单流跑不满而多流能跑满 → 瓶颈在单核处理或中断亲和，不是网卡带宽。

**时延**（这才是本项目关心的）：

```bash
# 基础 RTT 分布。-U 走 UDP 排除 TCP 拥塞控制干扰
ping -c 1000 -i 0.01 -q <对端IP>

# 更好的工具（若可安装）：sockperf 能给出单向与分位数
# sockperf server -i <本机IP> --tcp
# sockperf ping-pong -i <对端IP> --tcp -t 30 --full-log /tmp/sockperf.csv
```

**关键：记录分位数而不只是均值。** 网卡差异常常只体现在 p99 上。

### 1.4 路径 MTU

**必查。** MTU 配错会造成分片或黑洞，时延数据全废：

```bash
for s in 1472 1460 1440 1424 1400 1372; do
  printf "IP包=%-5s: " $((s+28))
  ping -c1 -W2 -M do -s $s <对端IP> >/dev/null 2>&1 && echo 通 || echo 不通
done
```

第一个"通"的值即路径 MTU。若小于接口 MTU，**且中间设备不回 ICMP**，就是黑洞——
表现为"握手秒过、传大文件卡死"。应急处理：

```bash
sudo sysctl -w net.ipv4.tcp_mtu_probing=1     # 启用 RFC 4821，让 TCP 自愈
```

---

## 2. 环境准备（L1 起需要）

### 2.1 Go

```bash
# 若公司内网有 Go 源就用源；否则从任一可达镜像下载
mkdir -p ~/sdk
curl -fL -o /tmp/go.tar.gz https://mirrors.aliyun.com/golang/go1.22.12.linux-amd64.tar.gz
#   aarch64 机器把 amd64 换成 arm64
tar -C ~/sdk -xzf /tmp/go.tar.gz && mv ~/sdk/go ~/sdk/go1.22.12
export PATH=~/sdk/go1.22.12/bin:$PATH
go version      # 应输出 go1.22.12
```

设置模块代理（内网若有私有 proxy 就填它）：

```bash
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off
```

**完全无外网的情况**：在有网的机器上执行 `go mod download` 后打包 `$(go env GOMODCACHE)` 带过去，
或直接带上整个 `vendor/` 目录（在有网机器上 `go mod vendor`）。

### 2.2 源码树布局

四个仓库必须是这个相对位置，`go.mod` 里的 `replace` 依赖它：

```
<任意目录>/
├── envoy/              # 分支 wip/kitex-e2e-probe（L2 才需要）
├── kitex/              # 分支 feat/detailed-trace-events
├── kitex-benchmark/    # 提供 echo 的 kitex_gen 代码
├── netpoll/            # 分支 feat/meshlab-read-probe
└── mesh-lab/           # 本仓库
```

---

## 3. L1：Kitex 直连测试

### 3.1 构建

```bash
export PATH=~/sdk/go1.22.12/bin:$PATH
cd <源码树>/mesh-lab/demo
go build -o bin/server ./server
go build -o bin/client ./client
ls -l bin/
```

**验证插桩确实编进去了**（重要，否则跑完才发现没数据）：

```bash
cd <源码树>/netpoll
go test -race -run TestReadProbe -v -count=1 .
```

必须看到 `TestReadProbeYieldOnRequestResponse` 的良率 ≥ 90%。低于此说明插桩位置在你的内核版本上不成立。

### 3.2 运行

**机器 B（服务端）**：

```bash
export KITEX_PROBE_HOST=hostB          # ← 必须两台不同！否则跨机会被误判成同机
mkdir -p /tmp/trace
./bin/server -addr 0.0.0.0:8888 -trace /tmp/trace/server.ndjson -node kitex-server
```

**机器 A（客户端）**：

```bash
export KITEX_PROBE_HOST=hostA
mkdir -p /tmp/trace
./bin/client -addr <机器B的IP>:8888 \
             -trace /tmp/trace/client.ndjson -node kitex-client \
             -concurrency 16 -duration 60s -sample 0.01
```

`-sample 0.01` 是 1% 采样。**压测时不要用 1.0** —— 实测 100% 采样有 6.6% 的开销，会改变被测系统的行为。

### 3.3 采集结果

```bash
cd <源码树>/mesh-lab/tools/merge
go run . -in /tmp/trace/client.ndjson,/tmp/trace/server.ndjson -format summary
go run . -in ... -format detail        # 逐阶段分解
go run . -in ... -format waterfall     # 单条请求的瀑布图
```

---

## 4. L2：双跳 + Envoy（可选，很贵）

### 4.1 构建 Envoy

**先确认机器扛得住**：Envoy 全量构建需要 ≥ 32 GB 内存、≥ 100 GB 磁盘，
高并行下数小时。低配机器会 OOM。

```bash
# bazel 版本必须与 envoy/.bazelversion 一致
cat <源码树>/envoy/.bazelversion
mkdir -p ~/bin
curl -fL -o ~/bin/bazel https://mirrors.huaweicloud.com/bazel/8.7.0/bazel-8.7.0-linux-arm64
#   x86_64 机器把 arm64 换成 x86_64
chmod +x ~/bin/bazel

cd <源码树>/envoy
ulimit -n 65536
~/bin/bazel --output_base=$HOME/bazel_out build -c opt \
    --curses=no --color=no --show_progress_rate_limit=30 \
    --cxxopt=-Wno-nullability-completeness \
    //source/exe:envoy-static
```

**`--cxxopt=-Wno-nullability-completeness` 不能省** —— 不加会在 cel-cpp 上编译失败。

**`--output_base` 不要放在 /tmp** —— tmpfs 的 inode 数写死在挂载参数里（通常 1048576），
bazel 高并行会耗尽 inode，报错是 "No space left on device" 但 `df -h` 显示还有空间，
要用 `df -i` 才看得出来。放 ext4/xfs 的 /home 下。

产物：`bazel-bin/source/exe/envoy-static`

### 4.2 运行

配置文件在 `mesh-lab/envoy-conf/`：

```bash
# 机器 A
./envoy-static -c <源码树>/mesh-lab/envoy-conf/two-hop-out-remote.yaml --base-id 1
# 机器 B
./envoy-static -c <源码树>/mesh-lab/envoy-conf/two-hop-in-remote.yaml --base-id 2
```

**`--base-id` 必须不同** —— 相同 base-id 的实例会通过共享域套接字互相杀掉（热重启机制）。

一键脚本：`mesh-lab/scripts/run-cross-machine.sh`（默认 `TOPO=two`；
`TOPO=direct` / `TOPO=single` 可拿到少一跳、少两跳的对照，
评测网卡时用 `direct` 能把 sidecar 的影响完全去掉）

---

## 5. 换网卡的对照方法

**这一节比前面所有内容都重要。方法错了，数据再多也没用。**

### 5.1 交错分组，不要顺序跑

❌ **错误做法**：先把旧网卡测完 6 轮，换卡，再把新网卡测 6 轮。

这会让后测的一组享受到缓存预热、页表建立、CPU 频率爬升的好处。
之前在本项目上踩过这个坑，结果荒谬到"加了插桩反而快 4.9%"——物理上不可能。

✅ **正确做法**：如果能双卡共存，按轮次交错：

```
第1轮: 旧卡 → 新卡
第2轮: 新卡 → 旧卡      ← 顺序反过来
第3轮: 旧卡 → 新卡
...共 6 轮
```

若只能物理换卡（无法交错），则**每次换卡后必须重新预热**：先跑 2 分钟丢弃，再开始采集。

### 5.2 先找饱和点

在饱和点以下测时延没有意义——那时测的是"发多少收多少"，网卡差异体现不出来。

```bash
for c in 1 2 4 8 16 32 64 128; do
  echo "=== concurrency=$c ==="
  ./bin/client -addr <IP>:8888 -concurrency $c -duration 20s -sample 0
done
```

QPS 随并发上升而饱和的那个点，就是后续测试该用的并发度。

### 5.3 必须同时采集的对照量

只看时延会得出错误结论。每轮测试同时记录：

```bash
# 测试前后各采一次，算差值
ethtool -S $IF | grep -iE 'drop|error|miss|discard' > /tmp/nic-stat-before.txt
# ... 跑测试 ...
ethtool -S $IF | grep -iE 'drop|error|miss|discard' > /tmp/nic-stat-after.txt
diff /tmp/nic-stat-before.txt /tmp/nic-stat-after.txt

# TCP 层：重传是 p99 突刺的头号成因
nstat -az | grep -iE 'TcpRetransSegs|TCPTimeouts|TCPLostRetransmit'

# 软中断占用（换网卡后中断合并策略变化直接体现在这里）
cat /proc/softirqs | grep NET_RX

# CPU 使用（区分 user/sys/softirq）
mpstat -P ALL 5 4      # 需要 sysstat 包
```

**如果新网卡时延更低但 `NET_RX` softirq 明显升高，那不是网卡更快，
而是中断合并被调松了——用 CPU 换的时延，高负载下会反噬。**

### 5.4 判定标准

| 观察 | 结论 |
|---|---|
| 两组的 [min, max] 区间重叠 | **不能声称有差异**，只能说"低于噪声下限"。本项目实测噪声下限约 3% |
| p50 改善但 p99 恶化 | 中断合并被调松，用抖动换了平均时延 |
| 时延改善且 softirq 时间同步下降 | 真实改善 |
| 时延改善但重传计数上升 | 可能是 offload 引入的乱序，先查 GRO/LRO 设置 |

**分位数不可加**：各阶段 p50 之和 ≠ 端到端 p50。分解表只能判断量级与占比，不能拿来做加减。

---

## 6. 补上插桩的盲区（想测网卡本身就必须做）

现有点位的下界是 **socket 系统调用**。网卡、驱动、中断、协议栈全在盲区里。
详见 [probe-coverage-audit.md](probe-coverage-audit.md) §4–5。

### 6.1 SO_TIMESTAMPING（优先）

若 §1.1 的 `ethtool -T` 显示支持硬件时间戳，用它能直接量出"网卡收到 → 协议栈处理完"。

最省事的验证方式是现成工具：

```bash
# linuxptp 包自带
ethtool -T $IF                    # 先确认能力
# 若有 hardware-receive：
sudo hwstamp_ctl -i $IF -r 1      # 开启 RX 硬件时间戳
```

或用内核自带示例 `tools/testing/selftests/net/txtimestamp.c`。

### 6.2 eBPF 挂协议栈

```bash
# 需要 bpftrace
# softirq NET_RX 的耗时分布
sudo bpftrace -e '
tracepoint:irq:softirq_entry /args->vec == 3/ { @start[cpu] = nsecs; }
tracepoint:irq:softirq_exit  /args->vec == 3 && @start[cpu]/ {
    @net_rx_us = hist((nsecs - @start[cpu]) / 1000); delete(@start[cpu]);
}'

# 包进协议栈到 TCP 处理的时延
sudo bpftrace -e '
tracepoint:net:netif_receive_skb { @t[args->skbaddr] = nsecs; }
kprobe:tcp_v4_rcv /@t[arg0]/ { @stack_us = hist((nsecs - @t[arg0])/1000); delete(@t[arg0]); }'
```

**换网卡前后各跑一次，对比直方图** —— 这是唯一能直接看到网卡影响的量。

---

## 7. 一次完整测试的推荐流程

```
□ 1. 两台机器都跑 §1.1 网卡自检，保存输出
□ 2. §1.4 确认路径 MTU 与接口 MTU 一致
□ 3. §1.3 iperf3 带宽 + ping 时延基线
□ 4. §5.2 找饱和点，确定并发度
□ 5. §6.2 bpftrace 采集 softirq 与协议栈直方图（旧卡）
□ 6. L1 Kitex 直连，6 轮，1% 采样（旧卡）
□ 7. ——— 换网卡 ———
□ 8. 重跑 1、2、3（确认新卡链路正常，MTU 没变）
□ 9. 重跑 5、6（新卡），注意 §5.1 的预热
□ 10. §5.3 对照量 diff
□ 11. 出结论时对照 §5.4 判定标准
```

**每一步的原始输出都留档**。结论可以重算，数据丢了就得重跑。

---

## 8. 数据带回来给我看

测完把这些打包，回到能联系我的环境后我可以帮你分析：

```bash
tar czf nic-bench-$(date +%F).tar.gz \
    ~/nic-bench/                  \
    /tmp/trace/*.ndjson           \
    /tmp/nic-stat-*.txt
```

NDJSON 里不含业务数据，只有时间戳、点位名和 trace id。

---

## 9. 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| **trace 文件是空的 / 只有少量事件** | 事件缓冲在内存中，进程退出时才刷盘 | 用 `SIGTERM` 优雅停止，不要 `kill -9` |
| **跨机分析结果荒谬（负数时延）** | 两台机器 `KITEX_PROBE_HOST` 相同，被误判为同机后做了跨机钟减法 | 两台必须设不同值。默认 hostname 在很多环境下都是 `localhost.localdomain` |
| **`waterfall` 图时间轴错乱** | 两机时钟未同步（本项目实测 15~17 秒，随时间漂移） | 分析结论不受影响（差值法对偏斜免疫），仅视觉错乱。介意就配 NTP |
| **传大文件卡死，小包正常** | MTU 黑洞（路径 MTU < 接口 MTU 且中间设备不回 ICMP） | `sysctl -w net.ipv4.tcp_mtu_probing=1`，见 §1.4 |
| **bazel 报 "No space left on device" 但 df 显示有空间** | tmpfs inode 耗尽 | `df -i` 确认；`--output_base` 换到 ext4/xfs 分区 |
| **Envoy 编译在 cel-cpp 失败** | 缺 `--cxxopt=-Wno-nullability-completeness` | 加上重跑 |
| **两个 Envoy 实例互相杀掉** | `--base-id` 相同 | 改成不同值 |
| **netpoll 探针良率极低** | 若 `TestReadProbeYieldOnRequestResponse` < 90%，插桩位置在你的内核上不成立 | 记录内核版本，带回来给我 |
| **压测数据抖动大到无法比较** | 未找饱和点，或顺序跑而非交错 | 见 §5.1、§5.2 |

---

## 10. 一句话版本

**如果时间只够做一件事**：跑 §1.1 的网卡自检 + §6.2 的 bpftrace softirq 直方图，换卡前后各一次。
这两样加起来 20 分钟，而它们直接测的就是网卡本身——比跑一整天全链路压测更能说明问题。
