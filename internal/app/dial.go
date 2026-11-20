package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// 出站连接的调优参数。
//
// Release 二进制以 CGO_ENABLED=0 构建，域名解析走 Go 自带的纯 Go resolver：
// 它不经过 nscd/systemd-resolved 的缓存，每次查询都是一次真实的 DNS 往返。
// v2rayNG 的 URL 延迟测试和网页加载都会对同一批域名频繁新建连接，如果每条
// 连接都解析一次，DNS 延迟会直接叠加到用户看到的“延迟”和“起速”上。
const (
	// dnsLookupTimeout 限制单次解析耗时，避免异常 DNS 把连接拖到 dialTimeout。
	dnsLookupTimeout = 5 * time.Second

	// dnsPositiveTTL 是解析成功结果的缓存时间。
	//
	// net.Resolver 不暴露 DNS 记录的 TTL，这里取保守折中：CDN 地址在 5 分钟
	// 内基本稳定；过期后重新解析，既省掉逐连接查询的 RTT，又不会长期使用
	// 已经下线的地址。
	dnsPositiveTTL = 5 * time.Minute

	// dnsNegativeTTL 是解析失败结果的缓存时间。短缓存让 DNS 故障期间每条
	// 连接快速失败，而不是各等满 dnsLookupTimeout；恢复后也能很快重试。
	dnsNegativeTTL = 10 * time.Second

	// dnsStaleTTL 是成功结果过期后仍可继续返回的“陈旧窗口”。
	//
	// 过期但仍在窗口内时不阻塞调用方，先返回旧地址并后台刷新：v2rayNG 的
	// URL 测试往往间隔数分钟以上，如果每次都同步重新解析，DNS 延迟会重新
	// 出现在关键路径上。窗口外或解析失败的缓存不会被当成新鲜结果使用。
	dnsStaleTTL = 30 * time.Minute

	// maxDNSCacheEntries 限制缓存条目数，防止长期运行后随唯一域名数量无限增长。
	maxDNSCacheEntries = 2048

	// dialRaceDelay 是 hedged dial 中启动下一个地址前的等待时间。
	//
	// 120ms 覆盖一次正常跨国 RTT：首选地址迟迟连不上时立刻并行尝试后续地址，
	// 而首选地址正常时又不会额外建立无用连接。
	dialRaceDelay = 120 * time.Millisecond
)

// lookupIPFunc 是域名解析函数；抽成类型便于测试注入。
//
// network 与 net.Resolver.LookupIP 的取值一致（"ip" / "ip4" / "ip6"）：
// 绑定 IPv4 源地址时必须只查 A 记录，否则纯 Go resolver 会同时发 A 和 AAAA
// 并等两者都返回，AAAA 无响应的域名会被拖到超时。
type lookupIPFunc func(ctx context.Context, network, host string) ([]net.IPAddr, error)

// dnsFamily 把拨号网络名转换成解析族。
//
// 返回 "ip4"/"ip6"/"ip"。它也参与缓存键：同一域名的 A 与 AAAA 结果是两份
// 独立记录，混用会让 IPv4-only 请求拿到 IPv6 地址。
func dnsFamily(network string) string {
	switch network {
	case "tcp4", "udp4":
		return "ip4"
	case "tcp6", "udp6":
		return "ip6"
	default:
		return "ip"
	}
}

// dnsEntry 是一条缓存记录：成功时 addrs 非空，失败时 err 非空。
type dnsEntry struct {
	addrs []net.IPAddr
	err   error
	// expires 是本次结果的新鲜截止时间：成功为 positiveTTL，失败为 negativeTTL。
	expires time.Time
	// staleUntil 是最后一次成功解析结果允许被继续返回的最终截止时间。
	//
	// 它只在解析成功时推进，刷新失败只更新 expires、不会延长它。否则连续
	// 刷新失败会让旧地址无限期可用，超出声明的陈旧窗口。零值表示这条记录
	// 从未成功解析过（纯负缓存），此时不参与新鲜度判定。
	staleUntil time.Time
	// version 每次写入自增，用于让拨号失败方只失效“自己用过的那一版”。
	version uint64
}

