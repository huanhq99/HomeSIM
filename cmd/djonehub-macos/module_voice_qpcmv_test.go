package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestImplicitUACVoiceDoesNotTransmitQPCMV(t *testing.T) {
	var transmitted []string
	a := &app{implicitUACVoice: true}
	command := a.moduleVoiceCommand(func(at string, _ time.Duration) (string, error) {
		transmitted = append(transmitted, at)
		return "OK\r\n", nil
	})
	response, err := command("AT+QPCMV=1,0", time.Second)
	if err != nil || !directQPCMVCommandUnsupported(response, "AT+QPCMV=1,0") {
		t.Fatalf("implicit UAC response = %q, %v", response, err)
	}
	if _, err := command("AT+CLCC", time.Second); err != nil {
		t.Fatal(err)
	}
	if len(transmitted) != 1 || transmitted[0] != "AT+CLCC" {
		t.Fatalf("transmitted commands = %#v", transmitted)
	}
}

func TestImplicitUACCallTeardownKeepsWarmRoute(t *testing.T) {
	a := &app{
		implicitUACVoice:          true,
		moduleVoiceReady:          true,
		moduleVoicePhase:          "ready",
		moduleVoiceCallGeneration: 9,
	}
	if err := a.stopModuleVoiceRouteForCall(9); err != nil {
		t.Fatalf("release warm route: %v", err)
	}
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	if a.moduleVoiceCallGeneration != 0 {
		t.Fatalf("call generation=%d, want released", a.moduleVoiceCallGeneration)
	}
	if !a.moduleVoiceReady || a.moduleVoicePhase != "ready" {
		t.Fatalf("warm route changed: ready=%v phase=%q", a.moduleVoiceReady, a.moduleVoicePhase)
	}
}

func TestOutgoingDialPrewarmsModuleAudioBeforeCallCommand(t *testing.T) {
	var received uint64
	a := &app{
		implicitUACVoice:  true,
		callTopologyKnown: true,
		callGeneration:    12,
		outgoingVoicePrewarm: func(generation uint64) error {
			received = generation
			return nil
		},
	}
	if err := a.prewarmOutgoingModuleVoiceRoute(12); err != nil {
		t.Fatal(err)
	}
	if received != 12 {
		t.Fatalf("prewarmed generation=%d, want 12", received)
	}
}

func TestOutgoingDialAudioPrewarmFailureStopsDialPreparation(t *testing.T) {
	want := errors.New("prewarm failed")
	a := &app{
		implicitUACVoice:     true,
		outgoingVoicePrewarm: func(uint64) error { return want },
	}
	if err := a.prewarmOutgoingModuleVoiceRoute(8); !errors.Is(err, want) {
		t.Fatalf("prewarm error=%v, want %v", err, want)
	}
}

func TestModuleVoicePrewarmRefreshesOncePerCallGeneration(t *testing.T) {
	var refreshed []uint64
	a := &app{moduleVoiceRefresh: func(generation uint64) error {
		refreshed = append(refreshed, generation)
		return nil
	}}
	if err := a.refreshModuleVoiceRouteForGenerationLocked(41); err != nil {
		t.Fatal(err)
	}
	// The second notification for the same call must not restart the module
	// helper again. Avoid the production ensure path by keeping the test seam.
	if err := a.refreshModuleVoiceRouteForGenerationLocked(41); err != nil {
		t.Fatal(err)
	}
	if err := a.refreshModuleVoiceRouteForGenerationLocked(42); err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 2 || refreshed[0] != 41 || refreshed[1] != 42 {
		t.Fatalf("refreshed generations = %#v", refreshed)
	}
}

func TestImplicitUACHangupDoesNotQueryQPCMV(t *testing.T) {
	called := false
	a := &app{implicitUACVoice: true}
	err := a.confirmVoiceRouteForHangup(func(string, time.Duration) (string, error) {
		called = true
		return "", errors.New("must not be called")
	})
	if err != nil || called {
		t.Fatalf("implicit UAC hangup route check err=%v called=%v", err, called)
	}
}

