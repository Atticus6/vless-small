package hy2

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// STUN 常量（RFC 5389）。
const (
	stunBindingRequest  = 0x0001
	stunBindingResponse = 0x0101
	stunMagicCookie     = 0x2112A442
	stunHeaderSize      = 20
	stunAttrXorMapped   = 0x0020
	stunAttrMapped      = 0x0001
	stunFamilyIPv4      = 0x01
	stunFamilyIPv6      = 0x02
)

// stunServers 是探测用的公共 STUN 服务器；任何一个返回即可。
var stunServers = []string{
	"stun.cloudflare.com:3478",
	"stun.l.google.com:19302",
}

// ProbeResult 是一次可达性探测的结果。
type ProbeResult struct {
	// Reachable 表示公网 IP:端口 与本地绑定一致，即外部可以直接连进来。
	Reachable bool
	// Mapped 是 STUN 返回的映射地址（ip:port），探测失败时为空。
	Mapped string
	// Reason 说明跳过或通过的原因，用于日志。
	Reason string
}

// ProbeReachability 判断 UDP 监听端口能否从公网直接访问。
//
// Hysteria 是 UDP/QUIC 入口，TCP 连通性测试对它没有意义；这里用 STUN
// binding 请求拿到出口 NAT 映射地址，再和预期地址比对：
//
//   - 映射 IP:端口 == 公网 IP:监听端口 → 没有 NAT 改写，外部可直接访问；
//   - 不一致（CGNAT、端口随机映射、PaaS 只放行 443）→ 外部连不进来，
//     启动 hy2 只会得到一个客户端永远连不上的入口。
//
// 探测会临时占用监听端口，必须在启动 QUIC 监听之前完成。
func ProbeReachability(ctx context.Context, listen, publicIP string) ProbeResult {
	addr, err := net.ResolveUDPAddr("udp", strings.TrimSpace(listen))
	if err != nil {
		return ProbeResult{Reason: fmt.Sprintf("invalid listen address: %v", err)}
	}
	if addr.Port == 0 {
		return ProbeResult{Reason: "listen port is 0"}
	}
	// 只绑回环时外部一定访问不到，不必发探测包。
	if addr.IP != nil && addr.IP.IsLoopback() {
		return ProbeResult{Reason: fmt.Sprintf("listen address %s is loopback only", addr)}
	}
	if strings.TrimSpace(publicIP) == "" {
		return ProbeResult{Reason: "public IP unknown"}
	}
	if ip := net.ParseIP(publicIP); ip == nil || !isPublicIP(ip) {
		return ProbeResult{Reason: fmt.Sprintf("public IP %q is not a public address", publicIP)}
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return ProbeResult{Reason: fmt.Sprintf("bind %s: %v", addr, err)}
	}
	defer conn.Close()

	var lastErr error
	for _, server := range stunServers {
		mapped, err := stunQuery(ctx, conn, server)
		if err != nil {
			lastErr = err
			continue
		}
		want := net.JoinHostPort(publicIP, strconv.Itoa(addr.Port))
		if sameEndpoint(mapped, want) {
			return ProbeResult{
				Reachable: true,
				Mapped:    mapped,
				Reason:    fmt.Sprintf("mapped %s matches listen port", mapped),
			}
		}
		return ProbeResult{
			Mapped: mapped,
			Reason: fmt.Sprintf("mapped %s != expected %s (NAT or port not exposed)", mapped, want),
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no STUN server available")
	}
	return ProbeResult{Reason: fmt.Sprintf("STUN probe failed: %v", lastErr)}
}

// stunQuery 向一个 STUN 服务器发 binding 请求并解析映射地址。
func stunQuery(ctx context.Context, conn *net.UDPConn, server string) (string, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return "", err
	}
	request, transactionID, err := buildSTUNRequest()
	if err != nil {
		return "", err
	}

	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := conn.WriteToUDP(request, serverAddr); err != nil {
		return "", err
	}

	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", err
		}
		// 只接受来自目标服务器的响应，避免把无关 UDP 包当成 STUN。
		if !from.IP.Equal(serverAddr.IP) {
			continue
		}
		mapped, err := parseSTUNResponse(buf[:n], transactionID)
		if err != nil {
			return "", err
		}
		return mapped, nil
	}
}

