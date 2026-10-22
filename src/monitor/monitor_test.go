package monitor

import (
	"errors"
	"github.com/godbus/dbus/v5"
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
// real hardware. It registers devices the way devices does: under their USB path when the port
// is known, otherwise as port-less. A port-less key is taken for any product, so only the
// monitor's own filters keep an unrelated removal from taking it.
type fakeDevices struct {
	paths    map[string]string // USB device path -> registered key
	portless []string          // keys of PSUs registered without a known USB port
	inits    []string          // USB device paths passed to devices.InitManual
	stops    []stopped
	devStops int   // devices.Stop calls
	inStops  int   // inputmanager.Stop calls
	exits    []int // os.Exit codes
}

func useFakeDevices(t *testing.T) *fakeDevices {
	t.Helper()
	f := &fakeDevices{paths: map[string]string{}}
	oldRoot, oldPause, oldInit, oldTake, oldStop, oldSleep := sysRoot, pause, initManual, takeSerialByDevPath, stopDirty, sleep.Load()
	oldStopDevices, oldStopInputs, oldExit := stopDevices, stopInputs, exit
	t.Cleanup(func() {
		sysRoot, pause, initManual, takeSerialByDevPath, stopDirty = oldRoot, oldPause, oldInit, oldTake, oldStop
		stopDevices, stopInputs, exit = oldStopDevices, oldStopInputs, oldExit
		sleep.Store(oldSleep)
	})

	sysRoot = t.TempDir()
	sleep.Store(false)
	pause = func(time.Duration) {}
	initManual = func(productId uint16, key, devPath string) {
		f.inits = append(f.inits, devPath)
	}
	takeSerialByDevPath = func(devPath string, productId uint16) (string, bool) {
		if key, ok := f.paths[devPath]; ok {
			delete(f.paths, devPath)
			return key, true
		}
		if len(f.portless) > 0 {
			key := f.portless[0]
			f.portless = f.portless[1:]
			return key, true
		}
		return "", false
	}
	stopDirty = func(serial string, productId uint16) {
		f.stops = append(f.stops, stopped{serial, productId})
	}
	stopDevices = func() { f.devStops++ }
	stopInputs = func() { f.inStops++ }
	exit = func(code int) { f.exits = append(f.exits, code) }
	return f
}

// sysfs will create the sysfs attributes of a serial-less USB device plugged at port.
// vid and pid are in hex, as sysfs shows them.
func sysfs(t *testing.T, port, vid, pid string) string {
	t.Helper()
	devPath := usbRoot + port
	dir := filepath.Join(sysRoot, devPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"idVendor": vid + "\n", "idProduct": pid + "\n"} {
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
	devPath := sysfs(t, "1-4", "1b1c", "1c1f")

	handleUEvent(uevent("add", devPath), cache)
	if !slices.Equal(f.inits, []string{devPath}) {
		t.Fatalf("InitManual calls = %v, want [%s]", f.inits, devPath)
	}
	if got := cache[devPath]; got != (USBInfo{VendorID: vendorId, ProductID: 7199}) {
		t.Fatalf("cache[%s] = %+v, want the PSU", devPath, got)
	}
}

func TestAddIgnoresUnsupportedDevice(t *testing.T) {
	tests := map[string]func(t *testing.T, msg map[string]string){
		"during suspend": func(*testing.T, map[string]string) { sleep.Store(true) },
		"other vendor":   func(t *testing.T, _ map[string]string) { sysfs(t, "1-4", "046d", "1c1f") },
		"excluded":       func(t *testing.T, _ map[string]string) { sysfs(t, "1-4", "1b1c", "2a00") },
		"usb_interface":  func(_ *testing.T, msg map[string]string) { msg["DEVTYPE"] = "usb_interface" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := useFakeDevices(t)
			cache := map[string]USBInfo{}
			msg := uevent("add", sysfs(t, "1-4", "1b1c", "1c1f"))
			change(t, msg)
			handleUEvent(msg, cache)
			if len(f.inits) != 0 || len(cache) != 0 {
				t.Fatalf("InitManual calls = %v, cache = %v, want none", f.inits, cache)
			}
		})
	}
}

func TestRemoveCachedPsu(t *testing.T) {
	f := useFakeDevices(t)
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}}
	f.paths[devPath] = "7199-1-4"
	f.portless = []string{"7199"}

	handleUEvent(uevent("remove", devPath), cache)
	wantStops(t, f, stopped{"7199-1-4", 7199})
	if _, ok := cache[devPath]; ok {
		t.Fatal("removed PSU is still cached")
	}
	if len(f.portless) != 1 {
		t.Fatal("a cached removal took the port-less PSU's key")
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

func TestRemoveCachedDeviceUnderUsbSerial(t *testing.T) {
	f := useFakeDevices(t)
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199, Serial: "ABC123"}}

	handleUEvent(uevent("remove", devPath), cache)
	wantStops(t, f, stopped{"ABC123", 7199})
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
		"other vendor":     "46d/1c1f/100",
		"excluded product": "1b1c/2a00/100",
		"no product":       "",
	}
	for name, product := range tests {
		t.Run(name, func(t *testing.T) {
			f := useFakeDevices(t)
			f.portless = []string{"7199"}
			msg := uevent("remove", usbRoot+"1-4")
			msg["PRODUCT"] = product
			handleUEvent(msg, map[string]USBInfo{})
			wantStops(t, f)
			if len(f.portless) != 1 {
				t.Fatal("an unknown device's removal took the port-less PSU's key")
			}
		})
	}
}

