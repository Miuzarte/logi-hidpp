//go:build windows

package logihidpp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	logitechVID     = 0x046D
	usagePageVendor = 0xFF00
	usageShort      = 0x0001
	usageLong       = 0x0002
)

var hidInterfaceGUID = windows.GUID{
	Data1: 0x4d1e55b2,
	Data2: 0xf16f,
	Data3: 0x11cf,
	Data4: [8]byte{0x88, 0xcb, 0x00, 0x11, 0x11, 0x00, 0x00, 0x30},
}

var (
	setupapiDLL                          = windows.NewLazySystemDLL("setupapi.dll")
	procSetupDiEnumDeviceInterfaces      = setupapiDLL.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = setupapiDLL.NewProc("SetupDiGetDeviceInterfaceDetailW")

	hidDLL                    = windows.NewLazySystemDLL("hid.dll")
	procHidDGetAttributes     = hidDLL.NewProc("HidD_GetAttributes")
	procHidDGetPreparsedData  = hidDLL.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData = hidDLL.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps           = hidDLL.NewProc("HidP_GetCaps")
)

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid windows.GUID
	flags              uint32
	reserved           uintptr
}

type spDeviceInterfaceDetailDataW struct {
	cbSize     uint32
	devicePath [1]uint16
}

type candidate struct {
	path      string
	vid, pid  uint16
	usagePage uint16
	usage     uint16
	inputLen  uint16
}

func init() {
	openDevices = openWindowsDevices
}

// Enumerate 只读枚举 Logitech vendor HID 设备, 不发送任何 HID++ 报文
func Enumerate() ([]DeviceInfo, error) {
	cands, err := enumerateCandidates(0)
	if err != nil {
		return nil, err
	}
	infos := make([]DeviceInfo, 0, len(cands))
	for _, c := range cands {
		infos = append(infos, DeviceInfo{
			Path:                  c.path,
			VID:                   c.vid,
			PID:                   c.pid,
			UsagePage:             c.usagePage,
			Usage:                 c.usage,
			InputReportByteLength: c.inputLen,
		})
	}
	return infos, nil
}

func openWindowsDevices(ctx context.Context, pid uint16) ([]device, error) {
	cands, err := enumerateCandidates(pid)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, ErrNoDevice
	}
	var devs []device
	for _, g := range groupCandidates(cands) {
		d, err := openGroup(g.cands)
		if err == nil {
			devs = append(devs, d)
		}
	}
	if len(devs) == 0 {
		return nil, ErrNoDevice
	}
	return devs, nil
}

