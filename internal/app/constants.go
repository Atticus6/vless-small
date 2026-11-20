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

	// bufferSize 是 TCP/UDP 转发缓冲，也是单次 read 的最大块。
	//
	// 取 64 KiB：常见客户端（gorilla 默认）把大消息拆成 4 KiB 帧发送，
	// 缓冲区足够大时内核可以在一次 read 里交付多帧；同时 64 KiB 也接近
	// 一次 GSO 聚合后的报文大小，高带宽链路下更容易跑满。
	bufferSize = 64 * 1024

	// upstreamFlushSize 是上行数据累积到多少字节就必须写目标连接。
	//
	// 与 bufferSize 分开是为了不改变转发时机：WebSocket 允许一条消息拆成
	// 多个非终结帧，客户端可能发完半条消息就等目标响应。若只在缓冲区满
	// 或消息 EOF 时写，32 KiB 的半条消息会被压到 64 KiB，客户端和目标
	// 就会互相等待。保持 32 KiB 的刷新阈值，既合并小帧，又保留原来的
	// 及时转发语义。
	upstreamFlushSize = 32 * 1024

	// wsReadBufferSize / wsWriteBufferSize 是 gorilla 每条 WebSocket 连接的
	// bufio 缓冲。读缓冲取 64 KiB 才能把客户端拆出的多个 4 KiB 帧在一次
	// 系统调用里收进来；写缓冲取 64 KiB 让一条 64 KiB 下行消息最多只多出
	// 一次零头写入。两者按连接分配，用内存换系统调用和延迟。
	wsReadBufferSize  = 64 * 1024
	wsWriteBufferSize = 64 * 1024

	// 从主控拉取设备的间隔.
	nodeSyncInterval = 60 * time.Second

	// 按设备统计的流量每 5 分钟检查一次；单批最多 200 个设备，
	// 保证 Worker 聚合后的用户数据点不超过 Analytics Engine 单次 250 点限制.
	// 本轮总量低于 4 KiB 时暂缓，继续累计；最多跳过 5 次（30 分钟）后强制上报，
	// 避免微小流量永久不落库.
	trafficReportInterval   = 5 * time.Minute
	trafficReportBatchSize  = 200
	trafficReportMinBytes   = 4 << 10
	trafficForceReportAfter = 5
)
