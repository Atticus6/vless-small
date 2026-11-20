package quicktunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	// 边缘发现：cloudflared 使用固定的 SRV 名称；端口来自 SRV 记录（当前为 7844）。
	edgeSRVName   = "_v2-origintunneld._tcp.argotunnel.com"
	edgePort      = 7844
	edgeTLSName   = "quic.cftunnel.com"
	edgeALPN      = "argotunnel"
	edgeDialLimit = 6 * time.Second
	// 每轮最多尝试的地址数：UDP 被封锁时避免逐个等待 5 秒握手超时。
	maxEdgeAttempts = 8
)

// Cloudflare Origin CA 根证书（公开信任锚，来自 Cloudflare 官方文档）：
// https://developers.cloudflare.com/ssl/static/origin_ca_ecc_root.pem
// https://developers.cloudflare.com/ssl/static/origin_ca_rsa_root.pem
//
// 边缘的 quic.cftunnel.com 证书由 Cloudflare Origin CA 签发，不在系统根证书池中。
const cloudflareOriginCAs = `
-----BEGIN CERTIFICATE-----
MIICiTCCAi6gAwIBAgIUXZP3MWb8MKwBE1Qbawsp1sfA/Y4wCgYIKoZIzj0EAwIw
gY8xCzAJBgNVBAYTAlVTMRMwEQYDVQQIEwpDYWxpZm9ybmlhMRYwFAYDVQQHEw1T
YW4gRnJhbmNpc2NvMRkwFwYDVQQKExBDbG91ZEZsYXJlLCBJbmMuMTgwNgYDVQQL
Ey9DbG91ZEZsYXJlIE9yaWdpbiBTU0wgRUNDIENlcnRpZmljYXRlIEF1dGhvcml0
eTAeFw0xOTA4MjMyMTA4MDBaFw0yOTA4MTUxNzAwMDBaMIGPMQswCQYDVQQGEwJV
UzETMBEGA1UECBMKQ2FsaWZvcm5pYTEWMBQGA1UEBxMNU2FuIEZyYW5jaXNjbzEZ
MBcGA1UEChMQQ2xvdWRGbGFyZSwgSW5jLjE4MDYGA1UECxMvQ2xvdWRGbGFyZSBP
cmlnaW4gU1NMIEVDQyBDZXJ0aWZpY2F0ZSBBdXRob3JpdHkwWTATBgcqhkjOPQIB
BggqhkjOPQMBBwNCAASR+sGALuaGshnUbcxKry+0LEXZ4NY6JUAtSeA6g87K3jaA
xpIg9G50PokpfWkhbarLfpcZu0UAoYy2su0EhN7wo2YwZDAOBgNVHQ8BAf8EBAMC
AQYwEgYDVR0TAQH/BAgwBgEB/wIBAjAdBgNVHQ4EFgQUhTBdOypw1O3VkmcH/es5
tBoOOKcwHwYDVR0jBBgwFoAUhTBdOypw1O3VkmcH/es5tBoOOKcwCgYIKoZIzj0E
AwIDSQAwRgIhAKilfntP2ILGZjwajktkBtXE1pB4Y/fjAfLkIRUzrI15AiEA5UCL
XYZZ9m2c3fKwIenMMojL1eqydsgqj/wK4p5kagQ=
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIIEADCCAuigAwIBAgIID+rOSdTGfGcwDQYJKoZIhvcNAQELBQAwgYsxCzAJBgNV
BAYTAlVTMRkwFwYDVQQKExBDbG91ZEZsYXJlLCBJbmMuMTQwMgYDVQQLEytDbG91
ZEZsYXJlIE9yaWdpbiBTU0wgQ2VydGlmaWNhdGUgQXV0aG9yaXR5MRYwFAYDVQQH
Ew1TYW4gRnJhbmNpc2NvMRMwEQYDVQQIEwpDYWxpZm9ybmlhMB4XDTE5MDgyMzIx
MDgwMFoXDTI5MDgxNTE3MDAwMFowgYsxCzAJBgNVBAYTAlVTMRkwFwYDVQQKExBD
bG91ZEZsYXJlLCBJbmMuMTQwMgYDVQQLEytDbG91ZEZsYXJlIE9yaWdpbiBTU0wg
Q2VydGlmaWNhdGUgQXV0aG9yaXR5MRYwFAYDVQQHEw1TYW4gRnJhbmNpc2NvMRMw
EQYDVQQIEwpDYWxpZm9ybmlhMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKC
AQEAwEiVZ/UoQpHmFsHvk5isBxRehukP8DG9JhFev3WZtG76WoTthvLJFRKFCHXm
V6Z5/66Z4S09mgsUuFwvJzMnE6Ej6yIsYNCb9r9QORa8BdhrkNn6kdTly3mdnykb
OomnwbUfLlExVgNdlP0XoRoeMwbQ4598foiHblO2B/LKuNfJzAMfS7oZe34b+vLB
yrP/1bgCSLdc1AxQc1AC0EsQQhgcyTJNgnG4va1c7ogPlwKyhbDyZ4e59N5lbYPJ
SmXI/cAe3jXj1FBLJZkwnoDKe0v13xeF+nF32smSH0qB7aJX2tBMW4TWtFPmzs5I
lwrFSySWAdwYdgxw180yKU0dvwIDAQABo2YwZDAOBgNVHQ8BAf8EBAMCAQYwEgYD
VR0TAQH/BAgwBgEB/wIBAjAdBgNVHQ4EFgQUJOhTV118NECHqeuU27rhFnj8KaQw
HwYDVR0jBBgwFoAUJOhTV118NECHqeuU27rhFnj8KaQwDQYJKoZIhvcNAQELBQAD
ggEBAHwOf9Ur1l0Ar5vFE6PNrZWrDfQIMyEfdgSKofCdTckbqXNTiXdgbHs+TWoQ
wAB0pfJDAHJDXOTCWRyTeXOseeOi5Btj5CnEuw3P0oXqdqevM1/+uWp0CM35zgZ8
VD4aITxity0djzE6Qnx3Syzz+ZkoBgTnNum7d9A66/V636x4vTeqbZFBr9erJzgz
hhurjcoacvRNhnjtDRM0dPeiCJ50CP3wEYuvUzDHUaowOsnLCjQIkWbR7Ni6KEIk
MOz2U0OBSif3FTkhCgZWQKOOLo1P42jHC3ssUZAtVNXrCk3fw9/E15k8NPkBazZ6
0iykLhH1trywrKRMVw67F44IE8Y=
-----END CERTIFICATE-----
`

