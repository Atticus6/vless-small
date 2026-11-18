//go:build !linux

package main

// 非 Linux 构建（本地 Windows/macOS 开发）不采集进程级 CPU、RSS 和磁盘信息，
// /healthz 仍返回其余运行状态，保证接口字段稳定。

func processCPUSeconds() (float64, bool) { return 0, false }

func processRSSBytes() (uint64, bool) { return 0, false }

func readDiskUsage(string) (*diskHealth, bool) { return nil, false }

func readSystemInfo() (*systemHealth, bool) { return nil, false }

func readSystemCPUCounters() (uint64, uint64, bool) { return 0, 0, false }
