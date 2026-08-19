package logihidpp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Bit 是 0x8110 位图里的原始 bit 编号 (0..15)
type Bit uint8

const (
	Bit0  Bit = 0
	Bit1  Bit = 1
	Bit2  Bit = 2
	Bit3  Bit = 3
	Bit4  Bit = 4
	Bit5  Bit = 5
	Bit6  Bit = 6
	Bit7  Bit = 7
	Bit8  Bit = 8
	Bit9  Bit = 9
	Bit10 Bit = 10
	Bit11 Bit = 11
	Bit12 Bit = 12
	Bit13 Bit = 13
	Bit14 Bit = 14
	Bit15 Bit = 15
)

// Event 表示一个按键的按下/抬起边沿
type Event struct {
	Bit     Bit
	Pressed bool
}

// device 是平台无关的 HID++ 设备接口, Windows 实现见 hid_windows.go
type device interface {
	FeatureIndex(ctx context.Context) (uint8, error)
	StartSpy(ctx context.Context) error
	ReadReport(ctx context.Context) ([]byte, error)
	Close() error
	Path() string
}

var (
	// openDevices 由平台实现注入: 枚举并打开候选设备, pid=0 表示任意
	openDevices = func(ctx context.Context, pid uint16) ([]device, error) {
		return nil, ErrUnsupported
	}

	ErrUnsupported = errors.New("logihidpp: unsupported platform")
	ErrNoDevice    = errors.New("logihidpp: no Logitech HID++ mouse with feature 0x8110")

	retryDelay     = 2 * time.Second
	reconnectDelay = time.Second
	learnTimeout   = 2 * time.Second
)

// Monitor 后台持续读取 0x8110 按键位图, 支持自动重连与重新 startSpy
type Monitor struct {
	ctx    context.Context
	cancel context.CancelFunc
	pid    uint16

	state atomic.Uint32

	mu   sync.Mutex
	dev  device
	feat uint8

	// devCursor 用于学习模式轮换: 所有候选都无法通过 getFeature 查询时,
	// 逐个监听候选, 先产生按键事件的设备被采用
	devCursor int

	events chan Event

	closeOnce sync.Once
	done      chan struct{}
}

// Start 打开第一个匹配的 Logitech HID++ 鼠标并启用 0x8110 spy
// 失败时返回错误, 但内部仍会每 2 秒重试 (生命周期由 ctx 控制)
func Start(ctx context.Context) (*Monitor, error) {
	return startWithPID(ctx, 0)
}

// OpenWithPID 显式指定 Product ID (如 0xC547), 避免多鼠标时选错设备
func OpenWithPID(ctx context.Context, pid uint16) (*Monitor, error) {
	return startWithPID(ctx, pid)
}

func startWithPID(ctx context.Context, pid uint16) (*Monitor, error) {
	mctx, cancel := context.WithCancel(ctx)
	m := &Monitor{
		ctx:    mctx,
		cancel: cancel,
		pid:    pid,
		events: make(chan Event, 256),
		done:   make(chan struct{}),
	}
	first := make(chan error, 1)
	go m.run(first)
	select {
	case err := <-first:
		return m, err
	case <-mctx.Done():
		m.Close()
		return m, mctx.Err()
	}
}

func (m *Monitor) run(first chan<- error) {
	defer close(m.done)
	defer m.closeDev()

	for m.ctx.Err() == nil {
		err := m.connectOnce()
		if first != nil {
			first <- err
			first = nil
		}
		if err != nil {
			if !m.wait(m.ctx, retryDelay) {
				return
			}
			continue
		}
		if err := m.readLoop(); err != nil && m.ctx.Err() == nil {
			m.clearState()
			m.closeDev()
			if !m.wait(m.ctx, reconnectDelay) {
				return
			}
		}
	}
}

