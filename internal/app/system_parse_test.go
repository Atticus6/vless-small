package app

import (
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	text := `MemTotal:       16384000 kB
MemFree:         2048000 kB
MemAvailable:   12288000 kB
Buffers:          512000 kB
Cached:          4096000 kB
SwapTotal:       2048000 kB
SwapFree:        1024000 kB
`

	got, ok := parseMeminfo(text)
	if !ok {
		t.Fatal("parseMeminfo returned false")
	}

	if got.TotalBytes != 16384000*1024 {
		t.Fatalf("total = %d, want %d", got.TotalBytes, 16384000*1024)
	}
	if got.AvailableBytes != 12288000*1024 {
		t.Fatalf("available = %d, want %d", got.AvailableBytes, 12288000*1024)
	}
	if got.UsedBytes != (16384000-12288000)*1024 {
		t.Fatalf("used = %d, want %d", got.UsedBytes, (16384000-12288000)*1024)
	}
	if got.UsedPercent != 25 {
		t.Fatalf("usedPercent = %v, want 25", got.UsedPercent)
	}
	if got.FreeBytes != 2048000*1024 {
		t.Fatalf("free = %d, want %d", got.FreeBytes, 2048000*1024)
	}
	if got.SwapUsedBytes != 1024000*1024 {
		t.Fatalf("swap used = %d, want %d", got.SwapUsedBytes, 1024000*1024)
	}
}

func TestParseMeminfoFallsBackWithoutMemAvailable(t *testing.T) {
	text := `MemTotal: 1000 kB
MemFree: 100 kB
Buffers: 50 kB
Cached: 150 kB
`

	got, ok := parseMeminfo(text)
	if !ok {
		t.Fatal("parseMeminfo returned false")
	}
	if got.AvailableBytes != 300*1024 {
		t.Fatalf("available = %d, want %d", got.AvailableBytes, 300*1024)
	}
	if got.UsedBytes != 700*1024 {
		t.Fatalf("used = %d, want %d", got.UsedBytes, 700*1024)
	}
	if got.UsedPercent != 70 {
		t.Fatalf("usedPercent = %v, want 70", got.UsedPercent)
	}
}

func TestParseMeminfoRejectsEmpty(t *testing.T) {
	if _, ok := parseMeminfo(""); ok {
		t.Fatal("parseMeminfo accepted empty input")
	}
}

func TestParseProcStatCPU(t *testing.T) {
	total, idle, ok := parseProcStatCPU("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4 5\n")
	if !ok {
		t.Fatal("parseProcStatCPU returned false")
	}
	if total != 1000 {
		t.Fatalf("total = %d, want 1000", total)
	}
	if idle != 850 {
		t.Fatalf("idle = %d, want 850", idle)
	}
}

func TestParseProcStatCPURejectsInvalid(t *testing.T) {
	if _, _, ok := parseProcStatCPU("cpu0 1 2 3 4 5\n"); ok {
		t.Fatal("accepted input without aggregate cpu line")
	}
	if _, _, ok := parseProcStatCPU("cpu  1 x 3 4 5\n"); ok {
		t.Fatal("accepted non-numeric counters")
	}
}

func TestSystemCPUPercent(t *testing.T) {
	tests := []struct {
		name                string
		prevTotal, prevIdle uint64
		total, idle         uint64
		previous            bool
		want                float64
	}{
		{
			name:     "首次采样用启动以来累计值",
			total:    1000,
			idle:     850,
			previous: false,
			want:     15,
		},
		{
			name:      "后续采样用差值",
			prevTotal: 1000,
			prevIdle:  850,
			total:     2000,
			idle:      1750,
			previous:  true,
			want:      10,
		},
		{
			name:      "计数器没有前进返回 0",
			prevTotal: 1000,
			prevIdle:  850,
			total:     1000,
			idle:      850,
			previous:  true,
			want:      0,
		},
		{
			name:      "空闲时间倒退返回 0",
			prevTotal: 1000,
			prevIdle:  900,
			total:     2000,
			idle:      850,
			previous:  true,
			want:      0,
		},
		{
			name:     "首次采样空闲大于总量返回 0",
			total:    1000,
			idle:     1200,
			previous: false,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := systemCPUPercent(tt.prevTotal, tt.prevIdle, tt.total, tt.idle, tt.previous)
			if got != tt.want {
				t.Fatalf("systemCPUPercent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseLoadAvg(t *testing.T) {
	load1, load5, load15 := parseLoadAvg("0.52 0.58 0.59 1/123 4567\n")
	if load1 != 0.52 || load5 != 0.58 || load15 != 0.59 {
		t.Fatalf("load = (%v, %v, %v), want (0.52, 0.58, 0.59)", load1, load5, load15)
	}

	load1, load5, load15 = parseLoadAvg("")
	if load1 != 0 || load5 != 0 || load15 != 0 {
		t.Fatalf("empty load = (%v, %v, %v), want zeros", load1, load5, load15)
	}
}

func TestParseProcUptime(t *testing.T) {
	if got := parseProcUptime("12345.67 999.00\n"); got != 12345 {
		t.Fatalf("uptime = %d, want 12345", got)
	}
	if got := parseProcUptime(""); got != 0 {
		t.Fatalf("empty uptime = %d, want 0", got)
	}
}

func TestParseProcMounts(t *testing.T) {
	text := `/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sda1 /boot ext4 rw 0 0
/dev/sdb1 /data\040disk xfs rw 0 0
overlay /var/lib/docker/overlay2/abc overlay rw 0 0
`

	entries := parseProcMounts(text)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}

	if entries[0].Device != "/dev/sda1" || entries[0].Path != "/" || entries[0].FSType != "ext4" {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[1].Path != "/data disk" || entries[1].FSType != "xfs" {
		t.Fatalf("second entry = %+v", entries[1])
	}
	if entries[2].FSType != "overlay" {
		t.Fatalf("third entry = %+v", entries[2])
	}
}

func TestDecodeMountField(t *testing.T) {
	if got := decodeMountField(`/data\040disk`); got != "/data disk" {
		t.Fatalf("decoded = %q, want %q", got, "/data disk")
	}
	if got := decodeMountField(`/a\011b\012c\134d`); got != "/a\tb\nc\\d" {
		t.Fatalf("decoded = %q", got)
	}
}
