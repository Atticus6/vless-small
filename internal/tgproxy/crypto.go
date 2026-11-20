// Package tgproxy 实现 Telegram Desktop 内置 web-proxy（tdesktop web proxy）
// 协议的服务端。
//
// 协议分层（与客户端 carrier 对应）：
//   - HTTP 信令：POST/DELETE /api/v1/session 创建/销毁会话，GET /api/v1/ws
//     升级为一条 "lane" WebSocket；一条 lane 对应一条到 Telegram DC 的 TCP 流。
//   - lane 帧协议：[type:1][sid:3][len:4][payload]，type 1=open 2=data 3=close
//     4=window 流控，17=welcome。
//   - TG 数据面：客户端首个 64 字节是 AES-CTR 加密的伪装头，解密后得到
//     mode(dd/ee/ef)、DC 编号、media 标志；服务端重写伪装头，连到 Telegram
//     WebK（wss://kws{N}.web.telegram.org/apiws），双向做 AES-CTR 重加密与
//     长度分帧（mode ef 为 1/4 字节词头，dd/ee 为 4 字节长度头）。
//
// 本文件实现密钥派生、HMAC token、AES-CTR 流密码，全部与 JS 参考实现逐字节
// 对齐（见 golden_test.go 中从参考实现生成的向量）。
package tgproxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// b64url 是 URL 安全的 base64（无填充），与 JS 的 b64() 一致。
func b64url(b []byte) string {
	return strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(base64Std(b)), "=")
}

func base64Std(b []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var v uint32
		n := 0
		for j := 0; j < 3 && i+j < len(b); j++ {
			v = v<<8 | uint32(b[i+j])
			n++
		}
		v <<= uint(3-n) * 8
		for j := 0; j < 4; j++ {
			if j <= n {
				sb.WriteByte(tbl[(v>>uint((3-j)*6))&0x3f])
			} else {
				sb.WriteByte('=')
			}
		}
	}
	return sb.String()
}

func un64(s string) ([]byte, error) {
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	for len(s)%4 != 0 {
		s += "="
	}
	return base64Decode(s)
}

func base64Decode(s string) ([]byte, error) {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	dec := map[byte]int{}
	for i := 0; i < len(tbl); i++ {
		dec[tbl[i]] = i
	}
	out := make([]byte, 0, len(s)*3/4)
	var buf uint32
	bits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			break
		}
		v, ok := dec[c]
		if !ok {
			return nil, errors.New("bad base64")
		}
		buf = buf<<6 | uint32(v)
		bits += 6
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(buf>>uint(bits)))
		}
	}
	return out, nil
}

// normHex 把 UUID 形式的 SECRET 归一成 32 hex；其余格式原样小写返回。
func normHex(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	parts := strings.Split(s, "-")
	if len(parts) == 5 && len(parts[0]) == 8 && len(parts[1]) == 4 && len(parts[2]) == 4 && len(parts[3]) == 4 && len(parts[4]) == 12 {
		return strings.Join(parts, "")
	}
	return s
}

