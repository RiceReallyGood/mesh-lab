# 操作手册：构建 → 运行 → 采集 → 分析

从零把整条链路跑起来并拿到时延归因数据的完整步骤。

**本文只讲"怎么做"。** 相关的"为什么"分散在别处，需要时再点进去：

| 想知道 | 看哪 |
|---|---|
| 环境为什么这么装（镜像、代理、bazel 参数的取舍） | [runbook-build-environment.md](runbook-build-environment.md) |
| 每个点位是什么意思、去掉会损失什么 | [probe-points.md](probe-points.md) |
| 哪些时间还测不到 | [probe-coverage-audit.md](probe-coverage-audit.md) |
| 已有的实测结论 | [test-report.md](test-report.md) |
| 专门评测网卡 | [runbook-nic-benchmark.md](runbook-nic-benchmark.md) |

---

## 0. 五分钟自检清单

跑之前先确认这几条，每一条都对应一个踩过的坑：

```
□ 五个仓库在同一父目录下，分支正确（§1）      —— go.mod 的 replace 靠相对路径
□ Envoy 构建带 --cxxopt=-Wno-nullability-completeness（§2.1）
□ bazel --output_base 不在 /tmp（§2.1）
□ 两台机器的 KITEX_PROBE_HOST 不同（§3）      —— 默认 hostname 都是 localhost.localdomain
□ Envoy 侧设了 KITEX_PROBE_PATH / KITEX_PROBE_NODE（§3）
□ 采集前先 stop 进程，不要在运行中读或删 trace 文件（§4）
```

---

## 1. 取代码

五个仓库**必须**在同一父目录下 —— `mesh-lab/demo/go.mod` 里的 `replace` 用的是相对路径 `../../xxx`。

```bash
mkdir -p ~/envoy_kitex && cd ~/envoy_kitex

git clone -b main                          git@github.com:RiceReallyGood/mesh-lab.git
git clone -b feat/detailed-trace-events    git@github.com:RiceReallyGood/kitex.git
git clone -b feat/meshlab-read-probe       git@github.com:RiceReallyGood/netpoll.git
git clone                                  git@github.com:cloudwego/kitex-benchmark.git
git clone -b wip/kitex-e2e-probe           git@github.com:RiceReallyGood/envoy.git
```

结果应是：

```
~/envoy_kitex/
├── envoy/            wip/kitex-e2e-probe        插桩版（含 TTHeader transport）
├── kitex/            feat/detailed-trace-events
├── kitex-benchmark/  main                       只用它的 echo kitex_gen，未修改
├── netpoll/          feat/meshlab-read-probe
└── mesh-lab/         main
```

**分支说明**：`envoy` 还有一个 `feat/thrift-ttheader-transport` —— 那是不含打点、可提 PR 到上游的纯净版。跑实验用 `wip/kitex-e2e-probe`（它基于前者，多了打点）。

---

## 2. 构建

### 2.1 Envoy（慢，数小时；只在 suzhou950 上做）

```bash
cd ~/envoy_kitex/envoy
ulimit -n 65536
source ~/proxy.env                      # 如需代理拉依赖

~/bin/bazel --output_base=$HOME/bazel_out build -c opt \
    --curses=no --color=no --show_progress_rate_limit=30 \
    --experimental_repository_downloader_retries=2 \
    --http_timeout_scaling=1.0 \
    --cxxopt=-Wno-nullability-completeness \
    //source/exe:envoy-static
```

产物：`bazel-bin/source/exe/envoy-static`（约 850 MB）

**两个不能省的参数**：

- `--cxxopt=-Wno-nullability-completeness` —— 不加会在 cel-cpp 上编译失败，报
  `pointer is missing a nullability type specifier`
- `--output_base=$HOME/bazel_out` —— **不能放 /tmp**。tmpfs 的 inode 上限写死在挂载参数里，
  bazel 高并行会耗尽它，报错是 `No space left on device` 但 `df -h` 显示还有空间，
  要 `df -i` 才看得出来

**改了 `envoy/network/connection.h` 这类核心头文件会触发大范围重编**（实测 1134 个动作）；
只改 `connection_impl.cc` 或 thrift extension 则是几分钟的增量。

