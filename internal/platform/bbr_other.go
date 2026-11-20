//go:build !linux || nobbr

package platform

// TryEnableBBR 在非 Linux 平台或被 `-tags nobbr` 剔除时为空操作。
//
// BBR 是 Linux 内核的 TCP 拥塞控制算法；Docker 镜像用 `-tags nobbr` 构建，
// 容器里既没有权限也不该修改宿主机内核参数，因此整段实现不编进镜像。
func TryEnableBBR() {}
