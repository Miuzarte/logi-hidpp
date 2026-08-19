package logihidpp

import (
	"testing"
)

func TestParseFeatureIndex(t *testing.T) {
	tests := []struct {
		name  string
		reply []byte
		want  uint8
		err   bool
	}{
		{"success", []byte{0x10, 0xFF, 0x00, 0x0A, 0x05, 0x00, 0x00}, 0x05, false},
		{"not found", []byte{0x10, 0xFF, 0x00, 0x0A, 0x00, 0x00, 0x00}, 0, true},
		{"hidpp error", []byte{0x10, 0xFF, 0x8F, 0x0A, 0x05, 0x00, 0x00}, 0, true},
		{"bad report id", []byte{0x11, 0xFF, 0x00, 0x0A, 0x05, 0x00, 0x00}, 0, true},
		{"too short", []byte{0x10, 0xFF, 0x00, 0x0A}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFeatureIndex(tt.reply)
			if tt.err {
				if err == nil {
					t.Fatalf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseStartSpyReply(t *testing.T) {
	ok := []byte{0x10, 0xFF, 0x05, 0x1A, 0x00, 0x00, 0x00}
	if err := ParseStartSpyReply(ok, 0x05); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ParseStartSpyReply(ok, 0x06); err == nil {
		t.Fatal("expected feature index mismatch error")
	}
	errReply := []byte{0x10, 0xFF, 0x8F, 0x1A, 0x00, 0x00, 0x00}
	if err := ParseStartSpyReply(errReply, 0x05); err == nil {
		t.Fatal("expected HID++ error")
	}
}

func TestParseReport(t *testing.T) {
	tests := []struct {
		name   string
		report []byte
		feat   uint8
		state  uint16
		ok     bool
	}{
		{
			name:   "low byte bit 4",
			report: []byte{0x11, 0xFF, 0x05, 0x00, 0x00, 0x10},
			feat:   0x05,
			state:  0x0010,
			ok:     true,
		},
		{
			name:   "high byte bit 8",
			report: []byte{0x11, 0xFF, 0x05, 0x00, 0x01, 0x00},
			feat:   0x05,
			state:  0x0100,
			ok:     true,
		},
		{
			name:   "full long report",
			report: []byte{0x11, 0xFF, 0x05, 0x00, 0x01, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			feat:   0x05,
			state:  0x0110,
			ok:     true,
		},
		{"wrong feature index", []byte{0x11, 0xFF, 0x06, 0x00, 0x00, 0x10}, 0x05, 0, false},
		{"non button event", []byte{0x11, 0xFF, 0x05, 0x01, 0x00, 0x10}, 0x05, 0, false},
		{"too short", []byte{0x11, 0xFF, 0x05}, 0x05, 0, false},
		{"bad report id", []byte{0x10, 0xFF, 0x05, 0x00, 0x00, 0x10}, 0x05, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, ok := ParseReport(tt.report, tt.feat)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && state != tt.state {
				t.Fatalf("state = %04X, want %04X", state, tt.state)
			}
		})
	}
}

func TestDiffState(t *testing.T) {
	events := DiffState(0x0000, 0x0010)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Bit != Bit4 || !events[0].Pressed {
		t.Fatalf("unexpected event: %+v", events[0])
	}

	events = DiffState(0x0010, 0x0000)
	if len(events) != 1 || events[0].Bit != Bit4 || events[0].Pressed {
		t.Fatalf("unexpected events: %+v", events)
	}

	events = DiffState(0x0001, 0x0010) // bit 0 抬起, bit 4 按下
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Bit != Bit0 || events[0].Pressed {
		t.Fatalf("unexpected events[0]: %+v", events[0])
	}
	if events[1].Bit != Bit4 || !events[1].Pressed {
		t.Fatalf("unexpected events[1]: %+v", events[1])
	}

	if DiffState(0x00FF, 0x00FF) != nil {
		t.Fatal("no state change should produce no events")
	}
}