func enumerateCandidates(pidFilter uint16) ([]candidate, error) {
	devInfo, err := windows.SetupDiGetClassDevsEx(
		&hidInterfaceGUID, "", 0,
		windows.DIGCF_PRESENT|windows.DIGCF_DEVICEINTERFACE,
		0, "",
	)
	if err != nil {
		return nil, fmt.Errorf("logihidpp: SetupDiGetClassDevsEx: %w", err)
	}
	defer windows.SetupDiDestroyDeviceInfoList(devInfo)

	var out []candidate
	for i := 0; ; i++ {
		var did spDeviceInterfaceData
		did.cbSize = uint32(unsafe.Sizeof(did))
		r1, _, e1 := procSetupDiEnumDeviceInterfaces.Call(
			uintptr(devInfo),
			0,
			uintptr(unsafe.Pointer(&hidInterfaceGUID)),
			uintptr(i),
			uintptr(unsafe.Pointer(&did)),
		)
		if r1 == 0 {
			if errors.Is(e1, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			return nil, fmt.Errorf("logihidpp: SetupDiEnumDeviceInterfaces: %w", e1)
		}

		var required uint32
		r1, _, e1 = procSetupDiGetDeviceInterfaceDetailW.Call(
			uintptr(devInfo),
			uintptr(unsafe.Pointer(&did)),
			0, 0,
			uintptr(unsafe.Pointer(&required)),
			0,
		)
		if r1 == 0 && !errors.Is(e1, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, fmt.Errorf("logihidpp: SetupDiGetDeviceInterfaceDetailW(size): %w", e1)
		}

		buf := make([]byte, required)
		detail := (*spDeviceInterfaceDetailDataW)(unsafe.Pointer(&buf[0]))
		detail.cbSize = interfaceDetailCBSize()
		r1, _, e1 = procSetupDiGetDeviceInterfaceDetailW.Call(
			uintptr(devInfo),
			uintptr(unsafe.Pointer(&did)),
			uintptr(unsafe.Pointer(detail)),
			uintptr(required),
			uintptr(unsafe.Pointer(&required)),
			0,
		)
		if r1 == 0 {
			continue // 单个接口失败不中断整段枚举
		}
		path := windows.UTF16PtrToString(&detail.devicePath[0])
		if path == "" {
			continue
		}

		c, err := queryCandidate(path)
		if err != nil {
			continue
		}
		if c.vid != logitechVID || c.usagePage != usagePageVendor {
			continue
		}
		if c.usage != usageShort && c.usage != usageLong {
			continue
		}
		if pidFilter != 0 && c.pid != pidFilter {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func interfaceDetailCBSize() uint32 {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		return 8 // x64: DevicePath 从偏移 8 开始
	}
	return 6 // x86
}

func queryCandidate(path string) (candidate, error) {
	h, err := openRawHandle(path)
	if err != nil {
		return candidate{}, err
	}
	defer windows.CloseHandle(h)

	attrs, err := getHidAttributes(h)
	if err != nil {
		return candidate{}, err
	}
	preparsed, err := hidPreparsed(h)
	if err != nil {
		return candidate{}, err
	}
	defer procHidDFreePreparsedData.Call(preparsed)

	caps, err := hidCaps(preparsed)
	if err != nil {
		return candidate{}, err
	}
	return candidate{
		path:      path,
		vid:       attrs.vendorID,
		pid:       attrs.productID,
		usagePage: caps.usagePage,
		usage:     caps.usage,
		inputLen:  caps.inputReportByteLength,
	}, nil
}

func openRawHandle(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
}

type hidDeviceAttributes struct {
	size          uint32
	vendorID      uint16
	productID     uint16
	versionNumber uint16
}

func getHidAttributes(h windows.Handle) (hidDeviceAttributes, error) {
	var a hidDeviceAttributes
	a.size = uint32(unsafe.Sizeof(a))
	r1, _, e1 := procHidDGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&a)))
	if r1 == 0 {
		return a, fmt.Errorf("logihidpp: HidD_GetAttributes: %w", e1)
	}
	return a, nil
}

func hidPreparsed(h windows.Handle) (uintptr, error) {
	var p uintptr
	r1, _, e1 := procHidDGetPreparsedData.Call(uintptr(h), uintptr(unsafe.Pointer(&p)))
	if r1 == 0 {
		return 0, fmt.Errorf("logihidpp: HidD_GetPreparsedData: %w", e1)
	}
	return p, nil
}

type hidpCaps struct {
	usage                     uint16
	usagePage                 uint16
	inputReportByteLength     uint16
	outputReportByteLength    uint16
	featureReportByteLength   uint16
	reserved                  [17]uint16
	numberLinkCollectionNodes uint16
	numberInputButtonCaps     uint16
	numberInputValueCaps      uint16
	numberInputDataIndices    uint16
	numberOutputButtonCaps    uint16
	numberOutputValueCaps     uint16
	numberOutputDataIndices   uint16
	numberFeatureButtonCaps   uint16
	numberFeatureValueCaps    uint16
	numberFeatureDataIndices  uint16
}

func hidCaps(preparsed uintptr) (hidpCaps, error) {
	var c hidpCaps
	r1, _, e1 := procHidPGetCaps.Call(preparsed, uintptr(unsafe.Pointer(&c)))
	if r1 != hidpStatusSuccess {
		return c, fmt.Errorf("logihidpp: HidP_GetCaps status=0x%08X: %w", r1, e1)
	}
	return c, nil
}

type deviceGroup struct {
	key   string
	cands []candidate
}

