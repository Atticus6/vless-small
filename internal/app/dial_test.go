package app

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIPAddrs 构造地址列表，便于测试排序和过滤。
func fakeIPAddrs(values ...string) []net.IPAddr {
	addrs := make([]net.IPAddr, 0, len(values))
	for _, value := range values {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(value)})
	}
	return addrs
}

// trackingConn 记录 Close 是否被调用，用于验证迟到连接不会泄漏。
type trackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestDNSCacheReusesResult(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		calls.Add(1)
		return fakeIPAddrs("192.0.2.1", "192.0.2.2"), nil
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(addrs) != 2 || addrs[0].IP.String() != "192.0.2.1" {
			t.Fatalf("addrs = %v", addrs)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls.Load())
	}
}

func TestDNSCacheNegativeResult(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("temporary failure")
		}
		return fakeIPAddrs("192.0.2.1"), nil
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err == nil {
		t.Fatal("expected first resolve to fail")
	}
	// 负缓存期内不再发起解析。
	now = now.Add(cache.negativeTTL - time.Second)
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err == nil {
		t.Fatal("expected cached failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls.Load())
	}

	// 负缓存过期后重新解析。
	now = now.Add(2 * time.Second)
	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatalf("resolve after negative TTL: %v", err)
	}
	if len(addrs) != 1 || calls.Load() != 2 {
		t.Fatalf("addrs = %v, calls = %d", addrs, calls.Load())
	}
}

func TestDNSCacheSingleflight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		calls.Add(1)
		close(started)
		<-release
		return fakeIPAddrs("192.0.2.1"), nil
	})

	const waiters = 8
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	wg.Add(waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			defer wg.Done()
			_, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
			errs <- err
		}()
	}

	<-started
	// 留出时间让其余调用进入 inflight 等待。
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls.Load())
	}
}

// TestDNSCacheKeySemantics 验证缓存键只统一大小写，保留末尾根点：
// 相对名与绝对名可能解析到不同地址，不能共用一个键。
func TestDNSCacheKeySemantics(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	cache := newDNSCache(func(_ context.Context, _ string, host string) ([]net.IPAddr, error) {
		mu.Lock()
		seen = append(seen, host)
		mu.Unlock()
		return fakeIPAddrs("192.0.2.1"), nil
	})

	// 大小写不同的等价域名共用缓存。
	for _, host := range []string{"Example.COM", "example.com", " EXAMPLE.com "} {
		if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), host); err != nil {
			t.Fatal(err)
		}
	}
	// 末尾根点是绝对名，必须单独解析。
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com."); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 {
		t.Fatalf("lookup calls = %v, want 2 (relative + absolute)", seen)
	}
	if seen[0] == seen[1] {
		t.Fatalf("relative and absolute names must resolve separately, got %v", seen)
	}
}

// TestDNSCacheFirstCallerHonorsCancellation 验证冷缓存时首个调用者也能被
// ctx 取消，不必陪着慢 DNS 等满 dnsLookupTimeout。
func TestDNSCacheFirstCallerHonorsCancellation(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		<-block
		return fakeIPAddrs("192.0.2.1"), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := cache.resolve(ctx, dnsFamily("tcp"), "slow.example.com"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("resolve took %s, want prompt cancellation", elapsed)
	}
}