建议挂后台跑，避免 ssh 断连打断：

```bash
nohup ~/build-envoy.sh > ~/build-envoy.log 2>&1 &
tail -f ~/build-envoy.log
```

### 2.2 Go 侧（快，几分钟）

```bash
export PATH=~/sdk/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOFLAGS=-mod=mod

cd ~/envoy_kitex/mesh-lab/demo
go build -o bin/server ./server
go build -o bin/client ./client

# merge 是独立 module，要单独构建
cd ~/envoy_kitex/mesh-lab/tools/merge
go build -o ~/envoy_kitex/mesh-lab/demo/bin/merge .
```

### 2.3 构建后自检

确认插桩真的编进去了，**别等跑完才发现没数据**：

```bash
# Envoy：新点位的字符串应在二进制里
for s in up_epoll_wake up_readv_start up_readv_done; do
  printf "%-16s " $s
  strings ~/envoy_kitex/envoy/bazel-bin/source/exe/envoy-static | grep -qx "$s" && echo 在 || echo 缺失
done

# netpoll：探针在请求—响应模式下的良率应 ≥90%
cd ~/envoy_kitex/netpoll && go test -race -run TestReadProbe -v -count=1 .
```

---

## 3. 运行

三种拓扑，按需要挑一种。

### 3.1 单跳（同机，最快，用于验证链路通不通）

```
client --UDS--> envoy --UDS--> server
```

```bash
cd ~/envoy_kitex/mesh-lab
./scripts/run-single-hop.sh start          # start | stop | status

cd demo
KITEX_PROBE_HOST=suzhou950 ./bin/client -n 300 -d 0 -c 1 -sample 1.0
```

### 3.2 双跳（同机两个 Envoy）

```
client --UDS--> envoy-out --TCP--> envoy-in --UDS--> server
```

```bash
./scripts/run-two-hop.sh start
```

### 3.3 跨机双跳（真实拓扑）

```
[suzhou950]  client --UDS--> envoy-out
                                 │ TCP 跨机
[suzhou920B]              envoy-in --UDS--> server
```

```bash
cd ~/envoy_kitex/mesh-lab
./scripts/run-cross-machine.sh start        # 自动 rsync 二进制到 920B
```

脚本会把 `envoy-static` 与 `server` 推到对端、起进程、检查监听状态。
`collect` 把对端的 trace 拉回本机。

**可选：限制 Envoy worker 数**

```bash
ENVOY_CONCURRENCY=2 ./scripts/run-cross-machine.sh start
```

默认按 CPU 核数开 worker（本机 384 个）。核数远多于连接数时每个 worker 一次 epoll
只拿到零星事件，**「事件循环排队」这个点位恒为几百纳秒，测不出任何东西**。
要观察排队必须把 worker 压下来（实测 384→2 时该值从 290ns 涨到 21µs）。

### 3.4 环境变量

| 变量 | 谁需要 | 说明 |
|---|---|---|
| `KITEX_PROBE_HOST` | 全部进程 | **两台机器必须不同**。默认取 hostname，而本环境两台都是 `localhost.localdomain`，一样的话跨机会被静默误判成同机，差值法的前提就没了 |
| `KITEX_PROBE_PATH` | Envoy | 打点输出路径。**不设则完全不落盘且不报错** |
| `KITEX_PROBE_NODE` | Envoy | 节点名，如 `envoy-out` / `envoy-in` |

三个 `run-*.sh` 都已内置这些变量，手工起进程时才需要自己设。

### 3.5 压测参数

```bash
./bin/client \
  -target /tmp/kitex-demo/out.sock \   # UDS 路径或 host:port
  -service echo-server \               # 写入 TTHeader ToService，供 Envoy 路由
  -c 16 \                              # 并发
  -d 60s \                             # 时长；-d 0 时改用 -n 指定请求数
  -sample 0.05 \                       # 采样率
  -size 128                            # payload 字节数
```

**采样率怎么选**：

