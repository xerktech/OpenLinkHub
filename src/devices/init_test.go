package devices

import (
	"OpenLinkHub/src/common"
	"errors"
	"sync"
	"testing"

	hid "github.com/sstallion/go-hid"
)

const usbRoot = "/devices/pci0000:00/0000:00:14.0/usb1/"

// fakePSUs stands in for HID enumeration, the sysfs USB port lookup and PSU initialization,
// so startup and InitManual can be driven with identical serial-less PSUs (HX1500i, product 7199).
type fakePSUs struct {
	mu      sync.Mutex
	plugged []string                 // hidraw names, in enumeration order
	ports   map[string]string        // hidraw name -> USB port
	hold    map[string]chan struct{} // HID path -> closed when its initialization may finish
	keys    map[string]string        // HID path -> key it was initialized with
}

func useFakePSUs(t *testing.T) *fakePSUs {
	t.Helper()
	f := &fakePSUs{ports: map[string]string{}, hold: map[string]chan struct{}{}, keys: map[string]string{}}
	usePlainKeyPorts(t)

	oldEnumerate, oldDevPath, oldNode, oldPSU := hidEnumerate, usbDevPathOf, hidNodeExists, deviceRegisterMap[7199]
	oldDevices, oldList, oldPaths, oldInterface := devices, deviceList, usbDevPaths, interfaceId
	devices, deviceList, usbDevPaths = map[string]*common.Device{}, map[string]Device{}, map[string]usbDevice{}
	t.Cleanup(func() {
		initWG.Wait()
		hidEnumerate, usbDevPathOf, hidNodeExists, deviceRegisterMap[7199] = oldEnumerate, oldDevPath, oldNode, oldPSU
		devices, deviceList, usbDevPaths, interfaceId = oldDevices, oldList, oldPaths, oldInterface
	})

	hidEnumerate = func(vid, pid uint16, fn hid.EnumFunc) error {
		f.mu.Lock()
		plugged := append([]string(nil), f.plugged...)
		f.mu.Unlock()
		if vid != vendorId || (pid != hid.ProductIDAny && pid != 7199) {
			return nil
		}
		for _, h := range plugged {
			if err := fn(&hid.DeviceInfo{Path: "/dev/" + h, VendorID: vid, ProductID: 7199}); err != nil {
				return err
			}
		}
		return nil
	}
	hidNodeExists = func(path string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, h := range f.plugged {
			if "/dev/"+h == path {
				return true
			}
		}
		return false
	}
	usbDevPathOf = func(hidraw string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if port, ok := f.ports[hidraw]; ok {
			return usbRoot + port, nil
		}
		return "", errors.New("usb device node not found")
	}
	deviceRegisterMap[7199] = Product{Name: "HX1500i", DeviceRegister: func(vid, pid uint16, key, path string) *common.Device {
		f.mu.Lock()
		f.keys[path] = key
		hold := f.hold[path]
		f.mu.Unlock()
		if hold != nil {
			<-hold
		}
		return &common.Device{ProductType: common.ProductTypePSUHid, Serial: "serial-" + key, Instance: &struct{}{}}
	}}
	return f
}

// plug adds a PSU on a USB port, or on an unresolvable one when port is empty; with hold, its initialization waits until hold is closed.
func (f *fakePSUs) plug(hidraw, port string, hold chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plugged = append(f.plugged, hidraw)
	if len(port) > 0 {
		f.ports[hidraw] = port
	}
	if hold != nil {
		f.hold["/dev/"+hidraw] = hold
	}
}

// unplug removes a PSU the way the hotplug monitor does
func (f *fakePSUs) unplug(t *testing.T, hidraw string) {
	t.Helper()
	f.mu.Lock()
	port, ok := f.ports[hidraw]
	if !ok {
		port = "9-9" // udev still reports a USB path, but it was never resolved from the HID node
	}
	for i, h := range f.plugged {
		if h == hidraw {
			f.plugged = append(f.plugged[:i], f.plugged[i+1:]...)
			break
		}
	}
	delete(f.ports, hidraw)
	f.mu.Unlock()
	serial, ok := TakeSerialByDevPath(usbRoot+port, 7199)
	if !ok {
		t.Fatalf("no registered device on port %s", port)
	}
	mutex.Lock()
	delete(devices, serial)
	mutex.Unlock()
}

func (f *fakePSUs) key(hidraw string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys["/dev/"+hidraw]
}

func wantKey(t *testing.T, f *fakePSUs, hidraw, want string) {
	t.Helper()
	if got := f.key(hidraw); got != want {
		t.Fatalf("%s initialized with key %q, want %q", hidraw, got, want)
	}
	mutex.Lock()
	_, ok := devices["serial-"+want]
	mutex.Unlock()
	if !ok {
		t.Fatalf("%s (key %q) is not listed", hidraw, want)
	}
}

// restart forgets every device and pinned port, as if the service was restarted
func restart() {
	mutex.Lock()
	devices, deviceList, usbDevPaths = map[string]*common.Device{}, map[string]Device{}, map[string]usbDevice{}
	mutex.Unlock()
	plainKeyMutex.Lock()
	plainKeyPorts = map[string]string{}
	plainKeyMutex.Unlock()
}

// startup does what Init does for HID devices, without touching real USB devices or the config folder
func startup() {
	initHIDDevices()
	initializeDeviceList()
	initWG.Wait()
}