// keyFrom 把 SECRET 转成 16 字节 AES key。
//
// 规则：接受 32 hex 或 dd/ee/ef 前缀 + 32 hex；keepTag 且 dd 前缀时保留完整
// 34 hex（取前 16 字节），否则取末 32 hex（即后 16 字节）。
func keyFrom(v string, keepTag bool) ([]byte, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	if len(s) != 32 && len(s) != 34 {
		return nil, errors.New("bad secret length")
	}
	if (len(s) == 34 && !strings.HasPrefix(s, "dd") && !strings.HasPrefix(s, "ee") && !strings.HasPrefix(s, "ef")) || !isHex(s) {
		return nil, errors.New("bad secret format")
	}
	use := s
	if !(keepTag && len(s) == 34 && strings.HasPrefix(s, "dd")) {
		use = s[len(s)-32:]
	}
	return hex.DecodeString(use)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// 协议里 HMAC 层的固定标签（与 JS 参考实现一致）。
const (
	masterLabel = "tg-webpro" + "xy-master-v1"
	bootLabel   = "tg-webpro" + "xy-boot-v1"
	bridgeLabel = "tdesktop-web-pr" + "oxy-bridge-v1"
)

// deriveSeed: HMAC(HMAC(seedKey, secret), masterKey) → 64 hex 字符串。
func deriveSeed(rawKey string) (string, error) {
	msg := []byte(strings.ToLower(strings.TrimSpace(rawKey)))
	master := hmacSHA256([]byte(masterLabel), msg)
	seed := hmacSHA256([]byte(bootLabel), master)
	return hex.EncodeToString(seed), nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// hmacTag = HMAC-SHA256(seed, "<kind>\n"+body) 的前 16 字节。
func hmacTag(seed, kind string, body []byte) []byte {
	msg := make([]byte, 0, len(kind)+1+len(body))
	msg = append(msg, kind...)
	msg = append(msg, '\n')
	msg = append(msg, body...)
	return hmacSHA256([]byte(seed), msg)[:16]
}

// mintToken = b64url(exp(4B BE) | nonce(12B) | mac(16B))。
func mintToken(seed, kind string, expMs int64, nonce []byte) string {
	body := make([]byte, 16)
	binary.BigEndian.PutUint32(body[0:4], uint32(expMs/1000))
	copy(body[4:], nonce)
	tok := make([]byte, 32)
	copy(tok, body)
	copy(tok[16:], hmacTag(seed, kind, body))
	return b64url(tok)
}

// sigCheck 校验 token 的签名与过期时间；kind 为 "bootstrap" 或 "session"。
func sigCheck(tok, seed, kind string) bool {
	if len(tok) != 43 {
		return false
	}
	raw, err := un64(tok)
	if err != nil || len(raw) != 32 || b64url(raw) != tok {
		return false
	}
	body := raw[:16]
	exp := int64(binary.BigEndian.Uint32(body[:4]))
	if exp <= time.Now().Unix() {
		return false
	}
	return hmac.Equal(raw[16:], hmacTag(seed, kind, body))
}

// checkSeed 接受原始 seed 或有效的 bootstrap token。
func checkSeed(v, seed string) bool {
	return v == seed || sigCheck(v, seed, "bootstrap")
}

// mkCap 生成 ?bridge= 签名（HMAC(keyFrom(skey,true), "bridge:"+skey) 对
// "tdesktop-webproxy-bridge-v1\n"+host 签名）。
func mkCap(skey, host string) (string, error) {
	key, err := keyFrom(skey, true)
	if err != nil {
		return "", err
	}
	host = validHost(host)
	mac := hmacSHA256(key, []byte("bridge:"+strings.ToLower(skey)))
	msg := []byte(bridgeLabel + "\n" + host)
	return b64url(hmacSHA256(mac, msg)), nil
}

// validHost 校验域名格式，去掉末尾点并转小写。
func validHost(v string) string {
	host := strings.ToLower(strings.TrimSpace(v))
	host = strings.TrimSuffix(host, ".")
	if !strings.Contains(host, ".") || len(host) > 253 || strings.Contains(host, ":") {
		return ""
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || !isHostLabel(label) {
			return ""
		}
	}
	return host
}

func isHostLabel(s string) bool {
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// sha256Sum 返回 SHA-256 摘要。
func sha256Sum(v []byte) []byte {
	h := sha256.Sum256(v)
	return h[:]
}

// streamCipher 是 AES-CTR 流密码，支持任意长度（含非 16 倍数）的连续加解密，
// 与 JS makeCtr 的 tail 机制一致。
type streamCipher struct {
	block cipher.Block
	iv    []byte
	tail  []byte
}

func newStreamCipher(key, iv []byte) (*streamCipher, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &streamCipher{block: b, iv: append([]byte(nil), iv...), tail: nil}, nil
}

func ctrStep(ctr []byte, n uint64) {
	carry := n
	for i := len(ctr) - 1; i >= 0 && carry > 0; i-- {
		next := uint64(ctr[i]) + (carry & 0xff)
		ctr[i] = byte(next)
		carry = carry>>8 + next>>8
	}
}

// crypt 对 in 就地流式加解密并返回结果。
func (s *streamCipher) crypt(in []byte) []byte {
	if len(in) == 0 {
		return []byte{}
	}
	out := make([]byte, len(in))
	if len(s.tail) == 0 && len(in)%16 == 0 {
		c := make([]byte, len(s.iv))
		copy(c, s.iv)
		cipher.NewCTR(s.block, c).XORKeyStream(out, in)
		ctrStep(s.iv, uint64(len(in)/16))
		return out
	}
	if len(in) <= len(s.tail) {
		for i := range in {
			out[i] = in[i] ^ s.tail[i]
		}
		s.tail = s.tail[len(in):]
		return out
	}
	skip := 0
	if len(s.tail) > 0 {
		skip = 16 - len(s.tail)
	}
	ctr := append([]byte(nil), s.iv...)
	if skip > 0 {
		for i := len(ctr) - 1; i >= 0; i-- {
			if ctr[i] > 0 {
				ctr[i]--
				break
			}
		}
	}
	end := skip + len(in)
	blockLen := (end + 15) / 16 * 16
	block := make([]byte, blockLen)
	copy(block[skip:], in)
	raw := make([]byte, blockLen)
	cipher.NewCTR(s.block, ctr).XORKeyStream(raw, block)
	s.tail = raw[end:]
	step := uint64(blockLen / 16)
	if skip > 0 {
		step--
	}
	ctrStep(s.iv, step)
	return raw[skip:end]
}

// cipherFor 从 64 字节伪装头派生 streamCipher。
// reverse=true 时把 head[8:56] 反转（用于解密客户端发来的方向）。
func cipherFor(head, skey []byte, reverse bool) (*streamCipher, error) {
	mat := append([]byte(nil), head[8:56]...)
	if reverse {
		for i, j := 0, len(mat)-1; i < j; i, j = i+1, j-1 {
			mat[i], mat[j] = mat[j], mat[i]
		}
	}
	var key []byte
	if len(skey) > 0 {
		key = sha256Sum(append(append([]byte(nil), mat[:32]...), skey...))
	} else {
		key = mat[:32]
	}
	return newStreamCipher(key, mat[32:48])
}

// makeHeadWire 生成 64 字节伪装头的线格式（前 56 字节明文，后 8 字节加密）。
func makeHeadWire(head []byte) ([]byte, *streamCipher, error) {
	c, err := cipherFor(head, nil, false)
	if err != nil {
		return nil, nil, err
	}
	wire := c.crypt(head)
	copy(wire[:56], head[:56])
	return wire, c, nil
}

// parseHead 从解密后的 64 字节头里读出 mode、dc 编号、media 标志。
func parseHead(head []byte) (mode byte, target int, media bool, err error) {
	tag := binary.LittleEndian.Uint32(head[56:60])
	switch tag {
	case 0xefefefef:
		mode = 0xef
	case 0xeeeeeeee:
		mode = 0xee
	case 0xdddddddd:
		mode = 0xdd
	default:
		return 0, 0, false, errors.New("bad head mode")
	}
	dc := int(int16(binary.LittleEndian.Uint16(head[60:62])))
	if dc == 0 {
		return 0, 0, false, errors.New("bad dc")
	}
	target = abs(dc) - 1
	if target < 0 || target >= 5 {
		return 0, 0, false, errors.New("dc out of range")
	}
	return mode, target, dc < 0, nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// 握手校验：9 字节 0x10 00 00 00 | ver=1(BE) | 1。
func checkHandshake(body []byte) bool {
	if len(body) != 9 || body[0] != 0x10 || body[1] != 0 || body[2] != 0 || body[3] != 0 || body[8] != 1 {
		return false
	}
	return binary.BigEndian.Uint32(body[4:8]) == 1
}

// tokenFrom 从 Authorization: Bearer <token> 中提取 token。
func tokenFrom(v string) string {
	if strings.HasPrefix(v, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	return ""
}

// readRand 填充随机字节（供 token nonce 使用）。
func readRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
}
