//go:build windows

package logihidpp

import (
	"context"
	"fmt"
	"time"
)

// DumpScan 扫描接收器所有设备号 (0xFF, 0x01..0x0F) 上的 0x8110 feature,
// 用于定位鼠标当前所在的设备号 (硬编码 0x01 失效时, 如接收器重枚举/重配对后)
func DumpScan(ctx context.Context, pid uint16) error {
	devs, err := openDevices(ctx, pid)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return ErrNoDevice
	}
	for i, d := range devs {
		dev, ok := d.(*hidDevice)
		if !ok {
			fmt.Printf("instance %d: unexpected type %T\n", i, d)
			continue
		}
		fmt.Printf("== instance %d: %s ==\n", i, dev.path)
		for _, idx := range []byte{0xFF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F} {
			probeGetFeature(ctx, dev, idx)
		}
		fmt.Println()
	}
	return nil
}

// probeGetFeature 对指定设备号发 getFeature(0x8110), 打印短/长集合的原始应答
func probeGetFeature(ctx context.Context, dev *hidDevice, idx byte) {
	req := buildGetFeatureRequest()
	req[1] = idx
	fmt.Printf("-- getFeature(0x8110) device 0x%02X --\n", idx)
	if err := dev.short.write(ctx, req); err != nil {
		fmt.Printf("   write: %v\n", err)
		return
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		sctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		rep, err := dev.short.read(sctx)
		if err == nil {
			fmt.Printf("   short: % X\n", rep)
		}
		cancel()
		sctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
		rep, err = dev.long.read(sctx)
		if err == nil {
			fmt.Printf("   long:  % X\n", rep)
		}
		cancel()
	}
}

// DumpDevice 打开候选设备并打印诊断信息, 用于排查 0x8110 通知链路:
//   - 完整 feature 列表 (确认按键通知实际由哪个 feature 提供, 如 0x1B04)
//   - getFeature(0x8110) + startSpy (swid 可配置) 的应答原文
//   - 短/长集合的原始报告流 (spy=true 时先激活, 然后持续打印到 ctx 取消)
func DumpDevice(ctx context.Context, pid uint16, spy bool, swid uint8) error {
	devs, err := openDevices(ctx, pid)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return ErrNoDevice
	}
	fmt.Printf("candidates: %d\n", len(devs))
	for i, d := range devs {
		fmt.Printf("  [%d] path=%s\n", i, d.Path())
	}
	dev, ok := devs[0].(*hidDevice)
	if !ok {
		return fmt.Errorf("logihidpp: unexpected device type %T", devs[0])
	}

	features, err := dev.FeatureList(ctx)
	if err != nil {
		fmt.Printf("feature list: err=%v\n", err)
	} else {
		fmt.Printf("feature list (%d):\n", len(features))
		for _, f := range features {
			fmt.Printf("  index=%2d id=0x%04X\n", f.Index, f.ID)
		}
	}

	if spy {
		idx, err := dev.FeatureIndex(ctx)
		fmt.Printf("getFeature(0x8110): index=%d err=%v\n", idx, err)
		if err == nil {
			req := buildStartSpyRequest(idx)
			req[1] = 0x01
			req[3] = funcStartSpy<<4 | swid
			if err := dev.short.write(ctx, req); err != nil {
				fmt.Printf("startSpy(swid=0x%02X): write err=%v\n", swid, err)
			} else {
				rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				reply, rerr := dev.long.read(rctx)
				cancel()
				fmt.Printf("startSpy(swid=0x%02X): reply=% X err=%v\n", swid, reply, rerr)
			}
		}
	}

	fmt.Println("dumping reports (Ctrl+C to stop)...")
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 2)
	go dumpHandle(dctx, done, "short", dev.short)
	go dumpHandle(dctx, done, "long", dev.long)
	<-done
	<-done
	return nil
}

func dumpHandle(ctx context.Context, done chan<- struct{}, name string, h *hidHandle) {
	defer func() { done <- struct{}{} }()
	for {
		buf, err := h.read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Printf("[%s] read error: %v\n", name, err)
			return
		}
		fmt.Printf("[%s] %s % X\n", name, time.Now().Format("15:04:05.000"), buf)
	}
}