func TestImplicitUACReadyRouteOwnsExactCallForHangup(t *testing.T) {
	ticket := callMediaTicket{Generation: 7, CallID: "call-7", Index: 3, Direction: "outgoing"}
	a := &app{}
	a.setDirectQPCMVStateForOwner(
		true,
		"ready",
		"QDC507 D4/UAC route active",
		0x01100000,
		"identity",
		directQPCMVCallIntent{direction: ticket.Direction, callID: ticket.CallID, index: ticket.Index},
		ticket.Generation,
		nil,
	)
	state := a.directQPCMVRouteState()
	if !directQPCMVStateOwnsRescueHangup(state, ticket) {
		t.Fatalf("exact implicit UAC route did not own hangup: %+v", state)
	}
	wrong := ticket
	wrong.CallID = "other-call"
	if directQPCMVStateOwnsRescueHangup(state, wrong) {
		t.Fatal("implicit UAC route owned a different call")
	}
}

const testDirectQPCMVLocation = uint32(0x12340000)

var testDirectQPCMVIdentityHash = moduleIdentityDigest(strings.Repeat("0", 15))

func testDirectQPCMVCompositionResponse() string {
	return `+QCFG: "usbcfg",0x2C7C,0x0125,1,1,1,1,1,0,1` + "\r\nOK"
}

func installDirectQPCMVCommands(t *testing.T, a *app, handler func(string) (string, error)) *[]string {
	t.Helper()
	commands := &[]string{}
	a.usbATSessionOverride = func(location uint32, operation func(usbATCommandFunc) error) error {
		if location != testDirectQPCMVLocation {
			t.Fatalf("USB location = 0x%08x, want 0x%08x", location, testDirectQPCMVLocation)
		}
		return operation(func(command string, _ time.Duration) (string, error) {
			*commands = append(*commands, command)
			return handler(command)
		})
	}
	return commands
}

func assertDirectQPCMVCommands(t *testing.T, got *[]string, want ...string) {
	t.Helper()
	if strings.Join(*got, "|") != strings.Join(want, "|") {
		t.Fatalf("command order = %v, want %v", *got, want)
	}
}

func setDirectQPCMVActiveCall(a *app, generation uint64, id string, index int, direction string) callMediaTicket {
	a.callMu.Lock()
	a.callTopologyKnown = true
	a.callTopologyKey = fmt.Sprintf("%d:%s:active", index, direction)
	a.callGeneration = generation
	a.callMediaEligible = true
	a.activeCall = &callRecord{ID: id, Index: index, Direction: direction, State: "active"}
	a.callMu.Unlock()
	return callMediaTicket{Generation: generation, CallID: id, Index: index, Direction: direction}
}

func TestDirectQPCMVStateParsingIsStrict(t *testing.T) {
	for _, response := range []string{
		"AT+QPCMV?\r\n+QPCMV: 1,2\r\nOK",
		"+qpcmv: 1, 2\nOK",
	} {
		if !directQPCMVUACEnabled(response) {
			t.Fatalf("enabled state rejected: %q", response)
		}
	}
	for _, response := range []string{
		"+QPCMV: (0,1),(0-2)\r\nOK",
		"+QPCMV: 1,1\r\nOK",
		"+QPCMV: 1,2\r\nERROR",
		"+QPCMV: 1,2",
		"+QPCMV: 1,2\r\n+QPCMV: 1,2\r\nOK",
	} {
		if directQPCMVUACEnabled(response) {
			t.Fatalf("invalid enabled state accepted: %q", response)
		}
	}
	for _, response := range []string{"+QPCMV: 0\r\nOK", "+QPCMV: 0,2\r\nOK"} {
		if !directQPCMVDisabled(response) {
			t.Fatalf("disabled state rejected: %q", response)
		}
	}
	if directQPCMVDisabled("+QPCMV: 1,2\r\nOK") {
		t.Fatal("enabled response accepted as disabled")
	}
	if !directQPCMVUninitialized("+QPCMV: (0,1),(0-2)\r\nOK") {
		t.Fatal("QDC507 cold-boot QPCMV state was not recognized")
	}
	for _, response := range []string{
		"+QPCMV: (0,1),(0-2)\r\nERROR",
		"+QPCMV: 0\r\nOK",
		"+QPCMV: 1,2\r\nOK",
	} {
		if directQPCMVUninitialized(response) {
			t.Fatalf("invalid cold-boot state accepted: %q", response)
		}
	}
	for _, response := range []string{
		"AT+QPCMV?\r\r\nERROR\r\n",
		"error\n",
	} {
		if !directQPCMVQueryUnsupported(response) {
			t.Fatalf("legacy unsupported query rejected: %q", response)
		}
	}
	for _, response := range []string{
		"+CME ERROR: 4\r\n",
		"AT+QPCMV?\r\nERROR\r\nERROR\r\n",
		"AT+QPCMV?\r\nERROR\r\nextra\r\n",
		"AT+QPCMV?\r\n",
		"+QPCMV: 0\r\nOK\r\n",
	} {
		if directQPCMVQueryUnsupported(response) {
			t.Fatalf("ambiguous query accepted as unsupported: %q", response)
		}
	}
	if !directQPCMVCommandUnsupported("AT+QPCMV=0\r\r\nERROR\r\n", directQPCMVDisableCommand) {
		t.Fatal("legacy unsupported disable was not recognized")
	}
	if directQPCMVCommandUnsupported("AT+QPCMV=1,2\r\nERROR\r\n", directQPCMVDisableCommand) {
		t.Fatal("a different unsupported command was accepted as disable")
	}
}

