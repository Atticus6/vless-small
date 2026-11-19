package app

import "time"

// VLESS 协议常量.
const (
	vlessVersion = 0

	cmdTCP = 1
	cmdUDP = 2

	atypIPv4   = 1
	atypDomain = 2
	atypIPv6   = 3
)

// 转发调优常量.
const (
	dialTimeout    = 10 * time.Second
	writeTimeout   = 10 * time.Second
	readTimeout    = 60 * time.Second // WS 读超时, 由 pong 续期
	headerTimeout  = 30 * time.Second
	pingInterval   = 25 * time.Second
	udpIdleTimeout = 2 * time.Minute
	maxMessageSize = 256 * 1024
	bufferSize     = 32 * 1024

	// 从主控拉取设备的间隔.
	nodeSyncInterval = 60 * time.Second

	// 按设备统计的流量每 30 分钟上报一次；单批最多 200 个设备，
	// 保证 Worker 聚合后的用户数据点不超过 Analytics Engine 单次 250 点限制.
	trafficReportInterval  = 30 * time.Minute
	trafficReportBatchSize = 200
)
