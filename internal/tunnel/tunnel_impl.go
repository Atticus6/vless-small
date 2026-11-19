//go:build !vercel

package tunnel

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/atticus6/vless-small/internal/quicktunnel"
)

// 非 Vercel 构建使用内置的纯 Go Quick Tunnel 客户端（quic-go），不需要在
// 节点上安装 cloudflared，也不依赖其源码。Vercel 构建使用 tunnel_stub.go。
func Supported() bool { return true }

func New(listen string) Controller {
	port, err := localTunnelPort(listen)
	return &quickTunnel{
		localURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		portErr:  err,
	}
}

type quickTunnel struct {
	mu sync.Mutex

	localURL string
	portErr  error

	enabled bool
	closed  bool
	cancel  context.CancelFunc
	url     string
	lastErr string
	// generation 每次启动/停止都会递增，用于丢弃上一轮 Run 的迟到回调。
	generation int
}

// Sync 启动/停止临时隧道；重复调用是幂等的。
func (t *quickTunnel) Sync(enabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}

	if enabled {
		t.enabled = true
		// 上一次运行已退出（cancel 为空）时重新申请隧道；运行中直接返回。
		if t.cancel == nil {
			t.startLocked()
		}
		return
	}

	if !t.enabled && t.cancel == nil {
		return
	}
	t.enabled = false
	t.stopLocked()
}

func (t *quickTunnel) Status() Report {
	t.mu.Lock()
	defer t.mu.Unlock()

	return Report{
		Supported: true,
		URL:       t.url,
		Error:     t.lastErr,
	}
}

func (t *quickTunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}
	t.closed = true
	t.enabled = false
	t.stopLocked()
}

// startLocked 异步申请并启动隧道；调用方必须持有 t.mu。
//
// 申请 API 与 QUIC 注册都可能耗时数秒，不能阻塞 60 秒的设备同步循环或
// /<token>/config 的状态读取。
func (t *quickTunnel) startLocked() {
	if t.portErr != nil {
		t.lastErr = t.portErr.Error()
		return
	}

	t.generation++
	generation := t.generation

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.url = ""
	t.lastErr = ""

	client := quicktunnel.New(quicktunnel.Config{
		LocalURL: t.localURL,
		Version:  Version,
		Logf:     log.Printf,
	})

	go func() {
		err := client.Run(ctx, func(url string) {
			t.setURL(generation, url)
		})
		client.Close()
		t.runExited(generation, err)
	}()
}

// stopLocked 取消 run 循环并清空公网地址。
func (t *quickTunnel) stopLocked() {
	t.generation++
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.url = ""
	t.lastErr = ""
}

// setURL 在首次申请成功后写入公网地址；旧 generation 的回调直接忽略。
func (t *quickTunnel) setURL(generation int, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed || t.generation != generation {
		return
	}
	t.url = url
	t.lastErr = ""
}

// runExited 处理 run 循环退出。
//
// 保留 URL（同一隧道凭证仍可重连），记录错误并清空 cancel，让下一次
// Sync(true) 重新启动；generation 防止旧循环覆盖新状态。
func (t *quickTunnel) runExited(generation int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed || t.generation != generation {
		return
	}
	t.cancel = nil
	if err != nil {
		t.lastErr = err.Error()
	}
}
