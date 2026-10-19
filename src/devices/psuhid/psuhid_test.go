package psuhid

import (
	"crypto/md5"
	"encoding/hex"
	"testing"
	"time"
)

// StopDirty must return 2 so devices.StopDirty drops the unplugged PSU from the device list,
// and must stop a running refresh loop without blocking.
func TestStopDirtyRemovesDevice(t *testing.T) {
	d := &Device{Serial: "x"}
	d.setAutoRefresh()

	done := make(chan uint8)
	go func() { done <- d.StopDirty() }()
	select {
	case r := <-done:
		if r != 2 {
			t.Fatalf("StopDirty returned %d, want 2", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StopDirty blocked")
	}
	if d.timer != nil {
		t.Fatal("refresh timer not cleared")
	}
}

// A single PSU keeps its md5(product) serial; an identical second PSU, keyed with its USB port,
// gets its own serial (and profile).
func TestSerialFromKeyKeepsIdenticalPsusApart(t *testing.T) {
	single := serialFromKey("HX1500i", "7199")
	sum := md5.Sum([]byte("HX1500i"))
	if want := hex.EncodeToString(sum[:]); single != want {
		t.Fatalf("single PSU serial changed: %q, want %q", single, want)
	}
	second := serialFromKey("HX1500i", "7199-1-3")
	if second == single {
		t.Fatal("second identical PSU shares the first one's serial")
	}
	if third := serialFromKey("HX1500i", "7199-1-4"); third == second {
		t.Fatal("PSUs on different ports share a serial")
	}
}
