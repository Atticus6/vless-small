//go:build linux

package app

import (
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// processCPUSeconds 返回进程累计 CPU 时间（用户态 + 内核态）。
//
// 用 getrusage 而不是解析 /proc/self/stat，避免依赖 USER_HZ。
func processCPUSeconds() (float64, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}

	seconds := float64(usage.Utime.Sec+usage.Stime.Sec) +
		float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	return seconds, true
}

// readSystemCPUCounters 读取 /proc/stat 的系统累计总时间和空闲时间。
func readSystemCPUCounters() (total, idle uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseProcStatCPU(string(data))
}

// readSystemInfo 汇总操作系统级 CPU 负载、内存、磁盘和运行时长。
//
// CPU 百分比需要两次采样，由 server.sampleSystemCPU 填充；这里只读取
// 负载、核数、内存和磁盘。
func readSystemInfo() (*systemHealth, bool) {
	memData, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, false
	}
	memory, ok := parseMeminfo(string(memData))
	if !ok {
		return nil, false
	}

	var load1, load5, load15 float64
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		load1, load5, load15 = parseLoadAvg(string(data))
	}

	var uptimeSeconds int64
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		uptimeSeconds = parseProcUptime(string(data))
	}

	return &systemHealth{
		UptimeSeconds: uptimeSeconds,
		CPU: systemCPU{
			Cores:  runtime.NumCPU(),
			Load1:  load1,
			Load5:  load5,
			Load15: load15,
		},
		Memory: memory,
		Disk:   readSystemDisks(),
	}, true
}

// readSystemDisks 读取 /proc/mounts 中真实文件系统的容量.
//
// 只保留目录挂载点，避免把容器里 bind mount 的单文件（/etc/hosts 等）
// 当成一块磁盘；根目录按挂载点排序后自然排在最前。
func readSystemDisks() []diskHealth {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}

	entries := parseProcMounts(string(data))
	disks := make([]diskHealth, 0, len(entries))
	for _, entry := range entries {
		info, err := os.Stat(entry.Path)
		if err != nil || !info.IsDir() {
			continue
		}

		usage, ok := statfsUsage(entry.Path)
		if !ok {
			continue
		}
		usage.Device = entry.Device
		usage.FSType = entry.FSType
		disks = append(disks, *usage)

		// 容器/桌面环境的挂载点可能很多，限制返回数量。
		if len(disks) >= 20 {
			break
		}
	}

	sort.SliceStable(disks, func(i, j int) bool {
		return disks[i].Path < disks[j].Path
	})
	return disks
}

// processRSSBytes 从 /proc/self/statm 读取当前常驻内存（RSS）。
func processRSSBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}

	// 第二列是常驻页数，乘以页大小得到字节数。
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}

// readDiskUsage 用 statfs 读取二进制所在目录的文件系统容量。
func readDiskUsage(path string) (*diskHealth, bool) {
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, false
		}
		path = cwd
	}
	return statfsUsage(path)
}

// statfsUsage 返回指定路径所在文件系统的容量信息.
func statfsUsage(path string) (*diskHealth, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, false
	}

	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	if total == 0 {
		return nil, false
	}

	// FreeBytes 使用 Bavail（服务用户实际可写），UsedBytes 使用 Bfree，
	// 两者之间可能包含文件系统为 root 保留的块。
	free := stat.Bavail * blockSize
	used := total - stat.Bfree*blockSize
	return &diskHealth{
		Path:        path,
		TotalBytes:  total,
		UsedBytes:   used,
		FreeBytes:   free,
		UsedPercent: round2(float64(used) / float64(total) * 100),
	}, true
}
