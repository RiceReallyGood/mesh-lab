# 复现手册：从零到出数据

- 日期：2026-08-07
- 适用：拿到两台干净的 Linux 机器，把这套「Kitex × Envoy 端到端时延归因」实验完整跑出来
- 前提：**网络已经可用**（§3 给了可检验的定义）。网络不通怎么办不在本文，见
  [runbook-build-environment.md](runbook-build-environment.md) §3
- 本文每一步的耗时、输出、报错都来自 2026-08-07 的一次**真实全量复现**：
  两台机器先清空到只剩系统，再从下载 Go 开始一路跑到出数据

---

## 0. 先看这里

### 0.1 三份 runbook 的分工

仓库里有三份操作类文档，很容易搞混。分工是这样的：

| 文档 | 回答的问题 | 什么时候看 |
|---|---|---|
| **本文** | **照着做，从零到出数据** | **第一次复现** |
| [runbook-build-environment.md](runbook-build-environment.md) | 环境为什么这么装？网络不通怎么排？ | 卡住了、或者想知道某个参数的来历 |
| [runbook-operations.md](runbook-operations.md) | 环境已就绪，日常怎么跑一轮实验 | 已经跑通过一次，之后每天用 |

本文会**重复**另两份里的关键结论（而不是只给链接），因为复现的时候来回跳文档很痛苦。
想深挖某一条的完整来历，再点进去。

### 0.2 全程耗时（本次实测）

| 阶段 | 内容 | 耗时 |
|---|---|---|
| §3 | 前置自检 | 1 分钟 |
| §4 | 装 Go + bazel | **41 秒** |
| §5 | clone 五个仓库 | （含在上面 41 秒里） |
| §6.1 | **Envoy 全量构建** | **17 分钟** ← 唯一的长活 |
| §6.2 | Go 侧构建 | **35 秒** |
| §6.3 | 构建后自检 | 1 分钟 |
| §7–§9 | 三种拓扑实跑 + 采集 + 分析 | 16 分钟 |
| | **净操作时间合计** | **约 36 分钟** |

（本次墙钟从 17:12 到 18:14 共 62 分钟，多出来的 26 分钟花在排查 §7.3.1 那个
`pipefail` bug 和写文档上。照着本文做不需要那部分。）

首次复现前请注意：**17 分钟这个数字是 384 核跑出来的**
（18,572 个 action、383 路并行、峰值 load average 326）。核数少的机器要按比例放大 ——
64 核约 1.5 小时，32 核约 3 小时，但都不会低于 7 分钟（关键路径，见 §6.1.6）。

**所以第一件事就是把 Envoy 构建挂后台跑起来**，其余步骤在它跑的时候并行做。

### 0.3 磁盘要求

本次实测占用：

| 位置 | 占用 | 说明 |
|---|---|---|
| `~/bazel_out` | **65 GB** | bazel 的 output base，编译中间产物 |
| `~/.cache/bazel` | **3.0 GB** | 依赖仓库缓存，按 sha256 索引，与 output_base 独立 |
| `~/sdk` + `~/go` + `~/bin` + `~/dl` | 669 MB | Go 工具链 + module 缓存 + bazel 二进制 |
| `~/envoy_kitex` | 299 MB | 五个仓库源码（`--depth 1`） |
| `/tmp/kitex-demo` | **1.2 GB / 轮** | trace 数据，见 §8.2 ③ |
| 合计 | **约 70 GB** | |

**准备 80 GB 以上的空闲空间。** 而且必须是 ext4/xfs 这类真实文件系统，不能是 tmpfs（§6.1.3）。

---

## 1. 你要搭的是什么

### 1.1 一句话

让一个 Go 写的 RPC 调用（Kitex）经过两个 Envoy 代理转发，
**并且在沿途每个关键位置打上时间戳**，最后把这些时间戳拼起来，
看清这次调用的时间到底花在哪。

### 1.2 如果你没接触过 Service Mesh

**先看没有它的时候是什么样。** 服务 A 要调服务 B，就是 A 直接连 B 的 IP 和端口：

```
A ────────── TCP ──────────▶ B
```

简单，但重试、超时、熔断、灰度、加密、可观测……这些逻辑得写进 A 和 B 的代码里。
公司里有几百个服务、四五种语言，每种语言都得实现一遍，还得同步升级。

**Service Mesh 的做法是把这些逻辑挪出业务进程**，放进一个跟业务进程一起部署的代理里。
这个代理叫 **sidecar**（边车，像挎斗摩托的挎斗）。于是变成：

```
A ──▶ [A 的 sidecar] ────网络────▶ [B 的 sidecar] ──▶ B
```

A 只管连本机的 sidecar，剩下的事 sidecar 干。**这就是「两跳」的由来** ——
一次调用要过两个代理，出去一跳、进来一跳。业界最常用的 sidecar 实现就是 **Envoy**。

代价是：本来 A 到 B 一次网络传输，现在变成四次进程间传递。**这套实验就是要量化这个代价。**

### 1.3 为什么要插桩

不插桩你能拿到什么？客户端打点，得到一个数字：这次调用花了 206 微秒。
服务端打点，得到另一个数字：业务逻辑花了 240 纳秒。

**中间差的 205.76 微秒，你不知道花在哪。** 是两个 Envoy 慢？是网络慢？
是 Go 的 goroutine 没被及时调度？是 epoll 唤醒晚了？还是数据在某个队列里排着？
这几种原因的处理方式完全不同 —— 网络慢该换网卡，排队该调 worker 数，
框架慢该改框架 —— 但从外面看它们长得一模一样。

所以要在 **Kitex（Go RPC 框架）**、**netpoll（Kitex 底下的网络库）**、
**Envoy（C++ 代理）** 三处源码里插时间戳。这也是为什么本文要你自己编译 Envoy：
**官方二进制里没有这些打点。**

### 1.4 最终拓扑

```
              suzhou950                              suzhou920B
   ┌──────────────────────────────┐        ┌──────────────────────────────┐
   │  kitex-client                │        │              ┌──▶ kitex-server│
   │       │                      │        │              │ UDS            │
   │       │ UDS (out.sock)       │        │          envoy-in             │
   │       ▼                      │        │              ▲                │
   │   envoy-out ─────────────────┼─TCP────┼──────────────┘                │
   └──────────────────────────────┘ :15006 └──────────────────────────────┘
        ↑ 打点落盘                              ↑ 打点落盘，最后拉回 950 合并
```

**UDS** = Unix Domain Socket，同一台机器上两个进程之间的套接字，
走内核内存不走网卡。业务进程连本机 sidecar 用的就是它。

---

## 2. 机器与角色

### 2.1 分工

| 机器 | 地址 | 跑什么 | 需要外网 |
|---|---|---|---|
| **suzhou950** | 192.168.25.145 | **唯一的构建机**；跑 client + envoy-out | **需要** |
| suzhou920B | 192.168.25.51 | 跑 envoy-in + server | **不需要** |

**920B 为什么不需要外网**：它上面跑的两个东西 —— `envoy-static` 和 `server` ——
都是在 950 上编好之后，由 `run-cross-machine.sh` 通过局域网 `rsync` 推过去的。
920B 自己不编译任何东西。

这条对公司内部复现很有用：**只需要一台机器能上外网。**

### 2.2 本次环境的实测配置

| | suzhou950 | suzhou920B |
|---|---|---|
| 系统 | openEuler 24.03 (LTS-SP3) | openEuler **22.03** (LTS-SP3) |
| 内核 | 6.6.0 | **5.10.0** |
| 架构 | aarch64（物理机） | aarch64（物理机） |
| CPU | **384 核** | 160 核 |
| 内存 | 1379 GB | 502 GB |
| glibc | **2.38** | **2.34** |
| gcc / clang | 12.3.1 / 17.0.6 | 10.3.1 / 16.0.6 |
| `/home` 可用 | 6.0 T | 112 G |
| `/tmp` | 690 G **tmpfs** | 252 G **tmpfs** |
| `ulimit -n` | soft 1024 / hard 524288 | soft 1024 / hard 524288 |
| `perf_event_paranoid` | **-1**（perf 全功能） | 2（受限） |
| 缺少的工具 | — | **`jq`** |
| **无 sudo** | ✓ | ✓ |

两台机器 RTT **0.068 ms**（同网段，`ping -c 3` 实测 min/avg/max = 0.047/0.068/0.104 ms）。

### 2.3 两台机器不一致带来的三个约束

这三条在别的环境里也会遇到，值得单列：

**① 二进制在新 glibc 机器上编，要在旧 glibc 机器上跑。**
950 是 glibc 2.38，920B 是 2.34。C++ 程序链接到高版本 glibc 的符号后，在低版本上会报
`version 'GLIBC_2.3x' not found`。本项目实测没有触发，验证方法见 §6.3 ③。
**方向反了才安全**：在旧的上编、新的上跑没问题，反过来才有风险。
如果你的两台机器 glibc 差得更多，把构建机换成版本更低的那台。

**② `/tmp` 是 tmpfs，两台都是。** 这直接决定了 §6.1.3 的 `--output_base` 不能放 `/tmp`。
trace 数据放 `/tmp/kitex-demo` 是可以的（写完就拉走），但注意它**占内存**。

**③ 920B 没有 `jq`。** 本项目的脚本没用到，但你要在 920B 上自己写分析脚本时会踩。

### 2.4 换到你自己的机器要改哪些地方

**机器名和 IP 是写死在配置与脚本里的**，不是从环境变量读的。换环境必须改这五处：

