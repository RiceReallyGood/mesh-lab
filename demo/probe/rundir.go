package probe

import (
	"os"
	"os/user"
	"path/filepath"
)

// DefaultRunDir 返回本次运行的工作目录，默认按**用户隔离**。
//
// 曾经写死 /tmp/kitex-demo。suzhou950/920B 是共享开发机，2026-08-10 就撞上了：
// 另一个用户同时在跑同一套实验，两台机器的 /tmp/kitex-demo 全变成他的文件。
// 我方进程写不进去（sticky 位挡着），报的是 permission denied 还算好的 ——
// 更糟的是采集脚本照样把**对方的** trace 拉了回来，数据看着「有」，只是不是自己的。
//
// 用 user.Current() 而不是 $USER：后者在非交互 ssh、systemd 等场景下可能为空。
// MESHLAB_RUN 可显式覆盖，方便多人在同一台机器上跑互不干扰的对照。
func DefaultRunDir() string {
	if d := os.Getenv("MESHLAB_RUN"); d != "" {
		return d
	}
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return filepath.Join("/tmp", "kitex-demo-"+name)
}

// RunPath 拼出运行目录下的文件路径。
func RunPath(name string) string { return filepath.Join(DefaultRunDir(), name) }
