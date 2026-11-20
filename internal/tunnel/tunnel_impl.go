package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

	"github.com/cloudflare/cloudflared/client"
	cfconfig "github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery/allregions"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/ingress/origins"
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

// 以下常量与 cloudflared quick tunnel 的推荐配置保持一致。这里按延迟和
// 吞吐优先取值，不再为省内存刻意调低；内存受限的节点仍可用环境变量回收。
const (
	tunnelGracePeriod          = 30 * time.Second
	tunnelDefaultHAConnections = 2
	tunnelMaxRetries           = 5
	tunnelMaxEdgeAddrRetries   = 8
	tunnelRPCTimeout           = 5 * time.Second
	// QUIC 流控窗口决定单条流可同时在途的字节上限（约等于带宽 × RTT）。
	// 取 32 MiB / 8 MiB：高 BDP 链路上不会因为窗口太小把吞吐卡在
	// 窗口/RTT；单连接预留给内存的仍是有界值。需要更省内存时用
	// TUNNEL_QUIC_CONN_FLOW_MIB / TUNNEL_QUIC_STREAM_FLOW_MIB 调小。
	tunnelQUICConnFlowLimit   = 32 * (1 << 20) // 32 MiB
	tunnelQUICStreamFlowLimit = 8 * (1 << 20)  // 8 MiB

	// 上报给 Cloudflare 的客户端版本标识，不影响隧道功能。
	// Cloudflare 会按连接器版本判断支持状态，需与内嵌 cloudflared 版本
	// 保持一致；升级依赖时同步修改。
	tunnelBuildVersion = "2026.11.1"

	// quickTunnelService 是 Cloudflare 的无账号临时隧道申请地址。
	// 可用 TUNNEL_QUICK_SERVICE 覆盖，便于测试或使用兼容服务。
	quickTunnelService = "https://api.trycloudflare.com"
	quickTunnelTimeout = 20 * time.Second
)

// quickTunnelHTTPClient 只用于申请临时隧道；超时防止上报循环被网络卡住。
var quickTunnelHTTPClient = &http.Client{
	Timeout: quickTunnelTimeout,
	Transport: &http.Transport{
		TLSHandshakeTimeout:   quickTunnelTimeout,
		ResponseHeaderTimeout: quickTunnelTimeout,
	},
}

// tunnelHAConnections 返回与 Cloudflare 边缘保持的连接数。
//
// 默认 2 条：cloudflared 会把新流分摊到各条边缘连接上，并发下载时
// 更容易吃满多核和链路带宽；其中一条抖动时另一条仍可用，握手/重连
// 不会让所有流一起卡住。内存受限时可用 TUNNEL_HA_CONNECTIONS=1 收回。
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
// 默认 32 MiB / 8 MiB，优先保证高 BDP 链路的吞吐；内存受限的节点可用
// TUNNEL_QUIC_CONN_FLOW_MIB / TUNNEL_QUIC_STREAM_FLOW_MIB 调小（1–256 MiB）。
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

// cloudflared 作为 Go 依赖直接嵌入进程，不需要在节点上单独安装二进制。
func Supported() bool { return true }

func New(listen string) Controller {
	port, err := localTunnelPort(listen)
	return &controller{
		origin:  fmt.Sprintf("%s:%d", localTunnelHost(listen), port),
		portErr: err,
	}
}

type controller struct {
	mu sync.Mutex

	// origin 是 cloudflared 的回源地址（host:port），按实际监听地址生成。
	origin  string
	portErr error

	enabled bool
	closed  bool
	cancel  context.CancelFunc
	url     string
	// edgeLocation 是 cloudflared 注册成功后边缘返回的 colo 代码（如 hkg）。
	edgeLocation string
	lastErr      string
	// generation 每次启动/停止都会递增，用于丢弃上一代 supervisor 的退出事件。
	generation int
}

// Sync 启动/停止临时隧道。
//
// 开启后重复调用不会重新申请地址；supervisor 退出或申请失败会清空
// cancel，下一次 Sync 重新申请。
func (t *controller) Sync(signal Signal) {
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
		t.stopLocked()
		return
	}

	t.enabled = true
	// 运行中或正在申请：不能重启，否则每次上报都会换一个新域名。
	if t.cancel != nil {
		return
	}
	t.startLocked()
}

func (t *controller) Status() Report {
	t.mu.Lock()
	defer t.mu.Unlock()

	return Report{
		Supported:    true,
		URL:          t.url,
		EdgeLocation: t.edgeLocation,
		Error:        t.lastErr,
	}
}

func (t *controller) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return
	}
	t.closed = true
	t.enabled = false
	t.stopLocked()
}

