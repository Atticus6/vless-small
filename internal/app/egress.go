package app

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atticus6/vless-small/internal/platform"
)

// egressAddress 是一个可选的出口源 IP。
//
// Path 固定为 /ip1、/ip2…，客户端通过 WS 路径选择；列表第一项是默认出口，
// 与 /ip1、普通 -path 使用同一个源地址。Default 仅用于展示和上报。
type egressAddress struct {
	IP      string `json:"ip"`
	Path    string `json:"path"`
	Default bool   `json:"default,omitempty"`
}

// 一个网卡上不太可能超过 16 个需要暴露的 IPv4；限制数量避免异常配置
// 生成过多订阅节点。
const maxEgressAddresses = 16

// egressSelector 是 WS 路径到源 IP 的只读映射。
//
// 启动时构建一次，运行期间只读，因此不需要加锁。
type egressSelector struct {
	addresses []egressAddress
	byPath    map[string]net.IP
}

// newEgressSelector 把已排序的地址列表转成路径索引；空列表返回 nil，
// 表示当前节点不支持按路径选择出口。
func newEgressSelector(addresses []egressAddress) *egressSelector {
	if len(addresses) == 0 {
		return nil
	}

	selector := &egressSelector{
		addresses: make([]egressAddress, 0, len(addresses)),
		byPath:    make(map[string]net.IP, len(addresses)),
	}
	for _, address := range addresses {
		ip := net.ParseIP(address.IP).To4()
		if ip == nil {
			continue
		}
		selector.addresses = append(selector.addresses, address)
		selector.byPath[address.Path] = ip
	}
	if len(selector.addresses) == 0 {
		return nil
	}
	return selector
}

// lookup 返回路径对应的源 IP；路径不存在时 ok=false。
func (e *egressSelector) lookup(path string) (net.IP, bool) {
	if e == nil {
		return nil, false
	}
	ip, ok := e.byPath[path]
	return ip, ok
}

// egressPathIndex 解析 /ipN，返回 N。
//
// 只接受规范的十进制编号：/ip1、/ip2；/ip0、/ip01、/ipabc 都不算出口路径，
// 避免和用户自定义的普通 WS 路径混淆。
func egressPathIndex(path string) (int, bool) {
	rest, ok := strings.CutPrefix(path, "/ip")
	if !ok || rest == "" {
		return 0, false
	}
	if len(rest) > 1 && rest[0] == '0' {
		return 0, false
	}
	for _, char := range rest {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(rest)
	if err != nil || index < 1 {
		return 0, false
	}
	return index, true
}

// detectEgressIPs 自动检测默认出口网卡上的 IPv4。
//
// 只在 runtime=local 时启用；优先通过 UDP 连接让内核按路由表选源地址，
// 不依赖 ip/ifconfig 命令，也不读取发行版配置，因此 Debian、Alpine 等
// 环境行为一致。UDP 探测失败时再解析 Linux 的 /proc/net/route 兜底。
func detectEgressIPs() []egressAddress {
	if platform.DetectRuntime() != "local" {
		return nil
	}

	defaultIP := defaultEgressIPv4()
	var iface *net.Interface
	if defaultIP != nil {
		iface = interfaceForIP(defaultIP)
	}

	// 没有默认路由时 UDP 探测可能失败；用 /proc/net/route 找到默认网卡，
	// 再取该网卡的第一个 IPv4 作为默认地址。
	if iface == nil {
		if name := linuxDefaultRouteInterface(); name != "" {
			if found, err := net.InterfaceByName(name); err == nil {
				iface = found
				defaultIP = firstIPv4(interfaceIPv4s(found))
			}
		}
	}
	if iface == nil {
		return nil
	}

	return buildEgressAddresses(interfaceIPv4s(iface), defaultIP)
}

// defaultEgressIPv4 让内核选择访问公网时使用的源地址。
//
// UDP Dial 只创建 socket、不发送数据包；目标使用 IPv4 字面量，不触发 DNS。
func defaultEgressIPv4() net.IP {
	conn, err := net.DialTimeout("udp4", "1.1.1.1:80", time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return addr.IP.To4()
}

// interfaceForIP 找到持有指定 IPv4 的 up 网卡。
func interfaceForIP(target net.IP) *net.Interface {
	target = target.To4()
	if target == nil {
		return nil
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for index := range interfaces {
		iface := &interfaces[index]
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		for _, ip := range interfaceIPv4s(iface) {
			if ip.Equal(target) {
				return iface
			}
		}
	}
	return nil
}

// interfaceIPv4s 返回网卡上的全局 IPv4，过滤回环、link-local 和重复地址。
func interfaceIPv4s(iface *net.Interface) []net.IP {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{}, len(addrs))
	result := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if !ip.IsGlobalUnicast() {
			continue
		}
		key := ip.String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, ip)
	}
	return result
}

