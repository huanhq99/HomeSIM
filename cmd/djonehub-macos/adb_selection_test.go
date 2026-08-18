package main

import (
	"errors"
	"testing"
)

func adbEndpoint(address, transferType byte) usbADBEndpointDescriptor {
	return usbADBEndpointDescriptor{Address: address, Attributes: transferType}
}

func adbAlternate(
	interfaceNumber, alternateSetting, class, subclass, protocol byte,
	endpoints ...usbADBEndpointDescriptor,
) usbADBAlternateDescriptor {
	return usbADBAlternateDescriptor{
		InterfaceNumber:  interfaceNumber,
		AlternateSetting: alternateSetting,
		Class:            class,
		Subclass:         subclass,
		Protocol:         protocol,
		Endpoints:        endpoints,
	}
}

func standardADBAlternate(
	interfaceNumber, alternateSetting byte,
	endpoints ...usbADBEndpointDescriptor,
) usbADBAlternateDescriptor {
	return adbAlternate(
		interfaceNumber,
		alternateSetting,
		usbClassVendorSpecific,
		usbADBSubclass,
		usbADBProtocol,
		endpoints...,
	)
}

func TestQDC507ADBInterfaceSelectionUsesDescriptorNotInterfaceNumber(t *testing.T) {
	for _, interfaceNumber := range []byte{3, 6, 9} {
		t.Run(string(rune('0'+interfaceNumber)), func(t *testing.T) {
			candidate, err := selectQDC507ADBInterface([]usbADBAlternateDescriptor{
				standardADBAlternate(
					interfaceNumber,
					0,
					adbEndpoint(0x83, usbEndpointTransferTypeBulk),
					adbEndpoint(0x04, usbEndpointTransferTypeBulk),
					adbEndpoint(0x85, 0x03), // Extra interrupt endpoint is harmless.
				),
			})
			if err != nil {
				t.Fatalf("select interface %d: %v", interfaceNumber, err)
			}
			if candidate.InterfaceNumber != int(interfaceNumber) ||
				candidate.AlternateSetting != 0 ||
				candidate.EndpointIn != 0x83 || candidate.EndpointOut != 0x04 {
				t.Fatalf("unexpected candidate: %+v", candidate)
			}
		})
	}
}

func TestQDC507ADBInterfaceSelectionRejectsUnsafeDescriptors(t *testing.T) {
	realAudioInterface6 := []usbADBAlternateDescriptor{
		adbAlternate(6, 0, usbClassAudio, usbAudioStreamingSubclass, 0x00),
		adbAlternate(
			6,
			1,
			usbClassAudio,
			usbAudioStreamingSubclass,
			0x00,
			adbEndpoint(0x86, 0x01), // Isochronous Audio Streaming endpoint.
		),
	}

	tests := []struct {
		name       string
		alternates []usbADBAlternateDescriptor
	}{
		{
			name: "no alternate descriptors",
		},
		{
			name:       "real interface 6 audio alt zero and one",
			alternates: realAudioInterface6,
		},
		{
			name: "audio streaming with apparent bulk pair",
			alternates: []usbADBAlternateDescriptor{
				adbAlternate(
					6,
					0,
					usbClassAudio,
					usbAudioStreamingSubclass,
					0x00,
					adbEndpoint(0x81, usbEndpointTransferTypeBulk),
					adbEndpoint(0x02, usbEndpointTransferTypeBulk),
				),
			},
		},
		{
			name: "bulk endpoints split across alternate settings",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(7, 0, adbEndpoint(0x81, usbEndpointTransferTypeBulk)),
				standardADBAlternate(7, 1, adbEndpoint(0x02, usbEndpointTransferTypeBulk)),
			},
		},
		{
			name: "valid pair only on nonzero alternate setting",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(
					7,
					1,
					adbEndpoint(0x81, usbEndpointTransferTypeBulk),
					adbEndpoint(0x02, usbEndpointTransferTypeBulk),
				),
			},
		},
		{
			name: "missing bulk input",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(7, 0, adbEndpoint(0x02, usbEndpointTransferTypeBulk)),
			},
		},
		{
			name: "missing bulk output",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(7, 0, adbEndpoint(0x81, usbEndpointTransferTypeBulk)),
			},
		},
		{
			name: "multiple bulk inputs",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(
					7,
					0,
					adbEndpoint(0x81, usbEndpointTransferTypeBulk),
					adbEndpoint(0x82, usbEndpointTransferTypeBulk),
					adbEndpoint(0x03, usbEndpointTransferTypeBulk),
				),
			},
		},
		{
			name: "multiple bulk outputs",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(
					7,
					0,
					adbEndpoint(0x81, usbEndpointTransferTypeBulk),
					adbEndpoint(0x02, usbEndpointTransferTypeBulk),
					adbEndpoint(0x03, usbEndpointTransferTypeBulk),
				),
			},
		},
		{
			name: "control endpoint zero is not bulk transport",
			alternates: []usbADBAlternateDescriptor{
				standardADBAlternate(
					7,
					0,
					adbEndpoint(0x80, usbEndpointTransferTypeBulk),
					adbEndpoint(0x00, usbEndpointTransferTypeBulk),
				),
			},
		},
		{
			name: "wrong subclass",
			alternates: []usbADBAlternateDescriptor{
				adbAlternate(
					7,
					0,
					usbClassVendorSpecific,
					0x41,
					usbADBProtocol,
					adbEndpoint(0x81, usbEndpointTransferTypeBulk),
					adbEndpoint(0x02, usbEndpointTransferTypeBulk),
				),
			},
		},
		{
			name: "wrong protocol",
			alternates: []usbADBAlternateDescriptor{
				adbAlternate(
					7,
					0,
					usbClassVendorSpecific,
					usbADBSubclass,
					0x02,
					adbEndpoint(0x81, usbEndpointTransferTypeBulk),
					adbEndpoint(0x02, usbEndpointTransferTypeBulk),
				),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := selectQDC507ADBInterface(test.alternates)
			if !errors.Is(err, errUSBADBInterfaceNotFound) {
				t.Fatalf("got %v, want errUSBADBInterfaceNotFound", err)
			}
		})
	}
}

func TestQDC507ADBInterfaceSelectionRejectsMultipleCandidates(t *testing.T) {
	alternates := []usbADBAlternateDescriptor{
		standardADBAlternate(
			5,
			0,
			adbEndpoint(0x81, usbEndpointTransferTypeBulk),
			adbEndpoint(0x02, usbEndpointTransferTypeBulk),
		),
		standardADBAlternate(
			8,
			0,
			adbEndpoint(0x83, usbEndpointTransferTypeBulk),
			adbEndpoint(0x04, usbEndpointTransferTypeBulk),
		),
	}

	_, err := selectQDC507ADBInterface(alternates)
	if !errors.Is(err, errUSBADBInterfaceAmbiguous) {
		t.Fatalf("got %v, want errUSBADBInterfaceAmbiguous", err)
	}
}