| 文件 | 位置 | 原值 | 改成 |
|---|---|---|---|
| `scripts/run-cross-machine.sh` | `PEER=` / `PEER_IP=` | `suzhou920B` / `192.168.25.51` | 你的对端 |
| `scripts/run-cross-machine.sh` | `PROBE_OUT` / `PROBE_IN` 里的 `KITEX_PROBE_HOST` | `suzhou950` / `suzhou920B` | **两个值必须不同**，见 §7.5 |
| `scripts/run-cross-machine.sh` | `SRV_PORT` | `15006` | 你的对端放行的端口 |
| `envoy-conf/two-hop-out-remote.yaml` | `to_inbound_sidecar` 集群的 `socket_address` | `192.168.25.51:15006` | 你的对端 IP |
| `envoy-conf/single-hop-remote.yaml` | `to_remote_server` 集群的 `socket_address` | `192.168.25.51:15006` | 同上 |
| `scripts/run-single-hop.sh` / `run-direct.sh` | `KITEX_PROBE_HOST` 默认值 | `suzhou950` | 你的本机名 |
| 压测命令 | `KITEX_PROBE_HOST=` | `suzhou950` | 同上 |

一条命令找全：

```bash
grep -rn "suzhou\|192\.168\.25\." ~/envoy_kitex/mesh-lab/{scripts,envoy-conf}
```

**顺带记下这套用到的端口**，跟你环境里已有的服务冲突了要改：

| 端口 | 用途 |
|---|---|
| 15000 / 15001 / 15002 / 15003 | 各份配置的 Envoy admin 接口（同机双跳 out=15001、in=15002；跨机单跳 out=15003） |
| **15006** | **唯一需要跨机放行的端口。** 双跳时是 envoy-in 在听，直连/单跳时是 kitex-server 在听 |
| `/tmp/kitex-demo/out.sock` | client → envoy-out 的 UDS |
| `/tmp/kitex-demo/app.sock` | envoy-in → server 的 UDS（仅双跳） |

> **三级拓扑刻意共用 15006**，不是省事：本环境的 920B 开着 firewalld 且没有 root
> 改不了规则，实测只有 15006 放行，另开的 15008 从 950 连过去报
> `No route to host`（ICMP host-prohibited，firewalld REJECT 的典型症状）。
> 共用一个端口也让防火墙、路由、conntrack 的行为在三级之间完全一致，
> 不会有额外差异混进级差。
>
> 要开新端口（在对端以 root 执行）：
> ```bash
> sudo firewall-cmd --permanent --add-port=15008/tcp && sudo firewall-cmd --reload
> firewall-cmd --list-ports        # 确认，不需要 root
> ```

---

## 3. 第 0 步：前置条件自检

### 3.1 「网络 ready」的可检验定义

"网络好不好"是个模糊说法。对这套构建来说，**ready 的操作性定义是下面六条同时成立**。
在 suzhou950 上跑：

```bash
sp() {  # $1=url $2=label —— 各取前 20MB 测速
  r=$(curl -sS -o /dev/null -m 25 -r 0-20000000 \
      -w "%{http_code} %{speed_download}" "$1" 2>&1)
  printf "%-40s http=%-4s %8.2f MB/s\n" "$2" "${r%% *}" \
      "$(echo "${r##* }/1048576" | bc -l)"
}
sp https://mirrors.aliyun.com/golang/go1.26.5.linux-arm64.tar.gz       "go (aliyun)"
sp https://mirrors.huaweicloud.com/bazel/8.7.0/bazel-8.7.0-linux-arm64 "bazel (huaweicloud)"
sp https://static.rust-lang.org/dist/channel-rust-stable.toml          "rust (static.rust-lang.org)"
sp https://codeload.github.com/envoyproxy/envoy/tar.gz/refs/heads/main "github archive (codeload)"
for u in https://github.com https://goproxy.cn; do
  printf "%-40s %s\n" "$u" "$(curl -sS -o /dev/null -m 15 -w 'http=%{http_code} %{time_total}s' $u)"
done
```

本次实测（走本机 Xray 代理）：

```
go (aliyun)                              http=206      2.70 MB/s
bazel (huaweicloud)                      http=206      4.39 MB/s
rust (static.rust-lang.org)              http=206      0.47 MB/s
github archive (codeload)                http=200      7.25 MB/s
https://github.com                       http=200 1.651907s
https://goproxy.cn                       http=200 1.957750s
```

**判读标准**：

| 项 | 及格线 | 不及格的后果 |
|---|---|---|
| `mirrors.aliyun.com` | > 1 MB/s | Go 下载慢，但只有 61 MB，能忍 |
| `mirrors.huaweicloud.com` | > 1 MB/s | 同上，62 MB |
| **`static.rust-lang.org`** | **> 1 MB/s** | **致命**。Envoy 有 Rust 扩展，`rules_rust` 在 analysis 阶段就要解析工具链，数百 MB 绕不过去。直连原始源实测只有 **29.6 KB/s**，要下 5–10 小时 |
| `codeload.github.com` | > 500 KB/s | bazel 的大部分依赖从这里拉 |
| `github.com/*/releases/download/*` | > 500 KB/s | `buf` 等工具从这里拉，直连实测 **5.9 KB/s** |
| `goproxy.cn` | 200 | Go module 拉不下来 |

**这几条里 `static.rust-lang.org` 和 GitHub release 是最容易不及格的**，
而且失败的表现是「构建静默停滞几小时」，不是报错。

不及格怎么办 → [runbook-build-environment.md](runbook-build-environment.md) §3 给了两条路：
bazel URL 重写（不需要代理）和 Xray 代理（需要自己的代理服务器）。**本次用的是后者。**

### 3.2 内网必须直连，不能走代理

```bash
ping -c 3 192.168.25.51        # RTT 应该是零点零几毫秒
ssh 192.168.25.51 'echo ok'    # 950 要能免密 ssh 到 920B
```

**如果你用了代理，务必确认私网流量不走它。** 950 ↔ 920B 这条链路是整个测量的核心，
绕道代理服务器再绕回来会让 RTT 从 0.068 ms 变成几十毫秒，
§9 的所有跨机结论直接作废。两层防护都要有：

- Xray 路由规则里把私网段指向 `direct` outbound
- `~/proxy.env` 里 `no_proxy` 包含 `192.168.0.0/16,10.0.0.0/8,172.16.0.0/12`

### 3.3 系统能力自检

```bash
command -v tmux git rsync unzip curl     # 四个都得有；tmux 是必须的，见 §7.1
ulimit -Hn                               # 硬限要 ≥ 65536（无需 sudo 即可提到硬限）
df -h  /home && df -i  /home             # 空间 ≥ 80G，且 inode 充足
df -T /tmp                               # 记下它是不是 tmpfs
nproc && free -g
```

`tmux` 不是可选的 —— §7.1 解释了为什么 Envoy 必须用 tmux 托管。

---

## 4. 第 1 步：装工具

两个工具，都**装在用户目录，不需要 sudo**。

### 4.1 Go

```bash
mkdir -p ~/sdk ~/dl
curl -fL -o ~/dl/go.tgz https://mirrors.aliyun.com/golang/go1.26.5.linux-arm64.tar.gz
tar -C ~/sdk -xzf ~/dl/go.tgz
~/sdk/go/bin/go version        # go version go1.26.5 linux/arm64
```

本次实测：下载 **11 秒**（61 MB），解压 **1 秒**。

**为什么用 aliyun 镜像**：`go.dev` 从国内机器很慢，aliyun 秒下。
**架构别选错**：`linux-arm64`，不是 `linux-amd64`。

Go 相关的三个环境变量，后面每次构建 Go 代码都要设：

```bash
export PATH=~/sdk/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off        # 校验和数据库在墙外，且我们用了 replace 指向本地源码树
export GOFLAGS=-mod=mod   # 允许 go build 自动更新 go.mod/go.sum
```

### 4.2 bazel —— 必须先取代码再装

**这一步和直觉相反：bazel 的版本不能自己定，要由 Envoy 源码决定。**

Envoy 仓库里有个 `.bazelversion` 文件写死了它要求的版本。
装错版本的话，bazel 启动时会发现不匹配，**自己去 `releases.bazel.build` 下载正确版本** ——
而那个源实测只有 75 KB/s，62 MB 要下十几分钟，还经常断。

所以正确顺序是：**先 clone（§5），读出版本号，再装 bazel**。

```bash
BV=$(cat ~/envoy_kitex/envoy/.bazelversion | tr -d '[:space:]')
echo "需要 bazel $BV"                    # 本次：8.7.0

mkdir -p ~/bin
curl -fL -o ~/bin/bazel \
  https://mirrors.huaweicloud.com/bazel/$BV/bazel-$BV-linux-arm64
chmod +x ~/bin/bazel
~/bin/bazel --version                    # bazel 8.7.0
```

本次实测：**9 秒**（62 MB，华为云 4.39 MB/s）。

**三个坑：**

1. **不要用 bazelisk。** 它只是个 launcher，最终还是去 `releases.bazel.build` 下 bazel 本体，
   绕不开慢源；而且它自己的 GitHub release 从国内也下不动。
2. **不要用 `releases.bazel.build`。** 实测 75 KB/s，华为云镜像 4~11 MB/s。
3. **`~/bin` 不在默认 PATH 里**（本机 PATH 只有 `/usr/local/bin:/usr/bin:/usr/local/sbin:/usr/sbin`）。
   脚本里一律写绝对路径 `~/bin/bazel`。

---

## 5. 第 2 步：取代码

### 5.1 五个仓库

```bash
mkdir -p ~/envoy_kitex && cd ~/envoy_kitex

git clone --depth 1 -b wip/kitex-e2e-probe        https://github.com/RiceReallyGood/envoy.git    envoy
git clone --depth 1 -b feat/detailed-trace-events https://github.com/RiceReallyGood/kitex.git    kitex
git clone --depth 1 -b feat/meshlab-read-probe    https://github.com/RiceReallyGood/netpoll.git  netpoll
git clone --depth 1 -b main                       https://github.com/RiceReallyGood/mesh-lab.git mesh-lab
git clone --depth 1 -b main  https://github.com/cloudwego/kitex-benchmark.git kitex-benchmark
```

本次实测（含 HEAD 校验）：

