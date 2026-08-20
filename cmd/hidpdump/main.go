// Command hidpdump 打印 Logitech 鼠标 HID++ 原始报文, 用于排查 0x8110 通知链路
//
// 用法:
//
//	hidpdump -pid C547            # 枚举 + feature 列表 + getFeature/startSpy + 持续打印报告
//	hidpdump -pid C547 -scan      # 扫描所有设备号 (0xFF, 0x01..0x0F) 定位 0x8110
//	hidpdump -pid C547 -no-spy    # 跳过 getFeature/startSpy, 只看设备自发流量
//	hidpdump -pid C547 -swid 0B   # 用不同 swid 发 startSpy (部分设备只给特定 swid 发通知)
//	hidpdump -list                # 只列出 vendor HID 设备
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

	logihidpp "github.com/Miuzarte/logi-hidpp"
)

func main() {
	listOnly := flag.Bool("list", false, "只列出 Logitech vendor HID 设备")
	scan := flag.Bool("scan", false, "扫描所有设备号定位 0x8110 feature")
	pidHex := flag.String("pid", "", "Product ID (十六进制, 如 C547)")
	noSpy := flag.Bool("no-spy", false, "跳过 getFeature+startSpy, 只看设备自发流量")
	swidHex := flag.String("swid", "0A", "startSpy 使用的 swid (十六进制)")
	flag.Parse()

	if *listOnly {
		listDevices()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var pid uint16
	if *pidHex != "" {
		p, perr := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(*pidHex), "0X"), 16, 16)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "invalid -pid:", perr)
			os.Exit(2)
		}
		pid = uint16(p)
	}
	if *scan {
		if err := logihidpp.DumpScan(ctx, pid); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			os.Exit(1)
		}
		return
	}
	swid, perr := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(*swidHex), "0X"), 16, 8)
	if perr != nil {
		fmt.Fprintln(os.Stderr, "invalid -swid:", perr)
		os.Exit(2)
	}
	if err := logihidpp.DumpDevice(ctx, pid, !*noSpy, uint8(swid)); err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
		os.Exit(1)
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
