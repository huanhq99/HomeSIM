//go:build !darwin || !cgo

package main

import (
	"errors"
	"time"
)

type usbAT struct{}

const (
	djiUSBVendorID      = 0x2ca3
	djiUSBProductID     = 0x4006
	quectelUSBVendorID  = 0x2c7c
	quectelUSBProductID = 0x0125
)

// Keep non-macOS builds able to identify a supported module in status and
// setup code, while deliberately leaving USB transport unavailable.
func isSupportedUSBModuleIdentity(vendorID, productID int) bool {
	return (vendorID == djiUSBVendorID && productID == djiUSBProductID) ||
		(vendorID == quectelUSBVendorID && productID == quectelUSBProductID)
}

func openDJIUSBAT() (*usbAT, error) {
	return nil, errors.New("USB AT requires macOS cgo build with libusb")
}

func openDJIUSBATAtLocation(_ uint32) (*usbAT, error) {
	return nil, errors.New("USB AT requires macOS cgo build with libusb")
}

func (u *usbAT) Close() {}

func (u *usbAT) Command(_ string, _ time.Duration) (string, error) {
	return "", errors.New("USB AT is unavailable in this build")
}

func (u *usbAT) WithExclusiveCommands(_ func(usbATCommandFunc) error) error {
	return errors.New("USB AT is unavailable in this build")
}

func (u *usbAT) CommandWithPrompt(_ string, _ []byte, _ time.Duration) (string, error) {
	return "", errors.New("USB AT is unavailable in this build")
}

func (u *usbAT) Description() string {
	return "USB AT (unavailable)"
}

func (u *usbAT) LocationID() uint32 { return 0 }

func (u *usbAT) PhysicalIdentity() usbATPhysicalIdentity { return usbATPhysicalIdentity{} }
