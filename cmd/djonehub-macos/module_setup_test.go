package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseUSBCompositionAndClassification(t *testing.T) {
	factory, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,0`)
	if err != nil || !factory.isFactoryDJI() || factory.hasUAC() {
		t.Fatalf("factory parse = %#v, %v", factory, err)
	}
	uac, err := parseUSBComposition("AT+QCFG=\"USBCFG\"\r\n+QCFG: \"usbcfg\",0x2C7C,0x125,1,1,1,1,1,0,1\r\nOK")
	if err != nil || !uac.hasUAC() || uac.hasADB() || !uac.isUACTarget() {
		t.Fatalf("UAC parse = %#v, %v", uac, err)
	}
	adbAndUAC, err := parseUSBComposition(`+QCFG: "usbcfg",0x2C7C,0x125,1,1,1,1,1,1,1`)
	if err != nil || !adbAndUAC.isADBAndUACTarget() || !adbAndUAC.isCallAudioCapable() || !adbAndUAC.hasADB() {
		t.Fatalf("ADB+UAC parse = %#v, %v", adbAndUAC, err)
	}
	fullDJI, err := parseUSBComposition(`+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,1,1`)
	if err != nil || !fullDJI.isFullUACTarget() || !fullDJI.isCallAudioCapable() || !fullDJI.hasADB() {
		t.Fatalf("DJI full UAC parse = %#v, %v", fullDJI, err)
	}
	if got := factory.command(); got != `AT+QCFG="USBCFG",0x2CA3,0x4006,1,1,1,1,1,0,0` {
		t.Fatalf("command = %q", got)
	}
}

func TestUSBCFGErrorIsTransientDuringReenumeration(t *testing.T) {
	if !atResponseIsError("AT+QCFG=\"USBCFG\"\r\nERROR") {
		t.Fatal("USBCFG ERROR must be detected as a transient AT response")
	}
	if atResponseIsError(`+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1\r\nOK`) {
		t.Fatal("a valid USBCFG response must not be classified as an error")
	}
}

func TestParseIMSConfiguration(t *testing.T) {
	configuration, capability, err := parseIMSConfiguration("AT+QCFG=\"ims\"\r\n+QCFG: \"ims\",1,1\r\nOK")
	if err != nil || configuration != 1 || capability != 1 {
		t.Fatalf("IMS parse = %d,%d err=%v", configuration, capability, err)
	}
	configuration, capability, err = parseIMSConfiguration(`+QCFG: "ims",2,0`)
	if err != nil || configuration != 2 || capability != 0 {
		t.Fatalf("disabled IMS parse = %d,%d err=%v", configuration, capability, err)
	}
}

func TestParseUSBNetModeValue(t *testing.T) {
	mode, err := parseUSBNetModeValue("AT+QCFG=\"usbnet\"\r\n+QCFG: \"usbnet\",1\r\nOK")
	if err != nil || mode != 1 {
		t.Fatalf("usbnet parse = %d err=%v", mode, err)
	}
	if _, err := parseUSBNetModeValue("OK"); err == nil {
		t.Fatal("missing usbnet response unexpectedly parsed")
	}
}

func TestVoLTEIsEnabled(t *testing.T) {
	for _, response := range []string{
		`+QCFG: "volte_disable",0`,
		`+QCFG: "volte/disable", 0`,
	} {
		if !volteIsEnabled(response) {
			t.Fatalf("enabled response rejected: %q", response)
		}
	}
	if volteIsEnabled(`+QCFG: "volte_disable",1`) {
		t.Fatal("disabled VoLTE response accepted")
	}
}

func TestParseModuleIMEIStrict(t *testing.T) {
	imeiOne := strings.Repeat("0", 15)
	imeiTwo := strings.Repeat("1", 15)
	for _, response := range []string{
		fmt.Sprintf("AT+CGSN\r\n%s\r\nOK\r\n", imeiOne),
		fmt.Sprintf("+CGSN: %s\r\nOK\r\n", imeiOne),
	} {
		imei, err := parseModuleIMEI(response)
		if err != nil || imei != imeiOne {
			t.Fatalf("parseModuleIMEI(%q) = %q, %v", response, imei, err)
		}
	}
	for _, response := range []string{
		"OK",
		"+CGSN: " + strings.Repeat("0", 14),
		fmt.Sprintf("%s\n%s\nOK", imeiOne, imeiTwo),
		"IMEI: " + imeiOne + "\nOK",
	} {
		if _, err := parseModuleIMEI(response); err == nil {
			t.Fatalf("invalid IMEI response accepted: %q", response)
		}
	}
}

func TestSameModuleSetupIdentityRequiresLocationAndDigest(t *testing.T) {
	imeiOne := strings.Repeat("0", 15)
	imeiTwo := strings.Repeat("1", 15)
	expected := moduleSetupSnapshot{USBLocationID: 0x12340000, DeviceIdentityHash: moduleIdentityDigest(imeiOne)}
	actual := expected
	if !sameModuleSetupIdentity(expected, actual) {
		t.Fatal("same module identity rejected")
	}
	actual.USBLocationID = 0x01200000
	if sameModuleSetupIdentity(expected, actual) {
		t.Fatal("different USB location accepted")
	}
	actual = expected
	actual.DeviceIdentityHash = moduleIdentityDigest(imeiTwo)
	if sameModuleSetupIdentity(expected, actual) {
		t.Fatal("different modem identity accepted")
	}
}

func TestSnapshotRejectsIdentityChangeMidRead(t *testing.T) {
	imeiOne := strings.Repeat("0", 15)
	copyIMEI := strings.Repeat("1", 15)
	instance := &app{}
	instance.usbATSessionOverride = func(location uint32, operation func(usbATCommandFunc) error) error {
		if location != 0x12340000 {
			t.Fatalf("location = 0x%08x", location)
		}
		identityReads := 0
		return operation(func(command string, _ time.Duration) (string, error) {
			switch command {
			case "AT+CGSN":
				identityReads++
				if identityReads == 1 {
					return imeiOne + "\r\nOK", nil
				}
				return copyIMEI + "\r\nOK", nil
			case `AT+QCFG="USBCFG"`:
				return `+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,0` + "\r\nOK", nil
			case `AT+QCFG="usbnet"`:
				return `+QCFG: "usbnet",0` + "\r\nOK", nil
			case `AT+QCFG="ims"`:
				return `+QCFG: "ims",0,1` + "\r\nOK", nil
			default:
				return "", errors.New("unexpected command: " + command)
			}
		})
	}
	if _, err := instance.readModuleSetupSnapshotAtLocation(0x12340000); err == nil || !strings.Contains(err.Error(), "身份发生变化") {
		t.Fatalf("mid-snapshot identity swap err = %v", err)
	}
}

func TestVerifiedMutationFreshCallGateAndOrdering(t *testing.T) {
	identity := strings.Repeat("0", 15)
	expected := moduleSetupSnapshot{USBLocationID: 0x12340000, DeviceIdentityHash: moduleIdentityDigest(identity)}
	tests := []struct {
		name          string
		clcc          string
		wantAttempted bool
		wantErr       bool
	}{
		{name: "idle writes", clcc: "OK", wantAttempted: true},
		{name: "packet data contexts still allow maintenance write", clcc: "+CLCC: 1,1,0,1,0\r\n+CLCC: 2,1,0,1,0\r\nOK", wantAttempted: true},
		{name: "incoming blocks write", clcc: `+CLCC: 1,1,4,0,0,"10000",129` + "\r\nOK", wantErr: true},
		{name: "data plus incoming blocks write", clcc: "+CLCC: 1,1,0,1,0\r\n" + `+CLCC: 3,1,4,0,0,"10000",129` + "\r\nOK", wantErr: true},
		{name: "query failure blocks write", clcc: "ERROR", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := &app{}
			var order []string
			instance.usbATSessionOverride = func(location uint32, operation func(usbATCommandFunc) error) error {
				return operation(func(command string, _ time.Duration) (string, error) {
					order = append(order, command)
					switch command {
					case "AT+CGSN":
						return identity + "\r\nOK", nil
					case "AT+CLCC":
						return test.clcc, nil
					case `AT+QCFG="ims",1`:
						return "OK", nil
					default:
						return "", errors.New("unexpected command: " + command)
					}
				})
			}
			response, attempted, err := instance.runVerifiedModuleSetupCommand(expected, `AT+QCFG="ims",1`, time.Second)
			if attempted != test.wantAttempted || (err != nil) != test.wantErr {
				t.Fatalf("response=%q attempted=%v err=%v order=%v", response, attempted, err, order)
			}
			if attempted {
				want := []string{"AT+CGSN", "AT+CLCC", `AT+QCFG="ims",1`}
				if strings.Join(order, "|") != strings.Join(want, "|") {
					t.Fatalf("command order = %v, want %v", order, want)
				}
			} else if len(order) != 2 {
				t.Fatalf("mutation executed after failed call gate: %v", order)
			}
		})
	}
}

func TestModuleMutationTryLockRejectsConcurrentWriter(t *testing.T) {
	instance := &app{}
	instance.moduleMutationMu.Lock()
	if instance.moduleMutationMu.TryLock() {
		instance.moduleMutationMu.Unlock()
		t.Fatal("second module mutation unexpectedly acquired lock")
	}
	instance.moduleMutationMu.Unlock()
	if !instance.moduleMutationMu.TryLock() {
		t.Fatal("module mutation lock was not released")
	}
	instance.moduleMutationMu.Unlock()
}

func TestModuleRebootAcceptedIsFailClosed(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		attempted bool
		err       error
		want      bool
	}{
		{name: "clear OK", response: "OK", attempted: true, want: true},
		{name: "expected detach", attempted: true, err: errors.New("LIBUSB_ERROR_NO_DEVICE"), want: true},
		{name: "expected not found", attempted: true, err: errors.New("USB NOT_FOUND"), want: true},
		{name: "ordinary timeout", attempted: true, err: errors.New("USB command timed out"), want: false},
		{name: "ordinary IO error", attempted: true, err: errors.New("input/output error"), want: false},
		{name: "modem ERROR", response: "ERROR", attempted: true, want: false},
		{name: "not attempted", attempted: false, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := moduleRebootAccepted(test.response, test.attempted, test.err); got != test.want {
				t.Fatalf("moduleRebootAccepted(%q, %v, %v) = %v, want %v", test.response, test.attempted, test.err, got, test.want)
			}
		})
	}
}

func TestUSBPhysicalIdentityMatchesComposition(t *testing.T) {
	target := usbComposition{VendorID: 0x2c7c, ProductID: 0x0125}
	identity := usbATPhysicalIdentity{VendorID: 0x2c7c, ProductID: 0x0125, Location: 0x12340000}
	if !usbPhysicalIdentityMatchesComposition(identity, target, 0x12340000) {
		t.Fatal("matching physical USB identity rejected")
	}
	identity.ProductID = 0x4006
	if usbPhysicalIdentityMatchesComposition(identity, target, 0x12340000) {
		t.Fatal("mismatched physical USB product accepted")
	}
	identity = usbATPhysicalIdentity{VendorID: 0x2c7c, ProductID: 0x0125, Location: 0x01200000}
	if usbPhysicalIdentityMatchesComposition(identity, target, 0x12340000) {
		t.Fatal("mismatched physical USB location accepted")
	}
}