| 仓库 | 分支 | commit | 用时 | 大小 |
|---|---|---|---|---|
| envoy | `wip/kitex-e2e-probe` | `448c7f0f` | 10 s | 243 M |
| kitex | `feat/detailed-trace-events` | `98311385` | 3 s | 8.7 M |
| netpoll | `feat/meshlab-read-probe` | `64d86026` | 2 s | 948 K |
| mesh-lab | `main` | `6f77005f` | 2 s | 744 K |
| kitex-benchmark | `main` | `5d7d01ea` | 2 s | 2.2 M |

clone 完先对一下 commit，避免后面出了问题不知道是代码版本不对：

```bash
for d in envoy kitex netpoll mesh-lab kitex-benchmark; do
  printf "%-16s %s %s\n" $d "$(git -C $d rev-parse --short=8 HEAD)" \
                            "$(git -C $d branch --show-current)"
done
```

### 5.2 为什么必须在同一个父目录下

`mesh-lab/demo/go.mod` 结尾有四条 `replace`，用的是**相对路径**：

```go
replace github.com/cloudwego/kitex-benchmark => ../../kitex-benchmark
replace github.com/cloudwego/kitex           => ../../kitex
replace github.com/cloudwego/netpoll         => ../../netpoll
replace github.com/apache/thrift => github.com/apache/thrift v0.13.0
```

`../../` 是相对 `mesh-lab/demo/` 而言，也就是 `~/envoy_kitex/`。
放错位置会报 `directory does not exist`。

前三条把依赖指向**本地插过桩的源码树** —— 这就是打点生效的机制。
第四条把 `apache/thrift` 钉死在 v0.13.0：Kitex 的 `bthrift/apache` 兼容层只能配这个版本，
v0.14+ 给 `TProtocol` 的方法加了 `context` 参数，签名对不上直接编译失败。
`kitex-benchmark` 自己也做了同样的 replace（其 `go.mod:128`），
但 **Go 的 replace 指令不会被依赖方继承**，必须在本模块重复声明。

### 5.3 分支都是什么

| 仓库 | 分支 | 内容 |
|---|---|---|
| envoy | `feat/thrift-ttheader-transport` | **可上游的纯净版** —— 让 `thrift_proxy` 原生识别 Kitex 的 TTHeader（magic `0x1000`），含 47 个单测。**跑实验不用这个** |
| envoy | **`wip/kitex-e2e-probe`** | 基于上一分支追加侵入式打点。**实验用这个** |
| kitex | `feat/detailed-trace-events` | 12 个细粒度事件 + 承接 netpoll 时间戳的槽位 |
| netpoll | `feat/meshlab-read-probe` | 读路径 5 个点位（epoll 唤醒 / readv / 唤醒通知） |
| mesh-lab | `main` | 文档、demo、Envoy 配置、merge 工具、运行脚本 |
| kitex-benchmark | `main` | **未修改**，只借用它已生成的 echo `kitex_gen` 代码，省一次代码生成 |

### 5.4 关于 `--depth 1`

用浅克隆是因为 envoy 全history 有好几个 GB。代价是拿不到别的分支 ——
比如你想看纯净版的 `feat/thrift-ttheader-transport`，要单独补一次：

```bash
git -C envoy fetch --depth 1 origin feat/thrift-ttheader-transport
git -C envoy checkout -b feat/thrift-ttheader-transport FETCH_HEAD
```

**注意：切分支会让 bazel 大范围重编。** 想两个分支都编，用 `git worktree` 加不同的
`--output_base`，别在同一个目录里来回切。

---

## 6. 第 3 步：构建

**先启动 Envoy 构建，再做别的。** 它是唯一的长活。

### 6.1 Envoy

#### 6.1.1 构建脚本

不要在前台跑 —— ssh 断连会连带杀掉它。用 `nohup setsid` 扔到后台，
外面套一层**只在网络错误时重试**的循环：

```bash
cat > ~/build-envoy.sh <<'EOF'
#!/bin/bash
set -u
cd ~/envoy_kitex/envoy || exit 1
source ~/proxy.env          # 有代理时；没有就删掉这行
ulimit -n 65536
LOG=~/build-envoy.log
T0=$(date +%s)
echo "=== START $(date) === cpus=$(nproc) nofile=$(ulimit -n)" | tee -a $LOG

for i in 1 2 3 4 5; do
  echo "##### ATTEMPT $i  已缓存依赖=$(ls ~/bazel_out/external 2>/dev/null|wc -l) #####" | tee -a $LOG
  # 上一轮可能留下服务端锁，不清会报 "Another command is running"
  ~/bin/bazel --output_base=$HOME/bazel_out shutdown >/dev/null 2>&1
  pkill -9 -x java >/dev/null 2>&1
  sleep 2

  ~/bin/bazel --output_base=$HOME/bazel_out build -c opt \
      --curses=no --color=no --show_progress_rate_limit=30 \
      --experimental_repository_downloader_retries=2 \
      --http_timeout_scaling=1.0 \
      --cxxopt=-Wno-nullability-completeness \
      //source/exe:envoy-static >> $LOG 2>&1
  rc=$?
  echo "----- attempt $i rc=$rc -----" | tee -a $LOG
  [ $rc -eq 0 ] && break

  if tail -400 $LOG | grep -qE "^ERROR:.*(Compiling|Linking).*failed"; then
    echo "!!! 编译错误，停止重试" | tee -a $LOG; break
  fi
  if tail -400 $LOG | grep -qE "Error downloading|timed out|failed to fetch|Checksum was"; then
    echo ">>> 网络错误，30s 后重试" | tee -a $LOG; sleep 30; continue
  fi
  echo "!!! 非网络非编译错误，停止" | tee -a $LOG; break
done

echo "=== EXIT=$rc 总耗时 $(( ($(date +%s)-T0)/60 )) 分钟 ===" | tee -a $LOG
ls -lh bazel-bin/source/exe/envoy-static | tee -a $LOG
echo BUILD_FINISHED | tee -a $LOG
EOF
chmod +x ~/build-envoy.sh
nohup setsid ~/build-envoy.sh >/dev/null 2>&1 </dev/null &
```

**为什么重试要区分错误类型**：编译错误重试一万次也还是错，只会白烧几十分钟 CPU。
只有网络错误才值得重试。

**为什么每轮开头要 `bazel shutdown` + `pkill java`**：bazel 是 client/server 架构，
上一轮异常退出会留下 server 进程持有 output base 的锁，下一轮直接报
`Another command is running`。

#### 6.1.2 每个参数的理由

| 参数 | 为什么 |
|---|---|
| `--output_base=$HOME/bazel_out` | **不能用 `/tmp`**，见 §6.1.3 |
| `--cxxopt=-Wno-nullability-completeness` | 不加会在 `cel-cpp` 上编译失败，见 §6.1.4 |
| `-c opt` | 优化编译。测时延必须用 opt，dbg 的数据没有意义 |
| `--curses=no --color=no` | 否则进度被 `Computing main repo mapping` 反复覆盖，看不出卡在哪 |
| `--show_progress_rate_limit=30` | 30 秒一行，日志不会爆炸 |
| `--http_timeout_scaling=1.0` | **低于**默认的 6.0。快速失败，让外层重试接管 |
| `--experimental_repository_downloader_retries=2` | 应对镜像回源冷启动返回 504 |
| `ulimit -n 65536` | 默认软限 1024 不够；硬限 524288，**无需 sudo** |
| **不加** `--config=gcc` | 它连带 `--config=libstdc++`，要求系统有静态库 `libstdc++.a`（本机只有 `.so`） |
| **不加** `--config=clang` | 它会改 `host_platform` 和 `-stdlib=libc++`，让已完成的上万个编译动作缓存全部失效 |

**一个重要事实：bazel 会自行下载 LLVM 工具链并用它编译，完全不依赖系统的 gcc/clang。**
证据是编译错误里会出现
`external/llvm_toolchain/bin/cc_wrapper.sh --target=aarch64-unknown-linux-gnu`。
所以"系统 gcc 版本太老"这类担心不成立 —— 那只在启用 `--config=gcc` 时才是问题。
（这也解释了为什么 920B 的 gcc 10.3.1 无所谓：它根本不编译。）

#### 6.1.3 `--output_base` 不能放 `/tmp`

**症状**：编译阶段大面积报错

```
Compiling c/dec/state.c failed: Could not copy inputs into sandbox:
  .../llvm_minimal_linux_arm64/include/c++/v1/... (No space left on device)
```

**但空间明明够**：

```
df -h /tmp   →  690G 总量，用了 5.6G，可用 685G   （1%）
df -i /tmp   →  1,048,576 inode，用了 843,548     （81%，失败时 100%）
```

**根因是 inode 耗尽，不是空间不足。** tmpfs 的挂载参数写死了 `nr_inodes=1048576`，
与容量无关。而 bazel 用几百路并行，每个 sandbox 都要为 LLVM 头文件创建成千上万个符号链接
—— **inode 消耗与并行度成正比，与数据量无关**。

改 `nr_inodes` 要 root，所以改用 `/home`：

| | `/tmp` (tmpfs) | `/home` (ext4 on NVMe) |
|---|---|---|
| inode 总量 | 1,048,576 | **231,944,192** |
| 可用空间 | 685 G | 6.0 T |

**这是个通用规律，不是本机特有**：凡是「空间充足却报 No space left on device」，
先 `df -i`。tmpfs、小分区、容器的 overlay 层都可能中招。

#### 6.1.4 `cel-cpp` 的 nullability 编译错误

**症状**：

```
external/cel-cpp/common/internal/reference_count.h:179:36:
  error: pointer is missing a nullability type specifier
         (_Nonnull, _Nullable, or _Null_unspecified)
         [-Werror,-Wnullability-completeness]
```

**不是 aarch64 特有问题。** 这是个 Clang 版本敏感的告警：一旦某个翻译单元里用到了
`_Nonnull`/`_Nullable`，Clang 就要求同一文件内所有指针都标注。Envoy 开了 `-Werror`，
告警即错误。