// TestDNSCacheFirstCallerDoesNotCancelSharedLookup 验证首个调用者取消后，
// 共享解析仍在进行，后续调用者仍能拿到结果。
func TestDNSCacheFirstCallerDoesNotCancelSharedLookup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	// 测试异常退出时也要放行假查询，避免 goroutine 永久阻塞。
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	// 计数用于证明两个调用者共享同一次查询：若实现改成“取消后重新查询”，
	// 地址断言仍会通过，但计数会变成 2。
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return fakeIPAddrs("192.0.2.7"), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := cache.resolve(ctx, dnsFamily("tcp"), "shared.example.com")
		errCh <- err
	}()

	<-started
	cancel() // 首个调用者放弃
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first caller err = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first caller did not return after cancel")
	}

	// 首个调用者取消后，共享查询必须仍然存在（仍在等待 release）。
	// 这直接验证了“取消首个调用者不会取消共享查询”。
	key := dnsCacheKey(dnsFamily("tcp"), "shared.example.com")
	cache.mu.Lock()
	_, stillRunning := cache.inflight[key]
	cache.mu.Unlock()
	if !stillRunning {
		t.Fatal("shared lookup was dropped when the first caller canceled")
	}

	// 第二个调用者应复用同一次查询。
	resultCh := make(chan []net.IPAddr, 1)
	go func() {
		addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "shared.example.com")
		if err != nil {
			resultCh <- nil
			return
		}
		resultCh <- addrs
	}()
	releaseOnce.Do(func() { close(release) })

	select {
	case addrs := <-resultCh:
		if len(addrs) != 1 || addrs[0].IP.String() != "192.0.2.7" {
			t.Fatalf("addrs = %v", addrs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second caller did not get shared result")
	}
	if calls.Load() != 1 {
		t.Fatalf("lookup calls = %d, want one shared query", calls.Load())
	}
}

// TestDNSCacheServesStaleWhileRefreshing 验证过期但在陈旧窗口内的结果会
// 立即返回，同时后台刷新缓存，避免 URL 测试再次等待 DNS。
func TestDNSCacheServesStaleWhileRefreshing(t *testing.T) {
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return fakeIPAddrs("192.0.2.1"), nil
		}
		startedOnce.Do(func() { close(started) })
		<-release
		return fakeIPAddrs("192.0.2.2"), nil
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(cache.positiveTTL + time.Second)
	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatalf("stale resolve: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "192.0.2.1" {
		t.Fatalf("stale addrs = %v, want old address", addrs)
	}

	<-started
	close(release)

	// 后台刷新完成后，缓存应换成新地址。
	deadline := time.Now().Add(2 * time.Second)
	for {
		addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
		if err != nil {
			t.Fatalf("resolve after refresh: %v", err)
		}
		if addrs[0].IP.String() == "192.0.2.2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache not refreshed, got %v", addrs)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("lookup calls = %d, want 2", calls.Load())
	}
}

// TestDNSCacheBeyondStaleRefreshesSynchronously 验证超出陈旧窗口后必须
// 同步解析，不能无限期返回旧地址。
func TestDNSCacheBeyondStaleRefreshesSynchronously(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return fakeIPAddrs("192.0.2.1"), nil
		}
		time.Sleep(30 * time.Millisecond)
		return fakeIPAddrs("192.0.2.2"), nil
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(cache.positiveTTL + cache.staleTTL + time.Second)
	start := time.Now()
	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("resolve returned in %s, want synchronous lookup", elapsed)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "192.0.2.2" {
		t.Fatalf("addrs = %v, want refreshed address", addrs)
	}
}

// TestDNSCacheKeepsLastGoodOnRefreshFailure 验证后台刷新失败时保留最后
// 一次可用地址，而不是立刻把所有连接变成解析失败。
func TestDNSCacheKeepsLastGoodOnRefreshFailure(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return fakeIPAddrs("192.0.2.1"), nil
		}
		return nil, errors.New("dns timeout")
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(cache.positiveTTL + time.Second)
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatalf("stale resolve: %v", err)
	}

	// 等后台刷新失败并写回缓存。
	deadline := time.Now().Add(2 * time.Second)
	for {
		cache.mu.Lock()
		_, running := cache.inflight[dnsCacheKey("ip", "example.com")]
		cache.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatalf("resolve after failed refresh: %v", err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "192.0.2.1" {
		t.Fatalf("addrs = %v, want last good address", addrs)
	}
	if calls.Load() != 2 {
		t.Fatalf("lookup calls = %d, want 2", calls.Load())
	}
}

// TestDNSCacheRefreshFailureDoesNotExtendStaleWindow 验证反复刷新失败不会
// 延长旧地址的最终有效期：过了 staleUntil 之后必须停止返回旧地址。
func TestDNSCacheRefreshFailureDoesNotExtendStaleWindow(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return fakeIPAddrs("192.0.2.1"), nil
		}
		return nil, errors.New("dns timeout")
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}
	// 第一次成功解析后算出的最终失效时间。
	cache.mu.Lock()
	staleUntil := cache.entries[dnsCacheKey("ip", "example.com")].staleUntil
	cache.mu.Unlock()

	// 反复“过期 + 刷新失败”，每次失败都会推迟重试时间。
	for i := 0; i < 5; i++ {
		now = now.Add(cache.positiveTTL + time.Second)
		addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
		if err != nil {
			t.Fatalf("round %d stale resolve: %v", i, err)
		}
		if len(addrs) != 1 {
			t.Fatalf("round %d addrs = %v", i, addrs)
		}
		waitForNoInflight(t, cache, "ip", "example.com")

		cache.mu.Lock()
		got := cache.entries[dnsCacheKey("ip", "example.com")].staleUntil
		cache.mu.Unlock()
		if !got.Equal(staleUntil) {
			t.Fatalf("round %d: staleUntil moved from %s to %s", i, staleUntil, got)
		}
	}

	// 越过 staleUntil 后，旧地址不能再被返回。
	now = staleUntil.Add(time.Second)
	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err == nil {
		t.Fatalf("expected failure past stale window, got addrs %v", addrs)
	}
}

