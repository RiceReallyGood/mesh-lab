# 构建环境搭建操作手册

- 日期:2026-08-06
- 适用:在 suzhou950(openEuler 24.03 SP3 / aarch64 / 无 sudo)上从零编译 Envoy 与 Kitex demo
- 定位:**可复现的操作步骤 + 每步的理由**。理由部分不可删 —— 这些坑绝大多数在报错信息里看不出根因

---

## 0. 先读这一节:三条贯穿全程的原则

这三条是踩了一下午坑之后总结出来的,后面每一步都在遵守:

**① 大数据绝不经开发机中转**

开发机(WSL2)到 suzhou950 只有 **~150 KB/s**。而 suzhou950 到国内镜像有 **11.5 MB/s**。同一个 bazel 二进制:目标机直接下 5.5 秒,从开发机传要 7 分钟。**差 76 倍。**

所以:源码在目标机 `git clone`,工具走国内镜像,开发机只传 KB 级 diff。

**② 面对间歇性故障要"快速失败 + 多次重试",不是"拉长超时"**

曾把 `--http_timeout_scaling` 设成 `10.0` 想"应对不稳网络",结果单个依赖最坏要 20 分钟才放弃,**内层重试把外层重试饿死**,白卡两次。

**③ 长任务一律 `nohup setsid` 扔到远端后台**

到 suzhou950 的 ssh 会不定时断开。前台等待的命令会被连带杀掉,而后台任务不受影响。

---

## 1. 机器与角色

| 机器 | 地址 | 角色 | 外网 |
|---|---|---|---|
| WSL2(开发机) | — | 编辑代码、跑 git | 有 |
| **suzhou950** | 192.168.25.145 | **唯一的构建机**;跑 client + envoy-out | 有(但很慢,见 §3) |
| suzhou920B | 192.168.25.51 | 跑 envoy-in + server | **完全没有外网** |

两台服务器均为 **aarch64 物理机**(非虚机,`systemd-detect-virt` = none),同网段,RTT 0.057 ms。

**suzhou920B 无外网**,所以它上面的一切(envoy 二进制、Go 程序)都必须由 950 通过局域网推过去。

---

## 2. 一次性环境准备

### 2.1 Go

```bash
mkdir -p ~/sdk ~/dl
curl -fL -o ~/dl/go.tgz https://mirrors.aliyun.com/golang/go1.26.5.linux-arm64.tar.gz
tar -C ~/sdk -xzf ~/dl/go.tgz
~/sdk/go/bin/go version    # go version go1.26.5 linux/arm64
```

**为什么用 aliyun 镜像**:`go.dev` 从这台机很慢,aliyun 秒下。

### 2.2 bazel

```bash
mkdir -p ~/bin
curl -fL -o ~/bin/bazel https://mirrors.huaweicloud.com/bazel/8.7.0/bazel-8.7.0-linux-arm64
chmod +x ~/bin/bazel
~/bin/bazel --version      # bazel 8.7.0
```

**三个坑:**

1. **不要用 bazelisk** —— 它的 GitHub release 从这台机下不动(20s 超时)。而 bazelisk 只是个 launcher,最终还是去 `releases.bazel.build` 下 bazel 本体,不如直接下。
2. **不要用 `releases.bazel.build`** —— 实测 **75 KB/s**,62.8 MB 要下十几分钟。华为云镜像 **11.5 MB/s**,5.5 秒。
3. 版本必须与 Envoy 的 `.bazelversion` 一致(当前 8.7.0),否则 bazel 会试图自己去下载正确版本,又回到慢源问题。

**`~/bin` 不在默认 PATH 里**(实测 PATH 只有 `/usr/local/bin:/usr/bin:/usr/local/sbin:/usr/sbin`),脚本里一律用绝对路径 `~/bin/bazel`。

### 2.3 源码

```bash
mkdir -p ~/envoy_kitex && cd ~/envoy_kitex
git clone --depth 1 --branch main https://github.com/envoyproxy/envoy.git
git clone --depth 1 https://github.com/cloudwego/kitex.git
```

**在目标机直接 clone,不要从开发机 rsync**(原则 ①)。Envoy 源码 244 MB,rsync 过去要二十多分钟且容易断。

---

## 3. 网络:整个搭建过程最大的坑

### 3.1 症状:静默停滞

构建卡住时,日志只显示 `Analyzing: ...` 或 `Computing main repo mapping`,**不告诉你在等谁**。表现为:

- 依赖数长时间不涨
- `ss` 看到连接处于 `ESTABLISHED` 且收发队列均为 0 —— 对端不发数据,TCP 层看不出异常
- 或者干脆没有任何连接(在重试退避里干等)

### 3.2 带宽地形实测

| 目标 | 从 suzhou950 直连 |
|---|---|
| `mirrors.huaweicloud.com` | 11.5 MB/s |
| `mirrors.aliyun.com` | 快 |
| `codeload.github.com` | 1.5 MB/s |
| `github.com` | 时快(0.9s)时超时 |
| `raw.githubusercontent.com` / `185.199.x` | **经常完全超时** |
| **`static.rust-lang.org`** | **29.6 KB/s** |
| **`github.com/*/releases/download/*`** | **5.9 KB/s** |
| `releases.bazel.build` | 75 KB/s |

**`static.rust-lang.org` 是最致命的** —— Envoy 有 Rust 扩展,`rules_rust` 在 analysis 阶段就要解析工具链,绕不过去。工具链数百 MB,按 29.6 KB/s **要下 5–10 小时**。

### 3.3 方案一(已停用):bazel URL 重写

在没有代理时的可用方案,保留在此作为退路。

`~/.bazelrc`:
```
build --experimental_downloader_config=/home/<user>/downloader.cfg
```

`~/downloader.cfg`:
```
rewrite static\.rust-lang\.org/(.*) rsproxy.cn/$1
rewrite raw\.githubusercontent\.com/(.*) gh-proxy.com/https://raw.githubusercontent.com/$1
rewrite github\.com/(.*)/releases/download/(.*) gh-proxy.com/https://github.com/$1/releases/download/$2
```

**一个关键认知**:bazel 的 downloader config **只重写它主动发起的 URL,HTTP 客户端跟随 302 重定向时不再应用规则**。GitHub release 的流程是

```
bazel 请求 github.com/.../releases/download/...   ← 只有这一步能被重写
       ↓ 302
     objects.githubusercontent.com/...             ← 在这里写规则永远不会命中
```

所以规则必须写在 `github.com/.../releases/download/` 这一层。

**安全性**:bazel 对每个外部依赖强制校验 sha256(取自 `repository_locations.bzl`),第三方镜像返回篡改内容会立刻失败,不会静默引入。

**`rsproxy.cn` 的特性**:首次请求某文件可能返回 `504`(回源冷启动),重试即 `200`。因此 `--experimental_repository_downloader_retries` 必须 ≥ 2。

### 3.4 方案二(当前采用):Xray 代理

有自己的代理服务器时,这是更干净的方案 —— 一次解决整类问题,且不依赖第三方镜像。

#### 3.4.1 安装 Xray 客户端

```bash
V=v26.3.27
mkdir -p ~/xray ~/bin && cd ~/xray
curl -fL -o xray.zip \
  "https://gh-proxy.com/https://github.com/XTLS/Xray-core/releases/download/$V/Xray-linux-arm64-v8a.zip"
unzip -o xray.zip
chmod +x xray && ln -sf ~/xray/xray ~/bin/xray
~/xray/xray version
```

注意架构是 **`arm64-v8a`**,不是 x86 的那个包。

#### 3.4.2 从服务端配置推导客户端配置

服务端用的是 **VLESS + Reality**。客户端需要 `publicKey`,而服务端配置里只有 `privateKey` —— 两者是 X25519 密钥对,用 Xray 自带命令推导:

```bash
~/xray/xray x25519 -i "<服务端的 privateKey>"
# 输出 Private key / Public key，取后者
```

**操作要点:全程在目标机上完成,凭据不要经过第三方(包括聊天记录)。** 参见 `mesh-lab/scripts/gen_xray_client.sh`,它做的事是:

1. 剥掉服务端配置里的 `//` 注释(Xray 配置常带注释,`jq` 解析不了)
2. 用 `jq` 提取 UUID、privateKey、serverName、shortId、flow、port
3. 调 `xray x25519 -i` 推导 publicKey
4. 用 `jq -n --arg` 组装客户端配置,写到 `~/.config/xray/client.json`
5. 只打印非敏感字段供核对

#### 3.4.3 客户端配置的两个关键点

**① 本地双入站**

```json
{"tag":"socks","listen":"127.0.0.1","port":10808,"protocol":"socks"}
{"tag":"http", "listen":"127.0.0.1","port":10809,"protocol":"http"}
```

bazel 用 HTTP 入站(10809)。

**② 私网直连规则 —— 这条不配会毁掉整个实验**

