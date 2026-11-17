package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
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

// parseRequestStream 从字节流中读取 VLESS 请求头.
// 与 parseRequest 的区别是：只消费头部，early data 留在 bufio.Reader 中，
// 供后续 TCP/UDP relay 直接读取.
func parseRequestStream(r *bufio.Reader) (id [16]byte, cmd byte, target string, err error) {
	head := make([]byte, 18)
	if _, err = io.ReadFull(r, head); err != nil {
		return
	}
	if head[0] != vlessVersion {
		err = fmt.Errorf("unsupported version: %d", head[0])
		return
	}
	copy(id[:], head[1:17])

	addonLen := int(head[17])
	if addonLen > 0 {
		if _, err = io.CopyN(io.Discard, r, int64(addonLen)); err != nil {
			return
		}
	}

	fixed := make([]byte, 4)
	if _, err = io.ReadFull(r, fixed); err != nil {
		return
	}
	cmd = fixed[0]
	port := binary.BigEndian.Uint16(fixed[1:3])
	atyp := fixed[3]

	var host string
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case atypDomain:
		n, readErr := r.ReadByte()
		if readErr != nil {
			err = readErr
			return
		}
		b := make([]byte, int(n))
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = string(b)
	case atypIPv6:
		b := make([]byte, 16)
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		err = fmt.Errorf("unsupported address type: %d", atyp)
		return
	}
	if host == "" {
		err = fmt.Errorf("empty target host")
		return
	}

	target = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return
}