// waitForNoInflight 等待某个域名在指定族下的解析结束。
func waitForNoInflight(t *testing.T, cache *dnsCache, network, host string) {
	t.Helper()
	key := dnsCacheKey(network, host)
	deadline := time.Now().Add(2 * time.Second)
	for {
		cache.mu.Lock()
		_, running := cache.inflight[key]
		cache.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lookup for %s did not finish", host)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDNSCacheForgetVersion 验证按版本失效：版本匹配时删除，不匹配时保留。
func TestDNSCacheForgetVersion(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		calls.Add(1)
		return fakeIPAddrs("192.0.2.1"), nil
	})

	_, version, err := cache.resolveVersioned(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if version == 0 {
		t.Fatal("expected non-zero cache version")
	}

	// 用错误的版本失效：条目应保留。
	cache.forgetVersion(dnsFamily("tcp"), "example.com", version+100)
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("lookup calls = %d, want cache kept", calls.Load())
	}

	// 用正确版本失效：下一条连接重新解析。
	cache.forgetVersion(dnsFamily("tcp"), "example.com", version)
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("lookup calls = %d, want 2", calls.Load())
	}
}

// TestDNSCacheForgetVersionKeepsRefreshedEntry 验证失败的旧地址不会把后台
// 刷新换上的新结果一起删掉。
func TestDNSCacheForgetVersionKeepsRefreshedEntry(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		if calls.Add(1) == 1 {
			return fakeIPAddrs("192.0.2.1"), nil
		}
		return fakeIPAddrs("192.0.2.2"), nil
	})
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }

	_, staleVersion, err := cache.resolveVersioned(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatal(err)
	}

	// 过期后拿到旧地址并触发后台刷新，缓存换成新地址。
	now = now.Add(cache.positiveTTL + time.Second)
	if _, _, err := cache.resolveVersioned(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}
	waitForNoInflight(t, cache, dnsFamily("tcp"), "example.com")

	// 用旧版本失效：新结果必须保留。
	cache.forgetVersion(dnsFamily("tcp"), "example.com", staleVersion)
	addrs, _, err := cache.resolveVersioned(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "192.0.2.2" {
		t.Fatalf("addrs = %v, want refreshed address kept", addrs)
	}
	if calls.Load() != 2 {
		t.Fatalf("lookup calls = %d, want 2", calls.Load())
	}
}

func TestDNSCacheLiteralSkipsLookup(t *testing.T) {
	// 解析在 resolve 内部的 goroutine 中执行，这里不能调用 t.Fatal：
	// 用原子标记记录是否被调用，断言放回测试主 goroutine。
	var lookedUp atomic.Bool
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		lookedUp.Store(true)
		return nil, errors.New("unexpected lookup")
	})

	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "2001:db8::1")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "2001:db8::1" {
		t.Fatalf("addrs = %v", addrs)
	}

	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "  "); err == nil {
		t.Fatal("expected error for empty host")
	}
	if lookedUp.Load() {
		t.Fatal("lookup must not be called for IP literals")
	}
}

