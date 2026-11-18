package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// 单个更新包的大小上限。Release 二进制目前只有几 MB，64 MiB 足够覆盖
// 未来增长，同时避免异常响应把磁盘写满。
const maxUpdateBytes = 64 << 20

// updateHTTPClient 下载更新包用独立客户端；同步设备用的 10 秒超时不够。
var updateHTTPClient = &http.Client{Timeout: 2 * time.Minute}

// nodeUpdate 是主控通过上报响应下发的更新指令。
type nodeUpdate struct {
	Version string `json:"version"`
	// URL 指向主控的鉴权代理，节点带自己的 token 下载，不需要 GitHub 凭据。
	URL string `json:"url"`
	// SHA256 是原始二进制（非压缩包）的十六进制摘要。
	SHA256 string `json:"sha256"`
}

// runtimeAssetArch 返回与 Release 资产名一致的架构后缀。
//
// Release 里 32 位 ARM 按 GOARM 区分为 armv6/armv7，而 runtime.GOARCH 只有
// "arm"；GOARM 只在构建信息里，需要从 debug.ReadBuildInfo 读取。
func runtimeAssetArch() string {
	goarm := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "GOARM" {
				goarm = setting.Value
				break
			}
		}
	}
	return assetArch(runtime.GOARCH, goarm)
}

// assetArch 是 runtimeAssetArch 的纯函数版本，便于测试。
func assetArch(goarch, goarm string) string {
	if goarch != "arm" {
		return goarch
	}
	switch strings.TrimPrefix(strings.ToLower(goarm), "v") {
	case "6":
		return "armv6"
	default:
		// 构建信息缺失时按最常见的 armv7 处理，与 Release 默认产物一致。
		return "armv7"
	}
}

// compareVersions 比较两个版本号，返回 -1/0/1。
//
// 支持 `v` 前缀和任意段数（v0.1.9 < v0.1.10）；无法解析的值（例如本地构建
// 的 dev）视为 [0]，比任何正式版本都旧。
func compareVersions(a, b string) int {
	pa := parseVersionParts(a)
	pb := parseVersionParts(b)

	for i := 0; i < len(pa) || i < len(pb); i++ {
		va, vb := 0, 0
		if i < len(pa) {
			va = pa[i]
		}
		if i < len(pb) {
			vb = pb[i]
		}

		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

func parseVersionParts(value string) []int {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "v")
	if value == "" || value == "dev" {
		return nil
	}

	parts := strings.Split(value, ".")
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		numbers = append(numbers, number)
	}
	return numbers
}

// handleNodeUpdate 在收到更新指令时替换自身二进制并退出。
//
// 成功替换后直接 os.Exit(0)：systemd（Restart=always）和 Docker
// （--restart=always）都会拉起新版本；手动运行时需要自行重启。
func handleNodeUpdate(cfg NodeConfig, update *nodeUpdate) {
	if update == nil {
		return
	}
	if compareVersions(update.Version, Version) <= 0 {
		log.Printf("ignore update %s (current %s)", update.Version, Version)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("update %s failed: resolve executable: %v", update.Version, err)
		return
	}

	log.Printf("updating %s -> %s", Version, update.Version)
	if err := installUpdate(cfg, *update, exePath); err != nil {
		log.Printf("update %s failed: %v", update.Version, err)
		return
	}

	log.Printf("update %s installed, restarting", update.Version)
	os.Exit(0)
}

// installUpdate 下载主控代理的原始二进制并原子替换 exePath。
//
// 临时文件放在目标文件同目录，保证 rename 不跨文件系统；只有 sha256 校验
// 通过才会覆盖。失败时保留旧二进制，错误信息会提示目录不可写这一常见原因。
func installUpdate(cfg NodeConfig, update nodeUpdate, exePath string) error {
	if strings.TrimSpace(update.URL) == "" ||
		strings.TrimSpace(update.SHA256) == "" ||
		strings.TrimSpace(update.Version) == "" {
		return fmt.Errorf("incomplete update payload")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, update.URL, nil)
	if err != nil {
		return fmt.Errorf("build update request: %w", err)
	}
	// 下载端点用节点自身的 name+token 鉴权，和上报使用同一套凭据。
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("User-Agent", "vless-small")

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf(
			"download update: %s: %s",
			resp.Status,
			strings.TrimSpace(string(body)),
		)
	}

	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".vless-small-update-*")
	if err != nil {
		return fmt.Errorf(
			"create update file in %s: %w (二进制目录需要可写；systemd 节点请重跑新版 install.sh，Docker 请使用新版镜像)",
			dir,
			err,
		)
	}
	tmpPath := tmp.Name()
	replace := false
	defer func() {
		if !replace {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(
		io.MultiWriter(tmp, hasher),
		io.LimitReader(resp.Body, maxUpdateBytes+1),
	)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write update: %w", err)
	}
	if written > maxUpdateBytes {
		_ = tmp.Close()
		return fmt.Errorf("update exceeds %d bytes", maxUpdateBytes)
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actual, update.SHA256) {
		_ = tmp.Close()
		return fmt.Errorf("checksum mismatch: got %s want %s", actual, update.SHA256)
	}

	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod update: %w", err)
	}
	// 先落盘再 rename，避免掉电后留下一个内容不完整的新二进制。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close update: %w", err)
	}

	if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("replace %s: %w", exePath, err)
	}
	replace = true
	return nil
}