Envoy 上游已知此问题，在 `.bazelrc` 里两处做了豁免（`build:macos` 和 `common:clang-common`），
但默认配置命不中。所以要手动加这一条。

> **查找上的坑**：`grep '^build:clang' .bazelrc` **查不到** clang config，
> 因为它的前缀是 `common:` 而不是 `build:`（`common:` 对所有 bazel 命令生效）。
> 据此很容易误判成"Envoy 没有 clang config"。

#### 6.1.5 怎么看它有没有在动

构建卡住时日志只显示 `Analyzing: ...`，**不告诉你在等谁**。标准排查序列：

| 步骤 | 命令 | 看什么 |
|---|---|---|
| 1 | `ls ~/bazel_out/external \| wc -l` | 依赖数不涨 = 卡在拉取 |
| 2 | `du -sm ~/bazel_out` | 总量不涨，同上 |
| 3 | **`lsof -p $(pgrep -x java)`** | **最有效**。看正在写哪个文件 → 直接指出是哪个依赖 |
| 4 | `ss -tan \| grep :443` | 有 ESTAB 且收发队列为 0 = 对端不发数据；没有连接 = 在退避里干等 |
| 5 | 反查对端 IP | `185.199.x` = GitHub 静态 CDN；`140.82.x` = github.com；`172.64.x` = Cloudflare |

两个容易误判的地方：

- 盯**单个文件**大小以为停滞 —— 其实那个文件下完了，转去下同 toolchain 的下一个。应看目录总量。
- 只看 `ss ... state established` —— 会漏掉 `SYN_SENT`（连不上的情况）。用 `ss -tan` 看全状态。

#### 6.1.6 本次实测

**一次通过，零重试，17 分钟。**

```
=== START Fri Aug  7 17:16:46 CST 2026 === cpus=384 nofile=65536
########## ATTEMPT 1  17:16:46  已缓存依赖=0 ##########
INFO: Analyzed target //source/exe:envoy-static (1571 packages loaded, 71777 targets configured).
INFO: Elapsed time: 1018.943s, Critical Path: 423.59s
INFO: 18572 processes: 7717 internal, 10853 linux-sandbox, 1 local, 1 worker.
INFO: Build completed successfully, 18572 total actions
=== EXIT=0 Fri Aug  7 17:33:49 CST 2026 总耗时 17 分钟 ===
-r-xr-xr-x. 1 f00620085 f00620085 810M Aug  7 17:33 bazel-bin/source/exe/envoy-static
```

| 指标 | 值 |
|---|---|
| 总耗时 | **1018.9 s（17 分钟）** |
| **关键路径** | **423.6 s（7 分钟）** —— 并行度再高也压不到这个数以下 |
| 总 action 数 | 18,572 |
| 加载的包 / 配置的 target | 1,571 / 71,777 |
| 外部依赖数 | **960 个** |
| 产物大小 | **810 MB** |
| 重试次数 | **0** |

**从 0 个依赖开始的真·冷启动** —— `~/.cache/bazel` 和 `~/bazel_out` 都是空的。
960 个依赖全部现拉，没有一次超时。这条印证了 §3.1：网络达标时，
依赖拉取根本不是瓶颈；不达标时它是唯一的瓶颈。

**关键路径 423.6 s 值得单独看**：它是依赖链最长的那条串行路径。
总耗时 1018.9 s 里有 595 s 是并行度不够（384 核也不够）造成的。
换句话说，核数再翻倍也只能省掉这 595 s 的一部分，**7 分钟是这台机器的下界**。

### 6.2 Go 侧

Envoy 在后台编的时候做这个，互不干扰。

```bash
export PATH=~/sdk/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOFLAGS=-mod=mod

cd ~/envoy_kitex/mesh-lab/demo
mkdir -p bin
go build -o bin/server ./server
go build -o bin/client ./client

# merge 是独立 module（meshlab/merge），必须单独构建
cd ~/envoy_kitex/mesh-lab/tools/merge
go build -o ~/envoy_kitex/mesh-lab/demo/bin/merge .
```

本次实测：**35 秒**（含拉 34 个 module）。产物：

```
client   21M
merge   3.1M
server   20M
```

### 6.3 构建后自检 —— 三条，一条都别省

这三条不做的话，你会跑完压测才发现没数据，白跑一轮。

**① Envoy 的十个打点符号真的编进去了吗**

```bash
E=~/envoy_kitex/envoy/bazel-bin/source/exe/envoy-static
miss=0
for s in dn_first_byte up_conn_new up_conn_reused up_encode_done up_epoll_wake \
         up_first_byte up_readv_done up_readv_start up_socket_write_done up_write_done; do
  printf "  %-22s " $s
  if strings $E | grep -qx "$s"; then echo 在; else echo 缺失; miss=$((miss+1)); fi
done
echo "  --> 缺失 $miss 个"
```

任何一个"缺失"都说明编的是没插桩的分支（比如切到了
`feat/thrift-ttheader-transport`），或者插桩代码被条件编译掉了。
点位各自的含义见 [probe-points.md](probe-points.md)。

> 用 `grep -qx`（`-x` = 整行匹配）而不是 `grep -q`。`strings` 从一个 810 MB 的二进制里
> 会抽出几百万条字符串，只要有任意一条**包含**这个点位名（比如某段日志格式串、
> 某个调试符号），`grep -q` 就会误报"在"。`-x` 要求整行恰好等于点位名，
> 而探针的点位名在 `probe.cc` 里就是独立的字符串字面量，正好是一行。

**② netpoll 探针的良率**

```bash
cd ~/envoy_kitex/netpoll && go test -race -run TestReadProbe -v -count=1 .
```

本次实测四个用例全过，关键指标是**请求—响应模式良率 200/200 = 100.0%**。
低于 90% 说明探针在丢数据，后面的 netpoll 相关结论都不可信。

**③ 跨机的二进制能不能跑**（§2.3 ① 的验证）

```bash
scp ~/envoy_kitex/envoy/bazel-bin/source/exe/envoy-static 192.168.25.51:/tmp/
ssh 192.168.25.51 '/tmp/envoy-static --version'
```

能打印版本号就说明 glibc 兼容。报 `version 'GLIBC_2.3x' not found` 就说明构建机的
glibc 太新，得换台机器编。

**本次三条自检的结果：**

```
===== ① Envoy 打点符号 =====
  dn_first_byte 在   up_conn_new 在   up_conn_reused 在   up_encode_done 在
  up_epoll_wake 在   up_first_byte 在  up_readv_done 在    up_readv_start 在
  up_socket_write_done 在              up_write_done 在
  --> 缺失 0 个

===== ② netpoll 读路径探针 -race =====
  --- PASS: TestReadProbeHappensBefore          (0.01s)
  --- PASS: TestReadProbeYieldOnRequestResponse (0.23s)   请求—响应模式良率: 200/200 = 100.0%
  --- PASS: TestReadProbeGateBalanced           (0.00s)
  --- PASS: TestReadProbeResetClearsStaleData   (0.00s)
  ok  github.com/cloudwego/netpoll  1.284s

===== ③ 跨机 glibc 兼容 =====
  [950  glibc 2.38] envoy-static version: 448c7f0f.../1.40.0-dev/Clean/RELEASE/BoringSSL
  [920B glibc 2.34] envoy-static version: 448c7f0f.../1.40.0-dev/Clean/RELEASE/BoringSSL   ✅
```

③ 之所以通过，是因为 `envoy-static` 只把 C++ 侧静态链接了，glibc 仍是动态链接
（`ldd` 只有 `libm/librt/libdl/libpthread/libc` 五个），而它用到的都是很老的符号。
**这不是必然的** —— 换个更新的构建机或用到新 glibc 特性的代码就会断，所以每次换机器都要重测。

---

## 7. 第 4 步：跑起来

拓扑分两类，**先用同机的把链路调通，再用跨机的出数据**。

**第一类：同机降级，只为验证链路通不通**

| 拓扑 | 脚本 | 验证什么 | 跑不通说明 |
|---|---|---|---|
| 直连 | `run-direct.sh` | Kitex client/server 与打点本身 | demo 或探针有问题，跟 Envoy 无关 |
| 单跳 | `run-single-hop.sh` | Envoy 能不能解 TTHeader、路由能不能匹配、打点能不能落盘 | 协议或 Envoy 打点有问题 |
| 双跳 | `run-two-hop.sh` | 两个 Envoy 级联、TCP 那一跳、`base-id` 隔离 | Envoy 之间的对接有问题 |

**第二类：跨机归因阶梯，出数据用这个**

同一个 `run-cross-machine.sh`，用 `TOPO` 选级别：

```bash
TOPO=direct ./scripts/run-cross-machine.sh start    # client ─────TCP──▶ server
TOPO=single ./scripts/run-cross-machine.sh start    # client ▶ envoy-out ─TCP─▶ server
TOPO=two    ./scripts/run-cross-machine.sh start    # 完整双跳（TOPO 可省，默认 two）

./scripts/run-cross-machine.sh target               # 打印本级 client 该打的地址
```

**为什么归因必须用跨机的三级，不能用同机的三级**：同机每加一跳就多一组 Envoy 进程
挤同一批 CPU，三级的资源竞争程度根本不同，级差里混着 CPU 争抢，不能归给「多一跳」。
跨机版三级都保留同一段跨机 TCP，每级只多一个 sidecar，级差才是干净的。

**做阶梯对照时必须给 `single` 和 `two` 设同一个 `ENVOY_CONCURRENCY`**，
否则两级默认按核数开 worker（950 是 384 个），线程规模差异会混进级差。

> `two` 这一级还有一个必须知道的差别：server 在双跳下是 **UDS 监听、对端是本机的
> envoy-in**，而直连/单跳下是 **TCP 监听、对端是跨机的 950**。这不是配置疏漏，
> 而是「加一个入向 sidecar」的定义本身 —— 它终结远端连接、在本地重新发起。
> 但读 `kitex-server` 那一节的数字时要知道三级之间它的处境不同。

### 7.1 三个必须的细节，少一个就跑不起来