// TestDNSCacheLiteralWithZone 验证带 zone 的 IPv6 字面量不会被当成域名查 DNS。
func TestDNSCacheLiteralWithZone(t *testing.T) {
	var lookedUp atomic.Bool
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		lookedUp.Store(true)
		return nil, errors.New("unexpected lookup")
	})

	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp6"), "fe80::1%eth0")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].IP.String() != "fe80::1" {
		t.Fatalf("addrs = %v", addrs)
	}
	if lookedUp.Load() {
		t.Fatal("lookup must not be called for IP literals")
	}
}

// TestDNSCacheSeparatesAddressFamilies 验证 A 与 AAAA 结果分开缓存：
// IPv4 绑定不能复用 AAAA 结果，反之亦然。
func TestDNSCacheSeparatesAddressFamilies(t *testing.T) {
	var calls atomic.Int64
	cache := newDNSCache(func(_ context.Context, network, _ string) ([]net.IPAddr, error) {
		calls.Add(1)
		if network == "ip4" {
			return fakeIPAddrs("192.0.2.1"), nil
		}
		return fakeIPAddrs("2001:db8::1"), nil
	})

	v4, err := cache.resolve(context.Background(), "ip4", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(v4) != 1 || v4[0].IP.To4() == nil {
		t.Fatalf("ip4 addrs = %v", v4)
	}

	v6, err := cache.resolve(context.Background(), "ip6", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(v6) != 1 || v6[0].IP.To4() != nil {
		t.Fatalf("ip6 addrs = %v", v6)
	}
	if calls.Load() != 2 {
		t.Fatalf("lookup calls = %d, want one per family", calls.Load())
	}

	// 重复请求命中各自族的缓存。
	for _, network := range []string{"ip4", "ip6"} {
		if _, err := cache.resolve(context.Background(), network, "example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("lookup calls = %d, want cache hits", calls.Load())
	}
}

// TestDNSCacheReturnsIndependentCopies 验证调用方修改返回的地址不会污染缓存。
func TestDNSCacheReturnsIndependentCopies(t *testing.T) {
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		return fakeIPAddrs("192.0.2.1"), nil
	})

	addrs, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	addrs[0].IP[3] = 99 // 篡改返回值

	again, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := again[0].IP.String(); got != "192.0.2.1" {
		t.Fatalf("cached address mutated to %s", got)
	}
}

func TestOrderAddressesPrefersIPv4(t *testing.T) {
	addrs := fakeIPAddrs("2001:db8::1", "192.0.2.2", "2001:db8::2", "192.0.2.1")
	ordered := orderAddresses(addrs)

	// 先比长度：否则返回空切片或前缀时循环不会暴露丢地址的问题。
	want := []string{"192.0.2.2", "192.0.2.1", "2001:db8::1", "2001:db8::2"}
	if len(ordered) != len(want) {
		t.Fatalf("ordered has %d addresses, want %d: %v", len(ordered), len(want), ordered)
	}
	for i, addr := range ordered {
		if addr.IP.String() != want[i] {
			t.Fatalf("ordered[%d] = %s, want %s", i, addr.IP, want[i])
		}
	}
}

func TestFilterAddresses(t *testing.T) {
	addrs := fakeIPAddrs("192.0.2.1", "2001:db8::1")

	v4 := filterAddresses(addrs, "tcp4")
	if len(v4) != 1 || v4[0].IP.String() != "192.0.2.1" {
		t.Fatalf("tcp4 filter = %v", v4)
	}

	v6 := filterAddresses(addrs, "udp6")
	if len(v6) != 1 || v6[0].IP.String() != "2001:db8::1" {
		t.Fatalf("udp6 filter = %v", v6)
	}

	if got := filterAddresses(addrs, "tcp"); len(got) != 2 {
		t.Fatalf("tcp filter = %v, want both families", got)
	}
}

// newTestDialer 构造一个使用假缓存和假拨号的 outboundDialer。
func newTestDialer(
	addrs []net.IPAddr,
	lookupErr error,
	dial dialFunc,
	raceDelay time.Duration,
) *outboundDialer {
	return &outboundDialer{
		cache: newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
			return addrs, lookupErr
		}),
		raceDelay: raceDelay,
		dial:      dial,
	}
}

