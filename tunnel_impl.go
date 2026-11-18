//go:build !vercel

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	cfconfig "github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/edgediscovery/allregions"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/metrics"
	"github.com/cloudflare/cloudflared/orchestration"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/supervisor"
	"github.com/cloudflare/cloudflared/tlsconfig"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v2"
)

// cloudflared 在创建隧道时会向 prometheus 默认注册表注册指标；关闭后再开启
// 会重复注册并 panic。这里换成忽略 AlreadyRegistered 的注册表，让开关幂等。
func init() {
	registry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = tolerantRegisterer{Registerer: registry}
	prometheus.DefaultGatherer = registry
}

type tolerantRegisterer struct {
	prometheus.Registerer
}

func (r tolerantRegisterer) MustRegister(collectors ...prometheus.Collector) {
	for _, collector := range collectors {
		if err := r.Register(collector); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				continue
			}
			panic(err)
		}
	}
}

var tunnelMetricsOnce sync.Once

// 以下常量与 cloudflared quick tunnel 的推荐配置保持一致。
const (
	tunnelGracePeriod         = 30
	tunnelHAConnections       = 2
	tunnelMaxRetries          = 5
	tunnelMaxEdgeAddrRetries  = 8
	tunnelRPCTimeout          = 5 * time.Second
	tunnelQUICConnFlowLimit   = 30 * (1 << 20) // 30 MiB
	tunnelQUICStreamFlowLimit = 6 * (1 << 20)  // 6 MiB

	// Quick Tunnel 分配接口；返回随机 *.trycloudflare.com 地址和临时凭证。
	tunnelAPIURL     = "https://api.trycloudflare.com/tunnel"
	tunnelAPITimeout = 30 * time.Second
	// 上报给 Cloudflare 的客户端版本标识，不影响隧道功能。
	tunnelBuildVersion = "2025.2.0"
)

// Vercel 之外的构建把 cloudflared 作为 Go 依赖直接嵌入进程，不需要在节点上
// 单独安装 cloudflared 二进制。Vercel 构建使用 tunnel_stub.go。
func tunnelSupported() bool { return true }

func newTunnelController(listen string) tunnelController {
	port, err := localTunnelPort(listen)
	return &quickTunnel{
		port:    port,
		portErr: err,
	}
}

