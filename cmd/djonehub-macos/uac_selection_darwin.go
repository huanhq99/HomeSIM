//go:build darwin && cgo

package main

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// validateDirectUACUSB checks the active descriptor tree for the exact module
// lifecycle at locationID. It does not claim an Audio interface or interfere
// with CoreAudio.
func validateDirectUACUSB(locationID uint32) error {
	if locationID == 0 {
		return errors.New("UAC validation requires a non-zero USB location")
	}
	var ctx *C.libusb_context
	if rc := C.libusb_init(&ctx); rc != 0 {
		return fmt.Errorf("libusb init: %s", usbErrorName(rc))
	}
	defer C.libusb_exit(ctx)
	handle, identity := openSupportedUSBModuleDeviceAtLocation(ctx, locationID)
	if handle == nil {
		return fmt.Errorf("UAC module at required USB location 0x%08x not found", locationID)
	}
	defer C.libusb_close(handle)
	actualLocation := libusbDarwinLocationID(C.libusb_get_device(handle))
	if identity.vendorID != quectelUSBVendorID || identity.productID != quectelUSBProductID || actualLocation != locationID {
		return fmt.Errorf("UAC validation found unexpected physical identity %s", identity.String())
	}
	device := C.libusb_get_device(handle)
	if device == nil {
		return errors.New("UAC module handle has no libusb device")
	}
	var config *C.struct_libusb_config_descriptor
	if rc := C.libusb_get_active_config_descriptor(device, &config); rc != 0 {
		return fmt.Errorf("get active UAC USB config descriptor: %s", usbErrorName(rc))
	}
	defer C.libusb_free_config_descriptor(config)
	var descriptors []usbUACAlternateDescriptor
	interfaces := unsafe.Slice(config._interface, int(config.bNumInterfaces))
	for _, intf := range interfaces {
		alternates := unsafe.Slice(intf.altsetting, int(intf.num_altsetting))
		for _, alternate := range alternates {
			endpoints := unsafe.Slice(alternate.endpoint, int(alternate.bNumEndpoints))
			converted := make([]usbADBEndpointDescriptor, 0, len(endpoints))
			for _, endpoint := range endpoints {
				converted = append(converted, usbADBEndpointDescriptor{
					Address: byte(endpoint.bEndpointAddress), Attributes: byte(endpoint.bmAttributes),
				})
			}
			descriptors = append(descriptors, usbUACAlternateDescriptor{
				InterfaceNumber: byte(alternate.bInterfaceNumber), AlternateSetting: byte(alternate.bAlternateSetting),
				Class: byte(alternate.bInterfaceClass), Subclass: byte(alternate.bInterfaceSubClass),
				Protocol: byte(alternate.bInterfaceProtocol), Endpoints: converted,
			})
		}
	}
	_, err := selectDirectUACLayout(descriptors)
	return err
}