// dnsInflight 表示一次正在进行中的解析，同一域名的并发请求共享结果。
type dnsInflight struct {
	done  chan struct{}
	addrs []net.IPAddr
	err   error
	// version 是本次解析写入缓存后的版本号，由 lookupAndStore 填写。
	version uint64
}

// dnsCache 是进程内的域名解析缓存。
//
// 热路径（缓存命中）只加一次互斥锁；未命中时同一域名只会发起一次解析，
// 避免连接突增时把 DNS 打爆。
type dnsCache struct {
	lookup      lookupIPFunc
	now         func() time.Time
	positiveTTL time.Duration
	negativeTTL time.Duration
	staleTTL    time.Duration
	maxEntries  int
	// version 是单调递增的写入序号，给每条缓存记录打版本。
	version uint64

	mu       sync.Mutex
	entries  map[string]dnsEntry
	inflight map[string]*dnsInflight
}

func newDNSCache(lookup lookupIPFunc) *dnsCache {
	return &dnsCache{
		lookup:      lookup,
		now:         time.Now,
		positiveTTL: dnsPositiveTTL,
		negativeTTL: dnsNegativeTTL,
		staleTTL:    dnsStaleTTL,
		maxEntries:  maxDNSCacheEntries,
		entries:     make(map[string]dnsEntry),
		inflight:    make(map[string]*dnsInflight),
	}
}

// resolve 返回域名在指定族下的地址列表；IP 字面量直接返回，不查 DNS。
//
// network 使用 "ip"/"ip4"/"ip6"，与 net.Resolver.LookupIP 一致。返回的切片
// 归调用方所有，调用方可以自由排序或过滤，不会影响缓存。
func (c *dnsCache) resolve(ctx context.Context, network, host string) ([]net.IPAddr, error) {
	addrs, _, err := c.resolveVersioned(ctx, network, host)
	return addrs, err
}

