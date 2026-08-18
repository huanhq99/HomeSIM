package main

import (
	"errors"
	"fmt"
)

const (
	usbAudioControlSubclass    = 0x01
	usbEndpointTransferTypeIso = 0x01
)

var (
	errUSBDirectUACNotFound  = errors.New("full-duplex USB Audio Class layout not found")
	errUSBDirectUACAmbiguous = errors.New("USB Audio Class layout is ambiguous")
)

// usbUACAlternateDescriptor is deliberately independent of libusb so the
// exact descriptor contract can be tested without opening a physical module.
type usbUACAlternateDescriptor struct {
	InterfaceNumber  byte
	AlternateSetting byte
	Class            byte
	Subclass         byte
	Protocol         byte
	Endpoints        []usbADBEndpointDescriptor
}

type usbUACLayout struct {
	ControlInterface int
	InputInterface   int
	OutputInterface  int
	InputEndpoint    byte
	OutputEndpoint   byte
}

// selectDirectUACLayout accepts the conservative QDC507 UAC layout observed
// after enabling the audio flag: one AudioControl alt 0 plus one input and one
// output AudioStreaming interface. Each stream must expose exactly one
// non-zero isochronous data endpoint in a non-zero alternate setting. Interface
// numbers are returned for diagnostics only and never used as identity.
func selectDirectUACLayout(alternates []usbUACAlternateDescriptor) (usbUACLayout, error) {
	controlSet := map[int]struct{}{}
	inputStreams := map[int]byte{}
	outputStreams := map[int]byte{}
	ambiguousStream := false
	for _, alternate := range alternates {
		if alternate.Class != usbClassAudio {
			continue
		}
		if alternate.Subclass == usbAudioControlSubclass {
			if alternate.AlternateSetting == 0 && len(alternate.Endpoints) == 0 {
				controlSet[int(alternate.InterfaceNumber)] = struct{}{}
			}
			continue
		}
		if alternate.Subclass != usbAudioStreamingSubclass || alternate.AlternateSetting == 0 {
			continue
		}
		var dataEndpoints []byte
		for _, endpoint := range alternate.Endpoints {
			if endpoint.Address&usbEndpointNumberMask == 0 ||
				endpoint.Attributes&usbEndpointTransferTypeMask != usbEndpointTransferTypeIso {
				continue
			}
			dataEndpoints = append(dataEndpoints, endpoint.Address)
		}
		if len(dataEndpoints) != 1 {
			continue
		}
		if dataEndpoints[0]&usbEndpointDirectionIn != 0 {
			if _, exists := inputStreams[int(alternate.InterfaceNumber)]; exists {
				ambiguousStream = true
			}
			inputStreams[int(alternate.InterfaceNumber)] = dataEndpoints[0]
		} else {
			if _, exists := outputStreams[int(alternate.InterfaceNumber)]; exists {
				ambiguousStream = true
			}
			outputStreams[int(alternate.InterfaceNumber)] = dataEndpoints[0]
		}
	}
	if len(controlSet) == 0 || len(inputStreams) == 0 || len(outputStreams) == 0 {
		return usbUACLayout{}, errUSBDirectUACNotFound
	}
	if ambiguousStream || len(controlSet) != 1 || len(inputStreams) != 1 || len(outputStreams) != 1 {
		return usbUACLayout{}, fmt.Errorf("%w: controls=%d inputs=%d outputs=%d",
			errUSBDirectUACAmbiguous, len(controlSet), len(inputStreams), len(outputStreams))
	}
	var layout usbUACLayout
	for number := range controlSet {
		layout.ControlInterface = number
	}
	for number, endpoint := range inputStreams {
		layout.InputInterface, layout.InputEndpoint = number, endpoint
	}
	for number, endpoint := range outputStreams {
		layout.OutputInterface, layout.OutputEndpoint = number, endpoint
	}
	if layout.InputInterface == layout.OutputInterface {
		return usbUACLayout{}, fmt.Errorf("%w: input and output share interface %d",
			errUSBDirectUACAmbiguous, layout.InputInterface)
	}
	return layout, nil
}