func TestPreflightDirectQPCMVSupportsExactWriteOnlyFirmware(t *testing.T) {
	imei := strings.Repeat("0", 15)
	a := &app{}
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return imei + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "+CLCC: 1,1,0,1,0\r\nOK", nil
		case directQPCMVEnableCommand, directQPCMVDisableCommand:
			return "OK", nil
		case directQPCMVQueryCommand:
			return "AT+QPCMV?\r\r\nERROR\r\n", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.preflightDirectQPCMVExpected(moduleSetupSnapshot{
		USBLocationID:      testDirectQPCMVLocation,
		DeviceIdentityHash: moduleIdentityDigest(imei),
	}); err != nil {
		t.Fatalf("write-only firmware preflight failed: %v", err)
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC",
		directQPCMVQueryCommand, "AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVEnableCommand, directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
	)
	state := a.directQPCMVRouteState()
	if state.ready || state.phase != "stopped" || state.generation != 0 {
		t.Fatalf("write-only preflight state = %+v", state)
	}
}

func TestPreflightDirectQPCMVWriteOnlyFirmwareStillSelectsUAC(t *testing.T) {
	imei := strings.Repeat("0", 15)
	a := &app{}
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return imei + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "+CLCC: 1,1,0,1,0\r\nOK", nil
		case directQPCMVDisableCommand, directQPCMVEnableCommand:
			return command + "\r\r\nERROR\r\n", nil
		case directQPCMVQueryCommand:
			return "AT+QPCMV?\r\r\nERROR\r\n", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.preflightDirectQPCMVExpected(moduleSetupSnapshot{
		USBLocationID:      testDirectQPCMVLocation,
		DeviceIdentityHash: moduleIdentityDigest(imei),
	}); err != nil {
		t.Fatalf("write-only UAC firmware preflight failed: %v", err)
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC",
		directQPCMVQueryCommand, "AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVEnableCommand, directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
	)
	state := a.directQPCMVRouteState()
	if state.ready || state.phase != "stopped" || state.generation != 0 {
		t.Fatalf("write-only UAC preflight state = %+v", state)
	}
}