```json
"routing": {"domainStrategy":"AsIs","rules":[
  {"type":"field","outboundTag":"direct",
   "ip":["127.0.0.0/8","10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","::1/128","fc00::/7"]}
]}
```

**不配的话,950 ↔ 920B 的流量会绕道代理服务器再绕回来** —— RTT 从 0.057 ms 变成几十毫秒,而这正是多机时序测量的关键链路,整套 §8.2 的测量就废了。

Xray 的路由规则里,**未匹配任何规则的流量走第一个 outbound**,所以 `proxy` 必须排在 `direct` 前面。

#### 3.4.4 启动

```bash
nohup setsid ~/xray/xray run -c ~/.config/xray/client.json > ~/xray.log 2>&1 </dev/null &
```

**该机 `systemd --user` 不可用**(`Failed to connect to bus: No medium found`,无 dbus session),所以不能做 user service。开机自启需要 root 装 system service。

验证:
```bash
ss -tln | grep -E ":1080[89]"          # 应看到两个监听
curl -x http://127.0.0.1:10809 -sI https://github.com | head -1
```

#### 3.4.5 环境变量

`~/proxy.env`:
```bash
export http_proxy=http://127.0.0.1:10809
export https_proxy=http://127.0.0.1:10809
export HTTP_PROXY=http://127.0.0.1:10809
export HTTPS_PROXY=http://127.0.0.1:10809
export no_proxy="127.0.0.1,localhost,192.168.0.0/16,10.0.0.0/8,172.16.0.0/12"
export NO_PROXY="$no_proxy"
```

`no_proxy` 是**第二层防护**(第一层是 Xray 路由规则),两层都要有。

#### 3.4.6 实测收益

| 源 | 直连 | 走代理 | 倍数 |
|---|---|---|---|
| GitHub release(`buf` 33 MB) | 7.2 KB/s | **2.6 MB/s** | **360×** |
| `static.rust-lang.org` | 13.9 KB/s | **2.73 MB/s** | **196×** |

体感:开代理后 **一分钟下载 1.8 GB**,external 仓库数从 448 涨到 890。

代理生效后,§3.3 的三条重写规则可全部停用。

---

## 4. 构建 Envoy

### 4.1 命令

```bash
source ~/proxy.env
ulimit -n 65536
~/bin/bazel --output_base=$HOME/bazel_out build -c opt \
    --curses=no --color=no --show_progress_rate_limit=15 \
    --experimental_repository_downloader_retries=2 \
    --http_timeout_scaling=1.0 \
    //source/exe:envoy-static
```

### 4.2 每个选项的理由

| 选项 | 为什么 |
|---|---|
| **`--output_base=$HOME/bazel_out`** | **不能用 `/tmp`**,原因见 §4.3。`/home` 是 NVMe 上的 ext4,2.32 亿 inode |
| **`--cxxopt=-Wno-nullability-completeness`** | 不加会在 `cel-cpp` 上编译失败,原因见 §4.4 |
| **不加 `--config=gcc`** | 它连带 `--config=libstdc++`,要求系统静态库 `libstdc++.a`(该机只有 `.so`) |
| **不加 `--config=clang`** | 它存在(见 §4.4),但连带改 `host_platform` 与 `-stdlib=libc++`,会让已完成的上万个编译动作缓存失效 |
| `--curses=no --color=no` | 否则日志被 `Computing main repo mapping` 反复覆盖,无法判断卡在哪 |
| `--http_timeout_scaling=1.0` | **低于**默认 6.0。快速失败(原则 ②) |
| `--experimental_repository_downloader_retries=2` | 应对 `rsproxy.cn` 的冷启动 504 |
| `ulimit -n 65536` | 默认软限 1024 不够;硬限 524288,无需 sudo |

**关于编译器的一个重要事实**:bazel 会**自行下载 LLVM 工具链**(`external/llvm_toolchain`)并用它编译,**完全不依赖系统的 gcc/clang**。编译错误里出现

```
external/llvm_toolchain/bin/cc_wrapper.sh --target=aarch64-unknown-linux-gnu
```

即为证据。所以"系统缺 `libstdc++.a`、缺 libc++"这类担心是不成立的 —— 那只在启用 `--config=gcc` / `--config=libstdc++` 时才成为问题。

### 4.3 必须把 output_base 放在 /home 而非 /tmp

**症状**:编译阶段大量报错

```
Compiling c/dec/state.c failed: Could not copy inputs into sandbox:
  .../llvm_minimal_linux_arm64/include/c++/v1/... (No space left on device)
```

