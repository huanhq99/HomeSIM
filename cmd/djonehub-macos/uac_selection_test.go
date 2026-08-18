package main

import (
	"errors"
	"testing"
)

func observedQDC507UACDescriptors() []usbUACAlternateDescriptor {
	return []usbUACAlternateDescriptor{
		{InterfaceNumber: 5, AlternateSetting: 0, Class: 0x01, Subclass: 0x01},
		{InterfaceNumber: 6, AlternateSetting: 0, Class: 0x01, Subclass: 0x02},
		{InterfaceNumber: 6, AlternateSetting: 1, Class: 0x01, Subclass: 0x02,
			Endpoints: []usbADBEndpointDescriptor{{Address: 0x05, Attributes: 0x01}}},
		{InterfaceNumber: 7, AlternateSetting: 0, Class: 0x01, Subclass: 0x02},
		{InterfaceNumber: 7, AlternateSetting: 1, Class: 0x01, Subclass: 0x02,
			Endpoints: []usbADBEndpointDescriptor{{Address: 0x86, Attributes: 0x01}}},
	}
}

func TestSelectDirectUACLayoutAcceptsObservedFullDuplexDescriptors(t *testing.T) {
	layout, err := selectDirectUACLayout(observedQDC507UACDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	if layout.ControlInterface != 5 || layout.OutputInterface != 6 || layout.InputInterface != 7 ||
		layout.OutputEndpoint != 0x05 || layout.InputEndpoint != 0x86 {
		t.Fatalf("layout = %#v", layout)
	}
}

func TestSelectDirectUACLayoutFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		edit func([]usbUACAlternateDescriptor) []usbUACAlternateDescriptor
		want error
	}{
		{name: "missing control", edit: func(in []usbUACAlternateDescriptor) []usbUACAlternateDescriptor { return in[1:] }, want: errUSBDirectUACNotFound},
		{name: "missing input", edit: func(in []usbUACAlternateDescriptor) []usbUACAlternateDescriptor { return in[:3] }, want: errUSBDirectUACNotFound},
		{name: "bulk is not audio", edit: func(in []usbUACAlternateDescriptor) []usbUACAlternateDescriptor {
			in[2].Endpoints[0].Attributes = 0x02
			return in
		}, want: errUSBDirectUACNotFound},
		{name: "endpoint zero rejected", edit: func(in []usbUACAlternateDescriptor) []usbUACAlternateDescriptor {
			in[2].Endpoints[0].Address = 0
			return in
		}, want: errUSBDirectUACNotFound},
		{name: "duplicate input ambiguous", edit: func(in []usbUACAlternateDescriptor) []usbUACAlternateDescriptor {
			return append(in, usbUACAlternateDescriptor{InterfaceNumber: 8, AlternateSetting: 1, Class: 1, Subclass: 2,
				Endpoints: []usbADBEndpointDescriptor{{Address: 0x88, Attributes: 1}}})
		}, want: errUSBDirectUACAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptors := observedQDC507UACDescriptors()
			_, err := selectDirectUACLayout(test.edit(descriptors))
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}
}