// Two identical PSUs found at startup get distinct keys, and each is found by its USB port.
func TestInitKeepsIdenticalPsusApart(t *testing.T) {
	f := useFakePSUs(t)
	f.plug("hidraw0", "1-2", nil)
	f.plug("hidraw1", "1-3", nil)
	startup()

	wantKey(t, f, "hidraw0", "7199")
	wantKey(t, f, "hidraw1", "7199-1-3")
	if serial, ok := TakeSerialByDevPath(usbRoot+"1-3", 7199); !ok || serial != "serial-7199-1-3" {
		t.Fatalf("port 1-3 maps to (%q, %v), want (\"serial-7199-1-3\", true)", serial, ok)
	}
}

// A PSU hotplugged next to an identical one gets a key of its own, and a replugged PSU gets its
// old key back.
func TestInitManualKeepsIdenticalPsusApart(t *testing.T) {
	f := useFakePSUs(t)
	f.plug("hidraw0", "1-2", nil)
	startup()

	f.plug("hidraw1", "1-3", nil)
	InitManual(7199, "", usbRoot+"1-3")
	initWG.Wait()
	wantKey(t, f, "hidraw1", "7199-1-3")

	f.unplug(t, "hidraw0")
	f.plug("hidraw2", "1-2", nil)
	InitManual(7199, "", usbRoot+"1-2")
	initWG.Wait()
	wantKey(t, f, "hidraw2", "7199")
}

// A PSU plugged in while an identical one is still initializing does not take its key.
func TestInitManualWhileIdenticalPsuInitializes(t *testing.T) {
	f := useFakePSUs(t)
	startup()

	hold := make(chan struct{})
	f.plug("hidraw0", "1-2", hold)
	InitManual(7199, "", usbRoot+"1-2")
	f.plug("hidraw1", "1-3", nil)
	InitManual(7199, "", usbRoot+"1-3")
	close(hold)
	initWG.Wait()

	wantKey(t, f, "hidraw0", "7199")
	wantKey(t, f, "hidraw1", "7199-1-3")
}

// Once two identical PSUs were seen, each keeps its key across restarts and replugs, whichever
// is found or plugged in first.
func TestIdenticalPsusKeepKeysAcrossOrder(t *testing.T) {
	f := useFakePSUs(t)
	f.plug("hidraw0", "1-2", nil)
	f.plug("hidraw1", "1-3", nil)
	startup()
	wantKey(t, f, "hidraw0", "7199")
	wantKey(t, f, "hidraw1", "7199-1-3")

	// Restart with only the second PSU plugged in
	f.unplug(t, "hidraw0")
	f.unplug(t, "hidraw1")
	restart()
	f.plug("hidraw2", "1-3", nil)
	startup()
	wantKey(t, f, "hidraw2", "7199-1-3")

	// Restart with the second PSU found first
	f.unplug(t, "hidraw2")
	restart()
	f.plug("hidraw3", "1-3", nil)
	f.plug("hidraw4", "1-2", nil)
	startup()
	wantKey(t, f, "hidraw3", "7199-1-3")
	wantKey(t, f, "hidraw4", "7199")

	// Both unplugged, the second replugged first
	f.unplug(t, "hidraw3")
	f.unplug(t, "hidraw4")
	f.plug("hidraw5", "1-3", nil)
	InitManual(7199, "", usbRoot+"1-3")
	initWG.Wait()
	wantKey(t, f, "hidraw5", "7199-1-3")
	f.plug("hidraw6", "1-2", nil)
	InitManual(7199, "", usbRoot+"1-2")
	initWG.Wait()
	wantKey(t, f, "hidraw6", "7199")
}

// A PSU whose USB port can't be resolved still holds its key, so an identical PSU hotplugged
// next to it gets a key of its own instead of replacing it in the device list.
func TestInitManualNextToPortlessPsu(t *testing.T) {
	f := useFakePSUs(t)
	f.plug("hidraw0", "", nil)
	startup()
	wantKey(t, f, "hidraw0", "7199")

	f.plug("hidraw1", "1-3", nil)
	InitManual(7199, "", usbRoot+"1-3")
	initWG.Wait()
	wantKey(t, f, "hidraw1", "7199-1-3")
	wantKey(t, f, "hidraw0", "7199")
	if _, pinned := plainKeyPorts["7199"]; pinned {
		t.Fatal("plain key pinned to a PSU whose USB port is unknown")
	}
}

// A PSU whose USB port can't be resolved is found on unplug, so it leaves no ghost entry and
// frees its key.
func TestUnplugPortlessPsu(t *testing.T) {
	f := useFakePSUs(t)
	f.plug("hidraw0", "", nil)
	startup()
	wantKey(t, f, "hidraw0", "7199")

	f.unplug(t, "hidraw0")
	mutex.Lock()
	left := len(devices)
	mutex.Unlock()
	if left != 0 {
		t.Fatalf("%d devices listed after unplug, want 0", left)
	}

	f.plug("hidraw1", "1-3", nil)
	InitManual(7199, "", usbRoot+"1-3")
	initWG.Wait()
	wantKey(t, f, "hidraw1", "7199")
}

// A port-less PSU whose unplug was missed, replugged on its old HID node, keeps its key.
func TestReplugPortlessPsuAfterMissedUnplug(t *testing.T) {
	f := useFakePSUs(t)
	f.plug("hidraw0", "", nil)
	startup()
	wantKey(t, f, "hidraw0", "7199")

	// Unplugged without the monitor noticing, then plugged back on a port that now resolves
	f.mu.Lock()
	f.plugged = nil
	f.mu.Unlock()
	f.plug("hidraw0", "1-3", nil)
	InitManual(7199, "", usbRoot+"1-3")
	initWG.Wait()
	wantKey(t, f, "hidraw0", "7199")
}
