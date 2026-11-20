package hy2

import (
	"testing"
	"time"
)

// newTestController 构造一个跳过探测的控制器，绑定回环随机端口。
//
// 探测走 STUN，单测里既慢又不稳定；SkipProbe 让 Sync 直接绑端口，正好
// 覆盖启停逻辑本身。
func newTestController(t *testing.T) Controller {
	t.Helper()
	return NewController(Config{
		Listen:       "127.0.0.1:0",
		Password:     "secret",
		CertDir:      t.TempDir(),
		SkipProbe:    true,
		Authenticate: func(string) bool { return true },
	})
}

// TestControllerSyncEnables 验证开启信号会真正绑定端口并把状态置为 enabled。
func TestControllerSyncEnables(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	if ctl.Status().Enabled {
		t.Fatal("controller should start disabled before any signal")
	}

	ctl.Sync(Signal{Enabled: true})
	if !ctl.Status().Enabled {
		t.Fatal("Sync(enabled) did not start the entry")
	}
}

// TestControllerSyncDisables 验证关闭信号会释放入口。
func TestControllerSyncDisables(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true})
	if !ctl.Status().Enabled {
		t.Fatal("setup: entry did not start")
	}

	ctl.Sync(Signal{Enabled: false})
	if ctl.Status().Enabled {
		t.Fatal("Sync(disabled) did not stop the entry")
	}
}

// TestControllerSyncIdempotent 验证重复的相同信号不会重启入口。
//
// 上报循环每 60 秒调用一次 Sync；如果每次都重启，QUIC 连接会被无谓地
// 掐断。用“重试计数不变 + 已启用”判断跳过。
func TestControllerSyncIdempotent(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true})
	first := ctl.(*controller).server
	if first == nil {
		t.Fatal("setup: entry did not start")
	}

	// 相同信号重复下发：不应替换 server 实例。
	for i := 0; i < 3; i++ {
		ctl.Sync(Signal{Enabled: true})
	}
	if ctl.(*controller).server != first {
		t.Fatal("identical signal restarted the entry")
	}
}

// TestControllerRetryRestarts 验证 Retry 递增会重新绑定入口。
//
// 这是“再次尝试开启”按钮的服务端语义：探测失败后主控递增 Retry，
// 节点必须重新走一次探测 + 绑定，而不是因为“已处理过该信号”而跳过。
func TestControllerRetryRestarts(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true, Retry: 1})
	first := ctl.(*controller).server
	if first == nil {
		t.Fatal("setup: entry did not start")
	}

	ctl.Sync(Signal{Enabled: true, Retry: 2})
	second := ctl.(*controller).server
	if second == nil {
		t.Fatal("retry did not restart the entry")
	}
	if second == first {
		t.Fatal("retry reused the old server instead of rebinding")
	}
	if !ctl.Status().Enabled {
		t.Fatal("entry is not enabled after retry")
	}
}

// TestControllerRetryAfterStop 验证入口被关掉后，Retry 递增仍能重新拉起。
func TestControllerRetryAfterStop(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true, Retry: 1})
	ctl.Sync(Signal{Enabled: false})
	if ctl.Status().Enabled {
		t.Fatal("setup: entry should be stopped")
	}

	// 关闭信号不带 Retry；重新开启时 Retry 递增，应该重新绑定。
	ctl.Sync(Signal{Enabled: true, Retry: 2})
	if !ctl.Status().Enabled {
		t.Fatal("entry did not start again")
	}
}