| 场景 | 采样率 | 理由 |
|---|---|---|
| 验证链路 / 调试 | `1.0` | 请求数少，要每条都有 |
| 压测归因 | `0.01` ~ `0.05` | 实测 100% 采样有 6.6% 开销，会改变被测系统行为；1% 时开销低于 3% 的噪声下限 |
| 极限吞吐 | `0` | 完全关闭 |

---

## 4. 采集数据

### 4.1 必须先停进程

```bash
./scripts/run-cross-machine.sh stop
sleep 5
./scripts/run-cross-machine.sh collect
```

**为什么必须先 stop**：事件先进内存缓冲，**在线程退出时才刷盘**。进程还活着时读到的
文件永远缺最后一批。实测 300 个请求只有 273 条落盘，剩下的还在内存里。

### 4.2 三个关于文件的坑

**① Envoy 按线程分文件**，实际名字是 `trace-envoy-out.ndjson.<tid>`，不是
`trace-envoy-out.ndjson`。用 `*.ndjson*` 通配，用 `*.ndjson` 会一个都匹配不到。

（这也是为什么早期清理脚本清不干净 —— 残留文件会污染下一次分析。三个 `run-*.sh` 已修。）

**② 不要在进程运行时 `rm` trace 文件。** 探针持有长开的 `FILE*`，删掉之后写入会进到
已删除的 inode，进程退出时数据直接消失，**且没有任何报错**。踩过一次，200 个请求的数据全丢。

正确顺序永远是：`stop` → 清理 → `start` → 压测 → `stop` → `collect`。

**③ 数据量不小。** 60 秒 5% 采样、40k QPS 会产出约 1 GB（client 495 MB + server 409 MB +
上百个 Envoy 分线程文件）。磁盘要留够。

---

## 5. 分析

`merge` 工具有五种输出格式：

| 格式 | 用途 |
|---|---|
| `summary` | 各节点总时长，最粗 |
| **`detail`** | **各阶段分位数，日常主力** |
| **`table`** | **逐条 trace 的 CSV，导进表格/pandas 自己筛** |
| `waterfall` | 单条请求的瀑布图，排查个案 |
| `chrome` | Chrome tracing 格式，`chrome://tracing` 里看 |

```bash
cd ~/envoy_kitex/mesh-lab/demo
FILES="/tmp/kitex-demo/trace-client.ndjson /tmp/kitex-demo/trace-server.ndjson \
       /tmp/kitex-demo/trace-envoy-out.ndjson.* /tmp/kitex-demo/trace-envoy-in.ndjson.*"

./bin/merge -format detail $FILES              # 分阶段分位数
./bin/merge -format table -limit 0 $FILES > trace-table.csv   # 全量 CSV
./bin/merge -format waterfall -limit 3 $FILES  # 看 3 条个案
```

### 5.1 table 格式

一行一条 trace，一列一个时间段，单位微秒。开头四行是**全量样本**的
`__avg__` / `__p50__` / `__p90__` / `__p99__`（不受 `-limit` 影响）。
注释行以 `#` 开头：

```python
import pandas as pd
df = pd.read_csv("trace-table.csv", comment="#")
agg = df[df.trace_id.str.startswith("__")]     # 聚合行
raw = df[~df.trace_id.str.startswith("__")]    # 逐条数据
```

`detail` 只给分位数，看不到单条请求内部各段的相关性 —— 比如"尾延迟那些请求到底卡在排队还是网络"。
`table` 保留原始逐条数据就是为了回答这类问题。

### 5.2 读数据时的三条纪律

**① 分位数不可加。** 各列 p50 之和 ≠ 总时长 p50。分解表只能判断量级与占比，不能做加减。

**② `net.*` 列不是纯线路时间。** 它是 `(外层等待 − 内层总时长) / 2`，而内层的"总时长"从
`dn_first_byte` 起算 —— 请求在此之前还可能在 listener/worker 队列里等过，那段落在测量区间之外，
全被这个差值吸收。实测 worker 从 384 压到 2，同机 UDS 的"单程"从 21µs 涨到 128µs，
**多出来的一百微秒是排队不是传输**。只有低负载无排队时这一列才近似真实传输时间。

除以 2 还隐含"去程回程对称"的假设。单条 trace 上该值可能为负（两次独立测量的噪声），属正常。

