package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// trafficDelta 是一个 UUID 在一个上报周期内的上下行字节数。
type trafficDelta struct {
	Up   uint64
	Down uint64
}

// JavaScript 的 Number 只能精确表示到 2^53-1；超过这个值 Worker 的 zod
// 校验会拒绝整批上报。正常 30 分钟窗口不可能达到，这里只做防御性截断。
const maxTrafficBytes uint64 = 1<<53 - 1

// trafficValues 用原子计数支持同一 UUID 的多条并发连接。
type trafficValues struct {
	up   atomic.Uint64
	down atomic.Uint64
}

// trafficCounter 按设备 UUID 累计待上报流量。
//
// add 在 RLock 内完成原子累加，take 在写锁内换 map 并读取快照，避免
// “add 已拿到旧 values 指针、take 已换走 map”导致增量丢失。RLock 可被
// 多条连接共享，热路径不会串行化。
type trafficCounter struct {
	mu   sync.RWMutex
	byID map[[16]byte]*trafficValues
}

func newTrafficCounter() *trafficCounter {
	return &trafficCounter{byID: make(map[[16]byte]*trafficValues)}
}

// add 累加一条连接的上下行字节；零增量直接忽略。
func (c *trafficCounter) add(id [16]byte, up, down uint64) {
	if c == nil || (up == 0 && down == 0) {
		return
	}

	c.mu.RLock()
	values := c.byID[id]
	if values != nil {
		if up > 0 {
			values.up.Add(up)
		}
		if down > 0 {
			values.down.Add(down)
		}
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	c.mu.Lock()
	values = c.byID[id]
	if values == nil {
		values = &trafficValues{}
		c.byID[id] = values
	}
	if up > 0 {
		values.up.Add(up)
	}
	if down > 0 {
		values.down.Add(down)
	}
	c.mu.Unlock()
}

// take 取走当前累计值并重置 map。
func (c *trafficCounter) take() map[[16]byte]trafficDelta {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.byID
	c.byID = make(map[[16]byte]*trafficValues)
	if len(previous) == 0 {
		return nil
	}

	result := make(map[[16]byte]trafficDelta, len(previous))
	for id, values := range previous {
		result[id] = trafficDelta{
			Up:   values.up.Load(),
			Down: values.down.Load(),
		}
	}
	return result
}

// restore 把未成功上报的增量合并回当前计数器。
func (c *trafficCounter) restore(deltas map[[16]byte]trafficDelta) {
	for id, delta := range deltas {
		c.add(id, delta.Up, delta.Down)
	}
}

// nodeTrafficEntry 是上报体里的单个设备流量。
type nodeTrafficEntry struct {
	ID   string `json:"id"`
	Up   uint64 `json:"up"`
	Down uint64 `json:"down"`
}

// nodeTrafficReport 是 POST /api/nodes/traffic 的请求体。
type nodeTrafficReport struct {
	Name    string             `json:"name"`
	Traffic []nodeTrafficEntry `json:"traffic"`
}

// reportTraffic 强制上报当前累计流量（用于退出前刷新和测试）。
func reportTraffic(cfg NodeConfig, srv *Server) error {
	_, err := reportTrafficNow(cfg, srv, true)
	return err
}

// reportTrafficNow 把当前累计流量上报给主控。
//
// force=false 时，本轮总量低于 trafficReportMinBytes 会回滚计数并暂缓上报，
// 等下一轮继续累计；force=true 时无论大小都发送（退出刷新或已累计 30 分钟）。
// 返回值 sent 表示是否真正发出了请求。
func reportTrafficNow(
	cfg NodeConfig,
	srv *Server,
	force bool,
) (bool, error) {
	deltas := srv.takeTraffic()
	if len(deltas) == 0 {
		return false, nil
	}

	entries := make([]nodeTrafficEntry, 0, len(deltas))
	var total uint64
	for id, delta := range deltas {
		if delta.Up == 0 && delta.Down == 0 {
			continue
		}
		up := min(delta.Up, maxTrafficBytes)
		down := min(delta.Down, maxTrafficBytes)
		total += up + down
		entries = append(entries, nodeTrafficEntry{
			ID:   formatUUID(id),
			Up:   up,
			Down: down,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
	if len(entries) == 0 {
		return false, nil
	}

	// 没有流量或流量很小：保留累计值，下一轮继续累计；最多跳过
	// trafficForceReportAfter 次后由 trafficLoop 强制发送。
	if !force && total < trafficReportMinBytes {
		srv.restoreTraffic(deltas)
		return false, nil
	}

	for start := 0; start < len(entries); start += trafficReportBatchSize {
		end := start + trafficReportBatchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := postTrafficReport(cfg, entries[start:end]); err != nil {
			// 失败批次和尚未发送的批次都回滚；成功批次已经计入 AE。
			srv.restoreTraffic(entriesToDeltas(entries[start:]))
			return false, err
		}
	}

	log.Printf("reported traffic for %d devices (%d bytes)", len(entries), total)
	return true, nil
}

// postTrafficReport 发送一批设备流量。
func postTrafficReport(cfg NodeConfig, entries []nodeTrafficEntry) error {
	payload, err := json.Marshal(nodeTrafficReport{
		Name:    cfg.Name,
		Traffic: entries,
	})
	if err != nil {
		return fmt.Errorf("encode traffic: %w", err)
	}

	req, err := http.NewRequest(
		http.MethodPost,
		cfg.BaseURL+"/api/nodes/traffic",
		bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("build traffic request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := syncHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("report traffic: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf(
			"report traffic %s: %s",
			resp.Status,
			strings.TrimSpace(string(body)),
		)
	}
	return nil
}

// entriesToDeltas 把上报条目还原成计数器增量，用于失败回滚。
func entriesToDeltas(entries []nodeTrafficEntry) map[[16]byte]trafficDelta {
	result := make(map[[16]byte]trafficDelta, len(entries))
	for _, entry := range entries {
		id, err := parseUUID(entry.ID)
		if err != nil {
			continue
		}
		result[id] = trafficDelta{Up: entry.Up, Down: entry.Down}
	}
	return result
}

// trafficLoop 每 5 分钟检查一次流量；小流量暂缓，最多累计到 30 分钟强制上报。
func trafficLoop(cfg NodeConfig, srv *Server) {
	ticker := time.NewTicker(trafficReportInterval)
	defer ticker.Stop()

	skipped := 0
	for range ticker.C {
		force := skipped >= trafficForceReportAfter
		sent, err := reportTrafficNow(cfg, srv, force)
		if err != nil {
			log.Printf("traffic report failed: %v", err)
			continue
		}
		if sent {
			skipped = 0
		} else {
			skipped++
		}
	}
}

// FlushTraffic 在进程退出前尽量把剩余流量发送出去。
//
// 手动停止、systemd 重启和 Docker stop 会走这里；自更新直接 os.Exit，
// 来不及刷新的部分会丢失。
func FlushTraffic(cfg NodeConfig, srv *Server) {
	if cfg.BaseURL == "" {
		return
	}
	if err := reportTraffic(cfg, srv); err != nil {
		log.Printf("final traffic report failed: %v", err)
	}
}

// addTraffic 记录一条已转发连接的字节数。
func (s *Server) addTraffic(id [16]byte, up, down uint64) {
	if s.traffic != nil {
		s.traffic.add(id, up, down)
	}
}

// takeTraffic 取走当前累计流量。
func (s *Server) takeTraffic() map[[16]byte]trafficDelta {
	if s.traffic == nil {
		return nil
	}
	return s.traffic.take()
}

// restoreTraffic 在批量上报失败时回滚未发送的增量。
func (s *Server) restoreTraffic(deltas map[[16]byte]trafficDelta) {
	if s.traffic != nil {
		s.traffic.restore(deltas)
	}
}
