//go:build darwin && cgo

package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func installTestUSBAT(a *app, device *usbAT) {
	a.usbATOpenMu.Lock()
	a.usbAT = device
	a.usbATBackoffUntil = time.Time{}
	a.usbATBackoffErr = ""
	a.usbATOpenMu.Unlock()
}

func TestDeviceStateConcurrentDiscoveryAndSnapshots(t *testing.T) {
	var sequence atomic.Uint64
	a := &app{}
	a.discoverUSBDevice = func() *usbDeviceStatus {
		value := sequence.Add(1)
		return &usbDeviceStatus{
			Product:    fmt.Sprintf("QDC-%d", value),
			Vendor:     "Baiwang",
			VendorID:   "2ca3",
			ProductID:  "4006",
			LocationID: fmt.Sprintf("0x%08x", value),
			Interfaces: []usbInterfaceStatus{{Number: int(value), Class: 255}},
		}
	}

	const iterations = 2000
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			for i := 0; i < iterations; i++ {
				switch id % 4 {
				case 0:
					device := a.currentUSBDevice()
					if device != nil && len(device.Interfaces) != 1 {
						t.Errorf("discovery returned torn interfaces: %+v", device)
						return
					}
				case 1:
					a.setDeviceConnectionState(fmt.Sprintf("port-%d", i), fmt.Sprintf("error-%d", i))
				case 2:
					snapshot := a.snapshotDeviceState()
					if snapshot.USBDevice != nil && len(snapshot.USBDevice.Interfaces) == 1 {
						// Mutating a returned snapshot must not alias the stored slice.
						snapshot.USBDevice.Interfaces[0].Class = id
					}
				case 3:
					a.setUSBDeviceState(&usbDeviceStatus{
						Product:    "injected",
						Interfaces: []usbInterfaceStatus{{Number: i, Class: 255}},
					})
				}
			}
		}(worker)
	}
	workers.Wait()

	snapshot := a.snapshotDeviceState()
	if snapshot.USBDevice != nil && len(snapshot.USBDevice.Interfaces) != 1 {
		t.Fatalf("final USB snapshot is torn: %+v", snapshot.USBDevice)
	}
}

func TestUSBATPointerSnapshotsRaceWithReplacement(t *testing.T) {
	a := &app{}
	devices := []*usbAT{
		{locationID: 0x11110000, identity: usbModuleIdentity{vendorID: 0x2ca3, productID: 0x4006}},
		{locationID: 0x22220000, identity: usbModuleIdentity{vendorID: 0x2c7c, productID: 0x0125}},
	}
	installTestUSBAT(a, devices[0])

	const iterations = 5000
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := 0; i < iterations; i++ {
			installTestUSBAT(a, devices[i%len(devices)])
		}
	}()
	for reader := 0; reader < 6; reader++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < iterations; i++ {
				device := a.currentUSBATSnapshot()
				if device == nil {
					continue
				}
				locationID := device.LocationID()
				_, _, _ = moduleVoiceUSBIdentity(device)
				_ = a.isCurrentUSBAT(device, locationID)
				_, _ = a.currentUSBATLocation()
			}
		}()
	}
	workers.Wait()

	// An error from an old handle must never detach the replacement handle.
	stale := devices[0]
	replacement := devices[1]
	installTestUSBAT(a, replacement)
	a.resetUSBATDeviceIfGone(stale, errors.New("LIBUSB_ERROR_NO_DEVICE"))
	if got := a.currentUSBATSnapshot(); got != replacement {
		t.Fatalf("stale handle error detached replacement: got %p want %p", got, replacement)
	}
}

func TestUSBATExecutionDetachNeverRemovesReplacement(t *testing.T) {
	a := &app{}
	oldDevice := &usbAT{locationID: 0x44440000}
	replacement := &usbAT{locationID: 0x44440000}
	installTestUSBAT(a, oldDevice)
	execution := usbATExecution{device: oldDevice}

	installTestUSBAT(a, replacement)
	a.markUSBATExecutionDetached(execution, "delayed CFUN cleanup")
	if got := a.currentUSBATSnapshot(); got != replacement {
		t.Fatalf("delayed exact-handle cleanup detached replacement: got %p want %p", got, replacement)
	}

	// A token for the current lifecycle object still performs the intended
	// detach, even when the replacement reused the same physical location.
	a.markUSBATExecutionDetached(usbATExecution{device: replacement}, "current CFUN cleanup")
	if got := a.currentUSBATSnapshot(); got != nil {
		t.Fatalf("current exact-handle cleanup left handle %p", got)
	}
}

func TestDelayedInventoryDetachNeverRemovesReplacement(t *testing.T) {
	a := &app{}
	oldDevice := &usbAT{locationID: 0x55550000}
	replacement := &usbAT{locationID: 0x55550000}
	installTestUSBAT(a, oldDevice)
	observed := a.currentUSBATSnapshot()

	// Model a replacement opening after an inventory scan captured the old
	// handle but before the scan publishes its disconnected result.
	installTestUSBAT(a, replacement)
	a.markUSBATDeviceDetached(observed, "delayed empty inventory")
	if got := a.currentUSBATSnapshot(); got != replacement {
		t.Fatalf("delayed inventory cleanup detached replacement: got %p want %p", got, replacement)
	}
}

func TestUSBATDetachDoesNotHoldLifecycleLockWhileClosing(t *testing.T) {
	a := &app{}
	device := &usbAT{locationID: 0x33330000}
	installTestUSBAT(a, device)

	// Simulate an in-flight Command holding usbAT.mu. markUSBATDeviceDetached
	// must publish nil under usbATOpenMu and release it before waiting in Close.
	device.mu.Lock()
	detached := make(chan struct{})
	go func() {
		a.markUSBATDeviceDetached(device, "race-test detach")
		close(detached)
	}()

	deadline := time.Now().Add(time.Second)
	for a.currentUSBATSnapshot() != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.currentUSBATSnapshot() != nil {
		device.mu.Unlock()
		t.Fatal("detach kept usbATOpenMu/pointer while waiting for usbAT.mu")
	}
	device.mu.Unlock()
	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("detach did not complete after command lock was released")
	}

	state := a.snapshotDeviceState()
	if state.USBDevice != nil || state.Port != "未检测到 DJI USB 设备" || state.DiscoveryError == "" {
		t.Fatalf("detached state not published atomically through helpers: %+v", state)
	}
}