// resolveVersioned 与 resolve 相同，但额外返回本次结果的缓存版本号。
//
// 调用方在连接全部失败后用它做条件失效：只有缓存仍是这一版（地址没有
// 被后台刷新替换）时才删除，避免把刚刷新出来的可用地址一起丢掉。
// 字面量与非缓存结果返回版本 0，调用方不应据此失效任何缓存。
func (c *dnsCache) resolveVersioned(
	ctx context.Context,
	network, host string,
) ([]net.IPAddr, uint64, error) {
	// 查询与缓存键必须用同一个规范化名称：只用原始 host 去查时，
	// " example.com " 会查失败，而失败结果写进 "example.com" 的负缓存，
	// 让正常名称在 negativeTTL 内也解析不了。
	normalized := normalizeDNSHost(host)
	key := dnsCacheKey(network, normalized)
	if key == "" {
		return nil, 0, errors.New("empty host")
	}
	// 字面量不查 DNS，也无需按族过滤：tcp4/tcp6 的族约束由调用方
	// filterAddresses 保证，这里返回原样地址即可。
	if ip, zone := parseHostLiteral(normalized); ip != nil {
		return []net.IPAddr{{IP: ip, Zone: zone}}, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	c.mu.Lock()
	now := c.now()
	if entry, ok := c.entries[key]; ok {
		// 新鲜判定同时受 staleUntil 约束：刷新失败会把 expires 往后推
		// negativeTTL，若这段恰好跨过最终失效时间，不能把旧地址当成新结果。
		fresh := now.Before(entry.expires) &&
			(entry.staleUntil.IsZero() || now.Before(entry.staleUntil))
		if fresh {
			addrs, err, version := cloneIPAddrs(entry.addrs), entry.err, entry.version
			c.mu.Unlock()
			return addrs, version, err
		}
		// 成功结果过期但仍在陈旧窗口内：先返回旧地址，后台刷新。
		if entry.err == nil && len(entry.addrs) > 0 && now.Before(entry.staleUntil) {
			addrs, version := cloneIPAddrs(entry.addrs), entry.version
			c.mu.Unlock()
			c.refresh(key, network, normalized)
			return addrs, version, nil
		}
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		addrs, err := waitDNSLookup(ctx, call)
		return addrs, call.version, err
	}
	call := &dnsInflight{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	// 解析在独立 goroutine 里执行，首个调用者也通过 waitDNSLookup 等待：
	// 共享查询不受调用方取消影响（其他等待者仍需要结果），但每个等待者
	// 自己的 ctx 到期时可以立即返回，而不是陪着慢 DNS 等满 dnsLookupTimeout。
	go func() {
		c.lookupAndStore(key, network, normalized, call)
	}()
	addrs, err := waitDNSLookup(ctx, call)
	return addrs, call.version, err
}

// refresh 在后台重新解析一个已过期但仍可用的缓存条目。
//
// 同一域名同时只有一个刷新；调用方已经拿到旧地址，不等待刷新结果。
func (c *dnsCache) refresh(key, network, host string) {
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && c.now().Before(entry.expires) {
		// 已有别的连接完成刷新，不必重复解析。
		c.mu.Unlock()
		return
	}
	if _, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		return
	}
	call := &dnsInflight{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	go func() {
		c.lookupAndStore(key, network, host, call)
	}()
}

// waitDNSLookup 等待同一域名的共享解析结果。
func waitDNSLookup(ctx context.Context, call *dnsInflight) ([]net.IPAddr, error) {
	select {
	case <-call.done:
		return cloneIPAddrs(call.addrs), call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// lookupAndStore 执行一次真实解析并写入缓存，最后唤醒所有等待者。
//
// 它总是在独立 goroutine 中运行，不接收调用方 context：同域名的并发连接
// 共享这一次解析，任何单个调用方取消都不应影响其他等待者。
func (c *dnsCache) lookupAndStore(key, network, host string, call *dnsInflight) {
	lookupCtx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	addrs, err := c.lookup(lookupCtx, network, host)
	cancel()

	if err == nil {
		addrs = dedupeIPAddrs(addrs)
		if len(addrs) == 0 {
			err = fmt.Errorf("no addresses for %s", host)
		}
	}

	ttl := c.positiveTTL
	if err != nil {
		ttl = c.negativeTTL
	}

	c.mu.Lock()
	now := c.now()
	c.version++
	version := c.version
	// 刷新失败时不要用错误覆盖最后一次可用地址：DNS 抖动不应该让本来能连的
	// 域名立刻不可用。保留旧地址，只推迟重试时间，staleUntil 保持不变——
	// 否则每次失败都续期，旧地址就永远不会真正过期。
	if err != nil {
		if previous, ok := c.entries[key]; ok &&
			previous.err == nil && len(previous.addrs) > 0 &&
			now.Before(previous.staleUntil) {
			keep := previous.addrs
			call.addrs, call.err = keep, nil
			call.version = version
			c.entries[key] = dnsEntry{
				addrs:      keep,
				expires:    now.Add(c.negativeTTL),
				staleUntil: previous.staleUntil,
				version:    version,
			}
			delete(c.inflight, key)
			close(call.done)
			c.mu.Unlock()
			return
		}
	}

	entry := dnsEntry{
		addrs:   addrs,
		err:     err,
		expires: now.Add(ttl),
		version: version,
	}
	if err == nil {
		// 只有成功解析才推进陈旧窗口。
		entry.staleUntil = entry.expires.Add(c.staleTTL)
	}
	call.addrs, call.err = addrs, err
	call.version = version
	c.entries[key] = entry
	c.pruneLocked(now)
	delete(c.inflight, key)
	close(call.done)
	c.mu.Unlock()
}

// forgetVersion 只在缓存仍是 version 这一版时删除条目。
//
// 拨号用的是某一版解析结果；等它失败时，后台刷新可能已经把缓存换成新的
// 可用地址。无条件按键删除会把这份新结果一起丢掉，所以要比对版本。
// version 为 0（字面量等未走缓存的路径）时不删除任何内容。
func (c *dnsCache) forgetVersion(network, host string, version uint64) {
	if version == 0 {
		return
	}
	key := dnsCacheKey(network, normalizeDNSHost(host))
	if key == "" {
		return
	}
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && entry.version == version {
		delete(c.entries, key)
	}
	c.mu.Unlock()
}

// pruneLocked 在缓存超限时清理过期条目，仍超限则移除任意条目。
//
// 只在超限时触发，正常规模下不会在热路径上做全表扫描。
func (c *dnsCache) pruneLocked(now time.Time) {
	if c.maxEntries <= 0 || len(c.entries) <= c.maxEntries {
		return
	}
	for key, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, key)
		}
	}
	for key := range c.entries {
		if len(c.entries) <= c.maxEntries {
			return
		}
		delete(c.entries, key)
	}
}

// normalizeDNSHost 规范化域名：统一大小写并去掉首尾空白，保留末尾根点。
//
// 缓存键和实际 DNS 查询都必须用这个结果，否则空白变体查失败后会把错误写进
// 正常名称的负缓存。
func normalizeDNSHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

// dnsCacheKey 生成缓存键：地址族 + 规范化后的域名。
//
// 地址族必须进入键：同一域名的 A 和 AAAA 是两份独立记录，混用会让
// IPv4-only 请求拿到 IPv6 地址。域名只统一大小写并去掉首尾空白，保留末尾
// 根点——Go resolver 会区分相对名（`api.example`，会拼接 search 域）和
// 绝对名（`api.example.`），两者结果可能不同，合并会连到错误目标。
func dnsCacheKey(network, host string) string {
	normalized := normalizeDNSHost(host)
	if normalized == "" {
		return ""
	}
	return network + "\x00" + normalized
}

// parseHostLiteral 解析 IP 字面量，同时返回 IPv6 的 zone（如 fe80::1%eth0）。
//
// net.ParseIP 不接受 zone 后缀；不处理的话带 zone 的地址会被当成域名去查
// DNS，必然失败。zone 必须一起返回：丢掉它之后拨号目标会变成 [fe80::1]
// 而丢失出站网卡，链路本地地址将无法连接。
func parseHostLiteral(host string) (net.IP, string) {
	trimmed := strings.TrimSpace(host)
	if ip := net.ParseIP(trimmed); ip != nil {
		return ip, ""
	}
	if index := strings.LastIndex(trimmed, "%"); index > 0 {
		ip := net.ParseIP(trimmed[:index])
		if ip != nil && ip.To4() == nil {
			return ip, trimmed[index+1:]
		}
	}
	return nil, ""
}

// cloneIPAddrs 复制地址列表，避免调用方修改缓存内容。
//
// 每个 IPAddr 的 IP 字节也要独立复制：只复制外层切片时，调用方改一个字节
// 就会污染缓存，下一次查询会拿到被改过的地址。
func cloneIPAddrs(addrs []net.IPAddr) []net.IPAddr {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]net.IPAddr, len(addrs))
	for i, addr := range addrs {
		out[i] = net.IPAddr{Zone: addr.Zone}
		if addr.IP != nil {
			out[i].IP = append(net.IP(nil), addr.IP...)
		}
	}
	return out
}