| 细节 | 不做会怎样 |
|---|---|
| **`ulimit -n 65536`** | Envoy 启动报 `Too many open files`，但**只是 warn** —— 进程看似起来了实则不可用 |
| **每个 Envoy 实例 `--base-id` 不同** | Envoy 的热重启机制：同 base-id 的新实例启动时会通过共享域套接字**通知旧实例退出**。同机跑两个 Envoy 不设就会互相杀死 |
| **用 tmux 托管，不能用 `nohup setsid`** | Envoy 注册了 SIGTERM 处理器，ssh 会话结束时会收到 SIGTERM 优雅退出（日志里是 `caught ENVOY_SIGTERM`）。**Go 进程和 xray 在同样条件下能存活，Envoy 不行** |

三个 `run-*.sh` 都已经内置了这三条。手工起进程时才需要自己注意。

**验证是否真的在监听，不能只看 socket 文件在不在**：

```bash
ss -xln | grep kitex-demo     # 文件存在 ≠ 有人 listen
```

### 7.2 单跳

```
client --UDS--> envoy --UDS--> server
```

```bash
cd ~/envoy_kitex/mesh-lab
./scripts/run-single-hop.sh start
./scripts/run-single-hop.sh status

cd demo
KITEX_PROBE_HOST=suzhou950 ./bin/client -n 300 -d 0 -c 1 -sample 1.0
```

`-n 300 -d 0 -c 1 -sample 1.0` = 单并发发 300 个请求、全采样。
**验证阶段一定要 `-sample 1.0`**，请求数少，要每条都有。

**本次实测：**

```
  app.sock : 正在监听
  out.sock : 正在监听

2026/08/07 17:58:39 [诊断] 实际传输协议 = TTHeader|Framed
2026/08/07 17:58:39 首个响应: action="echo" msg 长度=128

=== 压测结果 ===
并发=1 时长=31ms 请求=300 失败=0 QPS=9575
延迟 p50=82µs p90=99µs p99=213µs p999=327µs max=3.892ms
[probe] node=kitex-client 总请求=300 采样=300 丢弃=0

落盘: client 7500 条事件 / server 5901 条 / envoy 4096 条
```

**这一行要特别看**：

```
[诊断] 实际传输协议 = TTHeader|Framed
```

不是 `TTHeader`，而是 `TTHeader|Framed` —— **这是正常的，不是配错了**。
Kitex 的 `rpcConfig.SetTransportProtocol` 是**按位或**不是覆盖
（`pkg/rpcinfo/rpcconfig.go:173-181`），所以即使显式设了
`client.WithTransportProtocol(transport.TTHeader)`，实际生效的也是两个标志位的组合。
这层内层 framed 前缀正是当初 Envoy 报 `invalid binary protocol version 0x0000` 的原因，
现在 TTHeader transport 已经能正确处理它。**看到这行说明链路是对的。**

另外那条 `WARNING: dynamicgo only support amd64 && go1.17~go1.24` 是
Kitex 依赖在 aarch64 上的常规告警，不影响正确性，只是走了非加速路径。

### 7.3 双跳（同机）

```
client --UDS--> envoy-out --TCP:15006--> envoy-in --UDS--> server
```

```bash
./scripts/run-two-hop.sh start
./scripts/run-two-hop.sh status
cd demo && KITEX_PROBE_HOST=suzhou950 ./bin/client -n 300 -d 0 -c 1 -sample 1.0
```

**本次实测：**

```
  app.sock      : 监听中
  tcp :15006    : 监听中
  out.sock      : 监听中
  envoy 进程数  : 2 (应为 2)

并发=1 时长=52ms 请求=300 失败=0 QPS=5690
延迟 p50=149µs p90=187µs p99=465µs p999=492µs max=4.185ms
```

单跳 82 µs → 双跳 149 µs，**多出来的 67 µs 就是多一跳 sidecar 的代价**。

#### 7.3.1 这一步暴露了脚本里的一个真实 bug（已修）

第一次跑的时候 `status` 报：

```
  app.sock      : 监听中
  tcp :15006    : 未监听        ← 但压测明明成功了
  out.sock      : 监听中
```

**链路是通的，检查却说没通。** 这类"检查本身撒谎"的问题最耽误事 ——
照着做的人会以为环境没起来，去查根本不存在的问题。

根因是 bash 的一个经典陷阱：

```bash
set -o pipefail                        # 脚本第 8 行
ss -tln | grep -q ":15006"             # 期望 rc=0，实际 rc=141
```

`grep -q` **命中第一行就立刻退出**，此时 `ss` 还在往管道里写剩下的行，
写入无人读取的管道会收到 **SIGPIPE**，退出码 `128+13 = 141`。
而 `pipefail` 的语义是「取管道中最后一个非零退出码」，于是整条管道返回 141 而不是 grep 的 0。

**为什么偏偏是 `:15006` 这一条翻车**，`app.sock` 和 `out.sock` 却正常？

| 检查项 | `ss` 输出的匹配行数（实测） | 结果 |
|---|---|---|
| `app.sock` (UDS) | **1** | `ss` 写完就退了，碰不到 SIGPIPE → rc=0 ✅ |
| `out.sock` (UDS) | **1** | 同上 ✅ |
| **`:15006` (TCP)** | **384** | **必然 SIGPIPE → rc=141** ❌ |

384 行是因为 **Envoy 的每个 worker 线程各自 `SO_REUSEPORT` bind 同一个端口**，
而默认 worker 数 = CPU 核数 = 384。UDS 监听器不做 reuseport，所以只有 1 行 ——
**这就是为什么三项检查里只有 TCP 那一项翻车。**

**这也解释了它为什么藏了这么久**：之前的实验一直用 `ENVOY_CONCURRENCY=2`，
只有 2 行输出，`ss` 写完就退，永远碰不到。这次为了对照才用了默认 worker 数，一次就撞上。

实测确认：

```
带 pipefail:      ss -tln | grep -q ":15006"   →  rc=141
不带 pipefail:                                 →  rc=0
匹配只有 1 行时:                                →  rc=0
改用 grep -c:                                  →  rc=0, 计数=384
```

**修法**：先把输出收进变量，用 here-string 喂给 grep，根本不走管道。
三个 `run-*.sh` 都已按此修正：

```bash
uds=$(ss -xln 2>/dev/null)
tcp=$(ss -tln 2>/dev/null)
grep -qF -- "$RUN/app.sock" <<<"$uds" && echo 监听中 || { echo 未监听; ok=1; }
grep -qF -- ":15006"        <<<"$tcp" && echo 监听中 || { echo 未监听; ok=1; }
```

**这是条通用规律，不止本项目**：`set -o pipefail` 与任何**提前退出的读端**
（`grep -q`、`head -n`、`grep -m N`）组合，只要写端输出足够多就会假失败。
自己写检查脚本时留意。

### 7.4 跨机（真实拓扑，三级阶梯）

```bash
cd ~/envoy_kitex/mesh-lab

# 完整双跳 —— TOPO 可省，默认就是 two
ENVOY_CONCURRENCY=4 ./scripts/run-cross-machine.sh start   # 自动 rsync 二进制到 920B
./scripts/run-cross-machine.sh status                      # 三个监听点都要通

# 阶梯的另外两级
TOPO=direct ENVOY_CONCURRENCY=4 ./scripts/run-cross-machine.sh start
TOPO=single ENVOY_CONCURRENCY=4 ./scripts/run-cross-machine.sh start

# 别自己拼 client 的目标地址，问脚本
tgt=$(TOPO=single ./scripts/run-cross-machine.sh target)
```

脚本会把 `envoy-static` 和 `server` 推到 920B、起进程、检查监听。
`direct` 与 `single` 不需要对端有 Envoy，所以只推 `server`（省掉 810 MB 的比对）。

**`ENVOY_CONCURRENCY=2` 是什么**：Envoy 默认按 CPU 核数开 worker 线程（本机 384 个）。
核数远多于连接数时，每个 worker 一次 epoll 只拿到零星事件，
**「事件循环排队」这个点位恒为几百纳秒，测不出任何东西**。
要观察排队必须把 worker 数压下来 —— 实测 384 → 2 时该值从 290 ns 涨到 21 µs。

#### 7.4.1 本次实测：两轮

**第 1 轮 —— 链路验证**（300 请求，单并发，全采样）：

```
[同步] 推送二进制与配置到 suzhou920B
  [950] out.sock   : 监听中
  [920B] :15006    : 可达
  [920B] app.sock  : 监听中
  start 用时 19s        ← 含 rsync 810MB 二进制过局域网

并发=1 时长=76ms 请求=300 失败=0 QPS=3903
延迟 p50=223µs p90=242µs p99=644µs
```

**第 2 轮 —— 正式压测**（64 并发，40 秒，5% 采样）：

```
并发=64 时长=40.001s 请求=1280157 失败=0 QPS=32003
延迟 p50=1.929ms p90=2.605ms p99=3.779ms p999=5.62ms max=171.857ms
采样率=0.0500
[probe] node=kitex-client 总请求=1280157 采样=64225 丢弃=0
```

**128 万请求，零失败。** 采样落在 64,225 条 trace 上，数据量 **1.2 GB**。

先确认 `KITEX_PROBE_HOST` 生效了 —— merge 输出里两个 Envoy 必须挂在不同 host 上：

```
── envoy-out [host=suzhou950]      ← 对
── envoy-in  [host=suzhou920B]     ← 对
```

**如果两个都显示 `localhost.localdomain`，立刻停下改环境变量再重跑** ——
跨机会被静默当成同机，§9.3 ③ 的纪律就失效了，算出来的数是错的而且看不出来。

### 7.5 环境变量