func TestUninitializedQPCMVIsNormalizedOnceBeforeEnable(t *testing.T) {
	queryCount := 0
	var disables atomic.Int32
	handler := func(command string) (string, error) {
		switch command {
		case directQPCMVQueryCommand:
			queryCount++
			if queryCount == 1 {
				return "+QPCMV: (0,1),(0-2)\r\nOK", nil
			}
			return "+QPCMV: 0\r\nOK", nil
		case "AT+CLCC":
			return "OK", nil
		case directQPCMVDisableCommand:
			disables.Add(1)
			return "OK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	}
	result := ensureDirectQPCMVDisabledBeforeEnable(func(command string, _ time.Duration) (string, error) {
		return handler(command)
	}, false)
	if result.err != nil || !result.required || !result.attempted || !result.confirmed {
		t.Fatalf("normalization result=%+v", result)
	}
	if disables.Load() != 1 || queryCount != 2 {
		t.Fatalf("disables=%d queries=%d", disables.Load(), queryCount)
	}
}

func TestPreflightDirectQPCMVExpectedIdentityAndOrder(t *testing.T) {
	imei := strings.Repeat("0", 15)
	a := &app{}
	queryCount := 0
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return imei + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "+CLCC: 1,1,0,1,0\r\n+CLCC: 2,1,0,1,0\r\nOK", nil
		case directQPCMVEnableCommand, directQPCMVDisableCommand:
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			if queryCount == 2 {
				return "+QPCMV: 1,2\r\nOK", nil
			}
			return "+QPCMV: 0\r\nOK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	expected := moduleSetupSnapshot{
		USBLocationID:      testDirectQPCMVLocation,
		DeviceIdentityHash: moduleIdentityDigest(imei),
	}
	if err := a.preflightDirectQPCMVExpected(expected); err != nil {
		t.Fatalf("preflight failed: %v", err)
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC",
		directQPCMVQueryCommand, directQPCMVEnableCommand, directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
	)
	state := a.directQPCMVRouteState()
	if state.ready || state.phase != "stopped" || state.generation != 0 || state.locationID != testDirectQPCMVLocation {
		t.Fatalf("preflight state = %+v", state)
	}
}

func TestPreflightDirectQPCMVMismatchedIdentityNeverMutates(t *testing.T) {
	expectedIMEI := strings.Repeat("0", 15)
	actualIMEI := strings.Repeat("1", 15)
	a := &app{}
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		if command == "AT+CGSN" {
			return actualIMEI + "\r\nOK", nil
		}
		return "", errors.New("unexpected command: " + command)
	})
	err := a.preflightDirectQPCMVExpected(moduleSetupSnapshot{
		USBLocationID:      testDirectQPCMVLocation,
		DeviceIdentityHash: moduleIdentityDigest(expectedIMEI),
	})
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("identity mismatch error = %v", err)
	}
	assertDirectQPCMVCommands(t, commands, "AT+CGSN")
}

func TestPreflightDirectQPCMVLostEnableResponseCleansOnce(t *testing.T) {
	a := &app{}
	queryCount := 0
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "OK", nil
		case directQPCMVEnableCommand:
			return "", errors.New("response lost")
		case directQPCMVDisableCommand:
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			return "+QPCMV: 0\r\nOK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	err := a.preflightDirectQPCMV(testDirectQPCMVLocation)
	if err == nil || !strings.Contains(err.Error(), "response lost") {
		t.Fatalf("lost enable response error = %v", err)
	}
	assertDirectQPCMVCommands(t, commands,
		`AT+QCFG="USBCFG"`, "AT+CLCC", directQPCMVQueryCommand, directQPCMVEnableCommand,
		"AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
	)
	if queryCount != 2 {
		t.Fatalf("QPCMV queries = %d, want 2", queryCount)
	}
	state := a.directQPCMVRouteState()
	if state.phase != "stopped" || state.generation != 0 {
		t.Fatalf("state after confirmed cleanup = %+v", state)
	}
}

func TestAmbiguousDisableIsNeverRetriedAutomatically(t *testing.T) {
	a := &app{}
	a.setDirectQPCMVStateForIdentity(true, "ready", "test", testDirectQPCMVLocation, testDirectQPCMVIdentityHash, 7, nil)
	var disableCount atomic.Int32
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case "AT+CLCC":
			return "OK", nil
		case directQPCMVDisableCommand:
			disableCount.Add(1)
			return "", errors.New("response lost")
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.stopDirectQPCMVForCall(7); err == nil {
		t.Fatal("ambiguous disable unexpectedly succeeded")
	}
	beforeRetry := len(*commands)
	if err := a.stopDirectQPCMVForCall(7); err == nil || !strings.Contains(err.Error(), "refusing to repeat") {
		t.Fatalf("second stop error = %v", err)
	}
	if len(*commands) != beforeRetry || disableCount.Load() != 1 {
		t.Fatalf("ambiguous disable was retried: commands=%v disables=%d", *commands, disableCount.Load())
	}
	if state := a.directQPCMVRouteState(); state.phase != "cleanup_failed" || state.generation != 7 {
		t.Fatalf("ambiguous cleanup state = %+v", state)
	}
}

