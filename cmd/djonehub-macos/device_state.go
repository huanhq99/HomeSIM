package main

import (
	"errors"
	"time"
)

type deviceStateSnapshot struct {
	Port           string
	DiscoveryError string
	USBDevice      *usbDeviceStatus
}

// usbATExecution identifies the exact lifecycle object which executed a
// mutation. It is intentionally pointer-based: a replacement at the same USB
// location must not be detached by delayed cleanup from the old handle.
type usbATExecution struct {
	device *usbAT
}

func cloneUSBDeviceStatus(device *usbDeviceStatus) *usbDeviceStatus {
	if device == nil {
		return nil
	}
	cloned := *device
	cloned.Interfaces = append([]usbInterfaceStatus(nil), device.Interfaces...)
	return &cloned
}

func (a *app) snapshotDeviceState() deviceStateSnapshot {
	a.deviceStateMu.RLock()
	defer a.deviceStateMu.RUnlock()
	return deviceStateSnapshot{
		Port:           a.port,
		DiscoveryError: a.discoveryError,
		USBDevice:      cloneUSBDeviceStatus(a.usbDevice),
	}
}

func (a *app) setDeviceConnectionState(port, discoveryError string) {
	a.deviceStateMu.Lock()
	a.port = port
	a.discoveryError = discoveryError
	a.deviceStateMu.Unlock()
}

func (a *app) setUSBDeviceState(device *usbDeviceStatus) {
	a.deviceStateMu.Lock()
	a.usbDevice = cloneUSBDeviceStatus(device)
	a.deviceStateMu.Unlock()
}

func (a *app) setDetachedDeviceState() {
	a.deviceStateMu.Lock()
	a.port = "未检测到 DJI USB 设备"
	a.discoveryError = "DJI USB device is not connected"
	a.usbDevice = nil
	a.deviceStateMu.Unlock()
}

func (a *app) currentUSBATSnapshot() *usbAT {
	a.usbATOpenMu.Lock()
	device := a.usbAT
	a.usbATOpenMu.Unlock()
	return device
}

func (a *app) hasUSBAT() bool { return a.currentUSBATSnapshot() != nil }

func (a *app) isCurrentUSBAT(device *usbAT, locationID uint32) bool {
	if device == nil {
		return false
	}
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	return a.usbAT == device && device.LocationID() == locationID
}

func (a *app) currentUSBATLocation() (uint32, bool) {
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	device := a.usbAT
	if device == nil {
		return 0, false
	}
	locationID := device.LocationID()
	return locationID, locationID != 0
}

func (a *app) currentUSBATIdentitySnapshot() (*usbAT, usbATPhysicalIdentity, bool) {
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	device := a.usbAT
	if device == nil {
		return nil, usbATPhysicalIdentity{}, false
	}
	identity := device.PhysicalIdentity()
	return device, identity, identity.Location != 0 && identity.VendorID > 0 && identity.ProductID > 0
}

func (a *app) markUSBATExecutionDetached(execution usbATExecution, reason string) {
	if execution.device == nil {
		return
	}
	a.markUSBATDeviceDetached(execution.device, reason)
}

// commandUSBAT snapshots one exact handle. A detach can close that handle only
// after its own command mutex becomes available, and this call never migrates
// mid-command to a newly enumerated module.
func (a *app) commandUSBAT(command string, timeout time.Duration) (string, error) {
	device := a.currentUSBATSnapshot()
	if device == nil {
		return "", errors.New("USB AT device is not open")
	}
	response, err := device.Command(command, timeout)
	a.resetUSBATDeviceIfGone(device, err)
	return response, err
}

func (a *app) commandUSBATWithPrompt(command string, followUp []byte, timeout time.Duration) (string, error) {
	device := a.currentUSBATSnapshot()
	if device == nil {
		return "", errors.New("USB AT device is not open")
	}
	response, err := device.CommandWithPrompt(command, followUp, timeout)
	a.resetUSBATDeviceIfGone(device, err)
	return response, err
}