func TestDialOutboundIgnoresDNSForLiteral(t *testing.T) {
	var dialed atomic.Value
	dialer := newTestDialer(nil, errors.New("lookup must not run"), func(
		_ context.Context,
		network, address string,
		_ net.Addr,
	) (net.Conn, error) {
		dialed.Store(network + " " + address)
		return &fakeConn{}, nil
	}, time.Millisecond)

	if _, err := dialer.connect(
		context.Background(),
		"tcp",
		"[2001:db8::1]:443",
		nil,
	); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := dialed.Load(); got != "tcp [2001:db8::1]:443" {
		t.Fatalf("dialed = %v", got)
	}
}

func TestDialOutboundUDPUsesPreferredAddress(t *testing.T) {
	var dialed atomic.Value
	dialer := newTestDialer(
		fakeIPAddrs("2001:db8::1", "192.0.2.9"),
		nil,
		func(_ context.Context, network, address string, _ net.Addr) (net.Conn, error) {
			dialed.Store(network + " " + address)
			return &fakeConn{}, nil
		},
		0,
	)

	if _, err := dialer.connect(context.Background(), "udp", "dns.example.com:53", nil); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := dialed.Load(); got != "udp 192.0.2.9:53" {
		t.Fatalf("dialed = %v, want IPv4 address first", got)
	}
}

// TestDialOutboundUDPFallsBackToNextAddress 验证首选族不可达时 UDP 会继续
// 尝试后续地址（IPv6-only 节点上双栈域名不能整条会话失败）。
func TestDialOutboundUDPFallsBackToNextAddress(t *testing.T) {
	var mu sync.Mutex
	var attempts []string
	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.9", "2001:db8::1"),
		nil,
		func(_ context.Context, _, address string, _ net.Addr) (net.Conn, error) {
			mu.Lock()
			attempts = append(attempts, address)
			mu.Unlock()
			if strings.HasPrefix(address, "192.0.2.9:") {
				return nil, errors.New("network is unreachable")
			}
			return &fakeConn{}, nil
		},
		0,
	)

	conn, err := dialer.connect(context.Background(), "udp", "dns.example.com:53", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v, want IPv4 then IPv6", attempts)
	}
	if !strings.HasPrefix(attempts[1], "[2001:db8::1]:") {
		t.Fatalf("second attempt = %q, want IPv6 fallback", attempts[1])
	}
}

// TestDialOutboundUDPReportsAllFailures 验证 UDP 所有地址都失败时返回聚合错误。
func TestDialOutboundUDPReportsAllFailures(t *testing.T) {
	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.9", "2001:db8::1"),
		nil,
		func(_ context.Context, _, address string, _ net.Addr) (net.Conn, error) {
			return nil, errors.New("unreachable " + address)
		},
		0,
	)

	_, err := dialer.connect(context.Background(), "udp", "dns.example.com:53", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"192.0.2.9", "2001:db8::1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %s", err, want)
		}
	}
}

func TestDialOutboundResolveError(t *testing.T) {
	// dial 可能在工作 goroutine 中调用，断言用原子标记回主 goroutine 完成。
	var dialed atomic.Bool
	dialer := newTestDialer(nil, errors.New("dns down"), func(
		context.Context, string, string, net.Addr,
	) (net.Conn, error) {
		dialed.Store(true)
		return nil, errors.New("unexpected dial")
	}, 0)

	if _, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil); err == nil {
		t.Fatal("expected resolve error")
	}
	if dialed.Load() {
		t.Fatal("dial must not be attempted when resolve fails")
	}
}