func TestRemoveWithNothingToTakeIsIgnored(t *testing.T) {
	f := useFakeDevices(t)
	handleUEvent(uevent("remove", usbRoot+"1-4"), map[string]USBInfo{})
	wantStops(t, f)
}

func TestRemoveWithoutDevPathIsIgnored(t *testing.T) {
	f := useFakeDevices(t)
	f.portless = []string{"7199"}
	handleUEvent(uevent("remove", ""), map[string]USBInfo{})
	wantStops(t, f)
}

func TestRemoveDuringSuspendIsIgnored(t *testing.T) {
	f := useFakeDevices(t)
	devPath := usbRoot + "1-4"
	cache := map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}}
	sleep.Store(true)

	handleUEvent(uevent("remove", devPath), cache)
	wantStops(t, f)
}

// The suspend flag is written by the D-Bus goroutine while uevents are handled on another;
// run with -race to catch a regression to an unsynchronized flag.
func TestSuspendFlagSetDuringUEvent(t *testing.T) {
	useFakeDevices(t)
	done := make(chan struct{})
	go func() {
		sleep.Store(true)
		close(done)
	}()
	handleUEvent(uevent("remove", usbRoot+"1-4"), map[string]USBInfo{})
	<-done
}

// prepareForSleep will return a login1 PrepareForSleep signal carrying body.
func prepareForSleep(body ...interface{}) *dbus.Signal {
	return &dbus.Signal{Name: "org.freedesktop.login1.Manager.PrepareForSleep", Body: body}
}

func TestSuspendSignalSetsFlagAndStopsDevices(t *testing.T) {
	f := useFakeDevices(t)

	handleSleepSignal(prepareForSleep(true))
	if !sleep.Load() {
		t.Error("suspend flag not set")
	}
	if f.devStops != 1 || f.inStops != 1 || len(f.exits) != 0 {
		t.Errorf("devices.Stop = %d, inputmanager.Stop = %d, exits = %v; want one of each stop and no exit", f.devStops, f.inStops, f.exits)
	}

	// A device unplugged while suspended is then left alone
	devPath := usbRoot + "1-4"
	handleUEvent(uevent("remove", devPath), map[string]USBInfo{devPath: {VendorID: vendorId, ProductID: 7199}})
	wantStops(t, f)
}

func TestResumeSignalClearsFlagAndExits(t *testing.T) {
	f := useFakeDevices(t)
	sleep.Store(true)

	handleSleepSignal(prepareForSleep(false))
	if sleep.Load() {
		t.Error("suspend flag still set after resume")
	}
	if f.devStops+f.inStops != 0 || !slices.Equal(f.exits, []int{1}) {
		t.Errorf("stop calls = %d, exits = %v; want no stops and exit(1)", f.devStops+f.inStops, f.exits)
	}
}