// firstIPv4 返回列表中的第一个地址；列表为空时返回 nil。
func firstIPv4(ips []net.IP) net.IP {
	if len(ips) == 0 {
		return nil
	}
	return ips[0]
}

// buildEgressAddresses 生成最终地址列表：默认地址排第一，其余按数值排序，
// 去重后分配 /ip1、/ip2…。
func buildEgressAddresses(ips []net.IP, defaultIP net.IP) []egressAddress {
	seen := make(map[string]struct{}, len(ips)+1)
	list := make([]net.IP, 0, len(ips)+1)

	add := func(ip net.IP) {
		ip = ip.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() {
			return
		}
		key := ip.String()
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		list = append(list, ip)
	}

	add(defaultIP)
	for _, ip := range ips {
		add(ip)
	}
	if len(list) == 0 {
		return nil
	}

	sort.Slice(list, func(i, j int) bool {
		return bytes.Compare(list[i], list[j]) < 0
	})

	// 排序后把默认地址换到第一位；默认地址不在网卡列表里时也保留它，
	// 因为内核确实把它选为默认源地址。
	if def := defaultIP.To4(); def != nil {
		for index, ip := range list {
			if ip.Equal(def) {
				list[0], list[index] = list[index], list[0]
				break
			}
		}
	}

	if len(list) > maxEgressAddresses {
		list = list[:maxEgressAddresses]
	}

	result := make([]egressAddress, 0, len(list))
	for index, ip := range list {
		result = append(result, egressAddress{
			IP:      ip.String(),
			Path:    fmt.Sprintf("/ip%d", index+1),
			Default: index == 0,
		})
	}
	return result
}

// dialNetwork 在绑定 IPv4 源地址时强制使用 IPv4 网络。
//
// 普通连接保持 tcp/udp，继续支持 IPv6 目标；指定源 IP 的连接如果目标只
// 解析出 IPv6，会按预期失败，而不是静默绕开出口选择。
func dialNetwork(base string, source net.IP) string {
	if source == nil {
		return base
	}
	switch base {
	case "tcp":
		return "tcp4"
	case "udp":
		return "udp4"
	default:
		return base
	}
}

// sourceIPForPath 返回请求路径对应的源 IP，以及该路径是否允许。
//
// 返回值 ok=false 表示这是一个形如 /ipN 但不存在于当前出口列表的路径，
// 调用方应返回 404。普通路径返回 (nil, true)，由原有 -path 规则继续判断。
// 主控开关关闭时一律按普通路径处理，不暴露 /ipN。
func (s *Server) sourceIPForPath(path string) (net.IP, bool) {
	if s.egress == nil || !s.egressEnabled.Load() {
		return nil, true
	}
	if _, isEgressPath := egressPathIndex(path); !isEgressPath {
		return nil, true
	}
	return s.egress.lookup(path)
}

// SetEgressEnabled 应用主控下发的出口选择开关；节点启动时默认关闭。
func (s *Server) SetEgressEnabled(enabled bool) {
	s.egressEnabled.Store(enabled)
}

// EgressEnabled 返回主控下发的出口选择开关状态。
func (s *Server) EgressEnabled() bool {
	return s.egressEnabled.Load()
}

// EgressAddresses 返回当前出口列表的副本，供上报和 /<token>/config 使用。
// 即使开关关闭也返回检测结果，便于 Web 展示可用地址；订阅是否展开由
// 主控的 egress_enabled 决定。
func (s *Server) EgressAddresses() []egressAddress {
	if s.egress == nil {
		return nil
	}
	result := make([]egressAddress, len(s.egress.addresses))
	copy(result, s.egress.addresses)
	return result
}