// TestDialHedgedFallsBackAfterDelay 验证首选地址无响应时，第二个地址会在
// hedge 间隔后并行尝试并成功返回。
func TestDialHedgedFallsBackAfterDelay(t *testing.T) {
	var attempts atomic.Int64
	delay := 30 * time.Millisecond
	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.1", "192.0.2.2"),
		nil,
		func(ctx context.Context, network, address string, _ net.Addr) (net.Conn, error) {
			attempts.Add(1)
			if strings.HasPrefix(address, "192.0.2.1:") {
				// 模拟黑洞地址：一直等到 ctx 被取消。
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &fakeConn{}, nil
		},
		delay,
	)

	start := time.Now()
	conn, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn.Close()

	if elapsed < delay {
		t.Fatalf("returned in %s, want at least one hedge interval", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("hedged dial took %s, too slow", elapsed)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

// TestDialHedgedStartsNextOnFailure 验证地址立即失败时不需要等 hedge 间隔。
func TestDialHedgedStartsNextOnFailure(t *testing.T) {
	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.1", "192.0.2.2"),
		nil,
		func(_ context.Context, _, address string, _ net.Addr) (net.Conn, error) {
			if strings.HasPrefix(address, "192.0.2.1:") {
				return nil, errors.New("network unreachable")
			}
			return &fakeConn{}, nil
		},
		time.Hour, // 失败补位不应依赖这个间隔
	)

	start := time.Now()
	conn, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("failover took %s, want immediate", elapsed)
	}
}

func TestDialHedgedReportsAllErrors(t *testing.T) {
	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.1", "192.0.2.2"),
		nil,
		func(_ context.Context, _, address string, _ net.Addr) (net.Conn, error) {
			return nil, errors.New("connect failed for " + address)
		},
		time.Millisecond,
	)

	_, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"192.0.2.1", "192.0.2.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %s", err, want)
		}
	}
}

// TestDialOutboundKeepsIPv6Zone 验证带 zone 的 IPv6 字面量把 zone 带到拨号目标，
// 且完全不走 DNS。
func TestDialOutboundKeepsIPv6Zone(t *testing.T) {
	var dialed atomic.Value
	var lookedUp atomic.Bool
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		lookedUp.Store(true)
		return nil, errors.New("unexpected lookup")
	})
	dialer := &outboundDialer{
		cache:     cache,
		raceDelay: time.Millisecond,
		dial: func(_ context.Context, network, address string, _ net.Addr) (net.Conn, error) {
			dialed.Store(network + " " + address)
			return &fakeConn{}, nil
		},
	}

	conn, err := dialer.connect(context.Background(), "tcp", "[fe80::1%eth0]:443", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn.Close()

	if got := dialed.Load(); got != "tcp [fe80::1%eth0]:443" {
		t.Fatalf("dialed = %v, want zone preserved", got)
	}
	if lookedUp.Load() {
		t.Fatal("IP literal must not be resolved through DNS")
	}
}

// TestDialOutboundForgetsCacheOnInternalTimeout 验证内部拨号预算耗尽
// （调用方未取消）会失效缓存：地址可能已失效，下一条连接应重新解析。
//
// 直接让假拨号返回 DeadlineExceeded 来模拟内部超时，避免真的等满 dialTimeout。
func TestDialOutboundForgetsCacheOnInternalTimeout(t *testing.T) {
	var lookups atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return fakeIPAddrs("192.0.2.1"), nil
	})
	dialer := &outboundDialer{
		cache:     cache,
		raceDelay: time.Millisecond,
		dial: func(context.Context, string, string, net.Addr) (net.Conn, error) {
			return nil, context.DeadlineExceeded
		},
	}

	// 调用方 context 未取消，因此这是内部超时，应失效缓存。
	for i := 0; i < 2; i++ {
		if _, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil); err == nil {
			t.Fatal("expected dial error")
		}
	}
	if lookups.Load() != 2 {
		t.Fatalf("lookup calls = %d, want cache invalidated", lookups.Load())
	}
}

// TestDialOutboundKeepsCacheOnCancel 验证调用方取消不会清掉有效的解析缓存。
//
// 先预热缓存再取消连接：如果实现错误地在取消时失效缓存，第二次解析就会
// 触发新的查询，计数从 1 变成 2。
func TestDialOutboundKeepsCacheOnCancel(t *testing.T) {
	var lookups atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return fakeIPAddrs("192.0.2.1"), nil
	})
	dialer := &outboundDialer{
		cache:     cache,
		raceDelay: time.Millisecond,
		dial: func(ctx context.Context, _, _ string, _ net.Addr) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	// 预热缓存。
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("lookup calls = %d, want 1 after warmup", lookups.Load())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dialer.connect(ctx, "tcp", "example.com:443", nil); err == nil {
		t.Fatal("expected error")
	}

	// 缓存仍在：再解析一次不应触发新的查询。
	if _, err := cache.resolve(context.Background(), dnsFamily("tcp"), "example.com"); err != nil {
		t.Fatal(err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("lookup calls = %d, want cache preserved", lookups.Load())
	}
}

// TestDialOutboundForgetsCacheOnFailure 验证连接全部失败后会丢弃缓存的
// 地址，下一次连接重新解析，避免过期地址一直用到 TTL 结束。
func TestDialOutboundForgetsCacheOnFailure(t *testing.T) {
	var lookups atomic.Int64
	cache := newDNSCache(func(context.Context, string, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return fakeIPAddrs("192.0.2.1"), nil
	})
	dialer := &outboundDialer{
		cache:     cache,
		raceDelay: time.Millisecond,
		dial: func(context.Context, string, string, net.Addr) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
	}

	for i := 0; i < 2; i++ {
		if _, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil); err == nil {
			t.Fatal("expected dial error")
		}
	}
	if lookups.Load() != 2 {
		t.Fatalf("lookup calls = %d, want 2", lookups.Load())
	}
}

