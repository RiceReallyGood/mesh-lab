# 内网离线部署手册：把构建结果搬到不能上网的机器上跑

- 日期：2026-08-09
- 适用：公司内网机器**完全不能上外网**（拉不了 bazel 依赖，也拉不了 Go module），
  但你想在上面跑这套 Kitex × Envoy 打点实验
- 本文所有体积、链接方式、glibc 需求都是 2026-08-09 在 suzhou950 上**实测**的，不是估算

> **本文假定两个前提，你已确认成立：**
> 1. 内网机器与构建机**CPU 架构相同**（本项目是 aarch64）。这条**没有前提就没有办法**，见 §1.1。
> 2. 内网机器的 **glibc 版本 ≥ 构建机**。有这条保证，glibc 兼容性就不是问题了 ——
>    §1.2 保留了机制说明和实测数据，但你可以跳过它。

---

## 0. 先破除一个误解

**跑这套实验不需要 bazel，不需要 Go，不需要任何依赖。**

这是最重要的一句话。你担心的「内网拉不了 bazel 依赖和 Go 依赖」——那是**构建期**的事。
构建产物是几个自包含的可执行文件，运行期一个依赖都不用拉。

具体说，`envoy-static` 这个名字就是字面意思：Envoy 的几百个 C++ 依赖
（BoringSSL、protobuf、abseil、V8、re2……）在构建时就**静态链接进了这一个文件**。
运行时它不去找任何 `.so`，也不去找任何配置以外的东西。

其实这个结论你们的环境早就验证过了：跨机实验里的 **suzhou920B 就是一台不需要外网的机器**
（见 [runbook-reproduce.md](runbook-reproduce.md) §2.1）。它上面跑的 `envoy-static` 和
`server` 都是在 950 上编好后用 `rsync` 推过去的，920B 自己不编译任何东西、不下载任何东西。
**你的内网机器就是第二个 920B。**

所以本文真正要回答的只有三个问题：

| 问题 | 章节 |
|---|---|
| 内网机器跟构建机兼容吗？（架构、glibc） | §1 |
| 拷哪些文件、多大？ | §2 |
| 拷过去之后怎么跑？ | §3 |

只有当你**还想在内网改代码重新编译**时，才需要看 §4（Go 侧）和 §5（Envoy 侧）。

### 0.1 三档需求对照

| 档位 | 你要做的事 | 需要搬运 | 体积 |
|---|---|---|---|
| **A（推荐）** | 只跑实验、出数据 | 4 个二进制 + 配置 + 脚本 | 853 MB，**strip + zstd 后仅 31 MB**（§2.4） |
| B | 还要改 Go 侧（demo/kitex/netpoll）重编 | A + Go 工具链 + vendor 目录 | +310 MB |
| C | 还要改 Envoy 的 C++ 打点重编 | B + bazel + 依赖缓存 + Envoy 源码 | +3.3 GB 起，且有坑（§5） |

**绝大多数情况你只需要 A。** 打点位置的增删才需要 C，而那件事建议仍然在能上网的机器上做，
编完再把新的 `envoy-static` 推过去——毕竟一次全量构建只要 17 分钟。

---

## 1. 第一步：兼容性自检

二进制兼容性有两条约束，都跟内网不内网无关。**在你的场景里只剩第一条要管**：

| 约束 | 你的场景 | 结论 |
|---|---|---|
| CPU 架构必须一致 | 需要你确认 | **仍是硬约束，必须核对**（§1.1） |
| glibc 只能低编高跑 | 你已保证内网 ≥ 构建机 | **不是问题，可跳过**（§1.2 仅存档） |

### 1.1 约束一：CPU 架构必须一致（硬约束，无解）

构建机 suzhou950 是 **aarch64**。编出来的二进制**只能在 aarch64 上跑**。
内网机器如果是 x86_64，这些文件一个都用不了，`file` 会告诉你 `ARM aarch64`，
执行会报 `cannot execute binary file: Exec format error`。

**没有绕过的办法**——不能转译，不能兼容层。只能在一台 x86_64 的、能上网的机器上重新编一遍
（照 [runbook-reproduce.md](runbook-reproduce.md) 走一遍，把 §4 里下载 Go 和 bazel 的
`arm64` 改成 `amd64` 即可，其余完全一样）。

### 1.2 约束二：glibc —— **你的场景已排除，本节仅存档**

