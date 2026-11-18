package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
)

// parseRequest 解析 VLESS 请求头.
//
//	+---------+----------+--------+---------+-------+----------+---------+
//	| Version |  UUID    | Addon  | Command | Port  | AddrType | Address |
//	| 1 byte  | 16 bytes | var    | 1 byte  | 2 B   | 1 byte   | var     |
//	+---------+----------+--------+---------+-------+----------+---------+
//
// 返回目标地址和头之后剩余的 early data.
func parseRequest(b []byte) (id [16]byte, cmd byte, target string, payload []byte, err error) {
	if len(b) < 18 {
		err = fmt.Errorf("header too short: %d", len(b))
		return
	}
	if b[0] != vlessVersion {
		err = fmt.Errorf("unsupported version: %d", b[0])
		return
	}
	copy(id[:], b[1:17])

	off := 18 + int(b[17]) // 跳过 addon
	if len(b) < off+4 {
		err = fmt.Errorf("header truncated before command")
		return
	}
	cmd = b[off]
	port := binary.BigEndian.Uint16(b[off+1 : off+3])
	atyp := b[off+3]
	off += 4

	var host string
	switch atyp {
	case atypIPv4:
		if len(b) < off+4 {
			err = fmt.Errorf("header truncated in IPv4 address")
			return
		}
		host = net.IP(b[off : off+4]).String()
		off += 4
	case atypDomain:
		if len(b) < off+1 {
			err = fmt.Errorf("header truncated in domain length")
			return
		}
		n := int(b[off])
		off++
		if len(b) < off+n {
			err = fmt.Errorf("header truncated in domain")
			return
		}
		host = string(b[off : off+n])
		off += n
	case atypIPv6:
		if len(b) < off+16 {
			err = fmt.Errorf("header truncated in IPv6 address")
			return
		}
		host = net.IP(b[off : off+16]).String()
		off += 16
	default:
		err = fmt.Errorf("unsupported address type: %d", atyp)
		return
	}
	if host == "" {
		err = fmt.Errorf("empty target host")
		return
	}

	target = net.JoinHostPort(host, strconv.Itoa(int(port)))
	payload = b[off:]
	return
}
