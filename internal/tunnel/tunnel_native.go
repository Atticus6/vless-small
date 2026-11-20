//go:build !cloudflared

package tunnel

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/atticus6/vless-small/internal/quicktunnel"
)

// 默认构建使用纯 Go 实现：进程内直接完成临时隧道申请、QUIC 注册和
// HTTP/WebSocket 转发，不链接 cloudflared。加 `-tags cloudflared` 时改由
// tunnel_impl.go 内嵌官方 cloudflared；两者的对外 API 完全一致，上报、
// 主控开关和 /<token>/config 行为不变。
func Supported() bool { return true }

func New(listen string) Controller {
	port, err := localTunnelPort(listen)
	return &quickTunnel{
		localURL: fmt.Sprintf("http://%s:%d", localTunnelHost(listen), port),
		portErr:  err,
	}
}

// quickTunnel 管理一条纯 Go 临时隧道的生命周期。
//
// 字段语义与 cloudflared 实现保持一致：cancel 非空表示运行中；generation
// 在每次启动/停止时递增，用来丢弃上一轮 Run 的迟到回调。
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
func (t *quickTunnel) Sync(signal Signal) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}

	if signal.Enabled {
		t.enabled = true
		// 运行中或正在申请：不能重启，否则每次上报都会换一个新域名。
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

	// EdgeLocation 暂不解析：quicktunnel 客户端目前只把注册返回的 colo
	// 写进日志，没有暴露回调。字段保持为空，主控仍可正常展示其他状态。
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
// 清空 URL：下一次 Sync 会重新调用 Run，而 Run 会重新申请凭证并得到新的
// 公网地址，旧地址已经不可用，不能继续上报给主控。cancel 清空后，下一次
// Sync（上报循环每 60 秒一次）会重启；即使主控开关保持开启，首次申请
// 失败或永久注册失败也能在下一个周期自愈。generation 防止旧循环覆盖
// 新状态。
func (t *quickTunnel) runExited(generation int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed || t.generation != generation {
		return
	}
	t.cancel = nil
	t.url = ""
	if err != nil {
		t.lastErr = err.Error()
	}
}