// TestControllerFailedProbeDoesNotAutoRetry 验证探测失败后不会每次上报都重试。
//
// 探测失败时 server 为 nil，如果只用 server 判断“是否已尝试”，每 60 秒的
// 上报都会重新跑一次 STUN 探测——既浪费流量，也让“再次尝试开启”按钮失去
// 意义。必须靠 attempted 标志记住“已经试过”。
func TestControllerFailedProbeDoesNotAutoRetry(t *testing.T) {
	// 用一个必然探测失败的配置：公网 IP 为空时 ProbeReachability 直接返回
	// 不可达，不会真的发 STUN 包。
	ctl := NewController(Config{
		Listen:       "127.0.0.1:0",
		Password:     "secret",
		CertDir:      t.TempDir(),
		PublicIP:     "",
		Authenticate: func(string) bool { return true },
		Logf:         func(string, ...any) {},
	})
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true})
	if ctl.Status().Enabled {
		t.Fatal("entry should not be enabled when probe fails")
	}
	first := ctl.(*controller).lastRetry

	// 模拟后续几次上报：信号完全相同，不应重新探测。
	for i := 0; i < 3; i++ {
		ctl.Sync(Signal{Enabled: true})
	}
	if ctl.(*controller).lastRetry != first {
		t.Fatal("identical signal changed the retry counter")
	}
	if ctl.Status().Enabled {
		t.Fatal("entry unexpectedly enabled after failed probe")
	}
}

// TestControllerRetryAfterFailedProbe 验证重试计数递增能重新触发探测。
func TestControllerRetryAfterFailedProbe(t *testing.T) {
	ctl := NewController(Config{
		Listen:       "127.0.0.1:0",
		Password:     "secret",
		CertDir:      t.TempDir(),
		PublicIP:     "",
		Authenticate: func(string) bool { return true },
		Logf:         func(string, ...any) {},
	})
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true, Retry: 0})
	ctl.Sync(Signal{Enabled: true, Retry: 1})

	if got := ctl.(*controller).lastRetry; got != 1 {
		t.Fatalf("lastRetry = %d, want 1 after explicit retry", got)
	}
}

// TestControllerServeExitClearsServer 验证 Serve 退出后 c.server 被清空。
//
// Serve 出错退出时 Server 自己的 Status 会置 false，但如果 controller 的
// c.server 仍指向那个已结束的实例，“非空 = 正在运行”的约定就被破坏，后续
// 判断会误以为入口还活着。这里等 Serve goroutine 收尾后确认引用被清掉。
func TestControllerServeExitClearsServer(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true})
	if ctl.(*controller).server == nil {
		t.Fatal("setup: entry did not start")
	}

	// Close 会让 Serve 返回；等异步收尾把 c.server 清掉。
	srv := ctl.(*controller).server
	_ = srv.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ctl.(*controller).server == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := ctl.(*controller).server; got != nil {
		t.Fatal("c.server still points to a stopped server")
	}
	if ctl.Status().Enabled {
		t.Fatal("status stayed enabled after Serve exited")
	}
}

// TestControllerServeExitDoesNotClearNewServer 验证旧的 Serve goroutine 退出时
// 不会误清掉后来启动的新入口。
func TestControllerServeExitDoesNotClearNewServer(t *testing.T) {
	ctl := newTestController(t)
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true, Retry: 1})
	old := ctl.(*controller).server

	// 重试会停掉旧入口并绑定新的；旧 Serve goroutine 稍后才退出。
	ctl.Sync(Signal{Enabled: true, Retry: 2})
	newSrv := ctl.(*controller).server
	if newSrv == nil || newSrv == old {
		t.Fatal("setup: retry did not install a new server")
	}

	// 给旧 goroutine 一点时间收尾，确认它没有把新引用清掉。
	_ = old.Close()
	time.Sleep(100 * time.Millisecond)

	if ctl.(*controller).server != newSrv {
		t.Fatal("stale Serve goroutine cleared the new server")
	}
}

// TestControllerClose 验证 Close 后状态为 disabled 且可重复调用。
func TestControllerClose(t *testing.T) {
	ctl := newTestController(t)

	ctl.Sync(Signal{Enabled: true})
	ctl.Close()
	if ctl.Status().Enabled {
		t.Fatal("status stayed enabled after Close")
	}

	// 重复 Close 不应 panic。
	ctl.Close()
}

// TestControllerStatusZeroValue 验证未启动时 Status 返回 disabled。
func TestControllerStatusZeroValue(t *testing.T) {
	ctl := NewController(Config{Listen: "127.0.0.1:0", Password: "x"})
	defer ctl.Close()

	if ctl.Status().Enabled {
		t.Fatal("fresh controller must report disabled")
	}
}