> **你已确认内网 glibc ≥ 构建机，这一条就不用管了，可以直接跳到 §2。**
> 保留本节是因为：换构建机、或者以后有人拿这份产物去别的环境时，还需要这些数据。

C/C++ 程序链接 glibc 符号时会带上版本号（如 `memcpy@GLIBC_2.14`）。
glibc 保证**向后兼容**：老符号在新版本里一直保留。所以
**「低版本编、高版本跑」永远安全，反过来才会报 `version 'GLIBC_2.xx' not found`。**

**你的方向正是安全的那个**，而且比本项目已经验证过的还要安全一档 ——
我们实测是在 glibc **2.38** 上编、拿到 **2.34** 上跑（**高编低跑**，本来有风险的那个方向），
结果也通过了（§1.4 ③）。你的场景（低编高跑）严格更宽松。

实测本项目各二进制的 glibc 需求，留作存档：

| 二进制 | 链接方式 | 最高 glibc 符号需求 | 能跑在 |
|---|---|---|---|
| `envoy-static` | 动态链接 glibc（其余全静态） | **GLIBC_2.30** | glibc ≥ 2.30 |
| `server` / `client`（默认编法） | **动态**（CGO 开启） | **GLIBC_2.34** | glibc ≥ 2.34 |
| `server` / `client`（`CGO_ENABLED=0`） | **完全静态** | 无 | 任何 |
| `merge` | 完全静态（纯 Go） | 无 | 任何 |

**`envoy-static` 只依赖 5 个最基础的库**，实测 `ldd` 输出：

```
libm.so.6  librt.so.1  libdl.so.2  libpthread.so.0  libc.so.6
```

几百个 C++ 依赖（BoringSSL、protobuf、abseil、V8、re2……）全部静态链接进去了。
**GLIBC_2.30 对应 2019 年的 glibc，2.34 对应 2021 年**，任何还在维护的发行版都满足。

**在你的场景里，`CGO_ENABLED=0` 不是必需的**，默认编法直接拷过去就能跑。
它只是一个可选的加固手段，好处是产物彻底不依赖任何 `.so`，以后换到别的机器不用再验一次。
代价基本为零（13 秒重编一次），所以 §2.2 把它列为可选步骤。

> 顺带解释一个容易困惑的点：Go 程序默认不是静态链接吗？
> 不是——只要用到 `net` 包，Go 默认开 CGO 走系统的 NSS 解析器（所以 `ldd` 里有 `libresolv`）。
> 关掉 CGO 后 Go 用纯 Go 实现的 DNS 解析器。**本实验只用 UDS 路径和显式 IP，不做域名解析，
> 所以开不开 CGO 都不改变任何行为。**

### 1.3 自检脚本（在**内网机器**上跑）

```bash
echo "架构:   $(uname -m)          # 必须与构建机一致（本项目是 aarch64）"
echo "glibc:  $(ldd --version | head -1 | grep -o '[0-9]\+\.[0-9]\+$')"
echo "内核:   $(uname -r)"
echo "核数:   $(nproc)"
echo "nofile: soft=$(ulimit -Sn) hard=$(ulimit -Hn)   # 硬限需 >= 65536"
echo "/tmp:   $(df -T /tmp | tail -1 | awk '{print $2, $5"用量"}')   # tmpfs 的话 trace 占内存"
command -v tmux >/dev/null && echo "tmux:   有" || echo "tmux:   缺（必须装，见 §3.2）"
```

### 1.4 收到文件后的验证（在内网机器上跑）

```bash
file  ./envoy-static | head -c 80; echo
ldd   ./envoy-static
./envoy-static --version        # 能打印版本号 = 兼容性 OK
```

`--version` 能打印，就说明架构和 glibc 都过关了，可以放心往下走。
**这一条 30 秒的检查请务必做**——它是唯一能一次性证明「架构对 + glibc 对 + 文件没传坏」的动作。

（按你的前提，这里唯一可能失败的原因是**架构不匹配**，报错会是
`cannot execute binary file: Exec format error`，不是 glibc 那种报错。）

---

## 2. 第二步：拷什么（档位 A）

### 2.1 清单

在**构建机**上，这些是全部要搬的东西：

