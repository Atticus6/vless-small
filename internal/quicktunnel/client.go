package quicktunnel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/quic-go/quic-go"
)

// Config 是 Quick Tunnel 客户端的运行参数。
type Config struct {
	// LocalURL 是边缘请求要转发到的本地 HTTP 地址，例如 http://127.0.0.1:8080。
	LocalURL string
	// APIURL 覆盖临时隧道申请接口，主要用于测试。
	APIURL string
	// Version 写入 ConnectionOptions.ClientInfo.version。
	Version string
	// UserAgent 用于申请接口的 UA；为空时由 Version 生成。
	UserAgent string
	// Logf 是可选的日志函数。
	Logf func(format string, args ...any)
}

// Client 管理一条临时隧道：申请凭证、连接边缘、转发请求，并在断线后重连。
type Client struct {
	cfg       Config
	localURL  *url.URL
	transport *http.Transport
}

// New 创建客户端。Config.LocalURL 必须设置。
func New(cfg Config) *Client {
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "vless-small/" + cfg.Version
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	var localURL *url.URL
	if parsed, err := url.Parse(cfg.LocalURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		localURL = parsed
	}

	return &Client{
		cfg:      cfg,
		localURL: localURL,
		transport: &http.Transport{
			// 隧道只能访问本地/内网源站，不使用环境变量里的 HTTP 代理。
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     false,
			DisableCompression:    true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

func (c *Client) logf(format string, args ...any) {
	c.cfg.Logf(format, args...)
}

// Close 释放复用的源站连接。
func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}

// Run 申请临时隧道并持续转发，直到 ctx 取消。
//
// onURL 在首次申请成功后调用一次；连接断开后会复用同一份凭证重连，因此
// 公网地址不会变化。永久性注册失败会返回错误，调用方可重新申请隧道。
func (c *Client) Run(ctx context.Context, onURL func(string)) error {
	if c.cfg.LocalURL == "" || c.localURL == nil {
		return errors.New("quicktunnel: LocalURL must be an absolute http(s) URL")
	}

	creds, err := Request(ctx, c.cfg.APIURL, c.cfg.UserAgent)
	if err != nil {
		return err
	}
	if onURL != nil {
		onURL(creds.URL())
	}
	c.logf("tunnel: %s", creds.URL())

	backoff := time.Second
	for {
		err := c.serve(ctx, creds)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("edge connection closed")
		}

		var regErr *registrationError
		if errors.As(err, &regErr) && !regErr.ShouldRetry {
			return err
		}

		delay := backoff
		if regErr != nil && regErr.RetryAfter > 0 {
			delay = regErr.RetryAfter
		}
		c.logf("tunnel: connection lost: %v (retry in %s)", err, delay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// serve 建立一次边缘连接并处理数据流，返回时连接已不可用。
func (c *Client) serve(ctx context.Context, creds *Credentials) error {
	addrs := discoverEdgeAddrs(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := dialEdge(ctx, addrs, c.logf)
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "")

	// 边缘约定：第一条双向流必须是控制流。
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	control := newControlStream(stream)

	if err := control.bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap control stream: %w", err)
	}
	details, err := control.register(ctx, creds, c.cfg.Version)
	if err != nil {
		return err
	}
	c.logf(
		"tunnel: registered connection %s at %s",
		hex.EncodeToString(details.UUID), details.Location,
	)

	// quick tunnel 不由控制面托管，需要客户端推送本地 ingress 规则。
	if !details.RemotelyManaged {
		if err := control.sendLocalConfig(ctx, c.cfg.LocalURL); err != nil {
			// 与 cloudflared 一致：配置推送失败只记录，不中断连接。
			c.logf("tunnel: send local config: %v", err)
		}
	}

	go func() {
		control.drain(ctx)
		// 控制流结束意味着这条 edge 连接已不可用；主动关闭以让 acceptLoop
		// 退出并按退避策略重连。
		if ctx.Err() == nil {
			_ = conn.CloseWithError(0, "control stream closed")
		}
	}()
	return c.acceptLoop(ctx, conn)
}

func (c *Client) acceptLoop(ctx context.Context, conn *quic.Conn) error {
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept data stream: %w", err)
		}
		go c.handleDataStream(ctx, stream)
	}
}
