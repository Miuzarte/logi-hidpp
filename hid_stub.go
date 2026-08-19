//go:build !windows

package logihidpp

import "context"

func init() {
	openDevices = func(ctx context.Context, pid uint16) ([]device, error) {
		return nil, ErrUnsupported
	}
}

// Enumerate 在非 Windows 平台不可用
func Enumerate() ([]DeviceInfo, error) {
	return nil, ErrUnsupported
}
