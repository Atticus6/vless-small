package main

import (
	"strconv"
	"strings"
)

// mountEntry 是 /proc/mounts 中一条真实文件系统记录.
type mountEntry struct {
	Device string
	Path   string
	FSType string
}

// realFilesystems 是允许出现在 system.disk 里的文件系统类型。
//
// 只列块设备/容器根文件系统，跳过 proc、sysfs、tmpfs、cgroup 等伪文件系统，
// 也跳过 NFS/CIFS 等网络文件系统（statfs 可能阻塞）。
var realFilesystems = map[string]struct{}{
	"ext2": {}, "ext3": {}, "ext4": {},
	"xfs": {}, "btrfs": {}, "zfs": {}, "f2fs": {},
	"jfs": {}, "reiserfs": {},
	"vfat": {}, "ntfs": {}, "ntfs3": {}, "exfat": {}, "fuseblk": {},
	"overlay": {}, "aufs": {},
}

// parseMeminfo 解析 /proc/meminfo（单位 kB），返回操作系统内存占用.
//
// UsedBytes 采用 total - MemAvailable，和 `free -m` 的 used 列一致；
// 旧内核没有 MemAvailable 时用 MemFree + Buffers + Cached 估算。
func parseMeminfo(text string) (systemMemory, bool) {
	values := make(map[string]uint64)
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		values[key] = value * 1024
	}

	total := values["MemTotal"]
	if total == 0 {
		return systemMemory{}, false
	}

	free := values["MemFree"]
	available, ok := values["MemAvailable"]
	if !ok {
		available = free + values["Buffers"] + values["Cached"]
	}
	if available > total {
		available = total
	}

	swapTotal := values["SwapTotal"]
	swapFree := values["SwapFree"]
	swapUsed := uint64(0)
	if swapTotal > swapFree {
		swapUsed = swapTotal - swapFree
	}

	used := total - available
	return systemMemory{
		TotalBytes:     total,
		UsedBytes:      used,
		FreeBytes:      free,
		AvailableBytes: available,
		UsedPercent:    round2(float64(used) / float64(total) * 100),
		SwapTotalBytes: swapTotal,
		SwapUsedBytes:  swapUsed,
	}, true
}

// parseProcStatCPU 解析 /proc/stat 第一行 `cpu ...`，返回累计总时间和空闲时间.
//
// total 取 user..steal 八列之和，idle 取 idle + iowait。
func parseProcStatCPU(text string) (total, idle uint64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}

		for i := 1; i < len(fields) && i <= 8; i++ {
			value, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += value
		}

		idleValue, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		idle = idleValue
		if len(fields) > 5 {
			iowait, err := strconv.ParseUint(fields[5], 10, 64)
			if err == nil {
				idle += iowait
			}
		}

		return total, idle, total > 0
	}

	return 0, 0, false
}

// systemCPUPercent 用 /proc/stat 累计值计算系统平均 CPU 占用.
//
// previous=false 时用启动以来的累计值；否则用两次采样的差值。
func systemCPUPercent(prevTotal, prevIdle, total, idle uint64, previous bool) float64 {
	if previous {
		if total <= prevTotal || idle < prevIdle {
			return 0
		}
		deltaTotal := total - prevTotal
		deltaIdle := idle - prevIdle
		if deltaIdle > deltaTotal {
			deltaIdle = deltaTotal
		}
		return round2(float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100)
	}

	if total == 0 || idle > total {
		return 0
	}
	return round2(float64(total-idle) / float64(total) * 100)
}

// parseLoadAvg 解析 /proc/loadavg 的前三个负载值；缺失或非法时返回 0.
func parseLoadAvg(text string) (load1, load5, load15 float64) {
	fields := strings.Fields(text)
	if len(fields) > 0 {
		load1, _ = strconv.ParseFloat(fields[0], 64)
	}
	if len(fields) > 1 {
		load5, _ = strconv.ParseFloat(fields[1], 64)
	}
	if len(fields) > 2 {
		load15, _ = strconv.ParseFloat(fields[2], 64)
	}
	return load1, load5, load15
}

// parseProcUptime 解析 /proc/uptime 的第一列（秒）.
func parseProcUptime(text string) int64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || value < 0 {
		return 0
	}
	return int64(value)
}

// parseProcMounts 解析 /proc/mounts，过滤伪文件系统并按设备去重.
//
// 同一设备挂载到多个目录（bind mount）时只保留第一条；overlay 例外，
// 因为容器的根文件系统通常都叫 overlay。
func parseProcMounts(text string) []mountEntry {
	entries := make([]mountEntry, 0, 8)
	seenDevice := make(map[string]struct{}, 8)

	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		fsType := fields[2]
		if _, ok := realFilesystems[fsType]; !ok {
			continue
		}

		device := decodeMountField(fields[0])
		path := decodeMountField(fields[1])
		if device == "" || path == "" {
			continue
		}

		if _, ok := seenDevice[device]; ok && fsType != "overlay" {
			continue
		}
		seenDevice[device] = struct{}{}

		entries = append(entries, mountEntry{
			Device: device,
			Path:   path,
			FSType: fsType,
		})
	}

	return entries
}

// decodeMountField 还原 /proc/mounts 中的八进制转义（空格、制表符等）.
func decodeMountField(value string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(value)
}
