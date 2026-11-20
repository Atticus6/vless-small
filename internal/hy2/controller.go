package hy2

import (
	"context"
	"log"
	"sync"
	"time"
)

// Signal 是主控在节点上报响应里返回的 Hysteria 2 开关。
//
// 与 tunnel / cert 的 Signal 一致：nil 表示不改变。Retry 用来表达“再试
// 一次”——端口探测失败时入口会被跳过，主控把这个值递增后节点会重新探测
// 并尝试绑定端口，而不需要重启进程。
type Signal struct {
	// Enabled 表示主控希望开启 hy2 入口。
	Enabled bool `json:"enabled"`
	// Retry 是重试计数；值变化即触发一次重新探测，与 Enabled 无关。
	Retry int `json:"retry,omitempty"`
}

// Controller 管理 Hysteria 2 入口的运行时启停。
//
// 与启动时一次性绑定不同，这里允许主控在运行期间要求重新探测：STUN 探测
// 可能因为临时网络问题失败，重启进程代价太高。Sync 必须幂等——上报循环
// 每 60 秒调用一次，只在信号变化时才动作。
type Controller interface {
	// Sync 应用主控下发的信号；重复调用同样的信号不会重复探测。
	Sync(signal Signal)
	// Status 返回当前入口状态，供上报使用。
	Status() Report
	// Close 停止入口并释放端口。
	Close()
}

// controller 是 Controller 的实现。
//
// 状态机很简单：syncMu 串行化所有 Sync，避免两次上报并发时同时去绑端口；
// current 指向正在运行的 Server（未运行为 nil）；lastRetry 记住已经处理过
// 的重试计数，重复信号直接返回。
type controller struct {
	cfg Config

	syncMu sync.Mutex
	// server 是当前运行的入口；nil 表示未运行。
	server *Server
	// lastRetry 是已经处理过的重试计数。
	lastRetry int
	// attempted 表示已经处理过至少一次开启信号。
	//
	// 与 server 分开记录：探测失败时 server 仍为 nil，但“已经试过”这件事
	// 必须记住，否则每次上报（60 秒）都会重新探测一次——既产生无谓的 STUN
	// 流量，也让“再次尝试开启”按钮失去意义。
	attempted bool
	// lastErr 记录最近一次失败原因，便于日志排查。
	lastErr error
}

// NewController 创建可启停的控制器。
//
// 与 New 不同，这里不会立即绑定端口：是否启动由主控信号决定，首次探测
// 也推迟到第一次 Sync。cfg.Listen 必须是完整的监听地址（含固定端口）。
func NewController(cfg Config) Controller {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &controller{cfg: cfg}
}

// Sync 应用主控信号。
//
// 行为：
//   - 不要求开启 → 停止入口（幂等）；
//   - 要求开启、Retry 与上次相同、且已经尝试过 → 什么都不做。每 60 秒的
//     重复上报不会反复探测，探测失败的节点也要等管理员显式点重试；
//   - 要求开启且（首次 / Retry 递增）→ 重新探测并尝试绑定端口。
//
// 启动失败只记录原因并把状态保持为 disabled，不会让进程退出：hy2 是可选
// 入口，失败时节点仍能提供 WS。
func (c *controller) Sync(signal Signal) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	// 关闭信号优先：只要不要求开启就停掉入口。不能先比较 Retry——关闭信号
	// 的 Retry 通常是 0，与上一次处理过的值不同，会被误判成“重试”而重启。
	if !signal.Enabled {
		c.stopLocked()
		return
	}

	// 已尝试过且重试计数没变：保持现状（运行中继续跑，失败的不重试）。
	if c.attempted && signal.Retry == c.lastRetry {
		return
	}

	// 重新探测前先停掉旧入口：端口会被下一次 Listen 重新绑定，不先释放
	// 会得到 "address already in use"。
	// stopLocked 会清掉 attempted，所以标记必须放在它之后。
	c.stopLocked()
	c.attempted = true
	c.lastRetry = signal.Retry

	srv, err := c.start()
	if err != nil {
		c.lastErr = err
		c.cfg.Logf("hy2: start failed: %v", err)
		return
	}
	if srv == nil {
		// 探测不通过：start 已经记过具体原因。
		return
	}

	c.lastErr = nil
	c.server = srv
	go func() {
		c.cfg.Logf("hy2: listening on %s", srv.Addr())
		if err := srv.Serve(); err != nil {
			c.cfg.Logf("hy2: %v", err)
		}
		// Serve 退出后入口已不再接受连接，清掉 c.server，让“非空 = 正在
		// 运行”这个约定始终成立。不重置 attempted：探测失败的节点仍要等
		// 管理员显式递增 Retry 才重新尝试，不能因为 Serve 退出就自动重试。
		c.clearServerIfCurrent(srv)
	}()
}

// clearServerIfCurrent 在 c.server 仍指向 srv 时清空它。
//
// Sync 期间可能已经用新 server 替换或调用了 Close，此时不能清掉新的引用；
// 用指针相等判断，避免过期的 Serve goroutine 误伤后来启动的入口。
func (c *controller) clearServerIfCurrent(srv *Server) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	if c.server == srv {
		c.server = nil
	}
}

// start 探测端口可达性后绑定监听；探测不通过时返回 (nil, nil)。
//
// 探测会临时占用监听端口，因此必须在绑定之前完成——顺序反了会让自己
// 占着端口导致探测失败。
func (c *controller) start() (*Server, error) {
	// SkipProbe 供探测被防火墙干扰但端口实际可达的环境使用。
	if !c.cfg.SkipProbe {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		result := ProbeReachability(ctx, c.cfg.Listen, c.cfg.PublicIP)
		cancel()

		if !result.Reachable {
			c.cfg.Logf("hy2: skip, %s", result.Reason)
			return nil, nil
		}
		c.cfg.Logf("hy2: port reachable, %s", result.Reason)
	}

	return New(c.cfg)
}

// stopLocked 停止当前入口；调用方必须持有 syncMu。
//
// 同时清掉 attempted：关闭意味着“不再尝试”，下次收到开启信号应该重新探测，
// 而不是被当成已尝试过而跳过。
func (c *controller) stopLocked() {
	c.attempted = false
	if c.server == nil {
		return
	}
	_ = c.server.Close()
	c.server = nil
}

// Status 返回入口状态。
//
// 只读 c.server：它由 syncMu 保护，但上报循环是另一个 goroutine，这里用
// 一次加锁换取正确性。探测/启动是慢操作，不会在这里发生。
func (c *controller) Status() Report {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	if c.server == nil {
		return Report{Enabled: false}
	}
	return c.server.Status()
}

// Close 停止入口；进程退出时调用。
func (c *controller) Close() {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.stopLocked()
}
