package app

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// ParseUsers 解析逗号分隔的 UUID 列表.
func ParseUsers(raw string) (map[[16]byte]struct{}, error) {
	users := make(map[[16]byte]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := parseUUID(part)
		if err != nil {
			return nil, err
		}
		users[id] = struct{}{}
	}
	return users, nil
}

// parseUUID 接受带或不带连字符的 UUID.
func parseUUID(s string) ([16]byte, error) {
	var id [16]byte
	hexStr := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(hexStr) != 32 {
		return id, fmt.Errorf("invalid UUID %q", s)
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return id, fmt.Errorf("invalid UUID %q: %w", s, err)
	}
	copy(id[:], b)
	return id, nil
}

// formatUUID 把 16 字节 UUID 转成标准带连字符的形式.
func formatUUID(id [16]byte) string {
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(id[0:4]),
		hex.EncodeToString(id[4:6]),
		hex.EncodeToString(id[6:8]),
		hex.EncodeToString(id[8:10]),
		hex.EncodeToString(id[10:16]),
	)
}
