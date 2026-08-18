//go:build darwin && cgo

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveQPCMVTransientProbe is an explicit, opt-in hardware test. It does
// not change USBCFG, IMS, USBNET, radio state, or place a call. If the enable
// was attempted, cleanup is attempted exactly once, but only after a second
// fresh empty CLCC result.
func TestLiveQPCMVTransientProbe(t *testing.T) {
	if os.Getenv("MACCELLULAR_QPCMV_LIVE_PROBE") != "1" {
		t.Skip("set MACCELLULAR_QPCMV_LIVE_PROBE=1 for the transient hardware probe")
	}

	device, err := openDJIUSBAT()
	if err != nil {
		t.Fatalf("open exact DJI USB AT interface: %v", err)
	}
	defer device.Close()

	identity := device.PhysicalIdentity()
	if identity.VendorID != djiUSBVendorID || identity.ProductID != djiUSBProductID || identity.Location == 0 {
		t.Fatal("refusing probe on unexpected physical identity")
	}

	clcc, err := device.Command("AT+CLCC", 3*time.Second)
	if err != nil || !atProbeSucceeded(clcc) || len(parseCLCC(clcc)) != 0 {
		t.Fatalf("refusing probe without a fresh empty CLCC: command_error=%t", err != nil)
	}
	compositionResponse, err := device.Command(`AT+QCFG="USBCFG"`, 3*time.Second)
	composition, parseErr := parseUSBComposition(compositionResponse)
	if err != nil || parseErr != nil || !composition.isFactoryDJI() {
		t.Fatalf("refusing probe outside exact factory composition: response=%q command_err=%v parse_err=%v",
			compositionResponse, err, parseErr)
	}
	capability, err := device.Command("AT+QPCMV=?", 3*time.Second)
	if err != nil || !atProbeSucceeded(capability) || !strings.Contains(capability, "0-2") {
		t.Fatalf("QPCMV capability probe failed: response=%q err=%v", capability, err)
	}

	cleanupRequired := false
	defer func() {
		if !cleanupRequired {
			return
		}
		clcc, clccErr := device.Command("AT+CLCC", 3*time.Second)
		if clccErr != nil || !atProbeSucceeded(clcc) || len(parseCLCC(clcc)) != 0 {
			t.Errorf("QPCMV cleanup refused without a fresh empty CLCC: command_error=%t", clccErr != nil)
			return
		}
		response, cleanupErr := device.Command("AT+QPCMV=0", 3*time.Second)
		if cleanupErr != nil || !atProbeSucceeded(response) {
			t.Errorf("QPCMV cleanup was not confirmed: response=%q err=%v", response, cleanupErr)
			return
		}
		cleanupRequired = false
	}()

	// Set this before transmitting: a timeout can mean the modem applied the
	// command but its response was lost.
	cleanupRequired = true
	enable, err := device.Command("AT+QPCMV=1,2", 3*time.Second)
	if err != nil || !atProbeSucceeded(enable) {
		t.Fatalf("QPCMV UAC mode was not accepted: response=%q err=%v", enable, err)
	}
}
