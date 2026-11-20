package hy2

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// defaultSelfSignedDomain 是未指定域名时自签证书使用的 CN/SAN。
	//
	// 用一个常见的公开站点名，可以让 TLS 握手里的 SNI/证书字段看起来像
	// 普通流量；真正的安全边界仍然是 Hysteria 的密码认证。
	defaultSelfSignedDomain = "bing.com"

	// selfSignedValidity 是自签证书的有效期；缓存文件会一直复用，直到
	// 临近过期才重新生成。
	selfSignedValidity = 10 * 365 * 24 * time.Hour

	// selfSignedRenewBefore 是剩余有效期低于该值时重新生成。
	selfSignedRenewBefore = 30 * 24 * time.Hour

	certFileName = "hy2-selfsigned.crt"
	keyFileName  = "hy2-selfsigned.key"
)

// loadOrCreateSelfSigned 在 dir 中加载或生成一张自签证书。
//
// 未显式提供证书时使用：Hysteria 强制 TLS 1.3，客户端通常配置
// `tls.insecure: true`，自签证书足以完成握手。证书按域名缓存，重启不会
// 重新生成；临近过期或域名变化时覆盖旧文件。
func loadOrCreateSelfSigned(dir, domain string) (tls.Certificate, error) {
	if dir == "" {
		return tls.Certificate{}, fmt.Errorf("hy2: certificate directory is required")
	}
	if domain == "" {
		domain = defaultSelfSignedDomain
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("hy2: create certificate dir: %w", err)
	}

	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	if cert, ok := loadCachedSelfSigned(certPath, keyPath, domain); ok {
		return cert, nil
	}
	return generateSelfSigned(certPath, keyPath, domain)
}

// loadCachedSelfSigned 读取缓存证书；域名不匹配、临近过期或文件损坏时返回 false。
func loadCachedSelfSigned(certPath, keyPath, domain string) (tls.Certificate, bool) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	if time.Now().Add(selfSignedRenewBefore).After(leaf.NotAfter) {
		return tls.Certificate{}, false
	}
	if !matchesDomain(leaf, domain) {
		return tls.Certificate{}, false
	}
	cert.Leaf = leaf
	return cert, true
}

// matchesDomain 判断证书的 CN 或 SAN 是否包含指定域名。
func matchesDomain(cert *x509.Certificate, domain string) bool {
	if cert.Subject.CommonName == domain {
		return true
	}
	for _, name := range cert.DNSNames {
		if name == domain {
			return true
		}
	}
	return false
}

// generateSelfSigned 生成一张 ECDSA P-256 自签证书并写入磁盘。
//
// 写文件用 0600/0644 权限：私钥只给服务用户可读；证书本身是公开信息。
// 先写临时文件再 rename，避免生成过程中被其他进程读到半个文件。
func generateSelfSigned(certPath, keyPath, domain string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("hy2: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("hy2: generate serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: domain, Organization: []string{"bing"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(selfSignedValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// 回环地址用于本地测试；真实部署用域名访问。
		DNSNames:              []string{domain, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("hy2: create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("hy2: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("hy2: parse generated certificate: %w", err)
	}
	if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
		cert.Leaf = leaf
	}
	return cert, nil
}

// writeFileAtomic 写临时文件后 rename 覆盖目标。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("hy2: create %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("hy2: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hy2: close %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("hy2: chmod %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("hy2: rename %s: %w", path, err)
	}
	return nil
}

// defaultCertDir 返回自签证书的缓存目录：二进制同目录的 .certs/hy2。
//
// 与服务用户的可写目录一致（安装脚本把二进制放在 /opt/vless-small），
// 因此自更新和重启后证书都还在。
func defaultCertDir() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(".certs", "hy2")
	}
	return filepath.Join(filepath.Dir(exe), ".certs", "hy2")
}
