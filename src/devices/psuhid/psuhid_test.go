package psuhid

import (
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