| 来源路径（构建机上） | 体积 | 作用 |
|---|---|---|
| `envoy/bazel-bin/source/exe/envoy-static` | **810 MB** | Envoy 本体，含全部打点 |
| `mesh-lab/demo/bin/server` | 20 MB | Kitex 服务端 + 打点 |
| `mesh-lab/demo/bin/client` | 21 MB | Kitex 客户端 + 压测器 + 打点 |
| `mesh-lab/demo/bin/merge` | 3.1 MB | trace 合并分析工具 |
| `mesh-lab/envoy-conf/` | 36 KB | 6 份 Envoy 配置 |
| `mesh-lab/scripts/` | 44 KB | run-*.sh 运行脚本 |
| **合计** | **约 855 MB** | |

**注意 `bazel-bin` 是个符号链接**，直接 `scp` 它会失败或拷到链接本身。
下面的打包脚本用了 `readlink -f` 解引用。

### 2.2 打包（在构建机上执行）

**直接用现有产物即可**——你已保证内网 glibc ≥ 构建机，默认编法的动态链接没有问题。

<details>
<summary><b>可选加固：把 Go 侧编成完全静态</b>（不影响本次部署，点开看）</summary>

好处是产物彻底不依赖任何 `.so`，将来换到任何机器都不用再验兼容性。
代价是重编 13 秒。对本实验的行为**没有任何影响**（不做域名解析，见 §1.2）。

```bash
export PATH=~/sdk/go/bin:$PATH
cd ~/envoy_kitex/mesh-lab/demo
CGO_ENABLED=0 go build -o bin/server ./server
CGO_ENABLED=0 go build -o bin/client ./client
cd ~/envoy_kitex/mesh-lab/tools/merge
CGO_ENABLED=0 go build -o ~/envoy_kitex/mesh-lab/demo/bin/merge .

# 确认三个都是 statically linked
file ~/envoy_kitex/mesh-lab/demo/bin/* | sed 's/,.*\(statically\|dynamically\)/ -> \1/'
```

（注意 `envoy-static` 无论如何都是动态链接 glibc 的，这一步只影响 Go 那三个。）

</details>

打包：

```bash
R=~/envoy_kitex
P=/tmp/meshlab-offline
rm -rf $P && mkdir -p $P/bin $P/envoy-conf $P/scripts

cp "$(readlink -f $R/envoy/bazel-bin/source/exe/envoy-static)" $P/bin/
cp $R/mesh-lab/demo/bin/{server,client,merge}                  $P/bin/
cp $R/mesh-lab/envoy-conf/*.yaml                               $P/envoy-conf/
cp $R/mesh-lab/scripts/*.sh                                    $P/scripts/
chmod +x $P/bin/* $P/scripts/*

# 带上校验和，内网收到后要核对（大文件传输出错不罕见）
( cd $P && sha256sum bin/* > SHA256SUMS )

tar -C /tmp -czf /tmp/meshlab-offline.tgz meshlab-offline
ls -lh /tmp/meshlab-offline.tgz
```

**实测（2026-08-09，suzhou950）**：未压缩 **853 MB** → `tar czf` 后 **160 MB**，耗时 **21 秒**。

**160 MB 还是太大？先别急着换压缩算法 —— 那 810 MB 里 85% 是调试符号。** 见 §2.4。

### 2.3 内网机器上落地

```bash
tar -xzf meshlab-offline.tgz -C ~/      # 精简包按 §2.4 换成对应的解压命令
mv ~/meshlab-offline ~/meshlab
cd ~/meshlab && sha256sum -c SHA256SUMS      # 必须全 OK
./bin/envoy-static --version                 # §1.4 的兼容性验证
```

### 2.4 精简包：853 MB → 126 MB，压缩后 24 MB

**适用场景：内网传输有大小上限**（常见是 100 MB）。

#### 2.4.1 先看清 810 MB 里装的是什么

```bash
readelf -S -W envoy-static | ...    # 按 section 大小排序
```

实测前几大 section：

| section | 大小 | 是什么 |
|---|---:|---|
| `.gdb_index` | **412.6 MB** | gdb 调试索引 |
| `.debug_line` | **185.8 MB** | 行号调试信息 |
| `.strtab` + `.symtab` | 77.9 MB | 符号表 |
| **`.text`** | **58.6 MB** | **真正的可执行代码** |

**调试信息占了 85%，代码只有 58.6 MB。** 所以第一刀砍在这里，而不是换压缩算法。

#### 2.4.2 三步瘦身

