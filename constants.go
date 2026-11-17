package main

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
)

// XHTTP 默认路径。主控订阅固定下发 /xhttp，节点默认保持一致；
// 需要关闭时显式设置 XHTTP_PATH=none（或 off）。
const (
	defaultXHTTPPath   = "/xhttp"
	xhttpDisabledValue = "none"
)

// XHTTP packet-up 限制。
// 单次 POST 上限与 Xray 默认 sc-max-each-post-bytes 对齐；
// 缓冲上限用于约束乱序上传队列的内存占用。
const (
	xhttpPacketMaxBytes     = 4 << 20  // 4 MiB
	xhttpMaxBufferedPackets = 64       // 乱序包数量上限
	xhttpMaxBufferedBytes   = 16 << 20 // 16 MiB
	xhttpSessionReapTimeout = 30 * time.Second
)