| 变量 | 谁需要 | 说明 |
|---|---|---|
| `KITEX_PROBE_HOST` | 全部进程 | **两台机器必须不同**。默认取 hostname，而本环境两台都叫 `localhost.localdomain`，一样的话跨机会被**静默误判成同机**，差值法的前提就没了 |
| `KITEX_PROBE_PATH` | Envoy | 打点输出路径。**不设则完全不落盘，且不报错** |
| `KITEX_PROBE_NODE` | Envoy | 节点名，如 `envoy-out` / `envoy-in` |
| `KITEX_PROBE_DISABLE=1` | — | 探针代码在二进制里但完全不激活，用作打点开销对照组 |

三个 `run-*.sh` 都已内置。

### 7.6 压测参数与采样率

```bash
./bin/client \
  -target /tmp/kitex-demo/out.sock \   # UDS 路径或 host:port
  -service echo-server \               # 写入 TTHeader 的 ToService，供 Envoy 路由
  -c 16 \                              # 并发
  -d 60s \                             # 时长；-d 0 时改用 -n 指定请求数
  -sample 0.05 \                       # 采样率
  -size 128                            # payload 字节数
```

| 场景 | 采样率 | 理由 |
|---|---|---|
| 验证链路 / 调试 | `1.0` | 请求数少，要每条都有 |
| 压测归因 | `0.01` ~ `0.05` | 实测 100% 采样有 **6.6% 开销**，会改变被测系统行为；1% 时开销低于 3% 的噪声下限 |
| 极限吞吐 | `0` | 完全关闭 |

---

## 8. 第 5 步：采集数据

### 8.1 顺序不能换

```bash
./scripts/run-cross-machine.sh stop
sleep 5
./scripts/run-cross-machine.sh collect
```

**为什么必须先 stop**：事件先进内存缓冲，**线程退出时才刷盘**。进程还活着时读到的文件
永远缺最后一批 —— 实测 300 个请求只有 273 条落盘，剩下的还在内存里。

`collect` 把 920B 上的 trace 拉回 950，走两机间的局域网（实测 112 MB/s），
**不经开发机**（那条路只有 ~150 KB/s）。

**正确顺序永远是**：`stop` → 清理 → `start` → 压测 → `stop` → `collect`。

### 8.2 三个关于文件的坑

**① Envoy 按线程分文件。** 实际文件名是 `trace-envoy-out.ndjson.<tid>`，
不是 `trace-envoy-out.ndjson`。**用 `*.ndjson*` 通配，用 `*.ndjson` 一个都匹配不到。**
（早期清理脚本就是因为这个清不干净，残留文件污染了下一次分析。）

**② 绝对不要在进程运行时 `rm` trace 文件。** 探针持有长开的 `FILE*`，
删掉之后写入会进到已删除的 inode，进程退出时数据直接消失，**而且没有任何报错**。
踩过一次，200 个请求的数据全丢。

**③ 数据量不小。** 60 秒 5% 采样、40k QPS 会产出约 1 GB
（client 495 MB + server 409 MB + 上百个 Envoy 分线程文件）。
`/tmp` 是 tmpfs 的话，这 1 GB 直接占内存。

---

## 9. 第 6 步：分析

### 9.1 merge 的五种输出

```bash
cd ~/envoy_kitex/mesh-lab/demo
FILES="/tmp/kitex-demo/trace-client.ndjson /tmp/kitex-demo/trace-server.ndjson \
       /tmp/kitex-demo/trace-envoy-out.ndjson.* /tmp/kitex-demo/trace-envoy-in.ndjson.*"

./bin/merge -format detail $FILES                              # 分阶段分位数
./bin/merge -format table -limit 0 $FILES > trace-table.csv    # 全量逐条 CSV
./bin/merge -format waterfall -limit 3 $FILES                  # 看 3 条个案
```

| 格式 | 用途 |
|---|---|
| `summary` | 各节点总时长，最粗 |
| **`detail`** | **各阶段分位数，日常主力** |
| **`table`** | **逐条 trace 的 CSV，导进 pandas 自己筛** |
| `waterfall` | 单条请求的瀑布图，排查个案 |
| `chrome` | Chrome tracing 格式，在 `chrome://tracing` 里看 |

`table` 格式一行一条 trace、一列一个时间段（单位微秒）。
开头四行是**全量样本**的 `__avg__` / `__p50__` / `__p90__` / `__p99__`（不受 `-limit` 影响），
注释行以 `#` 开头：

```python
import pandas as pd
df  = pd.read_csv("trace-table.csv", comment="#")
agg = df[ df.trace_id.str.startswith("__")]   # 聚合行
raw = df[~df.trace_id.str.startswith("__")]   # 逐条数据
```

`detail` 只给分位数，看不到单条请求内部各段的相关性 ——
比如"尾延迟那些请求到底卡在排队还是网络"。`table` 保留原始逐条数据就是为了回答这类问题。

### 9.2 本次实测结果

跑完你应该拿到这样的东西。先看最粗的 `summary`（压测轮，64,225 条 trace）：

```
区间                          p50        p90        p99        max
链路开销(往返)                1.923ms    2.597ms    3.725ms  171.394ms
envoy-in                551.7µs    915.5µs    1.186ms   64.447ms
envoy-out               1.540ms    2.145ms    2.634ms   90.099ms
kitex-client            1.941ms    2.617ms    3.761ms  171.408ms
kitex-server             16.2µs     24.0µs     61.6µs    2.713ms
```

#### 9.2.1 两个负载点的对比 —— 这是本次复现最有价值的一张表

同一套代码、同一条链路，**只把负载从 1 并发提到 64 并发**：

| p50 | 验证轮（1 并发，3.9k QPS） | 压测轮（64 并发，32k QPS） | 倍数 |
|---|---|---|---|
| 端到端（kitex-client 总时长） | 216.6 µs | **1.941 ms** | 9.0× |
| envoy-out 自身处理 | 15.8 µs | 23.0 µs | 1.5× |
| envoy-in 自身处理 | 28.9 µs | 27.7 µs | **1.0×** |
| 业务 handler | 240 ns | 250 ns | **1.0×** |
| **envoy-out 事件循环排队** | **120 ns** | **30.2 µs** | **252×** |
| **envoy-in 事件循环排队** | **280 ns** | **57.2 µs** | **204×** |
| client goroutine 调度延迟 | 2.7 µs | 6.1 µs | 2.3× |
| **两跳 sidecar 自身占端到端** | **≈ 20.6 %** | **≈ 2.6 %** | |

（"自身处理" = 该节点总时长 − 等待上游。按 §9.3 ① 这是量级估计，不是精确加减。）

**一句话读法**：端到端涨了 9 倍，但**没有任何一段"处理"变慢** ——
Envoy 的解析、路由、编码，Kitex 的编解码，业务 handler，全都纹丝不动。
涨的全是**排队**：事件循环排队涨了两百多倍。

这正是插桩的意义所在。不插桩你只能看到"压力上来延迟从 216 µs 涨到 1.9 ms"，
然后开始猜是不是 Envoy 慢了、要不要换个代理。插了桩才能确定：
**代理一点没变慢，是请求在等 worker**。处理方式完全不同 —— 前者要换软件，后者只要加 worker。

#### 9.2.2 验证轮的结论与 test-report 一致

验证轮（低负载、无排队）的分解：

| 项 | p50 | 占端到端 216.6 µs |
|---|---|---|
| 两跳 sidecar 自身处理 | ≈ 44.7 µs | **≈ 20.6 %** |
| 业务 handler | 240 ns | 0.1 % |
| 其余（进程间传输 + 等待 + 框架编解码） | ≈ 171 µs | ≈ 79 % |

**拿什么对标**：这个占比在三次独立测量里落在同一区间 ——

| 测量 | 参数 | 两跳 sidecar 自身占端到端 |
|---|---|---:|
| 2026-08-06 版报告 | c=16、128 B、worker 默认 | 20.1 % |
| 本次复现（上面这组） | c=1、128 B、`ENVOY_CONCURRENCY=2` | **20.6 %** |
| [test-report.md](test-report.md) 的归因阶梯 | c=1、64 B、`ENVOY_CONCURRENCY=4` | 19.1 % |

**落在 19 %~21 % 就说明环境复现成功了**（区间宽度来自 payload 大小与 worker 数的差异）。
明显偏出这个区间，先核对附录 D 的 commit，再核对 §7.5 的环境变量。

#### 9.2.3 一条 waterfall 长什么样

```
╔══ trace 0000f5862e518367 ══
║  涉及 4 个节点，2 台主机（跨机，时长仅在同机内精确）
║
║ ▸ envoy-in         [host=suzhou920B]  本地区间 821.1µs
║          0ns  dn_first_byte          ← 收到下游第一个字节
║        4.2µs  hdr_decoded            ← TTHeader 解析完
║        5.1µs  msg_begin
║        5.8µs  route_resolved         ← 路由匹配完
║        9.1µs  up_conn_reused         ← 复用了上游连接（不是新建）
║       16.3µs  up_encode_done
║       17.0µs  up_socket_write_done   ← 写给 kitex-server 了
║       18.0µs  req_done
║      804.3µs  up_epoll_wake          ← ★ 中间这 786µs 全在等
║      806.8µs  up_readv_start
║      811.2µs  up_readv_done
║      818.9µs  resp_decoded
║      821.1µs  rpc_done
║ ▸ kitex-server     [host=suzhou920B]  本地区间 18.1µs
║          0ns  mesh_netpoll_onread
║        2.6µs  mesh_first_byte
║        5.7µs  mesh_hdr_decode_finish
║       11.9µs  server_handle_start
║       12.1µs  server_handle_finish   ← ★ 业务逻辑只有 200ns
║       17.8µs  mesh_socket_write_finish
║       18.1µs  rpc_finish
```

排查个案时最有用 —— 一眼能看出时间断层在哪两个点之间。
上面这条里 envoy-in 等了 786 µs，而它等的 kitex-server 只花了 18.1 µs，
**中间的差额就是排队**。

#### 9.2.4 采集与分析本身的开销

| 操作 | 耗时 |
|---|---|
| `collect`（从 920B 拉 390 MB 过局域网） | **4 秒**（≈ 98 MB/s） |
| `merge -format detail`（64,225 条 trace / 1.2 GB） | **17 秒** |
| `merge -format table -limit 0` → CSV | 25 MB / 64,237 行 |

