package logihidpp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDevice struct {
	featureIndex uint8
	featureErr   error
	startErr     error

	reports chan []byte

	failOnce  sync.Once
	failCh    chan struct{}
	startOnce sync.Once
	started   chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func newFakeDevice(feature uint8) *fakeDevice {
	return &fakeDevice{
		featureIndex: feature,
		reports:      make(chan []byte, 8),
		failCh:       make(chan struct{}),
		started:      make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (f *fakeDevice) FeatureIndex(ctx context.Context) (uint8, error) {
	if f.featureErr != nil {
		return 0, f.featureErr
	}
	return f.featureIndex, nil
}

func (f *fakeDevice) StartSpy(ctx context.Context) error {
	f.startOnce.Do(func() { close(f.started) })
	return f.startErr
}

func (f *fakeDevice) ReadReport(ctx context.Context) ([]byte, error) {
	select {
	case r := <-f.reports:
		return r, nil
	case <-f.failCh:
		return nil, errors.New("fake read failure")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeDevice) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeDevice) Path() string {
	return "fake"
}

func (f *fakeDevice) fail() {
	f.failOnce.Do(func() { close(f.failCh) })
}

func fakeFactory(devs ...device) func(ctx context.Context, pid uint16) ([]device, error) {
	return func(ctx context.Context, pid uint16) ([]device, error) {
		return devs, nil
	}
}

func withFastRetries(t *testing.T) {
	t.Helper()
	oldRetry := retryDelay
	oldReconnect := reconnectDelay
	retryDelay = time.Millisecond
	reconnectDelay = time.Millisecond
	t.Cleanup(func() {
		retryDelay = oldRetry
		reconnectDelay = oldReconnect
	})
}

func withFactory(t *testing.T, f func(ctx context.Context, pid uint16) ([]device, error)) {
	t.Helper()
	old := openDevices
	openDevices = f
	t.Cleanup(func() { openDevices = old })
}

func waitStarted(t *testing.T, f *fakeDevice) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("device startSpy not observed")
	}
}

func waitClosed(t *testing.T, f *fakeDevice) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("device not closed")
	}
}

func TestMonitorStateAndEvents(t *testing.T) {
	withFastRetries(t)
	fd := newFakeDevice(0x05)
	withFactory(t, fakeFactory(fd))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	waitStarted(t, fd)

	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x10}
	select {
	case ev := <-m.Events():
		if ev.Bit != Bit4 || !ev.Pressed {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pressed event")
	}
	if got := m.State(); got != 0x0010 {
		t.Fatalf("state = %04X, want 0010", got)
	}

	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x01, 0x10}
	select {
	case ev := <-m.Events():
		if ev.Bit != Bit8 || !ev.Pressed {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bit8 pressed event")
	}
	if got := m.State(); got != 0x0110 {
		t.Fatalf("state = %04X, want 0110", got)
	}

	// 非事件报告应被忽略
	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x01, 0x00, 0x10}
	// 错误 feature index 也应被忽略
	fd.reports <- []byte{0x11, 0xFF, 0x06, 0x00, 0x00, 0x10}
	if got := m.State(); got != 0x0110 {
		t.Fatalf("state after ignored reports = %04X, want 0110", got)
	}

	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x00}
	select {
	case ev := <-m.Events():
		if ev.Bit != Bit4 || ev.Pressed {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no release event")
	}
	if got := m.State(); got != 0 {
		t.Fatalf("state = %04X, want 0", got)
	}
}

func TestMonitorIgnoresStartSpyError(t *testing.T) {
	withFastRetries(t)
	fd := newFakeDevice(0x05)
	fd.startErr = errors.New("spy unavailable")
	withFactory(t, fakeFactory(fd))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	waitStarted(t, fd)

	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x10}
	select {
	case <-m.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("no event despite startSpy error")
	}
}

func TestMonitorSelectsFirstFeatureDevice(t *testing.T) {
	withFastRetries(t)
	bad := newFakeDevice(0x00)
	bad.featureErr = ErrFeatureNotFound
	good := newFakeDevice(0x05)
	withFactory(t, fakeFactory(bad, good))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	waitStarted(t, good)
	waitClosed(t, bad)
}