// startLocked 申请并启动临时隧道。
//
// 申请是网络请求，放在独立 goroutine 中，避免阻塞上报循环和 Status()；
// 失败时清空 cancel，下一次 Sync 会重新申请。调用方必须持有 t.mu。
func (t *controller) startLocked() {
	if t.portErr != nil {
		t.lastErr = t.portErr.Error()
		return
	}

	t.generation++
	generation := t.generation

	ctx, cancel := context.WithCancel(context.Background())
	protocol := tunnelProtocol()
	origin := t.origin
	t.cancel = cancel
	t.url = ""
	t.edgeLocation = ""
	t.lastErr = ""
	log.Printf(
		"tunnel: starting quick tunnel protocol=%s log_level=%s",
		protocol, tunnelLogLevel(),
	)

	go func() {
		quick, err := requestQuickTunnel(ctx)
		if err != nil {
			t.quickStartFailed(generation, err)
			return
		}

		// 申请期间可能已被关闭或换代；丢弃过期结果。
		t.mu.Lock()
		if t.closed || t.generation != generation {
			t.mu.Unlock()
			cancel()
			return
		}
		t.lastErr = ""
		t.mu.Unlock()

		log.Printf("tunnel: quick %s", quick.URL)
		if err := startTunnel(
			ctx,
			origin,
			quick.Credentials,
			quick.URL,
			protocol,
			// 只有首条边缘连接注册成功后才发布 URL：申请接口返回的地址在
			// 隧道真正连上之前不可用，提前上报会让订阅里出现打不开的入口。
			func() { t.publishURL(generation, quick.URL) },
			// 边缘注册成功时 cloudflared 会给出 colo 代码，记录下来供主控展示。
			func(location string) { t.recordEdgeLocation(generation, location) },
			func(err error) { t.supervisorExited(generation, err) },
		); err != nil {
			t.quickStartFailed(generation, err)
		}
	}()
}

// recordEdgeLocation 记录隧道实际接入的 Cloudflare 数据中心。
//
// generation/cancel 校验与 publishURL 一致：过期隧道或已退出的 supervisor
// 不能再写入位置，否则会显示出与当前地址不匹配的 colo。
func (t *controller) recordEdgeLocation(generation int, location string) {
	if location == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed || t.generation != generation || t.cancel == nil {
		return
	}
	if t.edgeLocation == location {
		return
	}
	t.edgeLocation = location
	log.Printf("tunnel: edge location %s", location)
}

// publishURL 在隧道连上边缘后写入地址。
//
// 等待期间可能已经关闭或换代，必须重新校验 generation；过期结果直接丢弃，
// 否则旧隧道的地址会覆盖新一轮的状态。
//
// t.cancel == nil 表示 supervisor 已经退出（supervisorExited 会清空它），
// 此时再发布地址会让主控拿到一个已经断开的隧道入口。
func (t *controller) publishURL(generation int, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed || t.generation != generation || t.cancel == nil || t.url != "" {
		return
	}
	t.url = url
	t.lastErr = ""
	log.Printf("tunnel: connected %s", url)
}

// quickStartFailed 处理临时隧道申请或启动失败。
//
// 清空 cancel 后，下一次 Sync 会重新申请；临时隧道地址不可预测，
// 不能复用旧 URL。
func (t *controller) quickStartFailed(generation int, err error) {
	message := err.Error()

	t.mu.Lock()
	if t.closed || t.generation != generation {
		t.mu.Unlock()
		return
	}
	cancel := t.cancel
	t.cancel = nil
	t.url = ""
	t.edgeLocation = ""
	t.lastErr = message
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	log.Printf("tunnel: quick start failed: %s", message)
}

// stopLocked 取消隧道上下文；cloudflared supervisor 会随 ctx 退出。
func (t *controller) stopLocked() {
	t.generation++
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.url = ""
	t.edgeLocation = ""
}

// supervisorExited 在 cloudflared supervisor 意外退出后清理隧道状态。
//
// 清空 cancel 与 URL 后，下一次 Sync 会重新申请地址；generation 用于忽略
// 已被 stopLocked 或新一次启动取代的旧 goroutine。
func (t *controller) supervisorExited(generation int, err error) {
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
	t.edgeLocation = ""
	t.lastErr = message
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	log.Printf("tunnel: %s", message)
}