### 9.3 读数据的三条纪律

**① 分位数不可加。** 各列的 p50 之和 **≠** 总时长的 p50。
分解表只能用来判断量级与占比，**不能做加减**。

**② `net.*` 列不是纯线路时间。** 它是 `(外层等待 − 内层总时长) / 2`，
而内层的"总时长"从 `dn_first_byte` 起算 —— 请求在这之前还可能在 listener/worker 队列里等过，
那段落在测量区间之外，全被这个差值吸收了。
实测把 worker 从 384 压到 2，同机 UDS 的"单程"从 21 µs 涨到 128 µs，
**多出来的一百微秒是排队不是传输**。只有低负载无排队时这一列才近似真实传输时间。

除以 2 还隐含"去程回程对称"的假设。单条 trace 上该值可能为负（两次独立测量的噪声），属正常。

**③ 跨机只能得往返，拆不出单向。** 拆单向需要 PTP + 硬件时间戳网卡，
这是物理限制，不是插桩能力问题。

### 9.4 时钟偏斜自检

两台机器的时钟不同步不影响结论 —— 分析用的是**差值法**（两个各自在本机测得的时长相减），
对偏斜免疫。想验证就注入一个人工偏移，**各段时长应该逐位不变**：

```bash
./bin/merge -format detail --inject-skew kitex-server=+50 $FILES > /tmp/skew.txt
./bin/merge -format detail                              $FILES > /tmp/noskew.txt
diff -q /tmp/skew.txt /tmp/noskew.txt && echo "✅ 完全一致 —— 分析对时钟偏斜免疫"
```

**本次实测**：

```
✅ 完全一致 —— 分析对时钟偏斜免疫

两机时钟实际偏差：950=1786097470.081  920B=1786097454.611  差 = -15.47 s
```

两台机器的墙钟差了 **15.47 秒**，而注入额外 50 秒偏移后，
`detail` 的输出与不注入时**逐字节相同**。这就是差值法免疫的证据。

> 唯一受影响的是 `waterfall` 的视觉呈现 —— 时间轴会错乱。
> 想让它好看就把两台机器的 NTP 开起来。本环境没开。

> 唯一受影响的是 `waterfall` 的视觉呈现 —— 时间轴会错乱。
> 想让它好看就把两台机器的 NTP 开起来。

---

## 10. 清理与重跑

### 10.1 只重跑实验（保留构建产物）

```bash
./scripts/run-cross-machine.sh stop
sleep 5
rm -rf /tmp/kitex-demo
ssh 192.168.25.51 'rm -rf /tmp/kitex-demo'
```

### 10.2 彻底清空重来

**这里有个坑：`rm -rf ~/bazel_out` 会大面积报 `Permission denied`。**

bazel 把 output tree 里的产物目录设成只读（`rules_go` 的 stdlib 尤其明显），
`GOPATH/pkg/mod` 也是只读的（Go 故意的，防止你改依赖源码）。
`rm -rf` 删不掉只读目录里的文件，**但退出码仍然是 0** ——
表现为"删完了，可是目录还在，还占着几百 MB"，而且不看输出根本发现不了。

正确做法是**先 `chmod -R u+w` 再删**：

```bash
for p in ~/bazel_out ~/go ~/.cache/bazel ~/.cache/go-build; do
  [ -e "$p" ] || continue
  chmod -R u+w "$p" 2>/dev/null
  rm -rf "$p"
  [ -e "$p" ] && echo "!!! $p 没删干净" || echo "$p 已删除"
done
rm -rf ~/envoy_kitex ~/sdk ~/bin ~/dl /tmp/kitex-demo
ssh 192.168.25.51 'rm -rf ~/meshlab /tmp/kitex-demo'
```

**每步都验证删掉了**，别信退出码。

（`bazel clean --expunge` 也能干这事且更正规，但前提是 bazel 二进制还在、
`.bazelversion` 对得上；上面这套 `chmod` 方案对"已经删了一半"的现场更管用。）

---

## 11. 排错速查

| 现象 | 原因 | 处理 |
|---|---|---|
| bazel 报 `No space left on device` 但 `df -h` 有空间 | tmpfs **inode 耗尽** | `df -i` 确认；`--output_base` 换到 ext4/xfs（§6.1.3） |
| `cel-cpp` 编译失败 `nullability` | 缺豁免参数 | 加 `--cxxopt=-Wno-nullability-completeness`（§6.1.4） |
| bazel 报 `Another command is running` | 上一轮的 bazel server 没退 | `bazel shutdown` + `pkill -9 -x java` |
| bazel 自己去下载别的版本 | `.bazelversion` 与已装版本不符 | 按 §4.2 装匹配版本 |
| 构建静默停滞几十分钟 | 卡在某个慢源 | `lsof -p $(pgrep -x java)` 定位（§6.1.5） |
| **Envoy 一个点位都没有** | 没设 `KITEX_PROBE_PATH`/`KITEX_PROBE_NODE`。**不落盘且不报错** | §7.5 |
| Envoy 启动报 `Too many open files` | fd 软限 1024 不够，**而且只是 warn** | `ulimit -n 65536` |
| 两个 Envoy 互相杀死 | `--base-id` 相同，热重启机制生效 | 给不同 base-id |
| ssh 一断 Envoy 就没了 | Envoy 收到 SIGTERM 优雅退出 | 用 tmux 托管（§7.1） |
| socket 文件在，但连不上 | 进程早死了 | `ss -xln \| grep kitex-demo` 才是真检查 |
| trace 文件比预期少一截 | 最后一批还在内存，线程退出才刷盘 | 先 `stop` 再读；用 SIGTERM 不要 `kill -9` |
| 明明跑了却一条数据都没有 | 在进程运行时 `rm` 过 trace 文件 | §8.2 ② |
| `*.ndjson` 匹配不到文件 | Envoy 按线程分文件，名字带 `.<tid>` | 用 `*.ndjson*` |
| 跨机分析出负数时延 | 两机 `KITEX_PROBE_HOST` 相同 | 两台设不同值 |
| **跨机端口连不上，报 `No route to host`** | 对端 firewalld REJECT（不是 `Connection refused`，那是没人监听）。**没 root 看不了规则**（`firewall-cmd --list-all` 报 `Authorization failed`） | 换用已放行的端口；或让管理员 `firewall-cmd --permanent --add-port=<p>/tcp && firewall-cmd --reload`。判定方法：两端各起一个 `nc -l` 分别测，能区分「防火墙挡了」与「服务没起来」 |
| 归因阶梯的级差数值反直觉 | 三级的 Envoy worker 数不同，或用了同机拓扑做阶梯 | 三级设同一个 `ENVOY_CONCURRENCY`；阶梯必须用跨机的 `TOPO=direct/single/two`（§7.4） |
| waterfall 时间轴错乱 | 两机时钟未同步 | 不影响结论（§9.4），只是不好看 |
| 「事件循环排队」恒为几百纳秒 | worker 数远多于连接数，压根没排队 | `ENVOY_CONCURRENCY=2` |
| `rm -rf ~/bazel_out` 删不干净 | bazel 产物目录只读，退出码仍为 0 | 先 `chmod -R u+w`（§10.2） |
| **`status` 报未监听但链路是通的** | `set -o pipefail` + `grep -q`：grep 提前退出让 `ss` 吃 SIGPIPE（rc=141），pipefail 取了 141。**只在匹配行数多时触发** | 先收进变量再 `grep -qF <<<"$var"`（§7.3.1） |
| Go 报 `directory does not exist` | 五个仓库没放在同一父目录 | §5.2 |
| `pkill -f xxx` 把自己的 shell 杀了 | `-f` 匹配完整命令行，**执行它的那条命令自己也含 pattern** | 一律用 `pkill -x <进程名>` |
| ControlMaster socket 僵死，所有 ssh 挂起 | — | `ssh -O exit` + 删 socket 文件 |
| `ControlPath too long` | socket 路径必须 < 108 字节（`sockaddr_un.sun_path` 限制） | 换到 `/tmp/cm-xxx` |

---

## 附录 A：本次复现的完整时间线

2026-08-07，两台机器先清空到只剩系统（`~/bazel_out`、`~/.cache/bazel`、
`~/envoy_kitex`、`~/sdk`、`~/go`、`~/bin`、`/tmp/kitex-demo` 全删），
从下载 Go 开始：

| 时刻 | 阶段 | 用时 | 结果 |
|---|---|---|---|
| 17:12 | 网络前置自检 | 1 min | 六项全绿 |
| 17:15 | 下载解压 Go 1.26.5 | 12 s | 61 MB @ 2.70 MB/s |
| 17:15 | clone 五个仓库 | 20 s | envoy 10 s / 其余各 2–3 s |
| 17:16 | 读 `.bazelversion` → 装 bazel 8.7.0 | 9 s | 62 MB @ 4.39 MB/s |
| **17:16→17:33** | **Envoy 全量构建** | **17 min** | 一次通过，18,572 actions，960 依赖，810 MB |
| 17:17 | Go 侧构建（与上一步并行） | 35 s | server/client/merge + netpoll 探针测试全过 |
| 17:34 | 构建后自检三条 | 1 min | 10/10 点位、良率 100%、920B 能跑 |
| 17:58 | 单跳实跑 | 1 min | 300 请求 0 失败，p50 = 82 µs |
| 18:00 | 双跳实跑 | 1 min | 300 请求 0 失败，p50 = 149 µs；**发现 `pipefail` bug 并修复** |
| 18:05 | 跨机双跳 · 验证轮 | 2 min | 300 请求 0 失败，p50 = 223 µs |
| 18:09 | 跨机双跳 · 压测轮 | 3 min | **128 万请求 0 失败，32k QPS**，1.2 GB 数据 |
| 18:13 | 采集 + 分析 + 时钟自检 | 1 min | 归因结论与 test-report 一致（20.6% vs 20.1%） |