func TestArmCleansOneStaleEnabledRouteBeforeReenable(t *testing.T) {
	a := &app{
		activeCall:        &callRecord{ID: "ring-stale", Index: 1, Direction: "incoming", State: "incoming"},
		callTopologyKnown: true,
		callGeneration:    8,
	}
	queryCount := 0
	var disableCount atomic.Int32
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return `+CLCC: 1,1,4,0,0,"10086",129` + "\r\nOK", nil
		case directQPCMVDisableCommand:
			disableCount.Add(1)
			return "OK", nil
		case directQPCMVEnableCommand:
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			switch queryCount {
			case 1:
				return "+QPCMV: 1,2\r\nOK", nil // stale route
			case 2:
				return "+QPCMV: 0\r\nOK", nil // stale cleanup readback
			default:
				return "+QPCMV: 1,2\r\nOK", nil // new arm readback
			}
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.armDirectQPCMV(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, false); err != nil {
		t.Fatalf("arm after stale cleanup failed: %v", err)
	}
	if disableCount.Load() != 1 {
		t.Fatalf("stale cleanup disables = %d, want 1", disableCount.Load())
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC", directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
		"AT+CLCC", directQPCMVEnableCommand, directQPCMVQueryCommand,
	)
}

func TestAmbiguousStaleCleanupBlocksEnableAndRetry(t *testing.T) {
	a := &app{}
	var enableCount atomic.Int32
	var disableCount atomic.Int32
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "OK", nil
		case directQPCMVQueryCommand:
			return "+QPCMV: 1,2\r\nOK", nil
		case directQPCMVDisableCommand:
			disableCount.Add(1)
			return "", errors.New("stale disable response lost")
		case directQPCMVEnableCommand:
			enableCount.Add(1)
			return "OK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.armDirectQPCMV(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, true); err == nil {
		t.Fatal("ambiguous stale cleanup unexpectedly allowed arm")
	}
	beforeRetry := len(*commands)
	if err := a.armDirectQPCMV(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, true); err == nil {
		t.Fatal("cleanup_failed route unexpectedly allowed automatic retry")
	}
	if len(*commands) != beforeRetry || disableCount.Load() != 1 || enableCount.Load() != 0 {
		t.Fatalf("ambiguous stale cleanup retried/mutated: commands=%v disables=%d enables=%d",
			*commands, disableCount.Load(), enableCount.Load())
	}
	if state := a.directQPCMVRouteState(); state.phase != "cleanup_failed" {
		t.Fatalf("ambiguous stale state = %+v", state)
	}
}

func TestArmThenActiveAdoptNeverEnablesWhileActive(t *testing.T) {
	a := &app{}
	physicalActive := false
	queryCount := 0
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			if physicalActive {
				return `+CLCC: 1,0,0,0,0,"10086",129` + "\r\nOK", nil
			}
			return "OK", nil
		case directQPCMVEnableCommand:
			if physicalActive {
				t.Fatal("QPCMV enable was sent after CLCC became active")
			}
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			if queryCount == 1 {
				return "+QPCMV: 0\r\nOK", nil
			}
			return "+QPCMV: 1,2\r\nOK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.armDirectQPCMV(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, true); err != nil {
		t.Fatalf("idle arm failed: %v", err)
	}
	physicalActive = true
	ticket := setDirectQPCMVActiveCall(a, 4, "call-4", 1, "outgoing")
	beforeAdopt := len(*commands)
	if err := a.adoptArmedDirectQPCMVForCall(ticket); err != nil {
		t.Fatalf("active adopt failed: %v", err)
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC", directQPCMVQueryCommand, directQPCMVEnableCommand, directQPCMVQueryCommand,
		"AT+CGSN", "AT+CLCC", directQPCMVQueryCommand,
	)
	for _, command := range (*commands)[beforeAdopt:] {
		if command == directQPCMVEnableCommand {
			t.Fatal("adoption issued QPCMV enable")
		}
	}
	if queryCount != 3 {
		t.Fatalf("QPCMV query count = %d, want 3", queryCount)
	}
	state := a.directQPCMVRouteState()
	if !state.ready || state.phase != "ready" || state.generation != ticket.Generation {
		t.Fatalf("adopted state = %+v", state)
	}
}