```bash
# ① strip envoy-static：810 MB → 97 MB
#    注意 bazel 产物是只读的，直接 strip 会报 Permission denied，
#    用 install 复制一份可写的（install 会同时设好 755）
E=$(readlink -f ~/envoy_kitex/envoy/bazel-bin/source/exe/envoy-static)
install -m755 "$E" /tmp/envoy-static-slim
strip --strip-all /tmp/envoy-static-slim

# ② Go 侧加 -ldflags="-s -w"：44 MB → 29 MB
export PATH=~/sdk/go/bin:$PATH
cd ~/envoy_kitex/mesh-lab/demo
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin-slim/server ./server
CGO_ENABLED=0 go build -ldflags="-s -w" -o bin-slim/client ./client
cd ../tools/merge && CGO_ENABLED=0 go build -ldflags="-s -w" -o ../../demo/bin-slim/merge .

# ③ 组包时用这些精简产物，然后挑一个压缩算法（见下表）
tar -C /tmp -I 'xz -9 -T0' -cf /tmp/meshlab-slim.tar.xz meshlab-offline
```

单个二进制的前后对比（实测）：

| 二进制 | 原 | 精简后 |
|---|---:|---:|
| `envoy-static` | 810 MB | **97 MB** |
| `client` | 20.6 MB | 15 MB |
| `server` | 19.4 MB | 14 MB |
| `merge` | 3.0 MB | 2.1 MB |
| **整包（未压缩）** | **853 MB** | **126 MB** |

#### 2.4.3 压缩算法对比（都在精简包上实测，384 核）

| 方案 | 体积 | 耗时 | 解压命令 |
|---|---:|---:|---|
| 原始 gzip 包（**未 strip**） | 159.8 MB | 21 s | — |
| **strip + gzip** | **43.0 MB** | **6 s** | `tar -xzf` |
| **strip + zstd -19 -T0** | **31.3 MB** | 11 s | `tar -I zstd -xf` |
| **strip + xz -9 -T0** | **24.1 MB** | 47 s | `tar -xf`（GNU tar 自动识别） |

**三个方案全部远低于 100 MB。** 注意看这张表的重点：

> **瘦身的功劳几乎全在 strip（160 → 43 MB），换算法只是锦上添花（43 → 24 MB）。**
> 如果只换压缩算法不 strip，xz 也只能把 160 MB 压到一百多 MB，仍然超限。

**推荐 `zstd -19`**：31 MB，11 秒，解压也快。除非你的传输上限卡得特别死才用 xz
（xz 压缩 47 秒、解压也明显慢于 zstd，换来 7 MB）。
内网机器没有 `zstd` 的话就用 gzip，43 MB 一样够用。

#### 2.4.4 代价：失去符号化的调试能力

strip 掉的是 `.symtab` / `.debug_*` / `.gdb_index`，所以：

- ❌ `perf` / `gdb` 看到的是地址不是 C++ 函数名，崩溃栈也没有符号
- ✅ **打点功能完全不受影响** —— 10 个点位名是 `.rodata` 里的字符串字面量，不在符号表里
- ✅ 运行行为、性能完全一致

**所以：构建机上的那份未 strip 的 810 MB 原件不要删。** 需要剖析 Envoy 内部时在构建机上做，
内网只放精简版跑实验。

#### 2.4.5 精简包实跑验证（2026-08-09 实测）

从 xz 包解压后直接跑同机单跳，**与完整版行为一致**：

```
sha256sum -c SHA256SUMS   →  bin/{envoy-static,client,server,merge} 全部 OK
out.sock: 监听中
[诊断] 实际传输协议 = TTHeader|Framed
并发=1 时长=30ms 请求=300 失败=0 QPS=9806
延迟 p50=82µs p90=101µs p99=207µs
落盘: client 7500 / envoy 4096 / server 6300 条
[probe] node=kitex-server 总请求=300 采样=300 丢弃=0      ← 完整性判据通过
merge -format summary 正常输出
```

p50 82 µs 与完整版的 79–80 µs 在噪声范围内。**strip 不影响任何测量结果。**

> 如果你的传输上限比 24 MB 还小，用 `split -b 20M meshlab-slim.tar.xz part-`
> 切片传，收到后 `cat part-* > meshlab-slim.tar.xz` 合并，再核对 sha256。

---

## 3. 第三步：怎么跑

### 3.1 目录布局：脚本有路径假设

`run-*.sh` 里写死了这个结构（`ROOT=$HOME/envoy_kitex`）：

```
$HOME/envoy_kitex/
├── envoy/bazel-bin/source/exe/envoy-static
└── mesh-lab/
    ├── demo/bin/{server,client,merge}
    ├── envoy-conf/*.yaml
    └── scripts/run-*.sh
```