// TestDialHedgedClosesConnectionsAfterTimeout 验证整体超时返回后，仍在途的
// 拨号即使最终成功建立连接也会被关闭，而不是泄漏。
//
// 用事件屏障而不是固定 sleep 控制时序：先确认两个拨号都已启动，再触发
// 取消，避免慢机器上第二个拨号还没开始就超时导致误报。
func TestDialHedgedClosesConnectionsAfterTimeout(t *testing.T) {
	var mu sync.Mutex
	var conns []*trackingConn

	bothStarted := make(chan struct{})
	var startedMu sync.Mutex
	started := 0
	release := make(chan struct{})

	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.1", "192.0.2.2"),
		nil,
		func(ctx context.Context, _, address string, _ net.Addr) (net.Conn, error) {
			startedMu.Lock()
			started++
			if started == 2 {
				close(bothStarted)
			}
			startedMu.Unlock()

			// 忽略 ctx：模拟内核里已经完成握手、但结果还没返回的连接。
			// 由测试显式放行，保证成功结果一定晚于取消。
			<-release
			conn := &trackingConn{Conn: &fakeConn{}}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			return conn, nil
		},
		5*time.Millisecond,
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := dialer.connect(ctx, "tcp", "example.com:443", nil)
		errCh <- err
	}()

	// 两个地址都在拨号中，此时取消才是确定的“超时后仍有在途连接”。
	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("hedged dial did not start both addresses")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return after cancel")
	}

	// 放行在途拨号：它们成功返回的连接必须被关闭。
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := len(conns)
		allClosed := got == 2
		for _, conn := range conns {
			allClosed = allClosed && conn.closed.Load()
		}
		mu.Unlock()
		if got == 2 && allClosed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connections = %d, not all closed", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDialHedgedClosesLateConnections 验证主流程采用第一个连接后，晚到的
// 成功连接会被关闭，不会泄漏。
func TestDialHedgedClosesLateConnections(t *testing.T) {
	var late *trackingConn
	var lateMu sync.Mutex
	// secondStarted 在第二个地址开始拨号后关闭，用它代替固定 sleep 来
	// 保证“两个拨号都已启动”这个前提成立，避免依赖机器速度。
	secondStarted := make(chan struct{})
	var secondOnce sync.Once

	dialer := newTestDialer(
		fakeIPAddrs("192.0.2.1", "192.0.2.2"),
		nil,
		func(ctx context.Context, _, address string, _ net.Addr) (net.Conn, error) {
			if strings.HasPrefix(address, "192.0.2.2:") {
				secondOnce.Do(func() { close(secondStarted) })
				// 等首选地址胜出后再返回，确保这是一个“晚到”的成功连接。
				<-ctx.Done()
				conn := &trackingConn{Conn: &fakeConn{}}
				lateMu.Lock()
				late = conn
				lateMu.Unlock()
				return conn, nil
			}
			// 首选地址等第二个拨号启动后再成功，确保 hedged 分支被真正触发。
			<-secondStarted
			return &fakeConn{}, nil
		},
		5*time.Millisecond,
	)

	conn, err := dialer.connect(context.Background(), "tcp", "example.com:443", nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	// 等晚到的拨号结束并确认连接被关闭。
	deadline := time.Now().Add(2 * time.Second)
	for {
		lateMu.Lock()
		candidate := late
		lateMu.Unlock()
		if candidate != nil && candidate.closed.Load() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("late connection was not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
