//go:build !windows

package logihidpp

// startDeviceNotifier 非 Windows 平台返回 (nil, nil), 表示无设备变化通知
// (Monitor 退化为静默超时看门狗)
func startDeviceNotifier() (deviceNotifier, error) {
	return nil, nil
}
