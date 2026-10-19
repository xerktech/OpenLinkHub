package devices

import (
	"OpenLinkHub/src/common"
	"testing"
)

func TestTakeSerialByDevPath(t *testing.T) {
	devPath := "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	usbDevPaths[devPath] = usbDevice{serial: "abc123", key: "7199"}
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

// Two identical PSUs (e.g. two HX1500i) have no USB serial. They must get distinct keys at startup
// and on hotplug, and a replugged PSU must get its old key back.
func TestSerialLessKeyKeepsIdenticalDevicesApart(t *testing.T) {
	const portA = "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	const portB = "/devices/pci0000:00/0000:00:14.0/usb1/1-3"

	// Startup: the second PSU finds the first in the device list
	list := map[string]bool{}
	inList := func(k string) bool { return list[k] }
	keyA := serialLessKey(7199, portA, "/dev/hidraw0", inList)
	list[keyA] = true
	keyB := serialLessKey(7199, portB, "/dev/hidraw1", inList)
	if keyA != "7199" || keyB != "7199-1-3" {
		t.Fatalf("startup keys = %q, %q; want \"7199\", \"7199-1-3\"", keyA, keyB)
	}
	// Port unknown: fall back to the HID path
	if k := serialLessKey(7199, "", "/dev/hidraw1", inList); k != "7199-hidraw1" {
		t.Fatalf("key without port = %q, want \"7199-hidraw1\"", k)
	}

	usbDevPaths[portA] = usbDevice{serial: "serialA", key: keyA}
	usbDevPaths[portB] = usbDevice{serial: "serialB", key: keyB}
	t.Cleanup(func() { delete(usbDevPaths, portA); delete(usbDevPaths, portB) })

	// Hotplug: B replugged while A is still plugged must not take A's key
	TakeSerialByDevPath(portB)
	if k := serialLessKey(7199, portB, "", keyRegistered); k != keyB {
		t.Fatalf("B replug key = %q, want %q", k, keyB)
	}
	usbDevPaths[portB] = usbDevice{serial: "serialB", key: keyB}

	// Hotplug: A replugged while B is still plugged gets its own key back
	TakeSerialByDevPath(portA)
	if k := serialLessKey(7199, portA, "", keyRegistered); k != keyA {
		t.Fatalf("A replug key = %q, want %q", k, keyA)
	}

	// Another product is not affected
	if k := serialLessKey(7203, portA, "", keyRegistered); k != "7203" {
		t.Fatalf("other product key = %q, want \"7203\"", k)
	}
}

// The key is reserved before a device initializes, so an identical PSU plugged in while the first
// one is still initializing does not take its key. The serial is filled in once it registers.
func TestReserveUSBDevPath(t *testing.T) {
	const portA = "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	const portB = "/devices/pci0000:00/0000:00:14.0/usb1/1-3"
	t.Cleanup(func() { delete(usbDevPaths, portA); delete(usbDevPaths, portB) })

	reserveUSBDevPath(portA, "7199")
	if k := serialLessKey(7199, portB, "", keyRegistered); k != "7199-1-3" {
		t.Fatalf("key while A initializes = %q, want \"7199-1-3\"", k)
	}
	// Unplugged while initializing: nothing to stop yet, and the late registration is not kept
	if _, ok := TakeSerialByDevPath(portA); ok {
		t.Fatal("device still initializing should report no serial")
	}
	trackUSBDevPath(portA, &common.Device{Serial: "serialA"})
	if _, ok := usbDevPaths[portA]; ok {
		t.Fatal("device unplugged while initializing should be forgotten")
	}

	// Failed to initialize: the key is released
	reserveUSBDevPath(portA, "7199")
	trackUSBDevPath(portA, nil)
	if keyRegistered("7199") {
		t.Fatal("key of a device that failed to initialize should be released")
	}

	// Registered: the serial is kept under the reserved key
	reserveUSBDevPath(portB, "7199-1-3")
	trackUSBDevPath(portB, &common.Device{Serial: "serialB"})
	if serial, ok := TakeSerialByDevPath(portB); !ok || serial != "serialB" {
		t.Fatalf("got (%q, %v), want (\"serialB\", true)", serial, ok)
	}
}
