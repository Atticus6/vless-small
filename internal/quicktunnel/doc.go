// Package quicktunnel 实现 Cloudflare Quick Tunnel 客户端。
//
// 它只依赖独立 QUIC 实现（quic-go）与 Go 标准库：临时隧道申请走
// https://api.trycloudflare.com/tunnel，随后通过 QUIC 与 Cloudflare 边缘建立
// 隧道控制连接，并把边缘转发的 HTTP/WebSocket 请求代理到本地地址。
//
// 协议要点（与 cloudflared 线格式兼容，但不依赖其任何源码/二进制）：
//   - 边缘地址来自 `_v2-origintunneld._tcp.argotunnel.com` 的 SRV 记录；
//   - TLS SNI 固定为 quic.cftunnel.com，ALPN 为 argotunnel；
//   - 控制流是第一条双向 QUIC 流，使用 Cap'n Proto RPC；
//   - 每条入站数据流以 6 字节魔数 + 版本 "01" + Cap'n Proto ConnectRequest 开头；
//   - 响应先写同样的前导与 ConnectResponse，再原样转发响应体。
//
// 该包只实现 HTTP/WebSocket 转发（VLESS over WebSocket 场景），不支持 TCP
// 类型的数据流与 WARP/ICMP/datagram。
package quicktunnel