// fallbackEdgeHosts 是 SRV 查询失败时使用的已知边缘域名。
var fallbackEdgeHosts = []string{
	"region1.v2.argotunnel.com",
	"region2.v2.argotunnel.com",
}

// discoverEdgeAddrs 解析边缘地址：优先 SRV，失败或目标无法解析时退回
// 固定区域域名。
//
// 返回顺序优先 IPv4，并做去重；调用方按顺序尝试拨号。
func discoverEdgeAddrs(ctx context.Context) []netip.AddrPort {
	addrs := resolveEdgeTargets(ctx, srvEdgeTargets(ctx))
	if len(addrs) == 0 {
		// SRV 有记录但目标域名全部解析失败（DNS 过滤、fake-IP 等）时，
		// 仍然尝试固定区域域名，否则每次重连都拿不到任何地址。
		addrs = resolveEdgeTargets(ctx, fallbackEdgeTargets())
	}
	return addrs
}

// edgeTarget 是一个待解析的边缘主机和端口。
type edgeTarget struct {
	host string
	port uint16
}

// srvEdgeTargets 通过 SRV 记录发现边缘目标；失败时返回空。
func srvEdgeTargets(ctx context.Context) []edgeTarget {
	_, records, err := net.DefaultResolver.LookupSRV(ctx, "", "", edgeSRVName)
	if err != nil {
		return nil
	}
	targets := make([]edgeTarget, 0, len(records))
	for _, record := range records {
		// SRV 目标带末尾的点，表示绝对域名；去掉它会让解析器再拼
		// search 域，可能解析到错误的主机。
		targets = append(targets, edgeTarget{
			host: record.Target,
			port: record.Port,
		})
	}
	return targets
}

