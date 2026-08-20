//go:build windows

package logihidpp

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// deviceNotifier 监听 HID 设备接口插拔事件 (WM_DEVICECHANGE),
// 用于接收器重枚举/鼠标休眠唤醒时立即触发重连, 不等 10s 静默看门狗
// (接口定义见 monitor.go, 平台无关)

// winDeviceNotifier 是 Windows 实现: 隐藏窗口 + RegisterDeviceNotification
type winDeviceNotifier struct {
	changed chan struct{}
	hwnd    uintptr
	notif   uintptr
	done    chan struct{}

	closeOnce sync.Once
}

const (
	wmDeviceChange = 0x0219
	wmDestroy      = 0x0002
	wmClose        = 0x0010

	dbtDeviceArrival         = 0x8000
	dbtDeviceRemoveComplete  = 0x8004
	dbtDevtypDeviceInterface = 0x0005

	deviceNotifyWindowHandle = 0x00000000
)

// devBroadcastDeviceInterfaceNameOffset 是 DEV_BROADCAST_DEVICEINTERFACE_W
// 中 dbcc_name 相对结构头的偏移: size(4) + type(4) + reserved(4) + guid(16)
const devBroadcastDeviceInterfaceNameOffset = 28

var (
	user32DLL                        = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW             = user32DLL.NewProc("RegisterClassExW")
	procCreateWindowExW              = user32DLL.NewProc("CreateWindowExW")
	procRegisterDeviceNotificationW  = user32DLL.NewProc("RegisterDeviceNotificationW")
	procUnregisterDeviceNotification = user32DLL.NewProc("UnregisterDeviceNotification")
	procDefWindowProcW               = user32DLL.NewProc("DefWindowProcW")
	procGetMessageW                  = user32DLL.NewProc("GetMessageW")
	procTranslateMessage             = user32DLL.NewProc("TranslateMessage")
	procDispatchMessageW             = user32DLL.NewProc("DispatchMessageW")
	procPostMessageW                 = user32DLL.NewProc("PostMessageW")
	procPostQuitMessage              = user32DLL.NewProc("PostQuitMessage")

	kernel32DLL          = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandleW = kernel32DLL.NewProc("GetModuleHandleW")
)

var (
	notifierClassSeq atomic.Uint32
	// notifierByHwnd 在回调线程 (窗口线程) 读写, 单线程访问无需锁
	notifierByHwnd = map[uintptr]*winDeviceNotifier{}
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type devBroadcastHdr struct {
	dbccSize       uint32
	dbccDeviceType uint32
}

type devBroadcastDeviceInterfaceW struct {
	dbccSize       uint32
	dbccDeviceType uint32
	dbccReserved   uint32
	dbccClassGuid  windows.GUID
	dbccName       [1]uint16
}

// startDeviceNotifier 创建隐藏窗口并注册 HID 接口级设备通知,
// 非 Windows 平台在 notify_other.go 中返回 (nil, nil)
func startDeviceNotifier() (deviceNotifier, error) {
	n := &winDeviceNotifier{
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	ready := make(chan error, 1)
	go n.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return n, nil
}

// run 在独立线程跑消息循环 (窗口必须由创建它的线程处理消息)
func (n *winDeviceNotifier) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	className := fmt.Sprintf("logihidpp_notify_%d", notifierClassSeq.Add(1))
	classNamePtr, err := windows.UTF16PtrFromString(className)
	if err != nil {
		ready <- err
		return
	}
	hInstance, _, _ := procGetModuleHandleW.Call(0)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInstance,
		lpszClassName: classNamePtr,
	}
	r1, _, e1 := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if r1 == 0 {
		ready <- fmt.Errorf("logihidpp: RegisterClassExW(%s): %v", className, e1)
		return
	}

	hwnd, _, e1 := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(classNamePtr)),
		0, // WS_POPUP, 不可见
		0, 0, 0, 0,
		0, 0, hInstance,
		0, // 不传 lpParam, notifier 通过 map 查找
	)
	if hwnd == 0 {
		ready <- fmt.Errorf("logihidpp: CreateWindowExW(%s): %v", className, e1)
		return
	}
	n.hwnd = hwnd
	notifierByHwnd[hwnd] = n

	notif := devBroadcastDeviceInterfaceW{
		dbccSize:       uint32(unsafe.Sizeof(devBroadcastDeviceInterfaceW{})),
		dbccDeviceType: dbtDevtypDeviceInterface,
		dbccClassGuid:  hidInterfaceGUID,
	}
	r1, _, e1 = procRegisterDeviceNotificationW.Call(
		hwnd,
		uintptr(unsafe.Pointer(&notif)),
		deviceNotifyWindowHandle,
	)
	if r1 == 0 {
		// 非致命: 通知不可用时退化为静默超时看门狗
		Logf("logihidpp: RegisterDeviceNotificationW failed: %v", e1)
	} else {
		n.notif = r1
	}

	ready <- nil
	for {
		var m msg
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0=WM_QUIT, -1=错误
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	close(n.done)
}

// Changed 返回设备插拔事件通道 (缓冲 1, 未消费的事件合并)
func (n *winDeviceNotifier) Changed() <-chan struct{} {
	return n.changed
}

// Close 注销通知并停止消息循环线程
func (n *winDeviceNotifier) Close() {
	n.closeOnce.Do(func() {
		if n.notif != 0 {
			procUnregisterDeviceNotification.Call(n.notif)
			n.notif = 0
		}
		if n.hwnd != 0 {
			procPostMessageW.Call(n.hwnd, wmClose, 0, 0)
			n.hwnd = 0
		}
		<-n.done
	})
}

// wndProc 处理 WM_DEVICECHANGE, 只关心 Logitech HID 接口的到达/移除
// (VID_046D 过滤, 避免其他 HID 设备插拔引发无谓重连)
// lParam 声明为 unsafe.Pointer 以贴合 Windows 消息参数, 避免 vet unsafeptr 报警
func wndProc(hwnd uintptr, msg uint32, wParam uintptr, lParam unsafe.Pointer) uintptr {
	n := notifierByHwnd[hwnd]
	switch msg {
	case wmDeviceChange:
		if n != nil && (wParam == dbtDeviceArrival || wParam == dbtDeviceRemoveComplete) && lParam != nil {
			hdr := (*devBroadcastHdr)(lParam)
			if hdr.dbccDeviceType == dbtDevtypDeviceInterface {
				name := deviceInterfaceName(lParam)
				if strings.Contains(name, "VID_046D") {
					select {
					case n.changed <- struct{}{}:
					default: // 已有未消费事件, 合并
					}
				}
			}
		}
		return 0
	case wmDestroy:
		delete(notifierByHwnd, hwnd)
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, uintptr(lParam))
	return r
}

// deviceInterfaceName 读取 DEV_BROADCAST_DEVICEINTERFACE_W 的 dbcc_name (UTF-16)
func deviceInterfaceName(lParam unsafe.Pointer) string {
	base := unsafe.Add(lParam, devBroadcastDeviceInterfaceNameOffset)
	const max = 256 // 设备接口路径远短于此
	for i := 0; i < max; i++ {
		if *(*uint16)(unsafe.Add(base, uintptr(i)*2)) == 0 {
			return windows.UTF16ToString(unsafe.Slice((*uint16)(base), i))
		}
	}
	return windows.UTF16ToString(unsafe.Slice((*uint16)(base), max))
}
