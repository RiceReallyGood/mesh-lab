module meshlab/demo

go 1.21

require (
	github.com/bytedance/gopkg v0.1.1
	github.com/cloudwego/kitex v0.16.3
	github.com/cloudwego/kitex-benchmark v0.0.0
)

// 复用 benchmark 仓库里已生成的 echo kitex_gen 代码，避免再跑一次代码生成
replace github.com/cloudwego/kitex-benchmark => ../../kitex-benchmark

// Kitex 的 bthrift/apache 兼容层只能配 apache/thrift v0.13.0：
// v0.14+ 给 TProtocol 的方法加了 context 参数，签名对不上会编译失败。
// kitex-benchmark 自己也做了同样的 replace（其 go.mod:128），
// 但 replace 不会被依赖方继承，必须在本模块重复声明。
replace github.com/apache/thrift => github.com/apache/thrift v0.13.0