// fallbackEdgeTargets 是 SRV 失败时使用的已知边缘域名。
func fallbackEdgeTargets() []edgeTarget {
	targets := make([]edgeTarget, 0, len(fallbackEdgeHosts))
	for _, host := range fallbackEdgeHosts {
		targets = append(targets, edgeTarget{host: host, port: edgePort})
	}
	return targets
}

// resolveEdgeTargets 解析目标主机并交错返回 IPv4/IPv6 地址。
func resolveEdgeTargets(ctx context.Context, targets []edgeTarget) []netip.AddrPort {
	seen := make(map[netip.AddrPort]struct{})
	var ipv4, ipv6 []netip.AddrPort
	for _, target := range targets {
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", target.host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			addr := netip.AddrPortFrom(ip.Unmap(), target.port)
			if _, ok := seen[addr]; ok {
				continue
			}
			seen[addr] = struct{}{}
			if addr.Addr().Is4() {
				ipv4 = append(ipv4, addr)
			} else {
				ipv6 = append(ipv6, addr)
			}
		}
	}

	// 交错返回 IPv4/IPv6：只连通其中一个协议栈的网络也能在前几个地址命中。
	addrs := make([]netip.AddrPort, 0, len(ipv4)+len(ipv6))
	for i := 0; i < len(ipv4) || i < len(ipv6); i++ {
		if i < len(ipv4) {
			addrs = append(addrs, ipv4[i])
		}
		if i < len(ipv6) {
			addrs = append(addrs, ipv6[i])
		}
	}
	return addrs
}

// edgeDialCursor 记录 dialEdge 的调用次数，用于轮转地址窗口。
var edgeDialCursor atomic.Uint64

// rotateAddrs 返回本次最多尝试 limit 个地址的窗口。
//
// 地址数超过 limit 时按调用次数轮转起点：否则每次重连都只试前 8 个，
// 后面的地址永远不会被尝试。地址不多于 limit 时原样返回。
func rotateAddrs(addrs []netip.AddrPort, limit int) []netip.AddrPort {
	if len(addrs) == 0 || limit <= 0 || len(addrs) <= limit {
		return addrs
	}
	start := int((edgeDialCursor.Add(1) - 1) % uint64(len(addrs)))
	out := make([]netip.AddrPort, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, addrs[(start+i)%len(addrs)])
	}
	return out
}

// dialEdge 依次尝试边缘地址，返回第一条成功建立的 QUIC 连接。
func dialEdge(
	ctx context.Context,
	addrs []netip.AddrPort,
	logf func(format string, args ...any),
) (*quic.Conn, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no Cloudflare edge address available")
	}

	var lastErr error
	for _, addr := range rotateAddrs(addrs, maxEdgeAttempts) {
		dialCtx, cancel := context.WithTimeout(ctx, edgeDialLimit)
		conn, err := quic.DialAddr(dialCtx, addr.String(), newEdgeTLSConfig(), newQUICConfig())
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if logf != nil {
			logf("tunnel: dial %s failed: %v", addr, err)
		}
	}
	return nil, fmt.Errorf("dial Cloudflare edge: %w", lastErr)
}

func newEdgeTLSConfig() *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	// 根证书是公开的固定 PEM，解析失败说明构建被篡改；此时保留系统池继续。
	pool.AppendCertsFromPEM([]byte(cloudflareOriginCAs))

	return &tls.Config{
		ServerName:       edgeTLSName,
		MinVersion:       tls.VersionTLS13,
		NextProtos:       []string{edgeALPN},
		RootCAs:          pool,
		CurvePreferences: []tls.CurveID{tls.CurveP256},
	}
}

func newQUICConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       5 * time.Second,
		KeepAlivePeriod:      time.Second,
		// 每条入站双向流对应一条代理连接（VLESS over WS）。不能设成
		// 1<<60：停滞的流会无限占用 goroutine 和缓冲；达到上限时边缘
		// 会等待已有流关闭，而不是直接失败。
		MaxIncomingStreams:         4096,
		MaxIncomingUniStreams:      256,
		EnableDatagrams:            true,
		MaxConnectionReceiveWindow: 30 << 20,
		MaxStreamReceiveWindow:     6 << 20,
		InitialPacketSize:          1232,
	}
}
