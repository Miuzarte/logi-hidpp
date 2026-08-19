package logihidpp

import (
	"errors"
	"fmt"
)

// HID++ 2.0 协议常量
const (
	reportIDShort     = 0x10
	reportIDLong      = 0x11
	featureIRoot      = 0x0000
	featureButtonSpy  = 0x8110
	swid              = 0x0A
	funcGetFeature    = 0x00
	funcStartSpy      = 0x01
	funcStopSpy       = 0x02
	eventButton       = 0x00
	errFeatureIndex   = 0x8F
	hidpStatusSuccess = 0x00110000
)

var (
	ErrShortReport     = errors.New("logihidpp: report too short")
	ErrBadReportID     = errors.New("logihidpp: unexpected report ID")
	ErrHIDPPError      = errors.New("logihidpp: HID++ returned error feature index")
	ErrFeatureNotFound = errors.New("logihidpp: feature 0x8110 not found")
)

// buildGetFeatureRequest 构造 IRoot.getFeature(0x8110) 短报文
func buildGetFeatureRequest() []byte {
	return []byte{
		reportIDShort,
		0xFF, // 有线直连设备号; 接收器场景由固件回显, 不影响请求
		featureIRoot,
		funcGetFeature<<4 | swid,
		featureButtonSpy >> 8,
		featureButtonSpy & 0xFF,
		0,
	}
}

// buildStartSpyRequest 构造 0x8110.startSpy() 短报文
func buildStartSpyRequest(featureIndex uint8) []byte {
	return []byte{
		reportIDShort,
		0xFF,
		featureIndex,
		funcStartSpy<<4 | swid,
		0, 0, 0,
	}
}

// ParseFeatureIndex 解析 getFeature 应答, 返回动态 feature index
func ParseFeatureIndex(reply []byte) (uint8, error) {
	if len(reply) < 7 {
		return 0, fmt.Errorf("%w: got %d bytes", ErrShortReport, len(reply))
	}
	if reply[0] != reportIDShort {
		return 0, fmt.Errorf("%w: 0x%02X", ErrBadReportID, reply[0])
	}
	if reply[2] == errFeatureIndex {
		return 0, ErrHIDPPError
	}
	idx := reply[4]
	if idx == 0 {
		return 0, ErrFeatureNotFound
	}
	return idx, nil
}

// ParseFeatureIndexLong 解析从 long 报告集合收到的 getFeature 应答,
// 部分接收器 (如 G502X Lightspeed) 通过 long 报告返回短命令的应答
func ParseFeatureIndexLong(reply []byte) (uint8, error) {
	if len(reply) < 5 {
		return 0, fmt.Errorf("%w: got %d bytes", ErrShortReport, len(reply))
	}
	if reply[0] != reportIDLong {
		return 0, fmt.Errorf("%w: 0x%02X", ErrBadReportID, reply[0])
	}
	if reply[2] == errFeatureIndex {
		return 0, ErrHIDPPError
	}
	if reply[2] != featureIRoot || reply[3] != funcGetFeature<<4|swid {
		return 0, fmt.Errorf("logihidpp: unexpected getFeature reply: % X", reply)
	}
	idx := reply[4]
	if idx == 0 {
		return 0, ErrFeatureNotFound
	}
	return idx, nil
}

// ParseStartSpyReply 校验 startSpy 应答
func ParseStartSpyReply(reply []byte, featureIndex uint8) error {
	if len(reply) < 7 {
		return fmt.Errorf("%w: got %d bytes", ErrShortReport, len(reply))
	}
	if reply[0] != reportIDShort {
		return fmt.Errorf("%w: 0x%02X", ErrBadReportID, reply[0])
	}
	if reply[2] == errFeatureIndex {
		return ErrHIDPPError
	}
	if reply[2] != featureIndex || reply[3] != funcStartSpy<<4|swid {
		return fmt.Errorf("logihidpp: unexpected startSpy reply: % X", reply)
	}
	return nil
}

// ParseStartSpyReplyLong 校验从 long 报告集合收到的 startSpy 应答
func ParseStartSpyReplyLong(reply []byte, featureIndex uint8) error {
	if len(reply) < 5 {
		return fmt.Errorf("%w: got %d bytes", ErrShortReport, len(reply))
	}
	if reply[0] != reportIDLong {
		return fmt.Errorf("%w: 0x%02X", ErrBadReportID, reply[0])
	}
	if reply[2] == errFeatureIndex {
		return ErrHIDPPError
	}
	if reply[2] != featureIndex || reply[3] != funcStartSpy<<4|swid {
		return fmt.Errorf("logihidpp: unexpected startSpy reply: % X", reply)
	}
	return nil
}

// ParseReport 解析 20 字节长报告; 属于 0x8110 button event 时返回当前位图
func ParseReport(report []byte, featureIndex uint8) (state uint16, ok bool) {
	if len(report) < 6 {
		return 0, false
	}
	if report[0] != reportIDLong || report[2] != featureIndex || report[3] != eventButton {
		return 0, false
	}
	return uint16(report[4])<<8 | uint16(report[5]), true
}

// sniffButtonReport 从报告里提取 0x8110 按键事件候选,
// 用于 getFeature 不可用时自动学习 feature index
func sniffButtonReport(report []byte) (featureIndex uint8, state uint16, ok bool) {
	if len(report) < 6 || report[0] != reportIDLong || report[3] != eventButton {
		return 0, 0, false
	}
	return report[2], uint16(report[4])<<8 | uint16(report[5]), true
}

// DiffState 计算两次位图之间的按键边沿事件
func DiffState(prev, curr uint16) []Event {
	diff := prev ^ curr
	if diff == 0 {
		return nil
	}
	events := make([]Event, 0, 4)
	for i := 0; i < 16; i++ {
		bit := uint16(1 << i)
		if diff&bit != 0 {
			events = append(events, Event{
				Bit:     Bit(i),
				Pressed: curr&bit != 0,
			})
		}
	}
	return events
}