func TestMonitorLearnsFeatureIndex(t *testing.T) {
	withFastRetries(t)
	fd := newFakeDevice(0)
	fd.featureErr = ErrHIDPPError
	withFactory(t, fakeFactory(fd))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()

	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x00}
	fd.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x10}
	select {
	case ev := <-m.Events():
		if ev.Bit != Bit4 || !ev.Pressed {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no learned pressed event")
	}
	if got := m.State(); got != 0x0010 {
		t.Fatalf("state = %04X, want 0010", got)
	}
}

func TestOpenWithPIDFiltersFactory(t *testing.T) {
	withFastRetries(t)
	gotPID := make(chan uint16, 1)
	withFactory(t, func(ctx context.Context, pid uint16) ([]device, error) {
		gotPID <- pid
		return nil, ErrNoDevice
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := OpenWithPID(ctx, 0xC547)
	if err == nil {
		t.Fatal("expected no-device error")
	}
	defer m.Close()
	if pid := <-gotPID; pid != 0xC547 {
		t.Fatalf("pid = %04X, want C547", pid)
	}
}

func TestMonitorReconnectsAndClearsState(t *testing.T) {
	withFastRetries(t)
	fd1 := newFakeDevice(0x05)
	fd2 := newFakeDevice(0x05)
	var calls atomic.Int32
	withFactory(t, func(ctx context.Context, pid uint16) ([]device, error) {
		if calls.Add(1) == 1 {
			return []device{fd1}, nil
		}
		return []device{fd2}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	waitStarted(t, fd1)

	fd1.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x10}
	select {
	case <-m.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("no event before disconnect")
	}
	if got := m.State(); got != 0x0010 {
		t.Fatalf("state = %04X, want 0010", got)
	}

	fd1.fail()
	waitClosed(t, fd1)
	waitStarted(t, fd2)
	if got := m.State(); got != 0 {
		t.Fatalf("state after disconnect = %04X, want 0", got)
	}

	fd2.reports <- []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x10}
	select {
	case <-m.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("no event after reconnect")
	}
	if got := m.State(); got != 0x0010 {
		t.Fatalf("state after reconnect = %04X, want 0010", got)
	}
}

func TestStartReturnsErrorAndRetries(t *testing.T) {
	withFastRetries(t)
	var calls atomic.Int32
	withFactory(t, func(ctx context.Context, pid uint16) ([]device, error) {
		calls.Add(1)
		return nil, ErrNoDevice
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err == nil {
		t.Fatal("expected error when no device")
	}
	defer m.Close()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("factory called %d times, want >= 2", calls.Load())
	}
}

// fakeNotifier 模拟设备插拔通知源 (平台无关, 测试用)
type fakeNotifier struct {
	changed chan struct{}
	closed  chan struct{}
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{changed: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (f *fakeNotifier) Changed() <-chan struct{} {
	return f.changed
}

func (f *fakeNotifier) Close() {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
}

func (f *fakeNotifier) fire() {
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// TestNotifyTriggersImmediateReconnect 验证: 收到设备插拔通知时立即重连,
// 不等 readIdleTimeout 静默超时
func TestNotifyTriggersImmediateReconnect(t *testing.T) {
	withFastRetries(t)
	// 拉长静默超时, 若没有通知机制, 重连只能靠超时 (测试会超时失败)
	oldIdle := readIdleTimeout
	readIdleTimeout = time.Minute
	t.Cleanup(func() { readIdleTimeout = oldIdle })

	fd1 := newFakeDevice(0x05)
	fd2 := newFakeDevice(0x05)
	var calls atomic.Int32
	withFactory(t, func(ctx context.Context, pid uint16) ([]device, error) {
		if calls.Add(1) == 1 {
			return []device{fd1}, nil
		}
		return []device{fd2}, nil
	})

	notif := newFakeNotifier()
	oldStart := startNotifier
	startNotifier = func() (deviceNotifier, error) { return notif, nil }
	t.Cleanup(func() { startNotifier = oldStart })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	waitStarted(t, fd1)

	// 触发设备插拔事件
	notif.fire()

	// 立即重连: fd1 被关闭, fd2 被打开
	waitClosed(t, fd1)
	waitStarted(t, fd2)
}