// dedupeIPAddrs 去掉重复地址，保留解析器返回的顺序。
// 会在入参的底层数组上原地压缩；入参必须是本次解析新建的切片。
func dedupeIPAddrs(addrs []net.IPAddr) []net.IPAddr {
	seen := make(map[string]struct{}, len(addrs))
	out := addrs[:0]
	for _, addr := range addrs {
		key := addr.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, addr)
	}
	return out
}

// orderAddresses 按 IPv4 优先排序，同族内保持解析器返回的顺序。
//
// 选 IPv4 优先与 sing-box 的 prefer_ipv4 默认值一致：VPS 上 IPv6 路由
// 残缺或黑洞比 IPv4 常见，先试 IPv4 能避免每条连接都踩一次 IPv6 超时；
// 真正的 IPv6-only 目标仍会由后面的地址列表兜底。
func orderAddresses(addrs []net.IPAddr) []net.IPAddr {
	ordered := make([]net.IPAddr, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			ordered = append(ordered, addr)
		}
	}
	for _, addr := range addrs {
		if addr.IP.To4() == nil {
			ordered = append(ordered, addr)
		}
	}
	return ordered
}

// filterAddresses 去掉网络族不匹配的地址。
//
// 绑定 IPv4 源地址时 network 是 tcp4/udp4，必须过滤 IPv6，否则内核会以
// “address family not supported”失败。普通 tcp/udp 保留两族地址。
func filterAddresses(addrs []net.IPAddr, network string) []net.IPAddr {
	wantV4, wantV6 := true, true
	switch network {
	case "tcp4", "udp4":
		wantV6 = false
	case "tcp6", "udp6":
		wantV4 = false
	}
	if wantV4 && wantV6 {
		return addrs
	}

	filtered := make([]net.IPAddr, 0, len(addrs))
	for _, addr := range addrs {
		isV4 := addr.IP.To4() != nil
		if (isV4 && wantV4) || (!isV4 && wantV6) {
			filtered = append(filtered, addr)
		}
	}
	return filtered
}

