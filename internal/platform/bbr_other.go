//go:build !linux

package platform

// TryEnableBBR 在非 Linux 平台为空操作。
//
// BBR 是 Linux 内核的 TCP 拥塞控制算法；其他系统没有对应的运行时可调项。
func TryEnableBBR() {}
