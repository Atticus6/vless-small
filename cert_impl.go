//go:build linux && !vercel

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

type certManager struct {
	mu        sync.Mutex
	handler   http.Handler
	supported bool

	domain    string
	enabled   bool
	expiresAt string
	issuer    string
	lastErr   string

	httpServer *http.Server
	tlsServer  *http.Server
	httpLn     net.Listener
	tlsLn      net.Listener
	cancel     context.CancelFunc
	generation int
	reporter   func(certReport)
}

func newCertController(handler http.Handler) certController {
	return &certManager{
		handler: handler,
		supported: !runningInContainer() &&
			certRuntimeAllowed(detectRuntime()),
	}
}

// Sync 幂等应用主控下发的证书域名；空字符串表示关闭。
func (c *certManager) Sync(domain string) {
	domain = strings.ToLower(strings.TrimSpace(domain))

	c.mu.Lock()
	changed := false
	defer func() {
		c.mu.Unlock()
		if changed {
			go c.notify()
		}
	}()

	if domain == c.domain && c.enabled {
		return
	}

	c.stopLocked()
	c.domain = domain
	changed = true
	if domain == "" {
		c.lastErr = ""
		return
	}
	if !c.supported {
		c.lastErr = "仅纯 Linux 非 Docker 环境支持自动证书"
		return
	}

	c.startLocked()
}

// SetReporter 注入状态回调；设置后立即发送一次当前状态。
func (c *certManager) SetReporter(reporter func(certReport)) {
	c.mu.Lock()
	c.reporter = reporter
	c.mu.Unlock()
	go c.notify()
}

func (c *certManager) notify() {
	c.mu.Lock()
	reporter := c.reporter
	status := certReport{
		Supported: c.supported,
		Enabled:   c.enabled,
		Domain:    c.domain,
		ExpiresAt: c.expiresAt,
		Issuer:    c.issuer,
		Error:     c.lastErr,
	}
	c.mu.Unlock()

	if reporter != nil {
		reporter(status)
	}
}

func (c *certManager) Status() certReport {
	c.mu.Lock()
	defer c.mu.Unlock()

	return certReport{
		Supported: c.supported,
		Enabled:   c.enabled,
		Domain:    c.domain,
		ExpiresAt: c.expiresAt,
		Issuer:    c.issuer,
		Error:     c.lastErr,
	}
}

func (c *certManager) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *certManager) startLocked() {
	cacheDir, err := certCacheDir()
	if err != nil {
		c.lastErr = fmt.Sprintf("创建证书缓存目录失败: %v", err)
		return
	}

	// 先占住 80/443，任一失败都视为端口被占用，不进入 ACME 流程。
	httpLn, err := net.Listen("tcp", ":80")
	if err != nil {
		c.lastErr = fmt.Sprintf("80 端口不可用: %v", err)
		return
	}
	tlsLn, err := net.Listen("tcp", ":443")
	if err != nil {
		_ = httpLn.Close()
		c.lastErr = fmt.Sprintf("443 端口不可用: %v", err)
		return
	}

	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cacheDir),
		HostPolicy: autocert.HostWhitelist(c.domain),
	}

	httpServer := &http.Server{
		Handler:           manager.HTTPHandler(nil),
		ReadHeaderTimeout: 10 * time.Second,
	}
	tlsServer := &http.Server{
		Handler:           c.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.generation++
	generation := c.generation

	c.httpServer = httpServer
	c.tlsServer = tlsServer
	c.httpLn = httpLn
	c.tlsLn = tlsLn
	c.cancel = cancel
	c.enabled = true
	c.expiresAt = ""
	c.issuer = ""
	c.lastErr = ""

	go func() {
		if err := httpServer.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.fail(generation, fmt.Sprintf("80 服务退出: %v", err))
		}
	}()
	go func() {
		if err := tlsServer.Serve(tls.NewListener(tlsLn, manager.TLSConfig())); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.fail(generation, fmt.Sprintf("443 服务退出: %v", err))
		}
	}()
	go c.refreshLoop(ctx, generation, c.domain, manager)

	log.Printf("cert: HTTPS enabled for %s (ports 80/443)", c.domain)
}

func (c *certManager) stopLocked() {
	c.generation++
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}

	shutdown := func(server *http.Server) {
		if server == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
	}

	shutdown(c.httpServer)
	shutdown(c.tlsServer)
	c.httpServer = nil
	c.tlsServer = nil
	c.httpLn = nil
	c.tlsLn = nil
	c.enabled = false
	c.expiresAt = ""
	c.issuer = ""
}

// refreshLoop 主动触发一次签发，并在之后定期续期/刷新到期时间。
//
// autocert 在 TLS 握手时也会自动续期；这里定时调用是为了在没有流量时
// 也能拿到证书并把有效期上报给主控。
func (c *certManager) refreshLoop(
	ctx context.Context,
	generation int,
	domain string,
	manager *autocert.Manager,
) {
	delay := 5 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		cert, err := manager.GetCertificate(&tls.ClientHelloInfo{ServerName: domain})
		if err != nil {
			c.updateCert(generation, domain, "", "", err.Error())
			delay = 10 * time.Minute
			continue
		}

		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			c.updateCert(generation, domain, "", "", err.Error())
			delay = 10 * time.Minute
			continue
		}

		issuer := leaf.Issuer.CommonName
		if issuer == "" && len(leaf.Issuer.Organization) > 0 {
			issuer = leaf.Issuer.Organization[0]
		}
		c.updateCert(
			generation,
			domain,
			leaf.NotAfter.UTC().Format(time.RFC3339),
			issuer,
			"",
		)
		delay = 12 * time.Hour
	}
}

func (c *certManager) updateCert(
	generation int,
	domain, expiresAt, issuer, errorText string,
) {
	c.mu.Lock()
	if c.generation != generation || c.domain != domain || !c.enabled {
		c.mu.Unlock()
		return
	}
	c.expiresAt = expiresAt
	c.issuer = issuer
	c.lastErr = errorText
	c.mu.Unlock()

	go c.notify()
}

func (c *certManager) fail(generation int, message string) {
	c.mu.Lock()
	if c.generation != generation {
		c.mu.Unlock()
		return
	}
	c.enabled = false
	c.lastErr = message
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.mu.Unlock()

	go c.notify()
}

// certCacheDir 使用二进制同目录的 .certs，service 用户可写且重启后保留。
func certCacheDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(filepath.Dir(exe), ".certs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// runningInContainer 识别 Docker/Podman/Kubernetes；这类环境不自动申请证书。
func runningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if strings.TrimSpace(os.Getenv("container")) != "" {
		return true
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}

	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	content := string(data)
	for _, marker := range []string{"docker", "containerd", "kubepods", "podman"} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}
