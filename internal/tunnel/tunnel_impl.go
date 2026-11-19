//go:build !vercel

package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
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
	return &fixedTunnel{
		port:    port,
		portErr: err,
	}
}

type fixedTunnel struct {
	mu sync.Mutex

	port    int
	portErr error

	enabled  bool
	closed   bool
	cancel   context.CancelFunc
	token    string
	hostname string
	url      string
	lastErr  string
	// generation 每次启动/停止都会递增，用于丢弃上一代 supervisor 的退出事件。
	generation int
}

// Sync 启动/停止固定隧道；token 或 hostname 变化时会重启 supervisor。
func (t *fixedTunnel) Sync(signal Signal) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}

	if !signal.Enabled {
		if !t.enabled && t.cancel == nil {
			return
		}
		t.enabled = false
		t.token = ""
		t.hostname = ""
		t.stopLocked()
		return
	}

	hostname := normalizeTunnelHostname(signal.Hostname)
	t.enabled = true
	// 已用同一份 token/hostname 运行中：只更新期望地址，不重启。
	if t.cancel != nil && t.token == signal.Token && t.hostname == hostname {
		t.url = tunnelURL(hostname)
		return
	}

	t.stopLocked()
	t.token = signal.Token
	t.hostname = hostname
	t.startLocked()
}

func (t *fixedTunnel) Status() Report {
	t.mu.Lock()
	defer t.mu.Unlock()

	return Report{
		Supported: true,
		URL:       t.url,
		Error:     t.lastErr,
	}
}

func (t *fixedTunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}
	t.closed = true
	t.enabled = false
	t.token = ""
	t.hostname = ""
	t.stopLocked()
}

// startLocked 使用主控下发的 token/hostname 启动 cloudflared supervisor。
// 调用方必须持有 t.mu。
func (t *fixedTunnel) startLocked() {
	if t.portErr != nil {
		t.lastErr = t.portErr.Error()
		return
	}
	if strings.TrimSpace(t.token) == "" {
		t.lastErr = "未配置固定隧道 token"
		return
	}
	if t.hostname == "" {
		t.lastErr = "未配置固定隧道 hostname"
		return
	}

	// 换代后再启动：即使旧 supervisor 立刻退出，它的回调也会因为
	// generation 不匹配而忽略，不会清掉本次的新状态。
	t.generation++
	generation := t.generation

	ctx, cancel := context.WithCancel(context.Background())
	protocol := tunnelProtocol()
	log.Printf(
		"tunnel: starting fixed hostname=%s protocol=%s ha_connections=%d log_level=%s",
		t.hostname, protocol, tunnelHAConnections(), tunnelLogLevel(),
	)
	if err := createNamedTunnel(
		ctx,
		t.port,
		t.hostname,
		t.token,
		protocol,
		func(err error) { t.supervisorExited(generation, err) },
	); err != nil {
		cancel()
		t.lastErr = err.Error()
		log.Printf("tunnel: start failed: %v", err)
		return
	}

	t.cancel = cancel
	t.url = tunnelURL(t.hostname)
	t.lastErr = ""
	log.Printf("tunnel: %s", t.url)
}

// stopLocked 取消隧道上下文；cloudflared supervisor 会随 ctx 退出。
func (t *fixedTunnel) stopLocked() {
	t.generation++
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.url = ""
}

// supervisorExited 在 cloudflared supervisor 意外退出后清理隧道状态。
//
// 清空 cancel 与 URL 后，下一次 Sync 会重新启动；固定 hostname 仍保存在
// 主控数据库和订阅里，节点会在下一轮上报后重连。
// generation 用于忽略已被 stopLocked 或新一次 startLocked 取代的旧 goroutine。
func (t *fixedTunnel) supervisorExited(generation int, err error) {
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

// createNamedTunnel 用固定 token/hostname 启动 cloudflared supervisor。
//
// token 是 Cloudflare 控制台/API 生成的命名隧道 token；ingress 只接受该
// hostname，未知 Host 返回 404，避免隧道被当成通用代理。
// onExit 在 supervisor 退出时调用（含主动停止），由调用方按 generation
// 判断是否需要清理状态。
func createNamedTunnel(
	ctx context.Context,
	port int,
	hostname string,
	token string,
	protocol string,
	onExit func(error),
) error {
	switch protocol {
	case "", "auto", "quic", "http2":
		if protocol == "" {
			protocol = "auto"
		}
	default:
		return fmt.Errorf("unsupported tunnel protocol %q", protocol)
	}

	credentials, err := parseTunnelToken(token)
	if err != nil {
		return err
	}

	tunnelMetricsOnce.Do(func() {
		metrics.RegisterBuildInfo("embedded", "unknown", tunnelBuildVersion)
	})

	clientID, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("can't generate connector UUID: %w", err)
	}

	logTransport := logger.Create(
		logger.CreateConfig(tunnelLogLevel(), false, "", ""),
	)
	observer := connection.NewObserver(logTransport, logTransport)

	ing, err := ingress.ParseIngress(&cfconfig.Configuration{
		Ingress: []cfconfig.UnvalidatedIngressRule{
			{
				Hostname: hostname,
				Service:  fmt.Sprintf("http://localhost:%d", port),
			},
			{Service: "http_status:404"},
		},
	})
	if err != nil {
		return fmt.Errorf("can't parse ingress: %w", err)
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
		return fmt.Errorf("can't create orchestrator: %w", err)
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
		return fmt.Errorf("unable to create protocol selector: %w", err)
	}

	edgeTLSConfigs, err := buildEdgeTLSConfigs()
	if err != nil {
		return err
	}

	tunnel := &connection.TunnelProperties{
		Credentials: credentials,
		Client: pogs.ClientInfo{
			ClientID: clientID[:],
			Features: []string{},
			Version:  tunnelBuildVersion,
			Arch:     runtime.GOOS + "_" + runtime.GOARCH,
		},
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

	return nil
}

// parseTunnelToken 解析 Cloudflare 命名隧道 token（base64 JSON）。
func parseTunnelToken(value string) (connection.Credentials, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return connection.Credentials{}, fmt.Errorf("decode tunnel token: %w", err)
	}

	var token connection.TunnelToken
	if err := json.Unmarshal(decoded, &token); err != nil {
		return connection.Credentials{}, fmt.Errorf("parse tunnel token: %w", err)
	}
	if token.AccountTag == "" || token.TunnelID == uuid.Nil || len(token.TunnelSecret) == 0 {
		return connection.Credentials{}, fmt.Errorf("tunnel token is missing account, id or secret")
	}
	return token.Credentials(), nil
}

// normalizeTunnelHostname 只保留裸 hostname，去掉协议、路径和末尾点。
func normalizeTunnelHostname(value string) string {
	hostname := strings.ToLower(strings.TrimSpace(value))
	hostname = strings.TrimPrefix(hostname, "https://")
	hostname = strings.TrimPrefix(hostname, "http://")
	if index := strings.IndexAny(hostname, "/?#"); index >= 0 {
		hostname = hostname[:index]
	}
	return strings.TrimSuffix(hostname, ".")
}

func tunnelURL(hostname string) string {
	if hostname == "" {
		return ""
	}
	return "https://" + hostname
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