func TestIncomingArmRequiresFreshCachedRingingCall(t *testing.T) {
	a := &app{
		activeCall:        &callRecord{ID: "ring-1", Index: 1, Direction: "incoming", State: "incoming"},
		callTopologyKnown: true,
		callGeneration:    3,
	}
	queryCount := 0
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return `+CLCC: 1,1,4,0,0,"10086",129` + "\r\nOK", nil
		case directQPCMVEnableCommand:
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			if queryCount == 1 {
				return "+QPCMV: 0\r\nOK", nil
			}
			return "+QPCMV: 1,2\r\nOK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.armDirectQPCMV(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, false); err != nil {
		t.Fatalf("ringing arm failed: %v", err)
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC", directQPCMVQueryCommand, directQPCMVEnableCommand, directQPCMVQueryCommand,
	)
}

func TestActiveCallCannotBeFirstDirectQPCMVEnable(t *testing.T) {
	a := &app{}
	setDirectQPCMVActiveCall(a, 5, "call-5", 1, "incoming")
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return `+CLCC: 1,1,0,0,0,"10086",129` + "\r\nOK", nil
		default:
			return "", errors.New("mutation unexpectedly reached: " + command)
		}
	})
	if err := a.armDirectQPCMV(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, false); err == nil {
		t.Fatal("active call was accepted as the first QPCMV enable point")
	}
	assertDirectQPCMVCommands(t, commands)
	for _, command := range *commands {
		if command == directQPCMVEnableCommand {
			t.Fatal("active-call arm issued QPCMV enable")
		}
	}
}

func TestStopDefersUntilFreshCLCCIsEmpty(t *testing.T) {
	a := &app{}
	a.setDirectQPCMVStateForIdentity(true, "ready", "test", testDirectQPCMVLocation, testDirectQPCMVIdentityHash, 12, nil)
	physicalActive := true
	var disableCount atomic.Int32
	queryCount := 0
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case "AT+CLCC":
			if physicalActive {
				return `+CLCC: 1,1,0,0,0,"10086",129` + "\r\nOK", nil
			}
			return "OK", nil
		case directQPCMVDisableCommand:
			disableCount.Add(1)
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			return "+QPCMV: 0,2\r\nOK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.stopDirectQPCMVForCall(12); err != nil {
		t.Fatalf("active stop should defer without poller noise: %v", err)
	}
	if disableCount.Load() != 0 {
		t.Fatal("QPCMV disable was sent while CLCC was active")
	}
	if state := a.directQPCMVRouteState(); state.phase != "cleanup_pending" || state.generation != 12 {
		t.Fatalf("active-stop state = %+v", state)
	}

	physicalActive = false
	if err := a.stopDirectQPCMVForCall(12); err != nil {
		t.Fatalf("idle cleanup failed: %v", err)
	}
	if disableCount.Load() != 1 || queryCount != 1 {
		t.Fatalf("cleanup disables=%d queries=%d", disableCount.Load(), queryCount)
	}
	state := a.directQPCMVRouteState()
	if state.ready || state.phase != "stopped" || state.generation != 0 {
		t.Fatalf("stopped state = %+v", state)
	}
	assertDirectQPCMVCommands(t, commands,
		"AT+CGSN", "AT+CLCC", "AT+CGSN", "AT+CLCC", directQPCMVDisableCommand, directQPCMVQueryCommand,
	)
}

func TestOldGenerationStopCannotDisableNewerRoute(t *testing.T) {
	a := &app{}
	a.moduleVoiceOpMu.Lock()
	a.setDirectQPCMVStateForIdentity(true, "ready", "old", testDirectQPCMVLocation, testDirectQPCMVIdentityHash, 20, nil)
	var commandCount atomic.Int32
	a.usbATSessionOverride = func(uint32, func(usbATCommandFunc) error) error {
		commandCount.Add(1)
		return errors.New("old stop reached USB session")
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- a.stopDirectQPCMVForCall(20)
	}()
	<-started
	a.setDirectQPCMVStateForIdentity(true, "ready", "new", testDirectQPCMVLocation, testDirectQPCMVIdentityHash, 21, nil)
	a.moduleVoiceOpMu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("old stop returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old generation stop did not finish")
	}
	if commandCount.Load() != 0 {
		t.Fatal("old generation stop reached the newer route's USB session")
	}
}