func (m *Monitor) connectOnce() error {
	devs, err := openDevices(m.ctx, m.pid)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return ErrNoDevice
	}
	m.mu.Lock()
	start := m.devCursor % len(devs)
	m.devCursor = start + 1
	m.mu.Unlock()

	// 先找能通过 getFeature 查询到 0x8110 的设备, 保持原有行为
	for i := range devs {
		dev := devs[(start+i)%len(devs)]
		idx, err := dev.FeatureIndex(m.ctx)
		if err != nil {
			continue
		}
		for j := range devs {
			if j != (start+i)%len(devs) {
				devs[j].Close()
			}
		}
		// startSpy 失败仍继续监听 (可能 GHub 已启用 spy)
		_ = dev.StartSpy(m.ctx)
		m.mu.Lock()
		m.dev = dev
		m.feat = idx
		m.mu.Unlock()
		return nil
	}

	// 全部查询失败 (如 G502X 接收器拒绝 getFeature), 进入学习模式:
	// 连接轮换起点的设备, 从报告流中识别 0x8110 feature index
	learn := devs[start%len(devs)]
	for j := range devs {
		if j != start%len(devs) {
			devs[j].Close()
		}
	}
	m.mu.Lock()
	m.dev = learn
	m.feat = 0
	m.mu.Unlock()
	return nil
}

func (m *Monitor) readLoop() error {
	m.mu.Lock()
	dev := m.dev
	feat := m.feat
	m.mu.Unlock()
	if dev == nil {
		return errors.New("logihidpp: monitor not connected")
	}

	var (
		prev           uint16
		featCandidates = map[uint8]uint16{}
		spySent        bool
	)
	for {
		rctx := m.ctx
		var cancel context.CancelFunc
		if feat == 0 {
			// 学习模式: 空设备无报告则超时, 触发重连轮换到下一个候选
			rctx, cancel = context.WithTimeout(m.ctx, learnTimeout)
		}
		report, err := dev.ReadReport(rctx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			return err
		}
		if feat == 0 {
			fi, state, ok := sniffButtonReport(report)
			if !ok {
				continue
			}
			last, seen := featCandidates[fi]
			if !seen {
				last = 0
			}
			featCandidates[fi] = state
			if state == 0 && last == 0 {
				continue // 空闲报告无法确认 feature index
			}
			m.mu.Lock()
			if m.feat == 0 {
				m.feat = fi
			}
			feat = m.feat
			m.mu.Unlock()
			if !spySent {
				spyCtx, spyCancel := context.WithTimeout(m.ctx, 500*time.Millisecond)
				_ = dev.StartSpy(spyCtx)
				spyCancel()
				spySent = true
			}
			m.state.Store(uint32(state))
			for _, ev := range DiffState(last, state) {
				select {
				case m.events <- ev:
				default: // 消费者未及时读取时丢弃, 不阻塞读取循环
				}
			}
			prev = state
			continue
		}
		state, ok := ParseReport(report, feat)
		if !ok {
			continue
		}
		m.state.Store(uint32(state))
		for _, ev := range DiffState(prev, state) {
			select {
			case m.events <- ev:
			default: // 消费者未及时读取时丢弃, 不阻塞读取循环
			}
		}
		prev = state
	}
}

// State 返回当前 16bit 位图 (原子读)
func (m *Monitor) State() uint16 {
	return uint16(m.state.Load())
}

// Events 返回按键边沿事件流 (按下/抬起)
func (m *Monitor) Events() <-chan Event {
	return m.events
}

// DevicePath 返回当前连接的设备路径 (供诊断与日志)
func (m *Monitor) DevicePath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dev == nil {
		return ""
	}
	return m.dev.Path()
}

// Close 关闭句柄并停止后台 goroutine
func (m *Monitor) Close() error {
	m.closeOnce.Do(m.cancel)
	<-m.done
	return nil
}

func (m *Monitor) closeDev() {
	m.mu.Lock()
	dev := m.dev
	m.dev = nil
	m.mu.Unlock()
	if dev != nil {
		dev.Close()
	}
	m.clearState()
}

func (m *Monitor) clearState() {
	m.state.Store(0)
}

func (m *Monitor) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