// startTunnel 用 Cloudflare quick tunnel 凭据启动 cloudflared supervisor。
//
// quick tunnel 的域名随机，无法预配置 Hostname；ingress 使用单条无
// Hostname 规则匹配所有 Host。onExit 在 supervisor 退出时调用（含主动
// 停止），由调用方按 generation 判断是否需要清理状态。
func startTunnel(
	ctx context.Context,
	origin string,
	credentials connection.Credentials,
	quickURL string,
	protocol string,
	onConnected func(),
	onEdgeLocation func(string),
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

	tunnelMetricsOnce.Do(func() {
		metrics.RegisterBuildInfo("embedded", "unknown", tunnelBuildVersion)
	})

	logTransport := logger.Create(
		logger.CreateConfig(tunnelLogLevel(), false, false, "", ""),
	)
	observer := connection.NewObserver(logTransport)

	// 边缘注册成功的事件里带着 colo 代码。注册一个 sink 取出来，供上报使用。
	if onEdgeLocation != nil {
		observer.RegisterSink(connection.EventSinkFunc(func(event connection.Event) {
			if event.EventType == connection.Connected && event.Location != "" {
				onEdgeLocation(event.Location)
			}
		}))
	}

	// 新版 cloudflared 把客户端信息收进 client.Config；FeatureSelector 决定
	// 连接器启用的边缘特性。
	featureSelector, err := features.NewFeatureSelector(
		ctx,
		credentials.AccountTag,
		nil,
		false,
		logTransport,
	)
	if err != nil {
		return fmt.Errorf("can't create feature selector: %w", err)
	}
	clientConfig, err := client.NewConfig(
		tunnelBuildVersion,
		runtime.GOOS+"_"+runtime.GOARCH,
		featureSelector,
	)
	if err != nil {
		return fmt.Errorf("can't create client config: %w", err)
	}

	// 临时隧道域名随机，无法预配置 Hostname；单条无 Hostname 规则
	// 即匹配所有 Host。后面不能再跟规则，否则 cloudflared 会拒绝。
	rules := []cfconfig.UnvalidatedIngressRule{
		{Service: "http://" + origin},
	}

	ing, err := ingress.ParseIngress(&cfconfig.Configuration{Ingress: rules})
	if err != nil {
		return fmt.Errorf("can't parse ingress: %w", err)
	}

	// 新版 orchestration 必须持有 OriginDialerService，且 supervisor 会启动
	// OriginDNSService 的刷新循环；两者都按上游 CLI 的最小配置创建。
	warpRouting := ingress.NewWarpRoutingConfig(&cfconfig.WarpRoutingConfig{})
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   ingress.NewDialer(warpRouting),
		TCPWriteTimeout: 0,
	}, logTransport)
	originDNS := origins.NewDNSResolverService(
		origins.NewDNSDialer(),
		logTransport,
		origins.NewMetrics(prometheus.DefaultRegisterer),
	)

	orchestrator, err := orchestration.NewOrchestrator(
		ctx,
		&orchestration.Config{
			Ingress:             &ing,
			WarpRouting:         warpRouting,
			OriginDialerService: originDialer,
			ConfigurationFlags:  map[string]string{},
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
		Credentials:    credentials,
		QuickTunnelUrl: quickURL,
	}

	tunnelConfig := buildTunnelConfig(
		clientConfig,
		tunnel,
		observer,
		logTransport,
		protocolSelector,
		edgeTLSConfigs,
		originDNS,
		originDialer,
	)

	connectedSignal := signal.New(make(chan struct{}))
	// 主动停止通过 ctx 取消；graceShutdownC 只保留给 cloudflared 内部
	// 优雅退出信号，本进程不会主动 close。
	shutdown := make(chan struct{})

	// cloudflared 在首条边缘连接注册后才 Notify(connectedSignal)。等它触发
	// 再发布地址，避免把还不能用的 quick tunnel 域名上报给主控。
	if onConnected != nil {
		go func() {
			select {
			case <-connectedSignal.Wait():
				onConnected()
			case <-ctx.Done():
			}
		}()
	}

	go func() {
		err := supervisor.StartTunnelDaemon(
			ctx,
			tunnelConfig,
			orchestrator,
			connectedSignal,
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

// quickTunnel 是一次临时隧道申请的结果。
type quickTunnel struct {
	URL         string
	Credentials connection.Credentials
}

// quickTunnelResponse 对应 https://api.trycloudflare.com/tunnel 的响应。
//
// Secret 在 JSON 中是 base64 字符串，encoding/json 会自动解码到 []byte。
type quickTunnelResponse struct {
	Success bool `json:"success"`
	Result  struct {
		ID         string `json:"id"`
		Hostname   string `json:"hostname"`
		AccountTag string `json:"account_tag"`
		Secret     []byte `json:"secret"`
	} `json:"result"`
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// parseQuickTunnelResponse 校验临时隧道申请结果并转换成 connection 凭据。
func parseQuickTunnelResponse(body []byte) (quickTunnel, error) {
	var payload quickTunnelResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return quickTunnel{}, fmt.Errorf("parse quick tunnel response: %w", err)
	}
	if !payload.Success {
		details := make([]string, 0, len(payload.Errors))
		for _, item := range payload.Errors {
			details = append(details, fmt.Sprintf("%d: %s", item.Code, item.Message))
		}
		if len(details) == 0 {
			return quickTunnel{}, fmt.Errorf("quick tunnel request was rejected")
		}
		return quickTunnel{}, fmt.Errorf(
			"quick tunnel request was rejected: %s",
			strings.Join(details, "; "),
		)
	}

	tunnelID, err := uuid.Parse(strings.TrimSpace(payload.Result.ID))
	if err != nil {
		return quickTunnel{}, fmt.Errorf("parse quick tunnel id: %w", err)
	}
	hostname := normalizeTunnelHostname(payload.Result.Hostname)
	if hostname == "" {
		return quickTunnel{}, fmt.Errorf("quick tunnel response is missing hostname")
	}
	if payload.Result.AccountTag == "" || len(payload.Result.Secret) == 0 {
		return quickTunnel{}, fmt.Errorf("quick tunnel response is missing account or secret")
	}

	return quickTunnel{
		URL: tunnelURL(hostname),
		Credentials: connection.Credentials{
			AccountTag:   payload.Result.AccountTag,
			TunnelSecret: payload.Result.Secret,
			TunnelID:     tunnelID,
		},
	}, nil
}

// requestQuickTunnel 向 Cloudflare 申请一条无账号临时隧道。
//
// 地址形如 https://random-words.trycloudflare.com。
// 支持 TUNNEL_QUICK_SERVICE 覆盖服务地址，便于在测试环境替换。
func requestQuickTunnel(ctx context.Context) (quickTunnel, error) {
	service := strings.TrimRight(
		strings.TrimSpace(os.Getenv("TUNNEL_QUICK_SERVICE")),
		"/",
	)
	if service == "" {
		service = quickTunnelService
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		service+"/tunnel",
		nil,
	)
	if err != nil {
		return quickTunnel{}, fmt.Errorf("build quick tunnel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vless-small/"+tunnelBuildVersion)

	resp, err := quickTunnelHTTPClient.Do(req)
	if err != nil {
		return quickTunnel{}, fmt.Errorf("request quick tunnel: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return quickTunnel{}, fmt.Errorf("read quick tunnel response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return quickTunnel{}, fmt.Errorf(
			"quick tunnel service %s: %s",
			resp.Status,
			strings.TrimSpace(string(body)),
		)
	}
	return parseQuickTunnelResponse(body)
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

	for _, p := range connection.ProtocolList {
		tlsSettings := p.TLSSettings()
		if tlsSettings == nil {
			return nil, fmt.Errorf("%s has unknown TLS settings", p)
		}

		// 不加载自定义 CA；使用系统根证书校验 Cloudflare 边缘。
		tlsConfig, err := tlsconfig.CreateTunnelConfig("", tlsSettings.ServerName)
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
	clientConfig *client.Config,
	tunnel *connection.TunnelProperties,
	observer *connection.Observer,
	logTransport *zerolog.Logger,
	protocolSelector connection.ProtocolSelector,
	edgeTLSConfigs map[connection.Protocol]*tls.Config,
	originDNS *origins.DNSResolverService,
	originDialer *ingress.OriginDialerService,
) *supervisor.TunnelConfig {
	quicConnFlow, quicStreamFlow := tunnelQUICFlowLimits()
	return &supervisor.TunnelConfig{
		ClientConfig:       clientConfig,
		GracePeriod:        tunnelGracePeriod,
		EdgeAddrs:          []string{},
		Region:             "",
		EdgeIPVersion:      allregions.Auto,
		EdgeBindAddr:       nil,
		HAConnections:      tunnelHAConnections(),
		IsAutoupdated:      false,
		LBPool:             "",
		Tags:               []pogs.Tag{},
		Log:                logTransport,
		Observer:           observer,
		ReportedVersion:    tunnelBuildVersion,
		Retries:            tunnelMaxRetries,
		MaxEdgeAddrRetries: tunnelMaxEdgeAddrRetries,
		RunFromTerminal:    true,
		// 内嵌场景不需要启动前的连通性探测，直接把失败交给重试逻辑。
		NoPrechecks:                         true,
		NamedTunnel:                         tunnel,
		ProtocolSelector:                    protocolSelector,
		EdgeTLSConfigs:                      edgeTLSConfigs,
		ICMPRouterServer:                    nil,
		OriginDNSService:                    originDNS,
		OriginDialerService:                 originDialer,
		RPCTimeout:                          tunnelRPCTimeout,
		WriteStreamTimeout:                  0,
		DisableQUICPathMTUDiscovery:         false,
		QUICConnectionLevelFlowControlLimit: quicConnFlow,
		QUICStreamLevelFlowControlLimit:     quicStreamFlow,
	}
}