**两条路，任选一条：**

**① 照搬原布局**（改动最小，推荐）——把解压出来的东西摆成上面的样子：

```bash
mkdir -p ~/envoy_kitex/envoy/bazel-bin/source/exe ~/envoy_kitex/mesh-lab/demo/bin
cd ~/meshlab
cp bin/envoy-static      ~/envoy_kitex/envoy/bazel-bin/source/exe/
cp bin/{server,client,merge} ~/envoy_kitex/mesh-lab/demo/bin/
cp -r envoy-conf scripts ~/envoy_kitex/mesh-lab/
```

**② 不摆布局，直接手工起进程**——见 §3.4，你需要自己处理 §3.2 那三条。

**Envoy 配置里的路径不用改**：实测 6 份 YAML 里出现的绝对路径**全部**在
`$RUN/` 下（`out.sock`、`app.sock`、access log），没有任何 `$HOME` 硬编码。
只有当你的 `/tmp` 不可写、或要换目录时才需要 `sed` 一遍。

### 3.2 三个必须的细节，少一个就跑不起来

这三条抄自 [runbook-reproduce.md](runbook-reproduce.md) §7.1，因为它们是**运行期**约束，
跟你在哪台机器上跑无关，而且**每一条的失败表现都具有欺骗性**：

| 细节 | 不做会怎样 |
|---|---|
| **`ulimit -n 65536`** | Envoy 启动报 `Too many open files`，但**只是 warn**——进程看似起来了，实则不可用 |
| **每个 Envoy 实例 `--base-id` 不同** | Envoy 的热重启机制：同 base-id 的新实例启动时会通过共享域套接字**通知旧实例退出**。同机跑两个 Envoy 不设就会互相杀死 |
| **用 tmux 托管，不能用 `nohup setsid`** | Envoy 注册了 SIGTERM 处理器，ssh 会话结束时收到 SIGTERM 会优雅退出。**Go 进程在同样条件下能存活，Envoy 不行** |

`run-*.sh` 三个脚本都已内置这三条。手工起进程（§3.4）时才要自己注意。

### 3.3 打点相关的环境变量——**漏设不报错，只是没数据**

| 变量 | 谁需要 | 说明 |
|---|---|---|
| `KITEX_PROBE_PATH` | Envoy | 打点输出路径。**不设则完全不落盘，且不报任何错** |
| `KITEX_PROBE_NODE` | Envoy | 节点名，如 `envoy-out` / `envoy-in` |
| `KITEX_PROBE_HOST` | 全部进程 | **跨机时两台必须不同**。默认取 hostname；如果你们内网机器 hostname 都一样（很常见），跨机会被**静默误判成同机**，分析结论直接作废 |
| `KITEX_PROBE_DISABLE=1` | — | 探针在二进制里但不激活，用作打点开销对照组 |

**`KITEX_PROBE_PATH` 这条是最容易踩的**：你会跑完一整轮压测，才发现 Envoy 一条数据都没有。

### 3.4 单机快速验证（不依赖脚本布局）

搬过去之后想最快确认「能跑」，用同机单跳，三个终端：

```bash
# 准备
# 运行目录按用户隔离 —— 共享机上写死 /tmp/kitex-demo 会与他人撞车
RUN=${MESHLAB_RUN:-/tmp/kitex-demo-$(id -un)}
mkdir -p "$RUN" && ulimit -n 65536
cd ~/meshlab

# 终端 1：server
KITEX_PROBE_HOST=$(hostname) \
  ./bin/server -addr "$RUN/app.sock" -trace "$RUN/trace-server.ndjson"

# 终端 2：envoy（tmux 里跑）
KITEX_PROBE_HOST=$(hostname) \
KITEX_PROBE_PATH="$RUN/trace-envoy.ndjson" \
KITEX_PROBE_NODE=envoy \
  ./bin/envoy-static -c ./envoy-conf/single-hop.yaml --log-level info --base-id 1

# 终端 3：压测
KITEX_PROBE_HOST=$(hostname) \
  ./bin/client -target "$RUN/out.sock" -service echo-server \
               -n 300 -d 0 -c 1 -sample 1.0
```

**看到这一行就说明链路是对的**：

```
[诊断] 实际传输协议 = TTHeader|Framed
```

