#!/bin/bash
# 从 Xray 服务端配置推导出客户端配置。
#
# 用法：
#   ./gen_xray_client.sh <服务器地址> [服务端配置路径] [客户端配置输出路径]
#   例：./gen_xray_client.sh 203.0.113.10 ~/xray_server.json
#
# 设计要点：
#   - 全程在目标机上执行，UUID 与 privateKey 不经过任何第三方（包括聊天记录、剪贴板）
#   - 只打印非敏感字段供人工核对
#   - VLESS + Reality 的客户端需要 publicKey，而服务端配置里只有 privateKey，
#     两者是 X25519 密钥对，用 `xray x25519 -i` 推导
set -euo pipefail

ADDR="${1:?用法: $0 <服务器地址> [服务端配置] [输出路径]}"
SRC="${2:-$HOME/xray_server.json}"
OUT="${3:-$HOME/.config/xray/client.json}"
XRAY="${XRAY_BIN:-$HOME/xray/xray}"

[ -f "$SRC" ] || { echo "找不到服务端配置: $SRC" >&2; exit 1; }
[ -x "$XRAY" ] || { echo "找不到 xray 可执行文件: $XRAY" >&2; exit 1; }

# Xray 配置常带 // 注释，jq 解析不了，先剥掉整行注释
CLEAN=$(mktemp)
trap 'rm -f "$CLEAN"' EXIT
sed -E '/^[[:space:]]*\/\//d' "$SRC" > "$CLEAN"

UUID=$(jq -r '.inbounds[0].settings.clients[0].id' "$CLEAN")
PRIV=$(jq -r '.inbounds[0].streamSettings.realitySettings.privateKey' "$CLEAN")
SNI=$(jq  -r '.inbounds[0].streamSettings.realitySettings.serverNames[0]' "$CLEAN")
SID=$(jq  -r '.inbounds[0].streamSettings.realitySettings.shortIds[0]' "$CLEAN")
FLOW=$(jq -r '.inbounds[0].settings.clients[0].flow // ""' "$CLEAN")
PORT=$(jq -r '.inbounds[0].port' "$CLEAN")

PUB=$("$XRAY" x25519 -i "$PRIV" 2>/dev/null | grep -iE 'public' | awk -F'[: ]+' '{print $NF}')
[ -n "$PUB" ] || { echo "ERROR: 推导 publicKey 失败" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")"
jq -n \
  --arg addr "$ADDR" --argjson port "$PORT" --arg uuid "$UUID" \
  --arg flow "$FLOW" --arg sni "$SNI" --arg pub "$PUB" --arg sid "$SID" \
'{
  log: {loglevel: "warning"},
  inbounds: [
    {tag:"socks", listen:"127.0.0.1", port:10808, protocol:"socks",
     settings:{udp:true, auth:"noauth"},
     sniffing:{enabled:true, destOverride:["http","tls"]}},
    {tag:"http", listen:"127.0.0.1", port:10809, protocol:"http",
     settings:{},
     sniffing:{enabled:true, destOverride:["http","tls"]}}
  ],
  # 未匹配任何 routing 规则的流量走第一个 outbound，所以 proxy 必须排在最前
  outbounds: [
    {tag:"proxy", protocol:"vless",
     settings:{vnext:[{address:$addr, port:$port,
               users:[{id:$uuid, encryption:"none", flow:$flow}]}]},
     streamSettings:{network:"tcp", security:"reality",
       realitySettings:{serverName:$sni, fingerprint:"chrome",
                        publicKey:$pub, shortId:$sid, spiderX:""}}},
    {tag:"direct", protocol:"freedom"},
    {tag:"block", protocol:"blackhole"}
  ],
  # 私网直连 —— 这条不配，同网段机器间的流量会绕道代理服务器再绕回来，
  # RTT 从 0.057ms 变几十毫秒，多机时序测量直接报废
  routing: {domainStrategy:"AsIs", rules:[
    {type:"field", outboundTag:"direct",
     ip:["127.0.0.0/8","10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","::1/128","fc00::/7"]}
  ]}
}' > "$OUT"

echo "客户端配置已生成: $OUT"
echo "  服务器    : $ADDR:$PORT"
echo "  协议      : vless + reality, flow=$FLOW"
echo "  SNI       : $SNI"
echo "  shortId   : $SID"
echo "  publicKey : ${PUB:0:8}...(已推导，长度 ${#PUB})"
echo "  本地入站  : socks5 127.0.0.1:10808 / http 127.0.0.1:10809"
echo "  直连网段  : 127/8, 10/8, 172.16/12, 192.168/16"
echo
echo "启动: nohup setsid $XRAY run -c $OUT > ~/xray.log 2>&1 </dev/null &"