type quickTunnel struct {
	mu sync.Mutex

	port    int
	portErr error

	enabled bool
	closed  bool
	cancel  context.CancelFunc
	url     string
	lastErr string
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
		// 上次启动失败时 cancel 为空，这里会重试；隧道已运行时直接返回。
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

func (t *quickTunnel) Status() tunnelReport {
	t.mu.Lock()
	defer t.mu.Unlock()

	return tunnelReport{
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

// startLocked 申请并启动新的 Quick Tunnel；调用方必须持有 t.mu。
func (t *quickTunnel) startLocked() {
	if t.portErr != nil {
		t.lastErr = t.portErr.Error()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	url, err := createCloudflareTunnel(ctx, t.port, "auto")
	if err != nil {
		cancel()
		t.lastErr = err.Error()
		log.Printf("tunnel: start failed: %v", err)
		return
	}

	t.cancel = cancel
	t.url = url
	t.lastErr = ""
	log.Printf("tunnel: %s", url)
}

// stopLocked 取消隧道上下文；cloudflared supervisor 会随 ctx 退出。
func (t *quickTunnel) stopLocked() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.url = ""
}

// createCloudflareTunnel 申请临时隧道并在后台启动 cloudflared supervisor。
//
// 这里的实现参考 cloudflared 的 RunQuickTunnel，但直接调用公开包，把
// cloudflared 编译进当前进程；返回的 URL 在隧道重启后会变化。
func createCloudflareTunnel(ctx context.Context, port int, protocol string) (string, error) {
	switch protocol {
	case "", "auto", "quic", "http2":
		if protocol == "" {
			protocol = "auto"
		}
	default:
		return "", fmt.Errorf("unsupported tunnel protocol %q", protocol)
	}

	tunnelMetricsOnce.Do(func() {
		metrics.RegisterBuildInfo("embedded", "unknown", tunnelBuildVersion)
	})

	clientID, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("can't generate connector UUID: %w", err)
	}

	logTransport := logger.Create(logger.CreateConfig("", false, "", ""))
	observer := connection.NewObserver(logTransport, logTransport)

	ing, err := ingress.ParseIngress(&cfconfig.Configuration{
		Ingress: []cfconfig.UnvalidatedIngressRule{
			{Service: fmt.Sprintf("http://localhost:%d", port)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("can't parse ingress: %w", err)
	}

	orchestrator, err := orchestration.NewOrchestrator(
		ctx,
		&orchestration.Config{
			Ingress:            &ing,
			WarpRouting:        ingress.NewWarpRoutingConfig(&cfconfig.WarpRoutingConfig{}),
			ConfigurationFlags: map[string]string{},
			WriteTimeout:       0,
		},
		[]pogs.Tag{},
		[]ingress.Rule{},
		logTransport,
	)
	if err != nil {
		return "", fmt.Errorf("can't create orchestrator: %w", err)
	}

	protocolSelector, err := connection.NewProtocolSelector(
		protocol,
		"random value",
		false,
		false,
		edgediscovery.ProtocolPercentage,
		connection.ResolveTTL,
		logTransport,
	)
	if err != nil {
		return "", fmt.Errorf("unable to create protocol selector: %w", err)
	}

	edgeTLSConfigs, err := buildEdgeTLSConfigs()
	if err != nil {
		return "", err
	}

	tunnel, err := createQuickTunnel(clientID)
	if err != nil {
		return "", err
	}

	tunnelConfig := buildTunnelConfig(clientID, tunnel, observer, logTransport, protocolSelector, edgeTLSConfigs)

	connectedSignal := signal.New(make(chan struct{}))
	reconnectCh := make(chan supervisor.ReconnectSignal, 4)
	shutdown := make(chan struct{})

	go func() {
		if err := supervisor.StartTunnelDaemon(ctx, tunnelConfig, orchestrator, connectedSignal, reconnectCh, shutdown); err != nil {
			if ctx.Err() == nil {
				log.Printf("tunnel: supervisor stopped: %v", err)
			}
		}
	}()

	return "https://" + tunnel.QuickTunnelUrl, nil
}

func buildEdgeTLSConfigs() (map[connection.Protocol]*tls.Config, error) {
	configs := make(map[connection.Protocol]*tls.Config, len(connection.ProtocolList))
	cliCtx := cli.NewContext(cli.NewApp(), &flag.FlagSet{}, nil)

	for _, p := range connection.ProtocolList {
		tlsSettings := p.TLSSettings()
		if tlsSettings == nil {
			return nil, fmt.Errorf("%s has unknown TLS settings", p)
		}

		tlsConfig, err := tlsconfig.CreateTunnelConfig(cliCtx, tlsSettings.ServerName)
		if err != nil {
			return nil, fmt.Errorf("unable to create TLS config: %w", err)
		}
		if len(tlsSettings.NextProtos) > 0 {
			tlsConfig.NextProtos = tlsSettings.NextProtos
		}
		configs[p] = tlsConfig
	}
	return configs, nil
}

func buildTunnelConfig(
	clientID uuid.UUID,
	tunnel *connection.TunnelProperties,
	observer *connection.Observer,
	logTransport *zerolog.Logger,
	protocolSelector connection.ProtocolSelector,
	edgeTLSConfigs map[connection.Protocol]*tls.Config,
) *supervisor.TunnelConfig {
	return &supervisor.TunnelConfig{
		GracePeriod:                         tunnelGracePeriod,
		ReplaceExisting:                     false,
		OSArch:                              runtime.GOOS + "_" + runtime.GOARCH,
		ClientID:                            clientID.String(),
		EdgeAddrs:                           []string{},
		Region:                              "",
		EdgeIPVersion:                       allregions.Auto,
		EdgeBindAddr:                        nil,
		HAConnections:                       tunnelHAConnections,
		IsAutoupdated:                       false,
		LBPool:                              "",
		Tags:                                []pogs.Tag{},
		Log:                                 logTransport,
		LogTransport:                        logTransport,
		Observer:                            observer,
		ReportedVersion:                     tunnelBuildVersion,
		Retries:                             tunnelMaxRetries,
		RunFromTerminal:                     true,
		NamedTunnel:                         tunnel,
		ProtocolSelector:                    protocolSelector,
		EdgeTLSConfigs:                      edgeTLSConfigs,
		FeatureSelector:                     &features.FeatureSelector{},
		MaxEdgeAddrRetries:                  tunnelMaxEdgeAddrRetries,
		RPCTimeout:                          tunnelRPCTimeout,
		WriteStreamTimeout:                  0,
		DisableQUICPathMTUDiscovery:         false,
		QUICConnectionLevelFlowControlLimit: tunnelQUICConnFlowLimit,
		QUICStreamLevelFlowControlLimit:     tunnelQUICStreamFlowLimit,
		ICMPRouterServer:                    nil,
	}
}

// createQuickTunnel 调 Cloudflare 的临时隧道分配接口，拿到随机域名和凭证。
func createQuickTunnel(clientID uuid.UUID) (*connection.TunnelProperties, error) {
	client := &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout:   tunnelAPITimeout,
			ResponseHeaderTimeout: tunnelAPITimeout,
		},
		Timeout: tunnelAPITimeout,
	}

	req, err := http.NewRequest(http.MethodPost, tunnelAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build tunnel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cloudflared/embedded-vless-small")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to request tunnel: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("tunnel provisioning failed with status %d", resp.StatusCode)
	}

	var result createTunnelResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	if !result.Success {
		return nil, fmt.Errorf("tunnel creation failed: %v", result.Errors)
	}

	tunnelID, err := uuid.Parse(result.Result.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tunnel ID: %w", err)
	}

	return &connection.TunnelProperties{
		Credentials: connection.Credentials{
			AccountTag:   result.Result.AccountTag,
			TunnelSecret: result.Result.Secret,
			TunnelID:     tunnelID,
		},
		QuickTunnelUrl: result.Result.Hostname,
		Client: pogs.ClientInfo{
			ClientID: clientID[:],
			Features: []string{},
			Version:  tunnelBuildVersion,
			Arch:     runtime.GOOS + "_" + runtime.GOARCH,
		},
	}, nil
}

type createTunnelResponse struct {
	Success bool                `json:"success"`
	Result  tunnelCredentials   `json:"result"`
	Errors  []createTunnelError `json:"errors"`
}

type createTunnelError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tunnelCredentials struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Hostname   string `json:"hostname"`
	AccountTag string `json:"account_tag"`
	Secret     []byte `json:"secret"`
}
