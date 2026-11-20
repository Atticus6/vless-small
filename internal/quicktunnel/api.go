package quicktunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultAPIURL 是 Cloudflare 的 Quick Tunnel 分配接口。
	DefaultAPIURL = "https://api.trycloudflare.com/tunnel"
	// apiTimeout 与 cloudflared 保持一致：申请临时隧道通常几百毫秒完成。
	apiTimeout = 15 * time.Second
)

// Credentials 是一次临时隧道的身份信息。
//
// 这些值只在申请成功后由主控返回一次；后续 QUIC 注册与重连都复用它们，
// 因此进程内必须保留同一份凭证，避免每次重连都生成新的公网地址。
type Credentials struct {
	TunnelID   string
	AccountTag string
	Secret     []byte
	Hostname   string
}

// URL 返回可直接展示/分享的临时隧道地址。
func (c Credentials) URL() string {
	if strings.HasPrefix(c.Hostname, "https://") || strings.HasPrefix(c.Hostname, "http://") {
		return c.Hostname
	}
	return "https://" + c.Hostname
}

type quickTunnelResponse struct {
	Success bool `json:"success"`
	Result  struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Hostname   string `json:"hostname"`
		AccountTag string `json:"account_tag"`
		Secret     []byte `json:"secret"`
	} `json:"result"`
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Request 向 Cloudflare 申请一个新的 Quick Tunnel，返回隧道凭证与公网域名。
//
// endpoint 为空时使用 DefaultAPIURL。接口不需要账号或 token：Cloudflare 会
// 分配随机的 *.trycloudflare.com 地址，并把后续 QUIC 注册所需的
// account_tag/secret 一并返回。
func Request(ctx context.Context, endpoint, userAgent string) (*Credentials, error) {
	if endpoint == "" {
		endpoint = DefaultAPIURL
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build quick tunnel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request quick tunnel: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read quick tunnel response: %w", err)
	}

	var parsed quickTunnelResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode quick tunnel response (status %s): %w", resp.Status, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || !parsed.Success {
		message := strings.TrimSpace(string(body))
		if len(parsed.Errors) > 0 {
			message = parsed.Errors[0].Message
		}
		return nil, fmt.Errorf("quick tunnel API %s: %s", resp.Status, message)
	}
	if parsed.Result.ID == "" || parsed.Result.Hostname == "" || parsed.Result.AccountTag == "" ||
		len(parsed.Result.Secret) == 0 {
		return nil, fmt.Errorf("quick tunnel API returned incomplete credentials")
	}

	return &Credentials{
		TunnelID:   parsed.Result.ID,
		AccountTag: parsed.Result.AccountTag,
		Secret:     parsed.Result.Secret,
		Hostname:   parsed.Result.Hostname,
	}, nil
}
