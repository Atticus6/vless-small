//go:build !vercel

package tsnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/tsnet"
)

// Supported 在普通构建里为 true；Vercel 构建由 stub 返回 false。
func Supported() bool { return true }

// New 创建内嵌 Tailscale 控制器。
//
// 状态目录使用临时目录：身份状态由主控数据库保存，启动时拉取、运行期间
// 定期回写，因此容器/实例重建后仍能复用同一个 Tailscale 设备身份。
func New(cfg Config, handler http.Handler) Controller {
	return &ControllerImpl{
		cfg:     cfg,
		handler: handler,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type ControllerImpl struct {
	cfg     Config
	handler http.Handler
	client  *http.Client

	mu         sync.Mutex
	running    bool
	generation int
	cancel     context.CancelFunc

	ip atomic.Value // string
}

func (c *ControllerImpl) Sync(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if enabled && !c.running {
		c.running = true
		c.generation++
		generation := c.generation
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		go c.run(ctx, generation)
		return
	}

	if !enabled && c.running {
		c.running = false
		if c.cancel != nil {
			c.cancel()
			c.cancel = nil
		}
	}
}

func (c *ControllerImpl) IP() string {
	value, _ := c.ip.Load().(string)
	return value
}

func (c *ControllerImpl) Close() {
	c.Sync(false)
}

func (c *ControllerImpl) run(ctx context.Context, generation int) {
	// 只清理自己这一代的状态：快速 disable/enable 时旧 goroutine 可能晚于
	// 新 goroutine 退出，不能把新的 running/cancel/IP 覆盖掉。
	defer func() {
		c.mu.Lock()
		if c.generation == generation {
			c.running = false
			c.cancel = nil
			c.ip.Store("")
		}
		c.mu.Unlock()
	}()

	config, err := c.fetchConfig(ctx)
	if err != nil {
		log.Printf("tsnet: fetch config failed: %v", err)
		return
	}
	if !config.Enabled {
		log.Printf("tsnet: disabled by control plane")
		return
	}
	if config.AuthKey == "" && config.State == "" {
		log.Printf("tsnet: no auth key or saved state, skip")
		return
	}

	hostname := config.Hostname
	if hostname == "" {
		hostname = sanitizeTSNetHostname(c.cfg.Name)
	}
	if hostname == "" {
		hostname = "vless-small"
	}

	dir, err := os.MkdirTemp("", "vless-small-tsnet-*")
	if err != nil {
		log.Printf("tsnet: create state dir: %v", err)
		return
	}
	defer os.RemoveAll(dir)

	if err := writeTSNetFile(dir, tsnetStateFile, config.State); err != nil {
		log.Printf("tsnet: restore state: %v", err)
		return
	}
	// 日志策略不是启动必需，损坏时忽略即可。
	if err := writeTSNetFile(dir, tsnetLogConfFile, config.LogConf); err != nil {
		log.Printf("tsnet: restore log conf: %v", err)
	}

	srv := &tsnet.Server{
		Dir:      dir,
		Hostname: hostname,
		AuthKey:  config.AuthKey,
		Logf: func(format string, args ...any) {
			log.Printf("tsnet: "+format, args...)
		},
		UserLogf: func(format string, args ...any) {
			log.Printf("tsnet: "+format, args...)
		},
	}
	defer srv.Close()

	// Up 可能在控制面不可达时长时间等待；加超时，失败后由下一次上报重试。
	upCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	status, err := srv.Up(upCtx)
	cancel()
	if err != nil {
		log.Printf("tsnet: up failed: %v", err)
		return
	}
	if status != nil && status.Self != nil {
		log.Printf(
			"tsnet: connected as %s (%v)",
			status.Self.HostName,
			status.Self.TailscaleIPs,
		)
	}

	ip4, _ := srv.TailscaleIPs()
	if ip4.IsValid() {
		c.mu.Lock()
		current := c.generation == generation
		c.mu.Unlock()
		if current {
			c.ip.Store(ip4.String())
		}
		log.Printf("tsnet: tailnet ip %s, listen %s", ip4, tsnetListenAddr)
	} else {
		log.Printf("tsnet: up without tailnet IPv4")
	}

	// 在 tailnet 上暴露同一个 handler（健康检查/WS），便于通过 Tailscale IP
	// 访问；公网监听端口保持不变。
	if listener, err := srv.Listen("tcp", tsnetListenAddr); err != nil {
		log.Printf("tsnet: listen %s: %v", tsnetListenAddr, err)
	} else {
		go func() {
			<-ctx.Done()
			_ = listener.Close()
		}()
		go func() {
			err := http.Serve(listener, c.handler)
			if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("tsnet: serve: %v", err)
			}
		}()
	}

	// 启动后立刻回写一次状态；之后每 5 分钟检查文件是否变化（密钥轮换等）。
	lastHash := ""
	c.uploadState(ctx, dir, &lastHash)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.uploadState(ctx, dir, &lastHash)
		}
	}
}

func (c *ControllerImpl) endpoint(path string) string {
	return strings.TrimRight(c.cfg.BaseURL, "/") +
		"/api/nodes/" + path +
		"?name=" + url.QueryEscape(c.cfg.Name)
}

func (c *ControllerImpl) fetchConfig(ctx context.Context) (tsnetConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("tsnet"), nil)
	if err != nil {
		return tsnetConfig{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "vless-small")

	resp, err := c.client.Do(req)
	if err != nil {
		return tsnetConfig{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return tsnetConfig{}, fmt.Errorf(
			"fetch tsnet config: %s: %s",
			resp.Status,
			strings.TrimSpace(string(body)),
		)
	}

	var config tsnetConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&config); err != nil {
		return tsnetConfig{}, fmt.Errorf("decode tsnet config: %w", err)
	}
	return config, nil
}

func (c *ControllerImpl) uploadState(
	ctx context.Context,
	dir string,
	lastHash *string,
) {
	state, err := readTSNetFile(dir, tsnetStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("tsnet: read state: %v", err)
		}
		return
	}
	if state == "" {
		return
	}

	logConf, err := readTSNetFile(dir, tsnetLogConfFile)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("tsnet: read log conf: %v", err)
	}

	hash := sha256.Sum256([]byte(state + "\x00" + logConf))
	sum := hex.EncodeToString(hash[:])
	if sum == *lastHash {
		return
	}

	payload := map[string]string{"state": base64.StdEncoding.EncodeToString([]byte(state))}
	if logConf != "" {
		payload["logConf"] = base64.StdEncoding.EncodeToString([]byte(logConf))
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("tsnet: encode state: %v", err)
		return
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		c.endpoint("tsnet"),
		bytes.NewReader(body),
	)
	if err != nil {
		log.Printf("tsnet: build state request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vless-small")

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("tsnet: upload state: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf(
			"tsnet: upload state: %s: %s",
			resp.Status,
			strings.TrimSpace(string(snippet)),
		)
		return
	}

	*lastHash = sum
	log.Printf("tsnet: state uploaded (%d bytes)", len(state))
}

func readTSNetFile(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeTSNetFile(dir, name, encoded string) error {
	if strings.TrimSpace(encoded) == "" {
		return nil
	}

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}