// buildSTUNRequest 构造 binding 请求，返回报文与事务 ID。
func buildSTUNRequest() ([]byte, []byte, error) {
	transactionID := make([]byte, 12)
	if _, err := rand.Read(transactionID); err != nil {
		return nil, nil, err
	}
	msg := make([]byte, stunHeaderSize)
	binary.BigEndian.PutUint16(msg[0:], stunBindingRequest)
	binary.BigEndian.PutUint16(msg[2:], 0) // 无属性
	binary.BigEndian.PutUint32(msg[4:], stunMagicCookie)
	copy(msg[8:], transactionID)
	return msg, transactionID, nil
}

// parseSTUNResponse 校验响应并提取 XOR-MAPPED-ADDRESS（或旧式 MAPPED-ADDRESS）。
func parseSTUNResponse(msg, transactionID []byte) (string, error) {
	if len(msg) < stunHeaderSize {
		return "", errors.New("short STUN response")
	}
	if binary.BigEndian.Uint16(msg[0:]) != stunBindingResponse {
		return "", fmt.Errorf("unexpected STUN type 0x%04x", binary.BigEndian.Uint16(msg[0:]))
	}
	if binary.BigEndian.Uint32(msg[4:]) != stunMagicCookie {
		return "", errors.New("bad STUN magic cookie")
	}
	if string(msg[8:20]) != string(transactionID) {
		return "", errors.New("STUN transaction ID mismatch")
	}

	length := int(binary.BigEndian.Uint16(msg[2:]))
	end := stunHeaderSize + length
	if end > len(msg) {
		return "", errors.New("truncated STUN response")
	}

	for offset := stunHeaderSize; offset+4 <= end; {
		attrType := binary.BigEndian.Uint16(msg[offset:])
		attrLen := int(binary.BigEndian.Uint16(msg[offset+2:]))
		valueStart := offset + 4
		valueEnd := valueStart + attrLen
		if valueEnd > end {
			return "", errors.New("truncated STUN attribute")
		}
		switch attrType {
		case stunAttrXorMapped:
			return decodeMappedAddress(msg[valueStart:valueEnd], transactionID, true)
		case stunAttrMapped:
			return decodeMappedAddress(msg[valueStart:valueEnd], transactionID, false)
		}
		// 属性按 4 字节对齐填充。
		offset = valueStart + (attrLen+3)/4*4
	}
	return "", errors.New("STUN response has no mapped address")
}

// decodeMappedAddress 解析 MAPPED-ADDRESS / XOR-MAPPED-ADDRESS。
func decodeMappedAddress(value, transactionID []byte, xor bool) (string, error) {
	if len(value) < 4 {
		return "", errors.New("short mapped address")
	}
	family := value[1]
	port := binary.BigEndian.Uint16(value[2:4])
	rawIP := value[4:]

	if xor {
		port ^= uint16(stunMagicCookie >> 16)
		// IPv4 与 magic cookie 异或；IPv6 再拼接事务 ID。
		mask := make([]byte, 16)
		binary.BigEndian.PutUint32(mask[0:], stunMagicCookie)
		copy(mask[4:], transactionID)
		for i := range rawIP {
			if i < len(mask) {
				rawIP[i] ^= mask[i]
			}
		}
	}

	switch family {
	case stunFamilyIPv4:
		if len(rawIP) < net.IPv4len {
			return "", errors.New("short IPv4 mapped address")
		}
		return net.JoinHostPort(net.IP(rawIP[:net.IPv4len]).String(), strconv.Itoa(int(port))), nil
	case stunFamilyIPv6:
		if len(rawIP) < net.IPv6len {
			return "", errors.New("short IPv6 mapped address")
		}
		return net.JoinHostPort(net.IP(rawIP[:net.IPv6len]).String(), strconv.Itoa(int(port))), nil
	default:
		return "", fmt.Errorf("unsupported address family 0x%02x", family)
	}
}

// sameEndpoint 比较两个 ip:port 是否等价（IPv6 表示差异由 ParseIP 归一）。
func sameEndpoint(a, b string) bool {
	hostA, portA, errA := net.SplitHostPort(a)
	hostB, portB, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || portA != portB {
		return false
	}
	ipA, ipB := net.ParseIP(hostA), net.ParseIP(hostB)
	if ipA == nil || ipB == nil {
		return strings.EqualFold(hostA, hostB)
	}
	return ipA.Equal(ipB)
}

// isPublicIP 判断地址是否是公网可路由地址。
//
// 排除回环、私网、链路本地、CGNAT（100.64/10）和未指定地址：这些情况下
// 无论端口是否放行，外部都无法直连。
func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10：运营商级 NAT（CGNAT），没有公网入站。
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
	}
	return true
}
