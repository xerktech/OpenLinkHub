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

	oldEnumerate, oldDevPath, oldPSU := hidEnumerate, usbDevPathOf, deviceRegisterMap[7199]
	oldDevices, oldList, oldPaths, oldInterface := devices, deviceList, usbDevPaths, interfaceId
	devices, deviceList, usbDevPaths = map[string]*common.Device{}, map[string]Device{}, map[string]usbDevice{}
	t.Cleanup(func() {
		initWG.Wait()
		hidEnumerate, usbDevPathOf, deviceRegisterMap[7199] = oldEnumerate, oldDevPath, oldPSU
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

// plug adds a PSU on a USB port; with hold, its initialization waits until hold is closed.
func (f *fakePSUs) plug(hidraw, port string, hold chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plugged = append(f.plugged, hidraw)
	f.ports[hidraw] = port
	if hold != nil {
		f.hold["/dev/"+hidraw] = hold
	}
}

// unplug removes a PSU the way the hotplug monitor does
func (f *fakePSUs) unplug(t *testing.T, hidraw string) {
	t.Helper()
	f.mu.Lock()
	port := f.ports[hidraw]
	for i, h := range f.plugged {
		if h == hidraw {
			f.plugged = append(f.plugged[:i], f.plugged[i+1:]...)
			break
		}
	}
	delete(f.ports, hidraw)
	f.mu.Unlock()
	serial, ok := TakeSerialByDevPath(usbRoot + port)
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

// startup does what Init does for HID devices, without touching real USB devices or the config folder
func startup() {
	listHIDDevices()
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
	if serial, ok := TakeSerialByDevPath(usbRoot + "1-3"); !ok || serial != "serial-7199-1-3" {
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
