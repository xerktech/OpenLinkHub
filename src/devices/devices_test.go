package devices

import (
	"OpenLinkHub/src/common"
	"os"
	"path/filepath"
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

	usePlainKeyPorts(t)

	// Startup: the second PSU finds the first in the device list
	list := map[string]string{}
	inList := func(k string) (string, bool) { p, ok := list[k]; return p, ok }
	keyA := serialLessKey(7199, portA, "/dev/hidraw0", inList)
	list[keyA] = portA
	keyB := serialLessKey(7199, portB, "/dev/hidraw1", inList)
	if keyA != "7199" || keyB != "7199-1-3" {
		t.Fatalf("startup keys = %q, %q; want \"7199\", \"7199-1-3\"", keyA, keyB)
	}
	// The first PSU's port now holds the plain key
	if plainKeyPorts["7199"] != "1-2" {
		t.Fatalf("plain key port = %q, want \"1-2\"", plainKeyPorts["7199"])
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
	if k := serialLessKey(7199, portB, "", keyHolder); k != keyB {
		t.Fatalf("B replug key = %q, want %q", k, keyB)
	}
	usbDevPaths[portB] = usbDevice{serial: "serialB", key: keyB}

	// Hotplug: A replugged while B is still plugged gets its own key back
	TakeSerialByDevPath(portA)
	if k := serialLessKey(7199, portA, "", keyHolder); k != keyA {
		t.Fatalf("A replug key = %q, want %q", k, keyA)
	}

	// Another product is not affected
	if k := serialLessKey(7203, portA, "", keyHolder); k != "7203" {
		t.Fatalf("other product key = %q, want \"7203\"", k)
	}
}

// The key is reserved before a device initializes, so an identical PSU plugged in while the first
// one is still initializing does not take its key. The serial is filled in once it registers.
func TestReserveUSBDevPath(t *testing.T) {
	usePlainKeyPorts(t)
	const portA = "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	const portB = "/devices/pci0000:00/0000:00:14.0/usb1/1-3"
	t.Cleanup(func() { delete(usbDevPaths, portA); delete(usbDevPaths, portB) })

	reserveUSBDevPath(portA, "7199")
	if k := serialLessKey(7199, portB, "", keyHolder); k != "7199-1-3" {
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
	if _, held := keyHolder("7199"); held {
		t.Fatal("key of a device that failed to initialize should be released")
	}

	// Registered: the serial is kept under the reserved key
	reserveUSBDevPath(portB, "7199-1-3")
	trackUSBDevPath(portB, &common.Device{Serial: "serialB"})
	if serial, ok := TakeSerialByDevPath(portB); !ok || serial != "serialB" {
		t.Fatalf("got (%q, %v), want (\"serialB\", true)", serial, ok)
	}
}

// usePlainKeyPorts starts a test with no pinned ports, saved to a file of its own, and returns
// that file.
func usePlainKeyPorts(t *testing.T) string {
	t.Helper()
	location := filepath.Join(t.TempDir(), "serialless.json")
	oldPorts, oldLocation := plainKeyPorts, plainKeyPortsLocation
	plainKeyPorts = map[string]string{}
	plainKeyPortsLocation = func() string { return location }
	t.Cleanup(func() { plainKeyPorts, plainKeyPortsLocation = oldPorts, oldLocation })
	return location
}

// Once a port holds the plain key, only a PSU on that port gets it, in whatever order the PSUs
// are found. A single PSU that never met an identical one keeps the plain key on any port.
func TestSerialLessKeyPinsPlainKeyToPort(t *testing.T) {
	const portA = "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	const portB = "/devices/pci0000:00/0000:00:14.0/usb1/1-3"
	none := func(string) (string, bool) { return "", false }
	location := usePlainKeyPorts(t)

	if k := serialLessKey(7199, portB, "", none); k != "7199" {
		t.Fatalf("single PSU key = %q, want \"7199\"", k)
	}
	if common.FileExists(location) {
		t.Fatal("a single PSU should not pin its port")
	}

	plainKeyPorts["7199"] = "1-2"
	// B alone (A unplugged, or found first) does not take A's key
	if k := serialLessKey(7199, portB, "", none); k != "7199-1-3" {
		t.Fatalf("B alone key = %q, want \"7199-1-3\"", k)
	}
	if k := serialLessKey(7199, portA, "", none); k != "7199" {
		t.Fatalf("A key = %q, want \"7199\"", k)
	}
	// Port unknown: it can't be the pinned port
	if k := serialLessKey(7199, "", "/dev/hidraw1", none); k != "7199-hidraw1" {
		t.Fatalf("key without port = %q, want \"7199-hidraw1\"", k)
	}
	// A stale entry left on A's own port does not cost A its key
	stale := func(string) (string, bool) { return portA, true }
	if k := serialLessKey(7199, portA, "", stale); k != "7199" {
		t.Fatalf("A key with stale entry = %q, want \"7199\"", k)
	}
	// A PSU on the pinned port never shares the plain key
	held := func(string) (string, bool) { return "", true }
	if k := serialLessKey(7199, portA, "", held); k != "7199-1-2" {
		t.Fatalf("A key while plain key held = %q, want \"7199-1-2\"", k)
	}
	if common.FileExists(location) {
		t.Fatal("a pinned port should not be saved again")
	}
}

// Pinned ports survive a restart, and a damaged file is ignored rather than crashing.
func TestLoadPlainKeyPorts(t *testing.T) {
	const portA = "/devices/pci0000:00/0000:00:14.0/usb1/1-2"
	const portB = "/devices/pci0000:00/0000:00:14.0/usb1/1-3"
	location := usePlainKeyPorts(t)

	serialLessKey(7199, portB, "", func(string) (string, bool) { return portA, true })
	plainKeyPorts = map[string]string{}
	loadPlainKeyPorts()
	if plainKeyPorts["7199"] != "1-2" {
		t.Fatalf("loaded ports = %v, want 7199 on 1-2", plainKeyPorts)
	}

	for _, data := range []string{"null", "", "[]", `{"7199":5}`, `{"7199":""}`} {
		if err := os.WriteFile(location, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		plainKeyPorts = map[string]string{}
		loadPlainKeyPorts()
		if len(plainKeyPorts) != 0 {
			t.Fatalf("file %q loaded ports %v, want none", data, plainKeyPorts)
		}
		// Pinning still works
		if k := serialLessKey(7199, portB, "", func(string) (string, bool) { return portA, true }); k != "7199-1-3" {
			t.Fatalf("file %q: B key = %q, want \"7199-1-3\"", data, k)
		}
	}
}