**但空间明明是够的:**

```
df -h /tmp   →  690G 总量，用了 5.6G，可用 685G   （1%）
df -i /tmp   →  1,048,576 inode，用了 843,548     （81%，失败时 100%）
```

**根因是 inode 耗尽,不是空间不足。** tmpfs 挂载参数写死 `nr_inodes=1048576`,与容量无关。而 bazel 用 383 路并行,每个 sandbox 都要为 LLVM 头文件创建成千上万个符号链接 —— **inode 消耗与并行度成正比,与数据量无关**。

改 `nr_inodes` 需要 root(`mount -o remount,nr_inodes=...`),所以改用 `/home`:

| | /tmp (tmpfs) | /home (ext4 on NVMe) |
|---|---|---|
| inode 总量 | 1,048,576 | **231,944,192** |
| 可用空间 | 685 G | 6.0 T |

**切换 output_base 不会导致重新下载** —— bazel 的 repository cache(`~/.cache/bazel/_bazel_$USER/cache/repos`,按 sha256 索引)独立于 output_base,已有 1.4 GB 缓存会被复用,只需重新解压。

### 4.4 cel-cpp 的 `-Wnullability-completeness` 编译错误

**症状**:

```
external/cel-cpp/common/internal/reference_count.h:179:36:
  error: pointer is missing a nullability type specifier
         (_Nonnull, _Nullable, or _Null_unspecified)
         [-Werror,-Wnullability-completeness]
```

**性质**:**不是 aarch64 特有问题**。这是个 Clang 版本敏感的告警 —— 一旦某翻译单元用到了 `_Nonnull`/`_Nullable`,Clang 就要求同文件内所有指针都标注。Envoy 开了 `-Werror`,告警即错误。

**Envoy 上游已知此问题**,在两处做了豁免:

```
.bazelrc:108   build:macos          --cxxopt=-Wno-nullability-completeness
.bazelrc:119   common:clang-common  --cxxopt=-Wno-nullability-completeness
```

**一个查找上的坑**:`grep '^build:clang' .bazelrc` **查不到 clang config**,因为它的前缀是 `common:` 而非 `build:`(`common:` 对所有 bazel 命令生效,`build:` 只对 build 生效)。我据此一度误判为"Envoy 没有 clang config"。

**处理**:直接加单条 `--cxxopt=-Wno-nullability-completeness`,而不是切到整套 `--config=clang`。后者会改 `host_platform` 和 `-stdlib`,使已完成的编译缓存失效 —— 在一次要跑一万多个 action 的构建里,这个代价很大。

### 4.5 后台运行与监控

```bash
nohup setsid ~/build_envoy.sh >/dev/null 2>&1 </dev/null &
```

脚本见 `mesh-lab/scripts/`,要点:

- 外层重试循环,**仅在识别为网络错误时重试**(编译错误立即停止,避免浪费)
- 每轮开始前 `bazel shutdown` + `pkill -9 -x java`,否则会遇到 `Another command is running` 的锁冲突

---

## 4.6 拉起单跳链路

```bash
mesh-lab/scripts/run-single-hop.sh start    # 起 server + envoy
mesh-lab/scripts/run-single-hop.sh status
mesh-lab/scripts/run-single-hop.sh stop
```

三个**必须**的细节,少一个就跑不起来:

| 细节 | 不做会怎样 |
|---|---|
| **`ulimit -n 65536`** | Envoy 启动时报 `Too many open files`,但**只是 warn** —— 进程看似起来了实则不可用 |
| **`--base-id` 各实例不同** | Envoy 的热重启机制:同 base-id 的新实例启动时会通过共享域套接字**通知旧实例退出**。同机跑多个 Envoy(双跳降级模式)不设就会互相杀死 |
| **用 tmux 托管,不用 `nohup setsid`** | Envoy 注册了 SIGTERM 处理器,ssh 会话结束时会收到 SIGTERM 优雅退出(日志里是 `caught ENVOY_SIGTERM`)。Go 进程和 xray 在同样条件下能存活,**但 Envoy 不行** |

**验证是否真的在监听**,不能只看 socket 文件存在:

```bash
ss -xln | grep kitex-demo     # 文件存在 ≠ 有人 listen
```

我第一版 status 只用 `[ -S "$file" ]` 判断,结果 Envoy 早就死了却报告"已监听"。

## 4.7 Kitex 侧的两个协议陷阱

### 4.7.1 `apache/thrift` 版本必须钉死 v0.13.0

