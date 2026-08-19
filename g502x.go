package logihidpp

// G502X 实测位 (2026-08 本机校准)
const (
	G502X_G1          = Bit0  // 左键
	G502X_G2          = Bit1  // 右键
	G502X_G3          = Bit2  // 中键 (滚轮按下)
	G502X_G4          = Bit3  // 后退侧键
	G502X_G5          = Bit5  // 前进侧键
	G502X_G6          = Bit4  // 拇指尖侧键
	G502X_WHEEL_LEFT  = Bit6  // 滚轮左倾
	G502X_WHEEL_RIGHT = Bit7  // 滚轮右倾
	G502X_G9          = Bit8  // 滚轮下方 (Profile cycle)
	G502X_G8_SNIPER   = Bit9  // 前狙击键 (远)
	G502X_G7_SNIPER   = Bit10 // 后狙击键 (近)
)
