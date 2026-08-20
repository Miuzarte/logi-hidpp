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

// FeatureInfo 是 IRoot.getFeatureList 返回的一条 feature (索引 + ID)
type FeatureInfo struct {
	Index uint8
	ID    uint16
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

	// startNotifier 由平台实现注入: 创建设备插拔通知源, 无通知源返回 (nil, nil)
	startNotifier = func() (deviceNotifier, error) {
		return startDeviceNotifier()
	}

	ErrUnsupported = errors.New("logihidpp: unsupported platform")
	ErrNoDevice    = errors.New("logihidpp: no Logitech HID++ mouse with feature 0x8110")

	retryDelay     = 2 * time.Second
	reconnectDelay = time.Second
	learnTimeout   = 2 * time.Second

	// readPollInterval 是阻塞读的检查间隔: 读报告带这个超时, 超时后检查
	// 首报告状态与设备插拔事件
	readPollInterval = 2 * time.Second
	// readIdleTimeout 是首报告兜底超时: 连接建立后从未收到任何报告且超过
	// 该时长才重连 (纯兜底, 用于极端 ghost 场景)
	// 注意: 0x8110 空闲时正常不发报告, 所以正常设备不应频繁触发;
	// 鼠标休眠/接收器重枚举的重连由设备插拔事件 (notify) 即时驱动
	readIdleTimeout = 5 * time.Minute
)

// Logf 是诊断日志回调 (默认空操作), 由宿主扩展 (mhub-logi) 设置,
// 用于排查连接/事件链路: 连接失败、重连、未匹配报告等
var Logf = func(format string, args ...any) {}

// deviceNotifier 监听 Windows HID 设备接口插拔事件 (WM_DEVICECHANGE),
// 接收器重枚举/鼠标休眠唤醒时立即触发重连, 不等 10s 静默看门狗
// 平台实现: notify_windows.go (Windows) / notify_other.go (其他, 返回 nil)
type deviceNotifier interface {
	Changed() <-chan struct{}
	Close()
}

// Monitor 后台持续读取 0x8110 按键位图, 支持自动重连与重新 startSpy
type Monitor struct {
	ctx    context.Context
	cancel context.CancelFunc
	pid    uint16

	state atomic.Uint32

	// notify 是设备插拔事件源 (nil 表示平台不支持, 退化为静默看门狗)
	notify deviceNotifier

	mu   sync.Mutex
	dev  device
	feat uint8

	// devCursor 用于学习模式轮换: 所有候选都无法通过 getFeature 查询时,
	// 逐个监听候选, 先产生按键事件的设备被采用
	devCursor int

	events chan Event

	// lastUnmatched 用于对未匹配报告做限流日志 (报告流可能很密)
	lastUnmatched atomic.Int64
	// firstReport 标记每条连接收到的第一个报告, 用于诊断设备是否在发报告
	firstReport atomic.Bool
	// lastReport 是最近一次收到报告的时间 (看门狗用, 仅 readLoop 单 goroutine 访问)
	lastReport time.Time

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
	if n, err := startNotifier(); err == nil {
		m.notify = n
	} else {
		Logf("logihidpp: device notifier unavailable: %v (watchdog fallback)", err)
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
			Logf("logihidpp: connect failed: %v (retry in %v)", err, retryDelay)
			if !m.wait(m.ctx, retryDelay) {
				return
			}
			continue
		}
		Logf("logihidpp: connected to %s (0x8110 feature index %d)", m.DevicePath(), m.featureIndex())
		if err := m.readLoop(); err != nil && m.ctx.Err() == nil {
			Logf("logihidpp: read loop error: %v (reconnect in %v)", err, reconnectDelay)
			m.clearState()
			m.closeDev()
			if !m.wait(m.ctx, reconnectDelay) {
				return
			}
		}
	}
}

// featureIndex 返回当前 0x8110 feature index (0 表示学习模式)
func (m *Monitor) featureIndex() uint8 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.feat
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
		// startSpy 失败仍继续监听, 但设备可能保持静默 (无按键报告)
		if err := dev.StartSpy(m.ctx); err != nil {
			Logf("logihidpp: startSpy failed: %v (device may stay silent)", err)
		}
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
	m.firstReport.Store(false)
	m.lastReport = time.Now()

	// 设备插拔通知立即打断阻塞读, 不等 readPollInterval 轮询
	// (接收器重枚举/休眠唤醒时旧连接可能永久静默)
	loopCtx, loopCancel := context.WithCancel(m.ctx)
	defer loopCancel()
	if m.notify != nil {
		go func() {
			select {
			case <-m.notify.Changed():
				Logf("logihidpp: device change notification, interrupting read")
				loopCancel()
			case <-loopCtx.Done():
			}
		}()
	}

	for {
		rctx := loopCtx
		var cancel context.CancelFunc
		if feat == 0 {
			// 学习模式: 空设备无报告则超时, 触发重连轮换到下一个候选
			rctx, cancel = context.WithTimeout(loopCtx, learnTimeout)
		} else {
			// 看门狗: 阻塞读带检查间隔超时, 超时后检查首报告状态
			rctx, cancel = context.WithTimeout(loopCtx, readPollInterval)
		}
		report, err := dev.ReadReport(rctx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			if m.ctx.Err() != nil {
				return m.ctx.Err()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				return err // 真实读取错误或通知打断, 触发重连
			}
			if feat == 0 {
				return err // 学习模式超时: 轮换候选设备
			}
			// 首报告等待超时: 连接后从未收到任何报告则重连 (ghost 实例特征)
			// 收到过报告后的空闲静默是 0x8110 正常状态, 不触发重连,
			// 设备重枚举由 WM_DEVICECHANGE 事件 (notify) 打断
			if !m.firstReport.Load() && time.Since(m.lastReport) > readIdleTimeout {
				Logf("logihidpp: no reports within %v of connect, reconnecting", readIdleTimeout)
				return err
			}
			continue
		}
		m.lastReport = time.Now()
		if m.firstReport.CompareAndSwap(false, true) {
			Logf("logihidpp: first report received: % X", report)
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
			m.logUnmatched(report)
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

// logUnmatched 每秒最多记录一次未匹配报告, 用于排查报告格式/feature index 变化
func (m *Monitor) logUnmatched(report []byte) {
	now := time.Now().UnixMilli()
	last := m.lastUnmatched.Load()
	if now-last < 1000 {
		return
	}
	if m.lastUnmatched.CompareAndSwap(last, now) {
		Logf("logihidpp: unmatched report: % X", report)
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
	if m.notify != nil {
		m.notify.Close()
	}
	<-m.done
	return nil
}

// notifyChanged 返回设备插拔事件通道, 无通知源时为 nil (select 永久阻塞)
func (m *Monitor) notifyChanged() <-chan struct{} {
	if m.notify == nil {
		return nil
	}
	return m.notify.Changed()
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
	case <-m.notifyChanged():
		// 设备插拔事件: 提前结束等待, 立即重连
		Logf("logihidpp: device change notification, reconnecting now")
		return true
	}
}