不是 `TTHeader` 而是 `TTHeader|Framed` —— 这是正常的，不是配错了
（Kitex 的 `SetTransportProtocol` 是按位或不是覆盖）。

### 3.5 采集与分析

**顺序不能换，`stop` 必须在 `collect` 之前**：事件先进内存缓冲，**线程退出时才刷盘**。
进程还活着时读到的文件永远缺最后一批。

**「什么时候停」和「用什么信号停」共同决定数据完整性，而两者都不会报错。**

打点落盘只有两个触发条件：**1 MB 缓冲写满**，或**每秒的 Ticker**。
几百请求的验证轮几十毫秒就跑完、只有几百 KB，两个条件都没触发 —— 数据全在内存里。
实测同一份 300 请求的验证轮：

| 停止方式 | 落盘 |
|---|---:|
| 跑完立刻关 tmux 窗口（SIGHUP，server 不处理它） | **0 条** |
| 跑完立刻 `pkill -TERM` | 346 条（每次还不一样） |
| **跑完等 1 秒再停** | **6300 条 = 全部** ✅ |

所以顺序是：**等 2 秒 → SIGTERM → 再关 tmux**。

```bash
# 1) 压测跑完先等 2 秒（让每秒 Ticker 触发一次），再发 SIGTERM，最后才关 tmux 窗口
sleep 2
pkill -x envoy-static; pkill -x server; sleep 5

# 2) 分析。注意 Envoy 按线程分文件，文件名带 .<tid>，必须用 *.ndjson* 通配
cd ~/meshlab
FILES="$RUN/trace-client.ndjson $RUN/trace-server.ndjson \
       $RUN/trace-envoy*.ndjson*"
./bin/merge -format summary   $FILES
./bin/merge -format detail    $FILES
./bin/merge -format waterfall -limit 3 $FILES
./bin/merge -format table -limit 0 $FILES > trace-table.csv
```

**数据完整性的判据是这一行，不是数行数**：

```bash
grep '\[probe\]' "$RUN"/*.log      # 四个节点的判据行都在这些日志里
# [probe] node=kitex-server 总请求=300 采样=300 丢弃=0
```

它由 tracer 的 `Close()` 打印。**这一行不在，说明 Close() 没跑完，数据不可信；
`丢弃` 非 0 说明事件队列满过，同样不可用于归因结论。**

**绝对不要在进程运行时 `rm` trace 文件**：探针持有长开的 `FILE*`，
删掉之后写入会进到已删除的 inode，进程退出时数据直接消失，**而且没有任何报错**。

`merge` 是纯 Go 静态二进制，在内网机器上直接能跑，不需要任何依赖。
如果内网机器不方便装 pandas 做后续分析，把 `trace-table.csv`（25 MB 量级）拷出来在别处分析即可。

### 3.6 跨机拓扑要额外注意的

`run-cross-machine.sh` 里写死了对端机器名和 IP，换环境必须改这几处：

| 文件 | 位置 | 原值 |
|---|---|---|
| `scripts/run-cross-machine.sh` | `PEER=` / `PEER_IP=` | `suzhou920B` / `192.168.25.51` |
| `scripts/run-cross-machine.sh` | `PROBE_OUT`/`PROBE_IN` 里的 `KITEX_PROBE_HOST` | **两个值必须不同** |
| `envoy-conf/two-hop-out-remote.yaml` | `to_inbound_sidecar` 的 `socket_address` | `192.168.25.51:15006` |
| `envoy-conf/single-hop-remote.yaml` | `to_remote_server` 的 `socket_address` | 同上 |

一条命令找全：

```bash
grep -rn "suzhou\|192\.168\.25\." ~/envoy_kitex/mesh-lab/{scripts,envoy-conf}
```

**端口 15006 需要在对端放行。** 判定方法：`Connection refused` = 防火墙放行了、只是没进程；
`No route to host` = 被防火墙 REJECT 了。内网机器往往防火墙更严，先用 `nc -l` 两端对测。

另外 `run-cross-machine.sh` 用 `rsync` 推二进制到对端，**要求两台内网机器之间 ssh 免密可达**。
如果不通，就手工把同一份 `meshlab-offline.tgz` 在两台机器上各解压一次。

---

## 4. 档位 B：内网还要改 Go 侧代码重编

Go 的离线方案很干净，**实测可行**：用 `vendor` 目录代替 module 缓存。

### 4.1 在构建机上准备（45 MB）

