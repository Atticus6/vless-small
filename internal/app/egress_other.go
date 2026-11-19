//go:build !linux

package app

// linuxDefaultRouteInterface 在非 Linux 上没有 /proc/net/route，直接返回空，
// 由 UDP 路由探测决定是否启用出口选择。
func linuxDefaultRouteInterface() string {
	return ""
}
