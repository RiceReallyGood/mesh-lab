// fixturegen 用 Kitex 官方的 gopkg/protocol/ttheader 编码器生成真实 TTHeader 帧，
// 输出为 C++ 单测可直接引用的 fixture。
//
// 之所以不手工构造字节：手工构造只能验证「实现符合我对协议的理解」，
// 而用官方编码器生成，验证的是「实现符合 Kitex 真正会发出的字节」。
// 这两者的差别正是 §9.2 那个 padding 陷阱会藏身的地方。
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/cloudwego/gopkg/protocol/ttheader"
)

// Case 描述一个 fixture 用例。
type Case struct {
	Name    string            `json:"name"`
	Why     string            `json:"why"`     // 这个用例存在的理由
	Param   ttheader.EncodeParam `json:"-"`
	Payload []byte            `json:"-"`

	// 以下为导出给 C++ 的期望值
	SeqID      int32             `json:"seq_id"`
	Flags      uint16            `json:"flags"`
	ProtocolID uint8             `json:"protocol_id"`
	IntInfo    map[string]string `json:"int_info"` // key 转成十进制字符串便于 JSON
	StrInfo    map[string]string `json:"str_info"`
	HeaderLen  int               `json:"header_len"`
	PayloadLen int               `json:"payload_len"`
	TotalBytes int               `json:"total_bytes"`
	Hex        string            `json:"hex"`
	ShouldFail bool              `json:"should_fail"` // C++ 侧应当拒绝
}

func mustEncode(p ttheader.EncodeParam, payload []byte) []byte {
	buf, err := ttheader.EncodeToBytes(context.Background(), p)
	if err != nil {
		panic(fmt.Sprintf("encode failed: %v", err))
	}
	frame := append(buf, payload...)
	// 按 gopkg 的约定回填总长度：header + payload - 4
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(frame)-4))
	return frame
}

// 一个最小的合法 thrift binary CALL 消息体，方便 C++ 侧后续接协议层。
// 格式：version|type(4) + nameLen(4) + name + seqid(4) + STOP(1)
func thriftBinaryCall(method string, seqID int32) []byte {
	b := make([]byte, 0, 16+len(method))
	b = binary.BigEndian.AppendUint32(b, 0x80010001) // version 1 | CALL
	b = binary.BigEndian.AppendUint32(b, uint32(len(method)))
	b = append(b, method...)
	b = binary.BigEndian.AppendUint32(b, uint32(seqID))
	b = append(b, 0x00) // field STOP
	return b
}