**净耗时约 45 分钟**，其中 17 分钟是 Envoy 构建。

### A.1 本次新发现的三个问题

复现的价值一半在于把旧文档的坑走一遍，另一半在于发现新的。这次发现了三个：

**① `rm -rf ~/bazel_out` 删不干净，且退出码为 0**

bazel 把 output tree 里的产物目录设成只读，`GOPATH/pkg/mod` 同理。
第一次清理时 46 GB 只删到剩 719 MB，`rm` 报了几百行 `Permission denied`，
**但脚本的退出码是 0**。必须先 `chmod -R u+w`。详见 §10.2。

**② `set -o pipefail` + `grep -q` 让 `status` 谎报"未监听"**

链路明明是通的却报未监听，原因是 `ss` 吃到 SIGPIPE 返回 141。
**只在匹配行数多时才触发**，所以以前用 `ENVOY_CONCURRENCY=2` 一直没暴露。
三个 `run-*.sh` 已修。详见 §7.3.1。

**③ 旧文档把 bazel 版本写死在装工具那一步，顺序是反的**

版本必须由 `envoy/.bazelversion` 决定。写死的话，换个 Envoy 版本就会触发
bazel 自己去 `releases.bazel.build`（75 KB/s）下载正确版本，
等于前面配的镜像全白费。本文改成"先 clone 再装 bazel"。详见 §4.2。

### A.2 与预期不符的地方

**Envoy 构建比预期快得多。** 事前按经验估的是 1.5–3 小时，实际 17 分钟。
原因是 384 核 + 依赖零阻塞。**但关键路径是 423.6 s** —— 核数少的机器不会有这个速度，
按核数线性外推：32 核约 3 小时，64 核约 1.5 小时，且都不可能低于 7 分钟。

## 附录 B：已排除的方案

复现时不用再试这几条，都验证过不可行：

| 方案 | 为什么不行 |
|---|---|
| 官方 Envoy 构建容器 `envoyproxy/envoy-build-ubuntu` | ① `registry-1.docker.io` 国内超时拉不动 ② 本机 docker 18.09（2018 年），无 buildx/多架构 manifest ③ **更根本的是它只提供工具链不提供依赖**，几百个外部依赖仍要 bazel 现拉，慢源一个躲不掉。而工具链问题本就不存在（bazel 自带 LLVM，§6.1.2） |
| 反向 SSH 隧道借开发机的代理 | 开发机（WSL2）的代理是 **Windows 侧监听**，`ss` 里都看不见，forwarded socket 够不着 |
| `--distdir` 预下载依赖 | 依赖总量 GB 级，开发机到目标机只有 ~150 KB/s，传输不现实 |
| 从开发机 rsync 源码 | Envoy 源码 244 MB，按 150 KB/s 要二十多分钟且容易断。**目标机直接 clone 只要 10 秒** |

**背后是一条通用原则：大数据绝不经开发机中转。** 源码在目标机 clone，
工具走国内镜像，开发机只传 KB 级的 diff。同一个 bazel 二进制：目标机从华为云 9 秒，
从开发机传要 7 分钟。

## 附录 C：一次完整复现的命令序列

前面每一步都解释了"为什么"。这里是把"怎么做"抽出来的纯命令版，可以直接抄。
**在 suzhou950 上执行**，`<PEER_IP>` 换成你的第二台机器。

```bash
# ───────── 0. 前置（网络自检见 §3.1，这里假定已通过）─────────
source ~/proxy.env                     # 有代理时；没有就跳过
ulimit -n 65536

# ───────── 1. Go ─────────
mkdir -p ~/sdk ~/dl ~/bin
curl -fL -o ~/dl/go.tgz https://mirrors.aliyun.com/golang/go1.26.5.linux-arm64.tar.gz
tar -C ~/sdk -xzf ~/dl/go.tgz
export PATH=~/sdk/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off GOFLAGS=-mod=mod
go version

# ───────── 2. 代码（五个仓库，必须同父目录）─────────
mkdir -p ~/envoy_kitex && cd ~/envoy_kitex
git clone --depth 1 -b wip/kitex-e2e-probe        https://github.com/RiceReallyGood/envoy.git    envoy
git clone --depth 1 -b feat/detailed-trace-events https://github.com/RiceReallyGood/kitex.git    kitex
git clone --depth 1 -b feat/meshlab-read-probe    https://github.com/RiceReallyGood/netpoll.git  netpoll
git clone --depth 1 -b main                       https://github.com/RiceReallyGood/mesh-lab.git mesh-lab
git clone --depth 1 -b main  https://github.com/cloudwego/kitex-benchmark.git kitex-benchmark

# ───────── 3. bazel（版本由源码决定，所以放在 clone 之后）─────────
BV=$(tr -d '[:space:]' < ~/envoy_kitex/envoy/.bazelversion)
curl -fL -o ~/bin/bazel https://mirrors.huaweicloud.com/bazel/$BV/bazel-$BV-linux-arm64
chmod +x ~/bin/bazel && ~/bin/bazel --version

# ───────── 4. Envoy（后台，17 min @384 核）─────────
#   build-envoy.sh 的完整内容见 §6.1.1
nohup setsid ~/build-envoy.sh >/dev/null 2>&1 </dev/null &

# ───────── 5. Go 侧（与上一步并行，35 s）─────────
cd ~/envoy_kitex/mesh-lab/demo && mkdir -p bin
go build -o bin/server ./server && go build -o bin/client ./client
cd ../tools/merge && go build -o ~/envoy_kitex/mesh-lab/demo/bin/merge .

# ───────── 6. 等 Envoy 编完，做三条自检（§6.3）─────────
until grep -q BUILD_FINISHED ~/build-envoy.log; do sleep 30; done
tail -4 ~/build-envoy.log

# ───────── 7. 改掉硬编码的机器名/IP（§2.4）─────────
grep -rn "suzhou\|192\.168\.25\." ~/envoy_kitex/mesh-lab/{scripts,envoy-conf}

# ───────── 8. 跑：验证轮（先确认链路通）─────────
cd ~/envoy_kitex/mesh-lab
export ENVOY_CONCURRENCY=4                         # 三级取同一个值，见 §7.4
./scripts/run-cross-machine.sh start
./scripts/run-cross-machine.sh status              # 三项必须全通
tgt=$(./scripts/run-cross-machine.sh target)
cd demo && KITEX_PROBE_HOST=<本机名> ./bin/client -target "$tgt" -n 300 -d 0 -c 1 -sample 1.0

# ───────── 9. 跑：归因阶梯（三级 × 三轮，交错）─────────
#   交错而非分组连跑：后跑的组会白占缓存预热与 CPU 频率爬升的便宜。
cd ~/envoy_kitex/mesh-lab
for r in 1 2 3; do
  for topo in direct single two; do
    TOPO=$topo ./scripts/run-cross-machine.sh stop >/dev/null 2>&1; sleep 2
    rm -rf /tmp/kitex-demo; mkdir -p /tmp/kitex-demo
    TOPO=$topo ./scripts/run-cross-machine.sh start >/dev/null 2>&1
    TOPO=$topo ./scripts/run-cross-machine.sh status >/dev/null || { echo "$topo r$r 未起来"; continue; }
    tgt=$(TOPO=$topo ./scripts/run-cross-machine.sh target)
    ( cd demo && KITEX_PROBE_HOST=<本机名> ./bin/client \
        -target "$tgt" -service echo-server -c 1 -size 64 -d 20s -sample 0.05 )
    # 顺序不能换：stop（线程退出才刷盘）→ collect（拉回对端数据）
    TOPO=$topo ./scripts/run-cross-machine.sh stop >/dev/null 2>&1; sleep 5
    TOPO=$topo ./scripts/run-cross-machine.sh collect >/dev/null 2>&1
    mkdir -p ~/ladder/$topo/r$r && mv /tmp/kitex-demo/trace-* ~/ladder/$topo/r$r/
  done
done

# ───────── 10. 采集（单次实验时的顺序，不能换）─────────
cd ~/envoy_kitex/mesh-lab
./scripts/run-cross-machine.sh stop
sleep 5
./scripts/run-cross-machine.sh collect

# ───────── 11. 分析 ─────────
cd demo
FILES="/tmp/kitex-demo/trace-client.ndjson /tmp/kitex-demo/trace-server.ndjson \
       /tmp/kitex-demo/trace-envoy-out.ndjson.* /tmp/kitex-demo/trace-envoy-in.ndjson.*"
./bin/merge -format summary  $FILES
./bin/merge -format detail   $FILES | tee /tmp/kitex-demo/detail.txt
./bin/merge -format table -limit 0 $FILES > /tmp/kitex-demo/trace-table.csv
./bin/merge -format waterfall -limit 1 $FILES
# 时钟偏斜自检：两次输出应逐字节相同
./bin/merge -format detail --inject-skew kitex-server=+50 $FILES > /tmp/skew.txt
./bin/merge -format detail                              $FILES > /tmp/noskew.txt
diff -q /tmp/skew.txt /tmp/noskew.txt && echo "✅ 对时钟偏斜免疫"
```

**做对照实验时（改代码前后、换网卡前后）必须按轮次交错跑，不能一组跑完再跑另一组。**
后跑的那组会白占缓存预热和 CPU 频率爬升的便宜 —— 踩过这个坑，
当时得出过"加了插桩反而快 4.9%"这种物理上不可能的结论。

---

## 附录 D：五个仓库的 commit（本次）

| 仓库 | 分支 | commit |
|---|---|---|
| envoy | `wip/kitex-e2e-probe` | `448c7f0f` |
| kitex | `feat/detailed-trace-events` | `98311385` |
| netpoll | `feat/meshlab-read-probe` | `64d86026` |
| mesh-lab | `main` | `6f77005f` |
| kitex-benchmark | `main` | `5d7d01ea` |

复现出的数字和 [test-report.md](test-report.md) 对不上时，先核对这张表。