Kitex 的 `bthrift/apache` 兼容层只能配 `v0.13.0`,v0.14+ 给 `TProtocol` 的方法加了 `context` 参数,签名对不上直接编译失败。

`kitex-benchmark` 自己 `go.mod:128` 做了这个 replace,但 **Go 的 replace 指令不会被依赖方继承**,必须在自己的模块里重复声明:

```
replace github.com/apache/thrift => github.com/apache/thrift v0.13.0
```

### 4.7.2 `WithTransportProtocol` 是按位或,不是覆盖

```go
// kitex pkg/rpcinfo/rpcconfig.go:173-181
func (r *rpcConfig) SetTransportProtocol(tp transport.Protocol) error {
    if tp == transport.PurePayload {
        r.transportProtocol = tp
    } else {
        r.transportProtocol |= tp      // ← 按位或
    }
}
```

后果:即使显式设 `client.WithTransportProtocol(transport.TTHeader)`,**实际生效的可能是 `TTHeader|Framed`**。

**怎么确认**:别推理,打出来。加个中间件:

```go
client.WithMiddleware(func(next endpoint.Endpoint) endpoint.Endpoint {
    return func(ctx context.Context, req, resp interface{}) error {
        ri := rpcinfo.GetRPCInfo(ctx)
        log.Printf("实际传输协议 = %s", ri.Config().TransportProtocol())
        return next(ctx, req, resp)
    }
})
```

实测输出 `TTHeader|Framed` —— 这直接解释了 Envoy 报 `invalid binary protocol version 0x0000`(详见 §5.6)。

### 4.7.3 `ClientTTHeaderHandler` 默认不注册

`internal/client/option.go:234` 里,client 的默认 MetaHandler **只有 `MetainfoClientHandler`**:

```go
MetaHandlers: []remote.MetaHandler{transmeta.MetainfoClientHandler},
```

而写 `FromService` / `ToService` / `ToMethod` 这些 **IntKV** 的是 `transmeta.ClientTTHeaderHandler`,**它不在默认列表里**。不显式注册的话,TTHeader 里根本没有 IntKV 段,Envoy 侧基于 `x-tt-to-service` 的路由无从匹配 —— 表现为所有请求 `route_missing`。

```go
client.WithMetaHandler(transmeta.ClientTTHeaderHandler)   // 必须
server.WithMetaHandler(transmeta.ServerTTHeaderHandler)   // 对称
```

### 4.7.4 metainfo 的前缀不要自己加

`metainfo.WithPersistentValue(ctx, key, val)` 在序列化时会自动加 `RPC_PERSIST_` 前缀。若自己在 key 里再加一遍,线上会出现:

```
RPC_PERSIST_RPC_PERSIST_traceparent
```

我就是这么写错的,靠抓包才发现。**排查这类问题必须看真实字节**,见 §5.7。

## 5. 排障手册(补)

### 5.7 用 UDS 转储工具看真实字节

Envoy 报"解码失败"时,不要靠推测协议格式 —— 直接看 Kitex 发的字节。

`mesh-lab/tools/udsdump` 是个 UDS 透明转发代理,把双向字节流原样 hexdump:

```bash
udsdump -listen /tmp/kitex-demo/dump.sock -upstream /tmp/kitex-demo/app.sock
# 然后让 client 打 dump.sock
```

本项目靠它一次性发现了三个问题:内层 framed 前缀、metainfo 前缀重复、IntKV 段完全缺失。**任何一个靠读代码都不容易发现。**

解析抓到的帧:

```
00 00 00 a1   LENGTH = 161
10 00         MAGIC = 0x1000        ← TTHeader
00 00         FLAGS
00 00 00 01   SEQ ID
00 19         HEADER SIZE = 25 × 4 = 100 字节
00            PROTOCOL ID = ThriftBinary
00            NUM TRANSFORMS
01            INFO ID = 0x01 (StrKV)
...
```

## 5. 排障手册

### 5.1 定位"卡在哪"的标准序列

| 步骤 | 命令 | 看什么 |
|---|---|---|
| 1 | `du -sm ~/bazel_out/external` | 总量不涨 = 卡在拉取 |
| 2 | `ls -lt ~/bazel_out/external \| head` | 最后落盘的是谁,下一个就是嫌疑人 |
| 3 | **`lsof -p $(pgrep -x java)`** | **最有效**。看正在写哪个文件 → 直接指出是哪个依赖 |
| 4 | `ss -tan \| grep :443` | 有 ESTAB 且队列为 0 = 对端不发数据;无连接 = 在退避里干等 |
| 5 | 反查对端 IP | `185.199.x` = GitHub 静态 CDN;`140.82.x` = github.com;`172.64.x` = Cloudflare |
| 6 | `curl -r 0-8000000 -w "%{speed_download}"` | 分别实测原始源与候选镜像 |

