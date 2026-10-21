package monitor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const usbRoot = "/devices/pci0000:00/0000:00:14.0/usb1/"

// stopped is one devices.StopDirty call: the key the device is removed under and its product.
type stopped struct {
	serial    string
	productId uint16
}

// fakeDevices stands in for sysfs and the devices package, so uevents can be handled without
// real hardware. It registers PSUs (HX1500i, product 7199 = 0x1c1f) the way devices does:
// under their USB path when the port is known, otherwise as port-less.
type fakeDevices struct {
	paths    map[string]string // USB device path -> registered key
	portless []string          // keys of PSUs registered without a known USB port
	inits    []string          // USB device paths passed to devices.InitManual
	stops    []stopped
}

func useFakeDevices(t *testing.T) *fakeDevices {
	t.Helper()
	f := &fakeDevices{paths: map[string]string{}}
	oldRoot, oldPause, oldInit, oldTake, oldStop, oldSleep := sysRoot, pause, initManual, takeSerialByDevPath, stopDirty, sleep
	t.Cleanup(func() {
		sysRoot, pause, initManual, takeSerialByDevPath, stopDirty, sleep = oldRoot, oldPause, oldInit, oldTake, oldStop, oldSleep
	})

	sysRoot = t.TempDir()
	sleep = false
	pause = func(time.Duration) {}
	initManual = func(productId uint16, key, devPath string) {
		f.inits = append(f.inits, devPath)
	}
	takeSerialByDevPath = func(devPath string, productId uint16) (string, bool) {
		if key, ok := f.paths[devPath]; ok {
			delete(f.paths, devPath)
			return key, true
		}
		if productId == 7199 && len(f.portless) > 0 {
			key := f.portless[0]
			f.portless = f.portless[1:]
			return key, true
		}
		return "", false
	}
	stopDirty = func(serial string, productId uint16) {
		f.stops = append(f.stops, stopped{serial, productId})
	}
	return f
}

// sysfs will create the sysfs attributes of a serial-less PSU plugged at port.
func (f *fakeDevices) sysfs(t *testing.T, port string) string {
	t.Helper()
	devPath := usbRoot + port
	dir := filepath.Join(sysRoot, devPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"idVendor": "1b1c\n", "idProduct": "1c1f\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return devPath
}

// uevent will return a kernel usb_device uevent for a PSU at devPath, as common.ParseUEvent parses it.
func uevent(action, devPath string) map[string]string {
	return map[string]string{
		"ACTION":    action,
		"DEVPATH":   devPath,
		"SUBSYSTEM": "usb",
		"DEVTYPE":   "usb_device",
		"PRODUCT":   "1b1c/1c1f/100", // vendor/product/bcdDevice in unpadded hex
	}
}

func wantStops(t *testing.T, f *fakeDevices, want ...stopped) {
	t.Helper()
	if !slices.Equal(f.stops, want) {
		t.Fatalf("StopDirty calls = %v, want %v", f.stops, want)
	}
}

func TestAddInitializesAndCachesPsu(t *testing.T) {
	f := useFakeDevices(t)
	cache := map[string]USBInfo{}
	devPath := f.sysfs(t, "1-4")

	handleUEvent(uevent("add", devPath), cache)
	if !slices.Equal(f.inits, []string{devPath}) {
		t.Fatalf("InitManual calls = %v, want [%s]", f.inits, devPath)
	}
	if got := cache[devPath]; got != (USBInfo{VendorID: vendorId, ProductID: 7199}) {
		t.Fatalf("cache[%s] = %+v, want the PSU", devPath, got)
	}
}

func TestRemoveCachedPsu(t *testing.T) {
	f := useFakeDevices(t)
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}}
	f.paths[devPath] = "7199-1-4"

	handleUEvent(uevent("remove", devPath), cache)
	wantStops(t, f, stopped{"7199-1-4", 7199})
	if _, ok := cache[devPath]; ok {
		t.Fatal("removed PSU is still cached")
	}
}

func TestRemoveCachedPsuWithoutRegisteredKey(t *testing.T) {
	f := useFakeDevices(t)
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}}

	// No key registered under the path: fall back to the product ID devices keys serial-less PSUs by
	handleUEvent(uevent("remove", devPath), cache)
	wantStops(t, f, stopped{"7199", 7199})
}

func TestRemovePortlessPsu(t *testing.T) {
	f := useFakeDevices(t)
	f.portless = []string{"7199"}

	// Registered without a known USB port, so the startup cache has no entry under its path
	handleUEvent(uevent("remove", usbRoot+"1-4"), map[string]USBInfo{})
	wantStops(t, f, stopped{"7199", 7199})
}

func TestRemoveTwoPortlessPsus(t *testing.T) {
	f := useFakeDevices(t)
	f.portless = []string{"7199", "7199-2"}
	cache := map[string]USBInfo{}

	handleUEvent(uevent("remove", usbRoot+"1-4"), cache)
	handleUEvent(uevent("remove", usbRoot+"1-5"), cache)
	wantStops(t, f, stopped{"7199", 7199}, stopped{"7199-2", 7199})
}

func TestRemoveIgnoresUsbInterface(t *testing.T) {
	f := useFakeDevices(t)
	f.portless = []string{"7199"}
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}}

	msg := uevent("remove", devPath+"/1-4:1.0")
	msg["DEVTYPE"] = "usb_interface"
	handleUEvent(msg, cache)
	wantStops(t, f)
	if len(f.portless) != 1 {
		t.Fatal("an interface removal took the port-less PSU's key")
	}
}

func TestRemoveIgnoresUncachedUnknownDevice(t *testing.T) {
	tests := map[string]string{
		"other vendor":     "46d/c52b/1211",
		"excluded product": "1b1c/2a00/100",
		"no product":       "",
		"no PSU to take":   "1b1c/1c1f/100",
	}
	for name, product := range tests {
		t.Run(name, func(t *testing.T) {
			f := useFakeDevices(t)
			if name != "no PSU to take" {
				f.portless = []string{"7199"}
			}
			msg := uevent("remove", usbRoot+"1-4")
			msg["PRODUCT"] = product
			handleUEvent(msg, map[string]USBInfo{})
			wantStops(t, f)
		})
	}
}

func TestRemoveDuringSuspendIsIgnored(t *testing.T) {
	f := useFakeDevices(t)
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}}
	sleep = true

	handleUEvent(uevent("remove", devPath), cache)
	wantStops(t, f)
}
