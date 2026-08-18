//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework CoreFoundation -framework IOKit
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOBSD.h>
#include <IOKit/IOKitLib.h>
#include <stdlib.h>

static int maccellular_usb_identity_for_bsd(
    const char *bsd_name,
    int *vendor_id,
    int *product_id,
    int *location_id
) {
    CFMutableDictionaryRef matching = IOBSDNameMatching(
        kIOMainPortDefault,
        0,
        bsd_name
    );
    if (matching == NULL) return 0;
    io_service_t interface = IOServiceGetMatchingService(kIOMainPortDefault, matching);
    if (interface == IO_OBJECT_NULL) return 0;

    IOOptionBits options = kIORegistryIterateParents | kIORegistryIterateRecursively;
    CFTypeRef vendor = IORegistryEntrySearchCFProperty(
        interface,
        kIOServicePlane,
        CFSTR("idVendor"),
        kCFAllocatorDefault,
        options
    );
    CFTypeRef product = IORegistryEntrySearchCFProperty(
        interface,
        kIOServicePlane,
        CFSTR("idProduct"),
        kCFAllocatorDefault,
        options
    );
    CFTypeRef location = IORegistryEntrySearchCFProperty(
        interface,
        kIOServicePlane,
        CFSTR("locationID"),
        kCFAllocatorDefault,
        options
    );
    IOObjectRelease(interface);

    int ok = 0;
    if (vendor != NULL && product != NULL && location != NULL &&
        CFGetTypeID(vendor) == CFNumberGetTypeID() &&
        CFGetTypeID(product) == CFNumberGetTypeID() &&
        CFGetTypeID(location) == CFNumberGetTypeID()) {
        int vid = 0;
        int pid = 0;
        int loc = 0;
        if (CFNumberGetValue((CFNumberRef)vendor, kCFNumberIntType, &vid) &&
            CFNumberGetValue((CFNumberRef)product, kCFNumberIntType, &pid) &&
            CFNumberGetValue((CFNumberRef)location, kCFNumberIntType, &loc)) {
            *vendor_id = vid;
            *product_id = pid;
            *location_id = loc;
            ok = 1;
        }
    }
    if (vendor != NULL) CFRelease(vendor);
    if (product != NULL) CFRelease(product);
    if (location != NULL) CFRelease(location);
    return ok;
}
*/
import "C"

import (
	"regexp"
	"unsafe"
)

func isVerifiedDJINetworkInterfaceAtLocation(device string, requiredLocation uint32) bool {
	if requiredLocation == 0 {
		return false
	}
	if !regexp.MustCompile(`^en\d+$`).MatchString(device) {
		return false
	}
	name := C.CString(device)
	defer C.free(unsafe.Pointer(name))
	var vendorID C.int
	var productID C.int
	var locationID C.int
	if C.maccellular_usb_identity_for_bsd(name, &vendorID, &productID, &locationID) == 0 {
		return false
	}
	if !isSupportedUSBModuleIdentity(int(vendorID), int(productID)) {
		return false
	}
	return requiredLocation == uint32(locationID)
}