func TestArmAndATDUseOneExactSessionInStrictOrder(t *testing.T) {
	a := &app{}
	var sessions atomic.Int32
	var commands []string
	queryCount := 0
	a.usbATSessionOverride = func(location uint32, operation func(usbATCommandFunc) error) error {
		sessions.Add(1)
		if location != testDirectQPCMVLocation {
			t.Fatalf("location = 0x%08x", location)
		}
		return operation(func(command string, _ time.Duration) (string, error) {
			commands = append(commands, command)
			switch command {
			case "AT+CGSN":
				return strings.Repeat("0", 15) + "\r\nOK", nil
			case `AT+QCFG="USBCFG"`:
				return testDirectQPCMVCompositionResponse(), nil
			case "AT+CLCC":
				return "OK", nil
			case directQPCMVEnableCommand:
				return "OK", nil
			case directQPCMVQueryCommand:
				queryCount++
				if queryCount == 1 {
					return "+QPCMV: 0\r\nOK", nil
				}
				return "+QPCMV: 1,2\r\nOK", nil
			case "ATD10086;":
				return "OK", nil
			default:
				return "", errors.New("unexpected command: " + command)
			}
		})
	}
	response, err := a.armDirectQPCMVAndExecuteCall(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, true, "ATD10086;", 8*time.Second)
	if err != nil || !directQPCMVExplicitOK(response) {
		t.Fatalf("arm+ATD response=%q err=%v", response, err)
	}
	if sessions.Load() != 1 {
		t.Fatalf("sessions=%d, want 1", sessions.Load())
	}
	want := []string{"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC", directQPCMVQueryCommand,
		directQPCMVEnableCommand, directQPCMVQueryCommand, "AT+CLCC", "ATD10086;"}
	if strings.Join(commands, "|") != strings.Join(want, "|") {
		t.Fatalf("commands=%v, want %v", commands, want)
	}
	state := a.directQPCMVRouteState()
	if state.phase != "armed" || state.identityHash != testDirectQPCMVIdentityHash || state.intent.direction != "outgoing" {
		t.Fatalf("state=%+v", state)
	}
}

func TestRemoteMediaGateRunsInsideArmSessionImmediatelyBeforeCallCommand(t *testing.T) {
	a := &app{}
	var commands []string
	queryCount := 0
	pinnedDevice := &usbAT{}
	pinnedIdentity := usbATPhysicalIdentity{
		VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Location: testDirectQPCMVLocation,
	}
	a.usbATPinnedSessionOverride = func(location uint32, operation func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error) error {
		if location != testDirectQPCMVLocation {
			t.Fatalf("location = 0x%08x", location)
		}
		return operation(pinnedDevice, pinnedIdentity, func(command string, _ time.Duration) (string, error) {
			commands = append(commands, command)
			switch command {
			case "AT+CGSN":
				return strings.Repeat("0", 15) + "\r\nOK", nil
			case `AT+QCFG="USBCFG"`:
				return testDirectQPCMVCompositionResponse(), nil
			case "AT+CLCC":
				return "OK", nil
			case directQPCMVEnableCommand:
				return "OK", nil
			case directQPCMVQueryCommand:
				queryCount++
				if queryCount == 1 {
					return "+QPCMV: 0\r\nOK", nil
				}
				return "+QPCMV: 1,2\r\nOK", nil
			case "ATD10086;":
				return "OK", nil
			default:
				return "", errors.New("unexpected command: " + command)
			}
		})
	}
	response, err := a.armDirectQPCMVAndExecuteCallWithGate(
		testDirectQPCMVLocation,
		testDirectQPCMVIdentityHash,
		true,
		"ATD10086;",
		8*time.Second,
		func(device *usbAT, identity usbATPhysicalIdentity) error {
			if device != pinnedDevice || identity != pinnedIdentity {
				t.Fatalf("gate received device=%p identity=%+v, want device=%p identity=%+v",
					device, identity, pinnedDevice, pinnedIdentity)
			}
			commands = append(commands, "MEDIA-GATE")
			return nil
		},
	)
	if err != nil || !directQPCMVExplicitOK(response) {
		t.Fatalf("arm+gate+ATD response=%q err=%v", response, err)
	}
	want := []string{"AT+CGSN", `AT+QCFG="USBCFG"`, "AT+CLCC", directQPCMVQueryCommand,
		directQPCMVEnableCommand, directQPCMVQueryCommand, "AT+CLCC", "MEDIA-GATE", "ATD10086;"}
	if strings.Join(commands, "|") != strings.Join(want, "|") {
		t.Fatalf("commands=%v, want %v", commands, want)
	}
}

