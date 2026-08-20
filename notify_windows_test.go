//go:build windows

package logihidpp

import (
	"context"
	"testing"
	"time"
)

// TestStartDeviceNotifierSmoke 验证 Windows 真实通知源能创建窗口并注册通知
func TestStartDeviceNotifierSmoke(t *testing.T) {
	withFastRetries(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err == nil {
		defer m.Close()
	}
	// 无鼠标时 Start 也返回错误, 但通知源应已成功创建 (m.notify != nil)
	if m == nil || m.notify == nil {
		t.Fatalf("device notifier not started (start err: %v, notify: %v)", err, m.notify)
	}
	time.Sleep(50 * time.Millisecond) // 给窗口线程一点启动时间
}
