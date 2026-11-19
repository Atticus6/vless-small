//go:build linux

package app

import "os"

// linuxDefaultRouteInterface 读取 /proc/net/route 中的默认路由网卡。
//
// 这条路径不依赖 iproute2/ifconfig，Alpine、Debian 等发行版以及容器内
// 只要有 procfs 就能工作。
func linuxDefaultRouteInterface() string {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer file.Close()

	return parseDefaultRouteInterface(file)
}