func TestRemoteMediaGateFailureNeverExecutesCallCommand(t *testing.T) {
	a := &app{}
	var commandAttempted atomic.Bool
	queryCount := 0
	installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "OK", nil
		case directQPCMVEnableCommand:
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			if queryCount == 1 {
				return "+QPCMV: 0\r\nOK", nil
			}
			return "+QPCMV: 1,2\r\nOK", nil
		case "ATD10086;":
			commandAttempted.Store(true)
			return "OK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	gateErr := errors.New("media lease changed")
	_, err := a.armDirectQPCMVAndExecuteCallWithGate(
		testDirectQPCMVLocation,
		testDirectQPCMVIdentityHash,
		true,
		"ATD10086;",
		8*time.Second,
		func(_ *usbAT, _ usbATPhysicalIdentity) error { return gateErr },
	)
	if !errors.Is(err, gateErr) {
		t.Fatalf("gate error=%v, want %v", err, gateErr)
	}
	if commandAttempted.Load() {
		t.Fatal("call command executed after media gate failure")
	}
	if state := a.directQPCMVRouteState(); state.phase != "armed" || state.ready {
		t.Fatalf("gate failure should preserve a non-active armed route for safe later cleanup: %+v", state)
	}
}

func TestAmbiguousATDUsesGraceBeforeOneIdleCleanup(t *testing.T) {
	a := &app{}
	queryCount := 0
	var disableCount atomic.Int32
	commands := installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("0", 15) + "\r\nOK", nil
		case `AT+QCFG="USBCFG"`:
			return testDirectQPCMVCompositionResponse(), nil
		case "AT+CLCC":
			return "OK", nil
		case directQPCMVEnableCommand:
			return "OK", nil
		case directQPCMVDisableCommand:
			disableCount.Add(1)
			return "OK", nil
		case directQPCMVQueryCommand:
			queryCount++
			if queryCount == 1 || queryCount == 3 {
				return "+QPCMV: 0\r\nOK", nil
			}
			return "+QPCMV: 1,2\r\nOK", nil
		case "ATD10086;":
			return "", errors.New("lost response")
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	_, err := a.armDirectQPCMVAndExecuteCall(testDirectQPCMVLocation, testDirectQPCMVIdentityHash, true, "ATD10086;", 8*time.Second)
	if !errors.Is(err, errDirectCallCommandAmbiguous) {
		t.Fatalf("ambiguous err=%v", err)
	}
	if state := a.directQPCMVRouteState(); state.phase != "call_command_ambiguous" {
		t.Fatalf("state=%+v", state)
	}
	before := len(*commands)
	if err := a.cleanupDirectQPCMVIfIdle(); err != nil {
		t.Fatalf("grace cleanup err=%v", err)
	}
	if len(*commands) != before || disableCount.Load() != 0 {
		t.Fatal("ambiguous call was cleaned before grace")
	}
	a.moduleVoiceMu.Lock()
	a.moduleVoiceLast = time.Now().Add(-directQPCMVCallCommandGrace - time.Second)
	a.moduleVoiceMu.Unlock()
	if err := a.cleanupDirectQPCMVIfIdle(); err != nil {
		t.Fatalf("post-grace cleanup err=%v", err)
	}
	if disableCount.Load() != 1 {
		t.Fatalf("disable count=%d, want 1", disableCount.Load())
	}
}

func TestCleanupRejectsSameLocationDifferentModuleIdentity(t *testing.T) {
	a := &app{}
	a.setDirectQPCMVStateForOwner(false, "cleanup_pending", "test", testDirectQPCMVLocation,
		testDirectQPCMVIdentityHash, directQPCMVCallIntent{direction: "outgoing", index: -1}, 0, nil)
	var disableCount atomic.Int32
	installDirectQPCMVCommands(t, a, func(command string) (string, error) {
		switch command {
		case "AT+CGSN":
			return strings.Repeat("1", 15) + "\r\nOK", nil
		case directQPCMVDisableCommand:
			disableCount.Add(1)
			return "OK", nil
		default:
			return "", errors.New("unexpected command: " + command)
		}
	})
	if err := a.cleanupDirectQPCMVIfIdle(); err == nil {
		t.Fatal("replacement module cleanup unexpectedly succeeded")
	}
	if disableCount.Load() != 0 {
		t.Fatal("replacement module received QPCMV disable")
	}
}