func TestNonBoolSleepSignalClearsFlag(t *testing.T) {
	f := useFakeDevices(t)
	sleep.Store(true)

	handleSleepSignal(prepareForSleep("true"))
	if sleep.Load() {
		t.Error("suspend flag still set after a non-bool signal")
	}
	if f.devStops+f.inStops != 0 || len(f.exits) != 0 {
		t.Errorf("stop calls = %d, exits = %v; want neither", f.devStops+f.inStops, f.exits)
	}
}

func TestEmptySleepSignalKeepsFlag(t *testing.T) {
	useFakeDevices(t)
	sleep.Store(true)

	handleSleepSignal(prepareForSleep())
	if !sleep.Load() {
		t.Error("suspend flag cleared by a signal with no body")
	}
}

func TestOnlyLogindSleepSignalIsTrusted(t *testing.T) {
	const owner = ":1.4"
	logind := func() *dbus.Signal {
		signal := prepareForSleep(true)
		signal.Sender, signal.Path = owner, logindPath
		return signal
	}
	if !fromLogind(logind(), owner) {
		t.Error("PrepareForSleep from logind rejected")
	}

	for name, change := range map[string]func(*dbus.Signal){
		"another peer":      func(s *dbus.Signal) { s.Sender = ":1.99" },
		"well-known sender": func(s *dbus.Signal) { s.Sender = logindName },
		"no sender":         func(s *dbus.Signal) { s.Sender = "" },
		"another path":      func(s *dbus.Signal) { s.Path = "/org/freedesktop/login1/session/self" },
		"another member":    func(s *dbus.Signal) { s.Name = "org.freedesktop.login1.Manager.PrepareForShutdown" },
		"another interface": func(s *dbus.Signal) { s.Name = "org.example.Manager.PrepareForSleep" },
	} {
		t.Run(name, func(t *testing.T) {
			signal := logind()
			change(signal)
			if fromLogind(signal, owner) {
				t.Errorf("signal %+v trusted", signal)
			}
		})
	}

	// No logind on the bus: an empty owner must not match an empty sender
	signal := logind()
	signal.Sender = ""
	if fromLogind(signal, "") {
		t.Error("signal trusted with logind absent")
	}
}

// watch will run watchSleepSignals over signals, with logind owned by each name of owners in turn.
func watch(owners []string, signals ...*dbus.Signal) {
	ch := make(chan *dbus.Signal, len(signals))
	for _, signal := range signals {
		ch <- signal
	}
	close(ch)
	watchSleepSignals(ch, func() (string, error) {
		if len(owners) == 0 {
			return "", errors.New("org.freedesktop.login1 has no owner")
		}
		owner := owners[0]
		owners = owners[1:]
		return owner, nil
	})
}

// sleepSignal will return a PrepareForSleep signal on logind's path from sender.
func sleepSignal(sender string, sleeping bool) *dbus.Signal {
	signal := prepareForSleep(sleeping)
	signal.Sender, signal.Path = sender, logindPath
	return signal
}

func TestWatchIgnoresSpoofedSleepSignals(t *testing.T) {
	f := useFakeDevices(t)

	watch([]string{":1.4", ":1.4"}, sleepSignal(":1.99", true), sleepSignal(":1.99", false))
	watch(nil, sleepSignal(":1.4", true), sleepSignal(":1.4", false))
	if sleep.Load() || f.devStops+f.inStops != 0 || len(f.exits) != 0 {
		t.Errorf("suspend = %v, stop calls = %d, exits = %v; want spoofs ignored", sleep.Load(), f.devStops+f.inStops, f.exits)
	}
}

func TestWatchFollowsLogindOwner(t *testing.T) {
	f := useFakeDevices(t)

	// logind restarts between the signals and comes back under a new unique name
	watch([]string{":1.4", ":1.50", ":1.50"}, sleepSignal(":1.4", true), sleepSignal(":1.4", false), sleepSignal(":1.50", false))
	if f.devStops != 1 || f.inStops != 1 || !slices.Equal(f.exits, []int{1}) {
		t.Errorf("devices.Stop = %d, inputmanager.Stop = %d, exits = %v; want one stop, then exit(1) from the new owner only", f.devStops, f.inStops, f.exits)
	}
}