```bash
export PATH=~/sdk/go/bin:$PATH
export GOFLAGS=-mod=mod GOPROXY=https://goproxy.cn,direct GOSUMDB=off
cd ~/envoy_kitex/mesh-lab/demo
go mod vendor          # 实测 2.1 秒，产出 45 MB
du -sh vendor
```

`go mod vendor` 会把**全部依赖的源码**（含 `replace` 指向的本地 kitex / netpoll /
kitex-benchmark 插桩源码树）复制进 `demo/vendor/`。**产物是自包含的**，
不再需要 `~/go/pkg/mod`，也不需要 goproxy。

### 4.2 要搬的东西

| 内容 | 体积 | 说明 |
|---|---|---|
| Go 工具链 `~/sdk/go` | 265 MB | 或在内网机器上从内部镜像装同版本（go1.26.5） |
| `mesh-lab/demo/vendor/` | 45 MB | 上一步产出 |
| `mesh-lab/` 源码 | 1.2 MB | demo 自身的代码 |

**不需要**搬 `~/go/pkg/mod`（284 MB）和 `~/.cache/go-build`（329 MB）——vendor 已经覆盖了。

### 4.3 内网机器上编译（实测通过）

```bash
export PATH=~/sdk/go/bin:$PATH
cd ~/envoy_kitex/mesh-lab/demo
GOFLAGS="-mod=vendor" GOPROXY=off CGO_ENABLED=0 go build -o bin/server ./server
GOFLAGS="-mod=vendor" GOPROXY=off CGO_ENABLED=0 go build -o bin/client ./client
```

`GOPROXY=off` 是**故意加的**：它让 Go 在试图联网时立刻报错而不是长时间超时重试。
实测在这个设置下编译成功——说明确实一次网络都没走。

> `merge` 是独立 module（`mesh-lab/tools/merge`），要单独 `go mod vendor` 一次。

---

## 5. 档位 C：内网还要改 Envoy 的 C++ 打点重编

**先说结论：不建议。** 建议的做法是在能上网的机器上改完编好，把新的 `envoy-static`
推进内网——一次全量构建只要 17 分钟（384 核），比折腾离线依赖划算得多。

如果确实必须在内网编，这是需要面对的：

### 5.1 要搬的东西

| 内容 | 体积 | 说明 |
|---|---|---|
| `~/bin/bazel` | 60 MB | 版本必须与 `envoy/.bazelversion` 一致（本项目 8.7.0），否则 bazel 会**自己去 `releases.bazel.build` 下载**，内网必然失败 |
| Envoy 源码 | 243 MB | `--depth 1` 的浅克隆 |
| `~/.cache/bazel` | **3.0 GB** | 依赖仓库缓存，按 sha256 索引 |
| （可选）`~/bazel_out` | **65 GB** | 完整 output base，含全部编译中间产物 |

外部依赖实测 **960 个**。

### 5.2 两条路，各有代价

**① 只搬 3 GB 的 repository cache，用 `--repository_cache` 指过去。**
理论上 bazel 会命中缓存不去联网。**但本项目未实测过**，而且已知风险是：
并非所有依赖都走可缓存的 http_archive，`git_repository` 和 `rules_rust` 的
crate 解析可能仍然联网。**建议先在构建机上做一次断网演练**（把 `~/proxy.env` 不 source、
`unset` 掉代理变量，换一个全新的 `--output_base` 重编），确认能过再搬。

**② 搬完整的 65 GB `~/bazel_out`。** 成功率高得多，因为所有产物都已就位、
增量编译只重编你改动的部分。代价是：
- 体积大
- **output base 里烧死了绝对路径**，内网机器的 `$HOME` 和用户名必须与构建机**完全一致**，
  否则大面积失效
- 拷贝前必须 `chmod -R u+w`，因为 bazel 把产物目录设成只读，
  `rm`/`cp` 会失败**而退出码仍然是 0**

### 5.3 已排除的方案

这几条在 [runbook-reproduce.md](runbook-reproduce.md) 附录 B 验证过不可行，别再试：

| 方案 | 为什么不行 |
|---|---|
| 官方构建容器 `envoyproxy/envoy-build-ubuntu` | **它只提供工具链不提供依赖**，几百个外部依赖仍要现拉。而工具链问题本就不存在——bazel 会自行下载 LLVM 并用它编译，完全不依赖系统 gcc/clang |
| `--distdir` 预下载 | 依赖总量 GB 级，得先有办法把它弄进内网，本质上和 §5.1 是同一个问题 |