### 5.2 三个容易误判的地方

| 误判 | 真相 |
|---|---|
| 盯**单个文件**大小,以为停滞 | 该文件下完了,转去下同一个 toolchain 里的下一个文件。应看目录总量 |
| 只看 `ss ... state established` | 会漏掉 `SYN_SENT`(连不上的情况)。应用 `ss -tan` 看全状态 |
| `No space left on device` 就是空间不够 | 可能是 **inode 耗尽**。必须同时看 `df -h` 和 `df -i` |

### 5.3 验证 URL 重写/代理是否真的生效

**看连接的对端 IP 归属** —— bazel 不打印它实际请求的 URL,日志靠不住。

- 走 `gh-proxy.com` → 对端是 Cloudflare 段 `172.64.x`
- 仍是 `185.199.x` → 规则没匹配上

### 5.4 ssh 相关

| 问题 | 处理 |
|---|---|
| ControlMaster socket 僵死后,所有复用它的 ssh 挂起 | `ssh -O exit` + `rm` 掉 socket 文件。**本项目已放弃使用 ControlMaster** |
| `ControlPath too long` | socket 路径必须 < 108 字节(`sockaddr_un.sun_path` 限制) |
| 长命令整体超时导致后半段没执行 | 拆成多条短命令;长任务用 `nohup setsid` |

### 5.5 `pkill -f` 会杀掉自己

`pkill -f <pattern>` 匹配的是**完整命令行**,而执行它的那条命令的命令行里就含有 pattern 本身 —— **会把自己的 shell 杀掉**(表现为 exit 143/144)。

本项目踩了三次(两次本地、一次远端)。

**一律用 `pkill -x <进程名>`**(只匹配进程名),或 `ps -eo pid,cmd | grep '[b]uild.sh' | awk '{print $1}'` 的括号技巧。

---

## 6. 已排除的方案

### 6.1 官方 Envoy 构建容器

`envoyproxy/envoy-build-ubuntu`,经查证**不可行**:

1. **`registry-1.docker.io` 从 suzhou950 超时不可达**,镜像拉不下来
2. 该机 docker 为 **18.09.0**(2018 年),无 buildx / 多架构 manifest
3. **更根本的是:该镜像只提供工具链,不提供依赖。** 几百个外部依赖仍由 bazel 现拉,§3 的慢源一个都躲不掉。Envoy 官方 CI 快是靠 RBE 远程缓存,不是镜像本身

而工具链问题本就不存在(bazel 自带 LLVM,§4.2),恰恰是容器唯一能帮上忙的地方。

### 6.2 反向 SSH 隧道借用开发机代理

`ssh -R 10808:127.0.0.1:10808 suzhou950` 建立成功但转发不通。开发机(WSL2)的代理是 **Windows 侧监听**,`ss` 里都看不见,forwarded socket 够不着。

已被 §3.4 的原生 Xray 客户端取代。

### 6.3 `--distdir` 预下载

依赖总量 GB 级,而开发机到 suzhou950 仅 ~150 KB/s,传输不现实。

---

## 附:时间线与各阶段结论

| 时刻 | 事件 | 结论 |
|---|---|---|
| — | 前三次构建卡在依赖拉取 | 一度误判为 GitHub 整体不稳 |
| — | `lsof` 定位到 Rust 工具链 | 真凶是 `static.rust-lang.org` 29.6 KB/s |
| — | 加 Rust 镜像重写 | 依赖数一分钟从 86 → 357,**首次进入 analysis** |
| — | 再次卡住,`lsof` 定位到 `buf` | GitHub release 5.9 KB/s |
| — | 加 release 重写 | 提升到 85 KB/s |
| — | 配置 Xray 代理 | GitHub release **2.6 MB/s**,一分钟下 1.8 GB |
| — | **首次进入编译阶段** | 报 `No space left on device` |
| — | `df -i` 定位 | **inode 耗尽**,非空间不足 |
| — | output_base 迁到 /home | 进行中 |

**至本文撰写时,"openEuler aarch64 能否编译 Envoy" 仍未有定论** —— 编译确实开始了(见到 `Compiling xxx.cc` 成功执行),但被 inode 问题打断,尚未跑完。不能声称此风险已消除。
