package logihidpp

// DeviceInfo 是 -list 使用的只读枚举结果
type DeviceInfo struct {
	Path                  string
	VID, PID              uint16
	UsagePage, Usage      uint16
	InputReportByteLength uint16
}