---

## 6. 排错速查（内网场景专属）

| 现象 | 原因 | 处理 |
|---|---|---|
| `cannot execute binary file: Exec format error` | **架构不匹配**（aarch64 二进制放到 x86_64 上） | 无解，去对应架构的联网机器重编（§1.1）。**这是你的场景里唯一还可能触发的兼容性错误** |
| ~~`version 'GLIBC_2.xx' not found`~~ | 构建机 glibc 比内网机器新 | **你的前提已排除**（内网 ≥ 构建机）。真遇到说明前提不成立，见 §1.2 |
| ~~Go 二进制报缺 `libresolv.so.2`~~ | 默认编法开了 CGO | **同上，已排除**。若真缺，`CGO_ENABLED=0` 重编（§2.2 可选加固） |
| **Envoy 一个点位都没有** | 没设 `KITEX_PROBE_PATH`/`KITEX_PROBE_NODE`。**不落盘且不报错** | §3.3 |
| Envoy 启动报 `Too many open files` | fd 软限 1024 不够，**而且只是 warn** | `ulimit -n 65536` |
| 同机两个 Envoy 互相杀死 | `--base-id` 相同，热重启机制生效 | 给不同 base-id |
| ssh 一断 Envoy 就没了 | Envoy 收到 SIGTERM 优雅退出 | 用 tmux 托管 |
| trace 文件比预期少一截 | 最后一批还在内存，线程退出才刷盘 | 先停进程再读；用 SIGTERM 不要 `kill -9` |
| `*.ndjson` 匹配不到文件 | Envoy 按线程分文件，名字带 `.<tid>` | 用 `*.ndjson*` |
| 跨机分析出负数时延 | 两机 `KITEX_PROBE_HOST` 相同（内网 hostname 常常一样） | 两台设不同值 |
| 跨机端口报 `No route to host` | 对端防火墙 REJECT（`Connection refused` 才是没人监听） | 让管理员放行，或换已放行端口 |
| bazel 自己去下载别的版本 | `.bazelversion` 与已装版本不符，内网必然失败 | 装严格匹配的版本（§5.1） |
| `rm -rf ~/bazel_out` 删不干净 | bazel 产物目录只读，**退出码仍为 0** | 先 `chmod -R u+w` |
| 时钟不同步，waterfall 时间轴错乱 | 两机 NTP 未开 | **不影响结论**（分析用差值法，对偏斜免疫），只是不好看 |

---

## 附录：一页纸速查

**内网机器上，从收到 tgz 到出数据：**

```bash
# 1. 落地 + 验兼容（精简包用 tar -I zstd -xf 或 tar -xf *.tar.xz）
tar -xzf meshlab-offline.tgz -C ~/ && mv ~/meshlab-offline ~/meshlab
cd ~/meshlab && sha256sum -c SHA256SUMS && ./bin/envoy-static --version

# 2. 摆成脚本期望的布局
mkdir -p ~/envoy_kitex/envoy/bazel-bin/source/exe ~/envoy_kitex/mesh-lab/demo/bin
cp bin/envoy-static ~/envoy_kitex/envoy/bazel-bin/source/exe/
cp bin/{server,client,merge} ~/envoy_kitex/mesh-lab/demo/bin/
cp -r envoy-conf scripts ~/envoy_kitex/mesh-lab/

# 3. 跑（同机单跳，最快的连通性验证）
RUN=${MESHLAB_RUN:-/tmp/kitex-demo-$(id -un)}
ulimit -n 65536 && mkdir -p "$RUN"
cd ~/envoy_kitex/mesh-lab && ./scripts/run-single-hop.sh start && ./scripts/run-single-hop.sh status
cd demo && KITEX_PROBE_HOST=$(hostname) ./bin/client -n 300 -d 0 -c 1 -sample 1.0

# 4. 停 + 分析（顺序不能换）
cd .. && ./scripts/run-single-hop.sh stop && sleep 5
cd demo && ./bin/merge -format detail "$RUN/trace-client.ndjson" \
    "$RUN/trace-server.ndjson" "$RUN"/trace-envoy*.ndjson*
```

**判断成功的三个信号：**
1. `./bin/envoy-static --version` 打印出版本号 → 二进制兼容
2. 压测输出里有 `[诊断] 实际传输协议 = TTHeader|Framed` → 链路和协议对
3. `merge -format detail` 里 envoy 和 kitex 的点位都有数 → 打点落盘了
