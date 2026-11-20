//go:build linux && !nobbr

package platform

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	tcpCongestionControlPath = "/proc/sys/net/ipv4/tcp_congestion_control"
	tcpAvailableControlPath  = "/proc/sys/net/ipv4/tcp_available_congestion_control"
)

// TryEnableBBR 在纯 Linux（非容器）环境尽力把 TCP 拥塞控制切到 BBR。
//
// 三层过滤，任何一层不满足都直接跳过：
//  1. 编译期：`-tags nobbr` 会把这个文件整体排除（Docker 镜像默认如此）；
//  2. 运行时：只有 DetectRuntime()=="local" 才尝试，PaaS/托管容器跳过；
//  3. 运行时：检测到容器标记（Docker/Podman/K8s/LXC）也跳过——容器里的
//     /proc/sys 通常属于宿主机或独立 netns，应用不应擅自修改内核参数。
//
// 失败只记日志：内核没有 tcp_bbr、缺少 CAP_NET_ADMIN、modprobe 不可用
// 都不影响代理本身启动。BBR 对高延迟/有丢包链路的吞吐提升明显，因此值得
// 在 VPS 上自动尝试一次。
func TryEnableBBR() {
	if runtimeName := DetectRuntime(); runtimeName != "local" {
		log.Printf("bbr: skip (runtime=%s)", runtimeName)
		return
	}
	if isContainerized() {
		log.Printf("bbr: skip (container detected)")
		return
	}

	current, err := os.ReadFile(tcpCongestionControlPath)
	if err != nil {
		log.Printf("bbr: read congestion control: %v", err)
		return
	}
	if strings.TrimSpace(string(current)) == "bbr" {
		return
	}

	if !hasBBR(readTrimmed(tcpAvailableControlPath)) {
		// 部分内核把 tcp_bbr 编成模块而非内置；尝试按需加载。
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = exec.CommandContext(ctx, "modprobe", "tcp_bbr").Run()
		cancel()
		if !hasBBR(readTrimmed(tcpAvailableControlPath)) {
			log.Printf("bbr: not available in kernel")
			return
		}
	}

	if err := os.WriteFile(tcpCongestionControlPath, []byte("bbr\n"), 0o644); err != nil {
		log.Printf("bbr: enable: %v", err)
		return
	}
	log.Printf("bbr: enabled (was %s)", strings.TrimSpace(string(current)))
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// hasBBR 判断内核提供的可用拥塞控制列表里是否包含 bbr。
func hasBBR(available string) bool {
	for _, field := range strings.Fields(available) {
		if field == "bbr" {
			return true
		}
	}
	return false
}

// isContainerized 判断进程是否运行在容器里。
//
// 检测常见标记：Docker 的 /.dockerenv、Podman 的 /run/.containerenv、
// systemd 的 container 环境变量，以及 cgroup 里的运行时关键字（cgroup v1
// 常见）。容器内改内核参数通常无效或会影响宿主机，宁可不改。
func isContainerized() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if strings.TrimSpace(os.Getenv("container")) != "" {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		cgroup := string(data)
		for _, marker := range []string{"docker", "kubepods", "containerd", "lxc", "podman"} {
			if strings.Contains(cgroup, marker) {
				return true
			}
		}
	}
	return false
}