func groupCandidates(cands []candidate) []deviceGroup {
	var groups []deviceGroup
	index := make(map[string]int)
	for _, c := range cands {
		key := deviceKey(c.path)
		if i, ok := index[key]; ok {
			groups[i].cands = append(groups[i].cands, c)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, deviceGroup{key: key, cands: []candidate{c}})
	}
	return groups
}

func deviceKey(path string) string {
	p := strings.ToLower(path)
	// 去掉集合序号段 (&colXX), 保留实例 ID
	if before, rest, found := strings.CutLast(p, "&col"); found {
		if j := strings.Index(rest, "#"); j >= 0 {
			p = before + rest[j:]
		}
	}
	// 同一物理设备的多个集合实例 ID 尾段不同 (&0000/&0001),
	// 去掉实例 ID 最后一个 & 段, 只保留设备实例主体
	if i := strings.Index(p, "#"); i >= 0 {
		if before, _, found := strings.CutLast(p[i+1:], "&"); found {
			p = p[:i+1] + before
		}
	}
	return p
}

func openGroup(cands []candidate) (*hidDevice, error) {
	var short, long *candidate
	for i := range cands {
		c := &cands[i]
		switch c.usage {
		case usageShort:
			if short == nil {
				short = c
			}
		case usageLong:
			if long == nil {
				long = c
			}
		}
	}
	if short == nil && long == nil {
		return nil, errors.New("logihidpp: device has no vendor HID collection")
	}
	// 单个集合的设备 (部分接收器固件) 由同一句柄承担短/长报文
	if short == nil {
		short = long
	}
	if long == nil {
		long = short
	}

	sh, err := openHidHandle(short.path, short.inputLen)
	if err != nil {
		return nil, err
	}
	lh, err := openHidHandle(long.path, long.inputLen)
	if err != nil {
		sh.Close()
		return nil, err
	}
	return &hidDevice{short: sh, long: lh, path: deviceKey(short.path)}, nil
}

type hidHandle struct {
	h         windows.Handle
	reportLen int
	ev        windows.Handle
}

func openHidHandle(path string, reportLen uint16) (*hidHandle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("logihidpp: CreateFile(%s): %w", path, err)
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("logihidpp: CreateEvent: %w", err)
	}
	n := int(reportLen)
	if n < 7 {
		n = 7
	}
	return &hidHandle{h: h, reportLen: n, ev: ev}, nil
}

func (c *hidHandle) Close() error {
	if c.ev != 0 {
		windows.CloseHandle(c.ev)
		c.ev = 0
	}
	if c.h != 0 {
		windows.CloseHandle(c.h)
		c.h = 0
	}
	return nil
}

