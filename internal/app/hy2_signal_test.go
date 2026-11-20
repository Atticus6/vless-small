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

// TestApplyNodeSignalsPassesHy2 验证主控下发的 hy2 信号会原样透传给控制器。
//
// runtime 门控在 controller 内部做（首次 Sync 与后续信号走同一条路径），
// app 层不再过滤——这样非 local 节点一旦启动过入口，主控的关闭信号也一定
// 能到达控制器。
func TestApplyNodeSignalsPassesHy2(t *testing.T) {
	signals := []hy2.Signal{
		{Enabled: true, Retry: 3},
		{Enabled: false},
		{Enabled: true, Retry: 4},
	}

	for _, signal := range signals {
		ctl := &fakeHy2Controller{}

		applyNodeSignals(
			&nodeSyncResult{Hy2: &signal},
			NewServer(nil, ""),
			noopTunnel{}, noopCert{}, ctl,
		)

		if len(ctl.signals) != 1 {
			t.Fatalf("hy2 sync calls = %d, want 1", len(ctl.signals))
		}
		if ctl.signals[0] != signal {
			t.Errorf("signal = %+v, want %+v", ctl.signals[0], signal)
		}
	}
}

// TestApplyNodeSignalsNoHy2 验证没有信号时不调用控制器。
func TestApplyNodeSignalsNoHy2(t *testing.T) {
	ctl := &fakeHy2Controller{}

	applyNodeSignals(
		&nodeSyncResult{},
		NewServer(nil, ""),
		noopTunnel{}, noopCert{}, ctl,
	)

	if len(ctl.signals) != 0 {
		t.Fatalf("hy2 sync calls = %d, want 0", len(ctl.signals))
	}
}

// TestApplyNodeSignalsNoHy2Controller 验证未配置 hy2 入口时不会 panic。
func TestApplyNodeSignalsNoHy2Controller(t *testing.T) {
	// 节点没设置 HY2_LISTEN 时 hy2Ctl 为 nil，主控仍可能下发信号。
	applyNodeSignals(
		&nodeSyncResult{Hy2: &hy2.Signal{Enabled: true}},
		NewServer(nil, ""),
		noopTunnel{}, noopCert{}, nil,
	)
}
