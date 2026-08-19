package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	logihidpp "github.com/Miuzarte/logi-hidpp"
)

func main() {
	listOnly := flag.Bool("list", false, "只读枚举 Logitech vendor HID 设备（不写设备）")
	pidHex := flag.String("pid", "", "显式指定设备 Product ID（十六进制，如 C547）")
	flag.Parse()

	if *listOnly {
		listDevices()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		m   *logihidpp.Monitor
		err error
	)
	if *pidHex != "" {
		pid, perr := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(*pidHex), "0X"), 16, 16)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "invalid -pid %q: %v\n", *pidHex, perr)
			os.Exit(2)
		}
		m, err = logihidpp.OpenWithPID(ctx, uint16(pid))
	} else {
		m, err = logihidpp.Start(ctx)
	}
	if err != nil {
		// Monitor 内部仍会后台重试, 先提示再继续等待事件
		fmt.Fprintf(os.Stderr, "warning: %v (retrying in background)\n", err)
	}
	if m == nil {
		os.Exit(1)
	}
	defer m.Close()

	fmt.Println("calibration mode: press every button on the mouse, Ctrl+C to quit")
	fmt.Println("output: bit <n> down / bit <n> up")

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastState := m.State()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-m.Events():
			verb := "up"
			if ev.Pressed {
				verb = "down"
			}
			fmt.Printf("bit %d %s\n", ev.Bit, verb)
		case <-ticker.C:
			// 偶发漏事件时按状态差补齐, 保证校准不漏键
			state := m.State()
			if state != lastState {
				for _, ev := range diff(lastState, state) {
					verb := "up"
					if ev.Pressed {
						verb = "down"
					}
					fmt.Printf("bit %d %s\n", ev.Bit, verb)
				}
				lastState = state
			}
		}
	}
}

func listDevices() {
	devs, err := logihidpp.Enumerate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "enumerate:", err)
		os.Exit(1)
	}
	if len(devs) == 0 {
		fmt.Println("(no Logitech vendor HID device found)")
		return
	}
	for _, d := range devs {
		fmt.Printf(
			"VID=%04X PID=%04X UsagePage=%04X Usage=%04X InLen=%d %s\n",
			d.VID, d.PID, d.UsagePage, d.Usage, d.InputReportByteLength, d.Path,
		)
	}
}

func diff(prev, curr uint16) []event {
	var out []event
	diffBits := prev ^ curr
	for i := 0; i < 16; i++ {
		bit := uint16(1 << i)
		if diffBits&bit != 0 {
			out = append(out, event{Bit: uint8(i), Pressed: curr&bit != 0})
		}
	}
	return out
}

type event struct {
	Bit     uint8
	Pressed bool
}