func (c *hidHandle) read(ctx context.Context) ([]byte, error) {
	buf := make([]byte, c.reportLen)
	var ov windows.Overlapped
	ov.HEvent = c.ev
	if err := windows.ResetEvent(c.ev); err != nil {
		return nil, err
	}
	err := windows.ReadFile(c.h, buf, nil, &ov)
	if err == windows.ERROR_IO_PENDING {
		if err := waitIO(ctx, c.h, &ov); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var n uint32
	if err := windows.GetOverlappedResult(c.h, &ov, &n, false); err != nil {
		return nil, err
	}
	if int(n) < len(buf) {
		buf = buf[:n]
	}
	return buf, nil
}

func (c *hidHandle) write(ctx context.Context, buf []byte) error {
	var ov windows.Overlapped
	ov.HEvent = c.ev
	if err := windows.ResetEvent(c.ev); err != nil {
		return err
	}
	err := windows.WriteFile(c.h, buf, nil, &ov)
	if err == windows.ERROR_IO_PENDING {
		return waitIO(ctx, c.h, &ov)
	}
	return err
}

func waitIO(ctx context.Context, h windows.Handle, ov *windows.Overlapped) error {
	done := make(chan error, 1)
	go func() {
		var n uint32
		done <- windows.GetOverlappedResult(h, ov, &n, true)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = windows.CancelIoEx(h, ov)
		<-done
		return ctx.Err()
	}
}

type hidDevice struct {
	short   *hidHandle
	long    *hidHandle
	featIdx uint8
	path    string
}

func (d *hidDevice) FeatureIndex(ctx context.Context) (uint8, error) {
	// 直连或部分接收器: 设备号 0xFF, 应答走 short 报告
	if err := d.short.write(ctx, buildGetFeatureRequest()); err == nil {
		rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		reply, rerr := d.short.read(rctx)
		cancel()
		if rerr == nil {
			if idx, perr := ParseFeatureIndex(reply); perr == nil {
				d.featIdx = idx
				Logf("logihidpp: getFeature via short (device 0xFF), index %d", idx)
				return idx, nil
			}
		}
	}

	// Lightspeed 接收器: 设备号 0x01, 应答走 long 报告
	req := buildGetFeatureRequest()
	req[1] = 0x01
	if err := d.short.write(ctx, req); err != nil {
		return 0, err
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for i := 0; i < 8; i++ {
		reply, rerr := d.long.read(rctx)
		if rerr != nil {
			return 0, rerr
		}
		if idx, perr := ParseFeatureIndexLong(reply); perr == nil {
			d.featIdx = idx
			Logf("logihidpp: getFeature via long (device 0x01), index %d", idx)
			return idx, nil
		}
		// 队列里先到的可能是既有按键事件, 忽略并继续等应答
	}
	return 0, errors.New("logihidpp: getFeature timeout")
}

func (d *hidDevice) StartSpy(ctx context.Context) error {
	// 直连或部分接收器: 设备号 0xFF, 应答走 short 报告
	if err := d.short.write(ctx, buildStartSpyRequest(d.featIdx)); err == nil {
		rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		reply, rerr := d.short.read(rctx)
		cancel()
		if rerr == nil && ParseStartSpyReply(reply, d.featIdx) == nil {
			Logf("logihidpp: startSpy ok via short (device 0xFF)")
			return nil
		}
	}

	// Lightspeed 接收器: 设备号 0x01, 应答走 long 报告
	req := buildStartSpyRequest(d.featIdx)
	req[1] = 0x01
	if err := d.short.write(ctx, req); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for i := 0; i < 8; i++ {
		reply, rerr := d.long.read(rctx)
		if rerr != nil {
			return rerr
		}
		if ParseStartSpyReplyLong(reply, d.featIdx) == nil {
			Logf("logihidpp: startSpy ok via long (device 0x01)")
			return nil
		}
	}
	return errors.New("logihidpp: startSpy timeout")
}

// FeatureList 通过 IRoot.getFeatureList (0x0000 func 0x02) 枚举设备全部 feature
// 用于排查 0x8110 通知失效: 确认设备实际支持的按键 feature (如 0x1B04 ReprogControls)
func (d *hidDevice) FeatureList(ctx context.Context) ([]FeatureInfo, error) {
	var out []FeatureInfo
	for start := 0; ; start += 4 {
		req := []byte{
			reportIDShort,
			0x01, // Lightspeed 接收器设备号
			featureIRoot,
			funcGetFeatureList<<4 | swid,
			byte(start),
			0, 0,
		}
		if err := d.short.write(ctx, req); err != nil {
			return nil, err
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		reply, rerr := d.long.read(rctx)
		cancel()
		if rerr != nil {
			return nil, rerr
		}
		if len(reply) < 6 || reply[0] != reportIDLong {
			return nil, fmt.Errorf("logihidpp: bad feature list reply: % X", reply)
		}
		entries := 0
		for i := 4; i+2 < len(reply); i += 3 {
			idx := reply[i]
			if idx == 0 {
				return out, nil // 列表结束标记
			}
			out = append(out, FeatureInfo{
				Index: idx,
				ID:    uint16(reply[i+1])<<8 | uint16(reply[i+2]),
			})
			entries++
		}
		if entries < 4 {
			return out, nil
		}
	}
}

func (d *hidDevice) ReadReport(ctx context.Context) ([]byte, error) {
	return d.long.read(ctx)
}

func (d *hidDevice) Path() string {
	return d.path
}

func (d *hidDevice) Close() error {
	if d.short != nil {
		d.short.Close()
	}
	if d.long != nil && d.long != d.short {
		d.long.Close()
	}
	return nil
}
