package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

/**
 * handleDevicePush 处理主控“新设备立即同步”的推送。
 *
 * 为了让反向代理无需额外放行新路径，推送复用节点的 WS 路径：
 * `POST <WS_PATH>`，用 CONFIG token 校验 Bearer。返回 false 表示这不是
 * 有效推送（例如普通 WS 升级请求），继续走原来的 WS 处理。
 *
 * 节点仍然会每 60 秒从 /api/nodes/report 拉取全量 UUID；这里只是加速
 * 新设备生效，推送内容也是完整设备列表，和拉取结果保持一致。
 */
func (s *server) handleDevicePush(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || s.controlToken == "" {
		return false
	}
	if s.path != "" && r.URL.Path != s.path {
		return false
	}

	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(authorization[len("Bearer "):])
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.controlToken)) != 1 {
		return false
	}

	var body struct {
		Devices []nodeDevice `json:"devices"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return true
	}

	users := make(map[[16]byte]struct{}, len(s.staticUsers)+len(body.Devices))
	for id := range s.staticUsers {
		users[id] = struct{}{}
	}
	for _, device := range body.Devices {
		if !device.Enabled {
			continue
		}
		id, err := parseUUID(device.ID)
		if err != nil {
			log.Printf("push: skip device %q: %v", device.Name, err)
			continue
		}
		users[id] = struct{}{}
	}

	s.setUsers(users)
	log.Printf("device push: %d users", len(users))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "users": len(users)})
	return true
}