// dialFunc 是底层拨号实现；抽成字段便于测试 hedged dial 而不访问真实网络。
type dialFunc func(ctx context.Context, network, address string, local net.Addr) (net.Conn, error)

// outboundDialer 负责把 VLESS 目标地址转换成实际连接。
type outboundDialer struct {
	cache     *dnsCache
	raceDelay time.Duration
	dial      dialFunc
}

// defaultOutboundDialer 是进程级单例，DNS 缓存跨连接共享。
var defaultOutboundDialer = newOutboundDialer()

// resolverLookup 是生产环境的解析函数，把 []net.IP 转成 []net.IPAddr。
//
// net.Resolver.LookupIP 按 network（ip/ip4/ip6）只发对应族的查询；相比
// LookupIPAddr（固定同时查 A 和 AAAA 并等两者都返回），绑定 IPv4 出口时
// 不会被 AAAA 无响应拖慢。
func resolverLookup(ctx context.Context, network, host string) ([]net.IPAddr, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, network, host)
	if err != nil {
		return nil, err
	}
	addrs := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.IPAddr{IP: ip})
	}
	return addrs, nil
}

func newOutboundDialer() *outboundDialer {
	return &outboundDialer{
		cache:     newDNSCache(resolverLookup),
		raceDelay: dialRaceDelay,
		dial:      netDial,
	}
}

// netDial 是生产环境的拨号实现；Timeout 限制单个地址的连接时间。
func netDial(ctx context.Context, network, address string, local net.Addr) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout, LocalAddr: local}
	return dialer.DialContext(ctx, network, address)
}

// dialOutbound 建立到目标的 TCP/UDP 连接，可按需绑定 IPv4 源地址。
//
// 域名会先经过进程内缓存解析，再按 IPv4 优先、失败并行补位的方式连接。
func dialOutbound(ctx context.Context, base, target string, sourceIP net.IP) (net.Conn, error) {
	return defaultOutboundDialer.connect(ctx, base, target, sourceIP)
}

// DialOutbound 用进程级 DNS 缓存和并行拨号建立出站连接。
//
// 供 Hysteria 等其他协议入口复用同一套解析/拨号逻辑；sourceIP 为空表示
// 使用默认出口。
func DialOutbound(ctx context.Context, network, target string) (net.Conn, error) {
	return dialOutbound(ctx, network, target, nil)
}

// connect 是 dialOutbound 的实现主体。
func (d *outboundDialer) connect(ctx context.Context, base, target string, sourceIP net.IP) (net.Conn, error) {
	// 保留调用方的 context 用于区分“谁超时”：内部的 dialTimeout 预算耗尽
	// 说明地址本身可能已失效，应该失效缓存；调用方主动取消只说明这条连接
	// 不要了（例如 WS 已断开），不能据此判断地址不可用。
	callerCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	network := dialNetwork(base, sourceIP)
	local := localAddr(base, sourceIP)

	host, port, err := net.SplitHostPort(target)
	if err != nil {
		// 目标格式异常时保持旧行为：交给底层拨号返回明确的错误。
		return d.dial(ctx, network, target, local)
	}

	addrs, version, err := d.cache.resolveVersioned(ctx, dnsFamily(network), host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	addrs = filterAddresses(addrs, network)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no %s address", host, network)
	}
	addrs = orderAddresses(addrs)

	conn, err := d.dialAny(ctx, network, port, addrs, local, base == "udp")
	if err != nil && callerCtx.Err() == nil {
		// 调用方没有取消，说明失败来自地址本身（拒绝连接、内部拨号超时）：
		// 失效这一版缓存，让下一条连接重新解析。只删自己用过的那一版，
		// 避免把后台刷新刚换上的可用地址一起丢掉。
		d.cache.forgetVersion(dnsFamily(network), host, version)
	}
	return conn, err
}