func main() {
	payload := thriftBinaryCall("Echo", 1)

	cases := []*Case{
		{
			Name: "Basic",
			Why:  "最典型的 Kitex 请求：ThriftBinary + 路由用的 IntKV + 一个 StrKV",
			Param: ttheader.EncodeParam{
				SeqID:      1,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
				IntInfo: map[uint16]string{
					3: "echo-client",   // FromService
					6: "echo-server",   // ToService
					9: "Echo",          // ToMethod
				},
				StrInfo: map[string]string{
					"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				},
			},
			Payload: payload,
		},
		{
			Name: "EmptyKV",
			Why:  "没有任何 info block，只有 PROTOCOL ID + NUM TRANSFORMS，验证最小 header",
			Param: ttheader.EncodeParam{
				SeqID:      2,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
			},
			Payload: payload,
		},
		{
			Name: "IntKVOnly",
			Why:  "只有 IntKV 没有 StrKV，验证 info block 类型分发",
			Param: ttheader.EncodeParam{
				SeqID:      3,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
				IntInfo:    map[uint16]string{6: "svc", 9: "M"},
			},
			Payload: payload,
		},
		{
			Name: "UnknownIntKey",
			Why:  "包含未在 metakey.go 定义的 IntKV id(999)，验证 C++ 侧 fallback 到 x-ttheader-int-999",
			Param: ttheader.EncodeParam{
				SeqID:      4,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
				IntInfo:    map[uint16]string{6: "svc", 999: "unknown-value"},
			},
			Payload: payload,
		},
		{
			Name: "MetainfoUppercase",
			Why:  "§9.1 大小写陷阱：metainfo 前缀是大写的，过一趟 Envoy 后必须原样还原",
			Param: ttheader.EncodeParam{
				SeqID:      5,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
				StrInfo: map[string]string{
					"RPC_PERSIST_TENANT":   "tenant-a",
					"RPC_TRANSIT_HOP":      "1",
					"RPC_BACKWARD_LATENCY": "42",
				},
			},
			Payload: payload,
		},
		{
			Name: "PerfKeys",
			Why:  "§3.5 Kitex 原生 mesh 打点头，我们的 Envoy 要读写这些 key",
			Param: ttheader.EncodeParam{
				SeqID:      6,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
				StrInfo: map[string]string{
					"pcs": "1754438400123456789",
					"pce": "1754438400123556789",
					"pss": "1754438400123656789",
				},
			},
			Payload: payload,
		},
		{
			Name: "ACLToken",
			Why:  "InfoIDACLToken(0x11) 分支，写法与普通 StrKV 不同(无 count 字段)",
			Param: ttheader.EncodeParam{
				SeqID:      7,
				ProtocolID: ttheader.ProtocolIDThriftBinary,
				StrInfo: map[string]string{
					"RPC_TRANSIT_gdpr-token": "tok-abc",
					"normal":                 "v",
				},
			},
			Payload: payload,
		},
		{
			Name: "CompactProtocol",
			Why:  "ProtocolID=0x02 应映射到 Envoy 的 Compact 而非报错",
			Param: ttheader.EncodeParam{
				SeqID:      8,
				ProtocolID: ttheader.ProtocolIDThriftCompact,
				IntInfo:    map[uint16]string{6: "svc"},
			},
			Payload: payload,
		},
		{
			Name:       "KitexProtobufMustReject",
			Why:        "§3.7：ProtocolID=0x04 必须显式拒绝，不能拿 thrift 解析器去啃 protobuf",
			ShouldFail: true,
			Param: ttheader.EncodeParam{
				SeqID:      9,
				ProtocolID: ttheader.ProtocolIDKitexProtobuf,
				IntInfo:    map[uint16]string{6: "svc"},
			},
			Payload: payload,
		},
	}

	// 补一组专门制造 header 长度恰为 4 倍数的用例 —— §9.2 的 padding 陷阱。
	// 用递增长度的 key 去扫，把命中「padding == 0」的那些留下。
	for n := 1; n <= 40; n++ {
		key := "k" + strings.Repeat("x", n)
		p := ttheader.EncodeParam{
			SeqID:      int32(1000 + n),
			ProtocolID: ttheader.ProtocolIDThriftBinary,
			StrInfo:    map[string]string{key: "v"},
		}
		frame := mustEncode(p, payload)
		// header 长度 = TTHeaderMetaSize + headerInfoSize
		hdrInfoSize := int(binary.BigEndian.Uint16(frame[12:14])) * 4
		// 实际写入的字节数(不含 padding) = 1(protoID) + 1(numTransform) + 1(infoID) + 2(count) + 2+len(key) + 2+len("v")
		written := 1 + 1 + 1 + 2 + 2 + len(key) + 2 + 1
		if written%4 == 0 && hdrInfoSize == written {
			cases = append(cases, &Case{
				Name:    fmt.Sprintf("PaddingZero_%d", n),
				Why:     "header 长度恰为 4 的倍数 → Kitex 补 0 字节，而 Apache 实现会补 4 字节。抄错即偶发失败",
				Param:   p,
				Payload: payload,
			})
			if len(cases) > 0 && n >= 4 {
				break // 一个足够
			}
		}
	}

	out := make([]*Case, 0, len(cases))
	for _, c := range cases {
		frame := mustEncode(c.Param, c.Payload)
		c.SeqID = c.Param.SeqID
		c.Flags = uint16(c.Param.Flags)
		c.ProtocolID = uint8(c.Param.ProtocolID)
		c.HeaderLen = int(ttheader.TTHeaderMetaSize) + int(binary.BigEndian.Uint16(frame[12:14]))*4
		c.PayloadLen = len(frame) - c.HeaderLen
		c.TotalBytes = len(frame)
		c.Hex = fmt.Sprintf("%x", frame)
		c.IntInfo = map[string]string{}
		for k, v := range c.Param.IntInfo {
			c.IntInfo[fmt.Sprintf("%d", k)] = v
		}
		c.StrInfo = map[string]string{}
		for k, v := range c.Param.StrInfo {
			c.StrInfo[k] = v
		}
		out = append(out, c)

		// 自检：用官方解码器解回来，确认我们生成的帧本身是合法的
		if !c.ShouldFail {
			dp, err := ttheader.DecodeFromBytes(context.Background(), frame)
			if err != nil {
				panic(fmt.Sprintf("%s: 自解码失败 %v", c.Name, err))
			}
			if dp.SeqID != c.SeqID {
				panic(fmt.Sprintf("%s: seqID 不一致 %d != %d", c.Name, dp.SeqID, c.SeqID))
			}
			if dp.HeaderLen != c.HeaderLen {
				panic(fmt.Sprintf("%s: headerLen 不一致 %d != %d", c.Name, dp.HeaderLen, c.HeaderLen))
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// 1) JSON：人可读，也给别的工具用
	jf, err := os.Create("ttheader_fixtures.json")
	if err != nil {
		panic(err)
	}
	defer jf.Close()
	enc := json.NewEncoder(jf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		panic(err)
	}

	// 2) C++ 头文件
	hf, err := os.Create("ttheader_fixtures.h")
	if err != nil {
		panic(err)
	}
	defer hf.Close()
	writeCppHeader(hf, out)

	fmt.Printf("生成 %d 个 fixture\n", len(out))
	for _, c := range out {
		flag := ""
		if c.ShouldFail {
			flag = "  [应被拒绝]"
		}
		fmt.Printf("  %-24s %4d bytes  hdr=%d payload=%d%s\n", c.Name, c.TotalBytes, c.HeaderLen, c.PayloadLen, flag)
	}
}

func writeCppHeader(f *os.File, cases []*Case) {
	fmt.Fprintf(f, `// 本文件由 mesh-lab/tools/fixturegen 自动生成，请勿手改。
// 字节来自 Kitex 官方 gopkg/protocol/ttheader 编码器，是真实线上格式。
#pragma once

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace Envoy {
namespace Extensions {
namespace NetworkFilters {
namespace ThriftProxy {
namespace TTHeaderFixtures {

struct Fixture {
  const char* name;
  const char* why;
  std::vector<uint8_t> bytes;
  int32_t seq_id;
  uint16_t flags;
  uint8_t protocol_id;
  std::map<uint16_t, std::string> int_info;
  std::map<std::string, std::string> str_info;
  uint32_t header_len;
  uint32_t payload_len;
  bool should_fail;
};

inline const std::vector<Fixture>& all() {
  static const std::vector<Fixture>* fixtures = new std::vector<Fixture>{
`)
	for _, c := range cases {
		fmt.Fprintf(f, "    // %s\n", c.Why)
		fmt.Fprintf(f, "    Fixture{\n")
		fmt.Fprintf(f, "      \"%s\",\n", c.Name)
		fmt.Fprintf(f, "      %q,\n", c.Why)
		fmt.Fprintf(f, "      {")
		for i := 0; i < len(c.Hex); i += 2 {
			if i%32 == 0 {
				fmt.Fprintf(f, "\n       ")
			}
			fmt.Fprintf(f, "0x%s, ", c.Hex[i:i+2])
		}
		fmt.Fprintf(f, "\n      },\n")
		fmt.Fprintf(f, "      %d, %d, %d,\n", c.SeqID, c.Flags, c.ProtocolID)
		fmt.Fprintf(f, "      {")
		ks := make([]string, 0, len(c.IntInfo))
		for k := range c.IntInfo {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Fprintf(f, "{%s, %q}, ", k, c.IntInfo[k])
		}
		fmt.Fprintf(f, "},\n")
		fmt.Fprintf(f, "      {")
		sk := make([]string, 0, len(c.StrInfo))
		for k := range c.StrInfo {
			sk = append(sk, k)
		}
		sort.Strings(sk)
		for _, k := range sk {
			fmt.Fprintf(f, "{%q, %q}, ", k, c.StrInfo[k])
		}
		fmt.Fprintf(f, "},\n")
		fmt.Fprintf(f, "      %d, %d, %t,\n", c.HeaderLen, c.PayloadLen, c.ShouldFail)
		fmt.Fprintf(f, "    },\n")
	}
	fmt.Fprintf(f, `  };
  return *fixtures;
}

} // namespace TTHeaderFixtures
} // namespace ThriftProxy
} // namespace NetworkFilters
} // namespace Extensions
} // namespace Envoy
`)
}
