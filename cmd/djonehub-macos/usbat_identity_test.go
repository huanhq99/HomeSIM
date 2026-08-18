package main

import "testing"

func TestSupportedUSBModuleIdentity(t *testing.T) {
	tests := []struct {
		name    string
		vendor  int
		product int
		want    bool
	}{
		{name: "DJI default", vendor: 0x2ca3, product: 0x4006, want: true},
		{name: "Quectel UAC", vendor: 0x2c7c, product: 0x0125, want: true},
		{name: "unsupported product", vendor: 0x2ca3, product: 0x9999, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSupportedUSBModuleIdentity(tc.vendor, tc.product); got != tc.want {
				t.Fatalf("isSupportedUSBModuleIdentity(%04x:%04x) = %t, want %t", tc.vendor, tc.product, got, tc.want)
			}
		})
	}
}