// dialAny 建立连接；udp=true 时顺序尝试地址，否则使用 hedged dial。
//
// UDP 没有握手，无法判断远端是否可用，但本地拨号失败是确定的（例如 IPv4
// 地址在 IPv6-only 节点上返回 network unreachable）。此时必须继续尝试下一个
// 地址，否则双栈域名会因为首选族不可达而让整条会话失败。
func (d *outboundDialer) dialAny(
	ctx context.Context,
	network, port string,
	addrs []net.IPAddr,
	local net.Addr,
	udp bool,
) (net.Conn, error) {
	if !udp {
		return d.dialHedged(ctx, network, port, addrs, local)
	}

	var errs []error
	for _, addr := range addrs {
		conn, err := d.dial(ctx, network, net.JoinHostPort(addr.String(), port), local)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", addr.String(), err))
	}
	return nil, errors.Join(errs...)
}

// dialResult 是一条地址的拨号结果，二者只会有一个非空。
type dialResult struct {
	conn net.Conn
	err  error
}

// dialHedged 在多个地址之间做 hedged dial（happy eyeballs 的简化版）。
//
// 先连第一个地址；等待 raceDelay 仍未成功就并行连下一个，任一失败则立即
// 补位。第一个成功建立的连接被采用，其余连接关闭。相比 net.Dialer 的串行
// 尝试，这避免了“答案里的第一个 IP 已失效，连接要等满 10 秒超时”的情况。
func (d *outboundDialer) dialHedged(
	ctx context.Context,
	network string,
	port string,
	addrs []net.IPAddr,
	local net.Addr,
) (net.Conn, error) {
	if len(addrs) == 1 {
		return d.dial(ctx, network, net.JoinHostPort(addrs[0].String(), port), local)
	}

	// 整体仍受 dialTimeout 约束，避免地址很多时把时间叠加起来。
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	// results 不设缓冲：主流程一旦返回就关闭 done，晚到的成功连接会走
	// done 分支自行关闭，不会滞留在无人接收的 channel 里泄漏。
	// （ctx 超时返回时尤其重要：此时可能还有拨号在途。）
	results := make(chan dialResult)
	done := make(chan struct{})
	defer close(done)

	launch := func(addr net.IPAddr) {
		target := net.JoinHostPort(addr.String(), port)
		go func() {
			conn, err := d.dial(dialCtx, network, target, local)
			if err != nil {
				select {
				case results <- dialResult{err: fmt.Errorf("%s: %w", addr.String(), err)}:
				case <-done:
				}
				return
			}
			select {
			case results <- dialResult{conn: conn}:
			case <-done:
				_ = conn.Close()
			}
		}()
	}

	launched := 1
	launch(addrs[0])
	hedge := time.NewTimer(d.raceDelay)
	defer hedge.Stop()

	var errs []error
	for {
		select {
		case result := <-results:
			if result.conn != nil {
				return result.conn, nil
			}
			errs = append(errs, result.err)
			if len(errs) == len(addrs) {
				return nil, errors.Join(errs...)
			}
			// 地址立即失败时不等定时器，马上尝试下一个：IPv6-only 节点
			// 解析到 IPv4 地址时，内核会立刻报无路由，不必白等一个间隔。
			if launched < len(addrs) {
				launch(addrs[launched])
				launched++
				resetTimer(hedge, d.raceDelay)
			}
		case <-hedge.C:
			if launched < len(addrs) {
				launch(addrs[launched])
				launched++
				hedge.Reset(d.raceDelay)
			}
		case <-dialCtx.Done():
			return nil, dialCtx.Err()
		}
	}
}

// resetTimer 重置一个可能已经触发过的定时器。
// 只用于 hedge 节奏，残留 tick 最多让下一次尝试提前，不影响正确性。
func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

// localAddr 返回绑定源 IP 所需的本地地址；未指定时返回 nil。
func localAddr(base string, sourceIP net.IP) net.Addr {
	if sourceIP == nil {
		return nil
	}
	ip := sourceIP.To4()
	switch base {
	case "udp":
		return &net.UDPAddr{IP: ip}
	case "tcp":
		return &net.TCPAddr{IP: ip}
	default:
		return nil
	}
}