**③ 跨机只能得往返。** 拆单向需要 PTP + 硬件时间戳网卡，是物理限制不是插桩能力问题。

### 5.3 时钟偏斜自检

本环境两台机器实测差 16.34 秒。分析用的是差值法（两个各自在本机测得的时长相减），
对偏斜免疫。想验证就注入一个人工偏移，**各段时长应逐位不变**：

```bash
./bin/merge -format detail --inject-skew kitex-server=+50 $FILES
```

---

## 6. 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| **Envoy 点位一条都没有** | 没设 `KITEX_PROBE_PATH` / `KITEX_PROBE_NODE`。不落盘且**不报错** | 见 §3.4；三个 `run-*.sh` 已内置 |
| **trace 文件比预期少一截** | 最后一批还在内存，线程退出才刷盘 | 先 `stop` 再读；用 `SIGTERM` 不要 `kill -9` |
| **明明跑了却一条数据都没有** | 在进程运行时 `rm` 过 trace 文件 | 见 §4.2 ② |
| **`*.ndjson` 匹配不到文件** | Envoy 按线程分文件，名字带 `.<tid>` 后缀 | 用 `*.ndjson*` |
| **跨机分析出负数时延** | 两机 `KITEX_PROBE_HOST` 相同，被误判为同机 | 两台设不同值 |
| **waterfall 时间轴错乱** | 两机时钟未同步 | 不影响分析结论（差值法免疫），仅视觉错乱 |
| **「事件循环排队」恒为几百纳秒** | worker 数远多于连接数，压根没排队 | `ENVOY_CONCURRENCY=2` |
| **bazel 报 `No space left on device` 但 df 有空间** | tmpfs inode 耗尽 | `df -i` 确认；`--output_base` 换到 ext4/xfs |
| **cel-cpp 编译失败 nullability** | 缺 `--cxxopt=-Wno-nullability-completeness` | 加上重跑 |
| **两个 Envoy 互相杀死** | `--base-id` 相同，热重启机制生效 | 给不同 base-id |
| **Envoy 启动报 Too many open files** | 默认 fd 软限 1024 不够，而且只是 warn（进程看似起来了实则不可用） | `ulimit -n 65536` |
| **ssh 断连导致构建中断** | — | `nohup` 起构建，不受 ssh 生命周期影响 |
| **传大文件卡死、小包正常** | MTU 黑洞（路径 MTU < 接口 MTU 且中间设备不回 ICMP） | `sudo sysctl -w net.ipv4.tcp_mtu_probing=1` |

---

## 7. 一次完整实验的动作序列

```bash
# ── 准备 ──
cd ~/envoy_kitex/mesh-lab
./scripts/run-cross-machine.sh stop          # 确保干净起步

# ── 起拓扑 ──
ENVOY_CONCURRENCY=2 ./scripts/run-cross-machine.sh start
./scripts/run-cross-machine.sh status        # 确认三个监听点都通

# ── 压测 ──
cd demo
KITEX_PROBE_HOST=suzhou950 ./bin/client \
    -target /tmp/kitex-demo/out.sock -service echo-server \
    -c 64 -d 40s -sample 0.05

# ── 收数据（顺序不能换）──
cd ~/envoy_kitex/mesh-lab
./scripts/run-cross-machine.sh stop
sleep 5
./scripts/run-cross-machine.sh collect

# ── 分析 ──
cd demo
FILES="/tmp/kitex-demo/trace-client.ndjson /tmp/kitex-demo/trace-server.ndjson \
       /tmp/kitex-demo/trace-envoy-out.ndjson.* /tmp/kitex-demo/trace-envoy-in.ndjson.*"
./bin/merge -format detail $FILES | tee /tmp/kitex-demo/detail.txt
./bin/merge -format table -limit 0 $FILES > /tmp/kitex-demo/trace-table.csv
```

做对照实验（改代码前后、换网卡前后）时，**必须按轮次交错跑，不能一组跑完再跑另一组** ——
后跑的那组会白占缓存预热和 CPU 频率爬升的便宜。踩过这个坑，当时得出过
"加了插桩反而快 4.9%"这种物理上不可能的结论。
