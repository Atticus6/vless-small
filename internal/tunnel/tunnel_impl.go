//go:build !vercel

package tunnel

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
	"os"
	"runtime"
	"strconv"
	"strings"
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

// 以下常量与 cloudflared quick tunnel 的推荐配置保持一致；内存敏感项
// （HA 连接数、QUIC 流控窗口）默认调低，可通过环境变量进一步覆盖。
const (
	tunnelGracePeriod          = 30
	tunnelDefaultHAConnections = 1
	tunnelMaxRetries           = 5
	tunnelMaxEdgeAddrRetries   = 8
	tunnelRPCTimeout           = 5 * time.Second
	// QUIC 流控窗口越小，单个连接预留/可增长的内存越少；8 MiB / 2 MiB
	// 对 VLESS 转发足够，极端高带宽场景可通过环境变量恢复更大值。
	tunnelQUICConnFlowLimit   = 8 * (1 << 20) // 8 MiB
	tunnelQUICStreamFlowLimit = 2 * (1 << 20) // 2 MiB

	// Quick Tunnel 分配接口；返回随机 *.trycloudflare.com 地址和临时凭证。
	tunnelAPIURL     = "https://api.trycloudflare.com/tunnel"
	tunnelAPITimeout = 30 * time.Second
	// 上报给 Cloudflare 的客户端版本标识，不影响隧道功能。
	tunnelBuildVersion = "2025.2.0"
)

// tunnelHAConnections 返回与 Cloudflare 边缘保持的连接数。
//
// 默认 1 条，足够单节点转发，也显著减少 cloudflared 的连接/缓冲内存；
// 需要冗余时可用 TUNNEL_HA_CONNECTIONS=2..4 调高。
func tunnelHAConnections() int {
	raw := strings.TrimSpace(os.Getenv("TUNNEL_HA_CONNECTIONS"))
	if raw == "" {
		return tunnelDefaultHAConnections
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return tunnelDefaultHAConnections
	}
	if value > 4 {
		return 4
	}
	return value
}

// tunnelProtocol 选择 cloudflared 边缘协议。
//
// 默认 auto 让 cloudflared 自行探测；内存敏感环境可显式设为 http2，
// 避免 quic-go 的额外缓冲。支持 auto / quic / http2。
func tunnelProtocol() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("TUNNEL_PROTOCOL")))
	switch raw {
	case "auto", "quic", "http2":
		return raw
	default:
		return "auto"
	}
}

// tunnelLogLevel 控制 cloudflared 内部日志级别，默认只保留 error，
// 减少高频日志带来的分配和噪声；排查问题时可设为 info 或 debug。
func tunnelLogLevel() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("TUNNEL_LOG_LEVEL")))
	switch raw {
	case "debug", "info", "error", "fatal":
		return raw
	default:
		return "error"
	}
}

// tunnelQUICFlowLimits 返回 QUIC 连接级/流级流控窗口（字节）。
//
// 默认 8 MiB / 2 MiB，比 cloudflared 默认更省内存；高带宽节点可用
// TUNNEL_QUIC_CONN_FLOW_MIB / TUNNEL_QUIC_STREAM_FLOW_MIB 调大（1–256 MiB）。
func tunnelQUICFlowLimits() (uint64, uint64) {
	conn := uint64(tunnelQUICConnFlowLimit)
	stream := uint64(tunnelQUICStreamFlowLimit)
	if value, ok := envMiB("TUNNEL_QUIC_CONN_FLOW_MIB"); ok {
		conn = value
	}
	if value, ok := envMiB("TUNNEL_QUIC_STREAM_FLOW_MIB"); ok {
		stream = value
	}
	return conn, stream
}

func envMiB(key string) (uint64, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 256 {
		return 0, false
	}
	return uint64(value) << 20, true
}

// Vercel 之外的构建把 cloudflared 作为 Go 依赖直接嵌入进程，不需要在节点上
// 单独安装 cloudflared 二进制。Vercel 构建使用 tunnel_stub.go。
func Supported() bool { return true }

func New(listen string) Controller {
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
	// generation 每次启动/停止都会递增，用于丢弃上一代 supervisor 的退出事件。
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

// startLocked 申请并启动新的 Quick Tunnel；调用方必须持有 t.mu。
func (t *quickTunnel) startLocked() {
	if t.portErr != nil {
		t.lastErr = t.portErr.Error()
		return
	}

	// 换代后再启动：即使旧 supervisor 立刻退出，它的回调也会因为
	// generation 不匹配而忽略，不会清掉本次新分配的 URL。
	t.generation++
	generation := t.generation

	ctx, cancel := context.WithCancel(context.Background())
	protocol := tunnelProtocol()
	log.Printf(
		"tunnel: starting protocol=%s ha_connections=%d log_level=%s",
		protocol, tunnelHAConnections(), tunnelLogLevel(),
	)
	url, err := createCloudflareTunnel(ctx, t.port, protocol, func(err error) {
		t.supervisorExited(generation, err)
	})
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
	t.generation++
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.url = ""
}

// supervisorExited 在 cloudflared supervisor 意外退出后清理隧道状态。
//
// 清空 cancel 与 URL 后，下一次 Sync(true) 会重新申请隧道；否则节点会
// 一直上报一个已经失效的 trycloudflare 地址，订阅里也会留下连不上的节点。
// generation 用于忽略已被 stopLocked 或新一次 startLocked 取代的旧 goroutine。
func (t *quickTunnel) supervisorExited(generation int, err error) {
	message := "隧道连接已断开，将在下次同步后重试"
	if err != nil {
		message = err.Error()
	}

	t.mu.Lock()
	if t.closed || t.generation != generation {
		t.mu.Unlock()
		return
	}
	cancel := t.cancel
	t.cancel = nil
	t.url = ""
	t.lastErr = message
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	log.Printf("tunnel: %s", message)
}

// createCloudflareTunnel 申请临时隧道并在后台启动 cloudflared supervisor。
//
// 这里的实现参考 cloudflared 的 RunQuickTunnel，但直接调用公开包，把
// cloudflared 编译进当前进程；返回的 URL 在隧道重启后会变化。
// onExit 在 supervisor 退出时调用（含主动停止），由调用方按 generation
// 判断是否需要清理状态。
func createCloudflareTunnel(
	ctx context.Context,
	port int,
	protocol string,
	onExit func(error),
) (string, error) {
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

	logTransport := logger.Create(
		logger.CreateConfig(tunnelLogLevel(), false, "", ""),
	)
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
		err := supervisor.StartTunnelDaemon(
			ctx,
			tunnelConfig,
			orchestrator,
			connectedSignal,
			reconnectCh,
			shutdown,
		)
		// 主动停止（stopLocked/Close）会让 ctx 取消，不能算故障；
		// 清理与否由 onExit 里的 generation 判断。
		exitErr := err
		if ctx.Err() != nil {
			exitErr = nil
		}
		onExit(exitErr)
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
	quicConnFlow, quicStreamFlow := tunnelQUICFlowLimits()
	return &supervisor.TunnelConfig{
		GracePeriod:                         tunnelGracePeriod,
		ReplaceExisting:                     false,
		OSArch:                              runtime.GOOS + "_" + runtime.GOARCH,
		ClientID:                            clientID.String(),
		EdgeAddrs:                           []string{},
		Region:                              "",
		EdgeIPVersion:                       allregions.Auto,
		EdgeBindAddr:                        nil,
		HAConnections:                       tunnelHAConnections(),
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
		QUICConnectionLevelFlowControlLimit: quicConnFlow,
		QUICStreamLevelFlowControlLimit:     quicStreamFlow,
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
