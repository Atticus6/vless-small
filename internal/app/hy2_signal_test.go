package app

import (
	"testing"

	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/hy2"
	"github.com/atticus6/vless-small/internal/tunnel"
)

// fakeHy2Controller 记录 Sync 收到的信号，不真正绑端口。
type fakeHy2Controller struct {
	signals []hy2.Signal
	status  hy2.Report
	closed  bool
}

func (f *fakeHy2Controller) Sync(signal hy2.Signal) {
	f.signals = append(f.signals, signal)
}

func (f *fakeHy2Controller) Status() hy2.Report { return f.status }

func (f *fakeHy2Controller) Close() { f.closed = true }

// noopTunnel / noopCert 只满足接口；本组测试不关心隧道与证书。
type noopTunnel struct{}

func (noopTunnel) Sync(tunnel.Signal)    {}
func (noopTunnel) Status() tunnel.Report { return tunnel.Report{} }
func (noopTunnel) Close()                {}

type noopCert struct{}

func (noopCert) Sync(string)                   {}
func (noopCert) Status() cert.Report           { return cert.Report{} }
func (noopCert) SetReporter(func(cert.Report)) {}
func (noopCert) Close()                        {}

// TestApplyNodeSignalsHy2LocalOnly 验证只有 runtime=local 才把 hy2 信号
// 下发给控制器。
//
// 托管容器/PaaS 的 UDP 端口通常不可公网直达，节点侧也会跳过；主控不该
// 让这类节点反复探测。
func TestApplyNodeSignalsHy2LocalOnly(t *testing.T) {
	cases := []struct {
		runtime   string
		wantCalls int
	}{
		{"local", 1},
		{"railway", 0},
		{"northflank", 0},
		{"docker", 0},
		{"unknown", 0},
	}

	for _, tc := range cases {
		ctl := &fakeHy2Controller{}
		signal := &hy2.Signal{Enabled: true, Retry: 3}

		applyNodeSignals(
			&nodeSyncResult{Hy2: signal},
			NewServer(nil, ""),
			noopTunnel{}, noopCert{}, ctl,
			tc.runtime,
		)

		if len(ctl.signals) != tc.wantCalls {
			t.Errorf("runtime %q: hy2 sync calls = %d, want %d",
				tc.runtime, len(ctl.signals), tc.wantCalls)
		}
		if tc.wantCalls == 1 && ctl.signals[0] != *signal {
			t.Errorf("runtime %q: signal = %+v, want %+v",
				tc.runtime, ctl.signals[0], *signal)
		}
	}
}

// TestApplyNodeSignalsNoHy2Controller 验证未配置 hy2 入口时不会 panic。
func TestApplyNodeSignalsNoHy2Controller(t *testing.T) {
	// 节点没设置 HY2_LISTEN 时 hy2Ctl 为 nil，主控仍可能下发信号。
	applyNodeSignals(
		&nodeSyncResult{Hy2: &hy2.Signal{Enabled: true}},
		NewServer(nil, ""),
		noopTunnel{}, noopCert{}, nil,
		"local",
	)
}

// TestIsLocalRuntime 验证运行时判断忽略大小写与空白。
func TestIsLocalRuntime(t *testing.T) {
	for _, value := range []string{"local", "LOCAL", " Local ", "local\n"} {
		if !isLocalRuntime(value) {
			t.Errorf("isLocalRuntime(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "railway", "docker", "localhost", "local2"} {
		if isLocalRuntime(value) {
			t.Errorf("isLocalRuntime(%q) = true, want false", value)
		}
	}
}
