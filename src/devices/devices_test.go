package devices

import "testing"

func TestTakeSerialByDevPath(t *testing.T) {
	devPath := "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	usbDevPaths[devPath] = "abc123"
	t.Cleanup(func() { delete(usbDevPaths, devPath) })

	serial, ok := TakeSerialByDevPath(devPath)
	if !ok || serial != "abc123" {
		t.Fatalf("got (%q, %v), want (\"abc123\", true)", serial, ok)
	}
	// The mapping is consumed so a later replug on the same port registers fresh
	if _, ok = TakeSerialByDevPath(devPath); ok {
		t.Fatal("mapping should be removed after it is taken")
	}
}
