package app

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parseDefaultRouteInterface 从 /proc/net/route 内容中解析默认路由网卡。
//
// 文件格式与内核文档 net/route 一致：
//
//	Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
//
// Destination 为 00000000 且带 RTF_GATEWAY(0x2) 的是默认路由；有多条时
// 选择 Metric 最小的一条。解析失败返回空字符串，由调用方回退为不启用。
func parseDefaultRouteInterface(reader io.Reader) string {
	scanner := bufio.NewScanner(reader)

	bestInterface := ""
	bestMetric := ^uint64(0)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 {
			continue
		}
		// 跳过表头；/proc/net/route 第一行是列名。
		if fields[1] == "Destination" {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}

		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x2 == 0 { // RTF_GATEWAY
			continue
		}
		metric, err := strconv.ParseUint(fields[6], 10, 32)
		if err != nil {
			metric = 0
		}
		if bestInterface == "" || metric < bestMetric {
			bestInterface = fields[0]
			bestMetric = metric
		}
	}

	return bestInterface
}
