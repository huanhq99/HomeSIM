package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	directQPCMVEnableCommand    = "AT+QPCMV=1,0"
	directQPCMVQueryCommand     = "AT+QPCMV?"
	directQPCMVDisableCommand   = "AT+QPCMV=0"
	directQPCMVCommandTimeout   = 5 * time.Second
	directQPCMVArmTTL           = 90 * time.Second
	directQPCMVCallCommandGrace = 9 * time.Second
)

var (
	directQPCMVEnabledLine         = regexp.MustCompile(`(?i)^\+QPCMV:\s*1\s*,\s*(?:0|2)\s*$`)
	directQPCMVDisabledLine        = regexp.MustCompile(`(?i)^\+QPCMV:\s*0(?:\s*,\s*[0-2])?\s*$`)
	directQPCMVUninitializedLine   = regexp.MustCompile(`(?i)^\+QPCMV:\s*\(\s*0\s*,\s*1\s*\)\s*,\s*\(\s*0\s*-\s*2\s*\)\s*$`)
	errDirectQPCMVOwnerStillInCall = errors.New("voice call is still present; QPCMV cleanup is pending")
	errDirectCallCommandRejected   = errors.New("modem explicitly rejected the call command")
	errDirectCallCommandAmbiguous  = errors.New("call command outcome is ambiguous")
)

type directQPCMVRouteState struct {
	ready        bool
	phase        string
	locationID   uint32
	identityHash string
	intent       directQPCMVCallIntent
	generation   uint64
	updatedAt    time.Time
}

type directQPCMVCallIntent struct {
	direction string
	callID    string
	index     int
}

type directQPCMVCleanupResult struct {
	required  bool
	attempted bool
	confirmed bool
	implicit  bool
	err       error
}

type directQPCMVCallGate func(*usbAT, usbATPhysicalIdentity) error

// directQPCMVExplicitOK accepts an echoed AT response only when the modem also
// returned a standalone OK line and no ERROR/CME/CMS marker. Transport success
// without an explicit modem acknowledgement never authorizes a state change.
func directQPCMVExplicitOK(response string) bool {
	return atQuerySucceeded(response) && !atResponseIsError(response)
}

func directQPCMVStateMatches(response string, statePattern *regexp.Regexp) bool {
	if !directQPCMVExplicitOK(response) {
		return false
	}
	found := false
	for _, rawLine := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(strings.ToUpper(line), "+QPCMV:") {
			continue
		}
		if found || !statePattern.MatchString(line) {
			return false
		}
		found = true
	}
	return found
}

// Capability output such as "+QPCMV: (0,1),(0-2)" is deliberately rejected.
func directQPCMVUACEnabled(response string) bool {
	return directQPCMVStateMatches(response, directQPCMVEnabledLine)
}

func directQPCMVDisabled(response string) bool {
	return directQPCMVStateMatches(response, directQPCMVDisabledLine)
}

// Some QDC507 firmware reports the AT test-form capability tuple from
// AT+QPCMV? after a cold boot, before any route has ever been selected. That
// state is not proof of enabled audio, but neither is it an explicit disabled
// state; it may only be normalized by one idle QPCMV=0 mutation and readback.
func directQPCMVUninitialized(response string) bool {
	return directQPCMVStateMatches(response, directQPCMVUninitializedLine)
}

// Some QDC507 firmware accepts QPCMV mutations but implements neither the
// read nor test form after UAC re-enumeration.  Accept only the exact echoed
// command plus one bare ERROR line as that legacy, write-only capability.  A
// transport failure, CME/CMS error, extra payload, or duplicate ERROR remains
// ambiguous and must fail closed.
func directQPCMVCommandUnsupported(response, command string) bool {
	found := false
	for _, rawLine := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.EqualFold(line, command) {
			continue
		}
		if !strings.EqualFold(line, "ERROR") || found {
			return false
		}
		found = true
	}
	return found
}

func directQPCMVQueryUnsupported(response string) bool {
	return directQPCMVCommandUnsupported(response, directQPCMVQueryCommand)
}

func directQPCMVCommandError(command, response string, err error) error {
	if err != nil {
		return fmt.Errorf("%s transport failed: %w", command, err)
	}
	if !directQPCMVExplicitOK(response) {
		return fmt.Errorf("%s was not explicitly acknowledged with OK", command)
	}
	return nil
}

func directCallCommandError(command, response string, err error) error {
	label := "call command"
	upper := strings.ToUpper(strings.TrimSpace(command))
	if strings.HasPrefix(upper, "ATD") {
		label = "ATD"
	} else if upper == "ATA" {
		label = "ATA"
	}
	if err != nil {
		return fmt.Errorf("%w: %s transport failed: %v", errDirectCallCommandAmbiguous, label, err)
	}
	if directQPCMVExplicitOK(response) {
		return nil
	}
	if atResponseIsError(response) {
		return fmt.Errorf("%w: %s", errDirectCallCommandRejected, label)
	}
	return fmt.Errorf("%w: %s returned neither explicit OK nor explicit ERROR", errDirectCallCommandAmbiguous, label)
}

func (a *app) setDirectQPCMVState(ready bool, phase, detail string, locationID uint32, generation uint64, err error) {
	a.setDirectQPCMVStateForIdentity(ready, phase, detail, locationID, "", generation, err)
}

func (a *app) setDirectQPCMVStateForIdentity(ready bool, phase, detail string, locationID uint32, identityHash string, generation uint64, err error) {
	a.setDirectQPCMVStateForOwner(ready, phase, detail, locationID, identityHash, directQPCMVCallIntent{index: -1}, generation, err)
}

func (a *app) setDirectQPCMVStateForOwner(ready bool, phase, detail string, locationID uint32, identityHash string, intent directQPCMVCallIntent, generation uint64, err error) {
	a.moduleVoiceMu.Lock()
	a.moduleVoiceReady = ready
	a.moduleVoiceLast = time.Now()
	a.moduleVoicePhase = phase
	a.moduleVoiceDetail = detail
	a.moduleVoiceLocation = locationID
	a.moduleVoiceIdentityHash = identityHash
	a.moduleVoiceIntent = intent
	a.moduleVoiceCallGeneration = generation
	if err != nil {
		a.moduleVoiceErr = err.Error()
	} else {
		a.moduleVoiceErr = ""
	}
	a.moduleVoiceMu.Unlock()
}

func (a *app) directQPCMVRouteState() directQPCMVRouteState {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	return directQPCMVRouteState{
		ready:        a.moduleVoiceReady,
		phase:        a.moduleVoicePhase,
		locationID:   a.moduleVoiceLocation,
		identityHash: a.moduleVoiceIdentityHash,
		intent:       a.moduleVoiceIntent,
		generation:   a.moduleVoiceCallGeneration,
		updatedAt:    a.moduleVoiceLast,
	}
}

func (a *app) directQPCMVBlocksPersistentMutation() bool {
	state := a.directQPCMVRouteState()
	if state.ready || state.generation != 0 {
		return true
	}
	switch state.phase {
	case "", "stopped", "unavailable", "failed":
		return false
	default:
		return true
	}
}

func validateDirectQPCMVIdentity(command usbATCommandFunc, expectedIdentityHash string) error {
	if expectedIdentityHash == "" {
		return nil
	}
	response, err := command("AT+CGSN", directQPCMVCommandTimeout)
	if err != nil {
		return fmt.Errorf("read module identity before QPCMV: %w", err)
	}
	imei, err := parseModuleIMEI(response)
	if err != nil {
		return err
	}
	if moduleIdentityDigest(imei) != expectedIdentityHash {
		return errors.New("current module identity does not match the QPCMV transaction")
	}
	return nil
}

func directQPCMVIntentMatchesTicket(intent directQPCMVCallIntent, ticket callMediaTicket) bool {
	if intent.direction == "" || intent.direction != ticket.Direction {
		return false
	}
	if intent.callID != "" && intent.callID != ticket.CallID {
		return false
	}
	if intent.index >= 0 && intent.index != ticket.Index {
		return false
	}
	return true
}

func validateDirectQPCMVComposition(command usbATCommandFunc) error {
	response, err := command(`AT+QCFG="USBCFG"`, directQPCMVCommandTimeout)
	if err != nil {
		return fmt.Errorf("read USB composition before QPCMV: %w", err)
	}
	composition, err := parseUSBComposition(response)
	if err != nil {
		return err
	}
	if !composition.isUACTarget() {
		return errors.New("direct QPCMV requires the exact ADB-off/UAC-on USB composition")
	}
	return nil
}

func queryDirectQPCMVEnabled(command usbATCommandFunc) error {
	response, err := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
	if err != nil {
		return fmt.Errorf("%s transport failed: %w", directQPCMVQueryCommand, err)
	}
	if !directQPCMVUACEnabled(response) && !directQPCMVQueryUnsupported(response) {
		return errors.New("QPCMV query did not confirm enabled UAC option 2")
	}
	return nil
}

// Some QDC507 firmware exposes the UAC device but implements QPCMV as a
// write-only selector: both the mutation and the read form echo the command
// and return a bare ERROR.  The selector still has to be sent for every call;
// an unsupported disable/readback must never be mistaken for an already
// enabled route.
func enableDirectQPCMV(command usbATCommandFunc) error {
	response, err := command(directQPCMVEnableCommand, directQPCMVCommandTimeout)
	if err != nil {
		return fmt.Errorf("%s transport failed: %w", directQPCMVEnableCommand, err)
	}
	if directQPCMVCommandUnsupported(response, directQPCMVEnableCommand) {
		queryResponse, queryErr := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
		if queryErr != nil || !directQPCMVQueryUnsupported(queryResponse) {
			return errors.New("QPCMV write-only enable was not followed by the same unsupported query state")
		}
		return nil
	}
	if err := directQPCMVCommandError(directQPCMVEnableCommand, response, nil); err != nil {
		return err
	}
	return queryDirectQPCMVEnabled(command)
}

// cleanupDirectQPCMVIdle checks CLCC immediately before the single disable
// attempt. It never sends QPCMV=0 while any voice call is present. If disable
// or its readback is ambiguous, callers must publish cleanup_failed and must
// not retry the mutation automatically.
func cleanupDirectQPCMVNonActive(command usbATCommandFunc, allowRinging bool) directQPCMVCleanupResult {
	clccResponse, err := command("AT+CLCC", directQPCMVCommandTimeout)
	if err != nil {
		return directQPCMVCleanupResult{err: fmt.Errorf("fresh CLCC before QPCMV cleanup failed: %w", err)}
	}
	if !directQPCMVExplicitOK(clccResponse) {
		return directQPCMVCleanupResult{err: errors.New("fresh CLCC before QPCMV cleanup was not explicitly acknowledged")}
	}
	calls := parseCLCC(clccResponse)
	if len(calls) != 0 {
		ringing := allowRinging && len(calls) == 1 &&
			(calls[0].State == "incoming" || calls[0].State == "waiting")
		if !ringing {
			return directQPCMVCleanupResult{err: errDirectQPCMVOwnerStillInCall}
		}
	}

	disableResponse, disableErr := command(directQPCMVDisableCommand, directQPCMVCommandTimeout)
	result := directQPCMVCleanupResult{attempted: true}
	if disableErr == nil && directQPCMVCommandUnsupported(disableResponse, directQPCMVDisableCommand) {
		queryResponse, queryErr := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
		if queryErr != nil || !directQPCMVQueryUnsupported(queryResponse) {
			result.err = errors.New("QPCMV implicit-UAC capability was not stable after unsupported disable")
			return result
		}
		result.confirmed = true
		result.implicit = true
		return result
	}
	if err := directQPCMVCommandError(directQPCMVDisableCommand, disableResponse, disableErr); err != nil {
		result.err = err
		return result
	}
	queryResponse, queryErr := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
	if queryErr != nil {
		result.err = fmt.Errorf("QPCMV disabled-state query failed: %w", queryErr)
		return result
	}
	if !directQPCMVDisabled(queryResponse) && !directQPCMVQueryUnsupported(queryResponse) {
		result.err = errors.New("QPCMV query did not confirm the disabled state")
		return result
	}
	result.confirmed = true
	return result
}

func cleanupDirectQPCMVIdle(command usbATCommandFunc) directQPCMVCleanupResult {
	return cleanupDirectQPCMVNonActive(command, false)
}

// cleanupDirectQPCMVForModuleSetup is intentionally separate from the live
// call-route cleanup. The QDC507 reports attached packet data contexts in
// CLCC; only the confirmed setup transaction may treat fully validated
// non-voice rows as maintenance-idle before disabling the reversible route.
func cleanupDirectQPCMVForModuleSetup(command usbATCommandFunc) directQPCMVCleanupResult {
	clccResponse, err := command("AT+CLCC", directQPCMVCommandTimeout)
	if err != nil {
		return directQPCMVCleanupResult{err: fmt.Errorf("fresh CLCC before setup QPCMV cleanup failed: %w", err)}
	}
	if !directQPCMVExplicitOK(clccResponse) {
		return directQPCMVCleanupResult{err: errors.New("fresh CLCC before setup QPCMV cleanup was not explicitly acknowledged")}
	}
	if err := validateCLCCVoiceIdleForModuleSetup(clccResponse); err != nil {
		return directQPCMVCleanupResult{err: errDirectQPCMVOwnerStillInCall}
	}

	disableResponse, disableErr := command(directQPCMVDisableCommand, directQPCMVCommandTimeout)
	result := directQPCMVCleanupResult{attempted: true}
	if disableErr == nil && directQPCMVCommandUnsupported(disableResponse, directQPCMVDisableCommand) {
		queryResponse, queryErr := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
		if queryErr != nil || !directQPCMVQueryUnsupported(queryResponse) {
			result.err = errors.New("QPCMV implicit-UAC capability was not stable after unsupported disable")
			return result
		}
		result.confirmed = true
		result.implicit = true
		return result
	}
	if err := directQPCMVCommandError(directQPCMVDisableCommand, disableResponse, disableErr); err != nil {
		result.err = err
		return result
	}
	queryResponse, queryErr := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
	if queryErr != nil {
		result.err = fmt.Errorf("QPCMV disabled-state query failed: %w", queryErr)
		return result
	}
	if !directQPCMVDisabled(queryResponse) && !directQPCMVQueryUnsupported(queryResponse) {
		result.err = errors.New("QPCMV query did not confirm the disabled state")
		return result
	}
	result.confirmed = true
	return result
}

// ensureDirectQPCMVDisabledBeforeEnable never trusts the in-memory route
// phase. It reads the modem immediately before every enable. A clearly
// disabled modem may proceed. A stale enabled UAC-option-2 route is disabled
// exactly once while CLCC is still non-active, with disabled readback; any
// other or ambiguous state fails closed without issuing enable.
func ensureDirectQPCMVDisabledBeforeEnable(command usbATCommandFunc, allowRinging bool) directQPCMVCleanupResult {
	response, err := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
	if err != nil {
		return directQPCMVCleanupResult{err: fmt.Errorf("QPCMV pre-enable state query failed: %w", err)}
	}
	if directQPCMVDisabled(response) {
		return directQPCMVCleanupResult{confirmed: true}
	}
	if directQPCMVUninitialized(response) {
		result := cleanupDirectQPCMVNonActive(command, allowRinging)
		result.required = true
		return result
	}
	if directQPCMVQueryUnsupported(response) {
		result := cleanupDirectQPCMVNonActive(command, allowRinging)
		result.required = true
		return result
	}
	if !directQPCMVUACEnabled(response) {
		return directQPCMVCleanupResult{err: errors.New("QPCMV pre-enable state was neither explicitly disabled nor enabled UAC option 2")}
	}
	result := cleanupDirectQPCMVNonActive(command, allowRinging)
	result.required = true
	return result
}

func ensureDirectQPCMVDisabledBeforeModuleSetupEnable(command usbATCommandFunc) directQPCMVCleanupResult {
	response, err := command(directQPCMVQueryCommand, directQPCMVCommandTimeout)
	if err != nil {
		return directQPCMVCleanupResult{err: fmt.Errorf("QPCMV pre-enable state query failed: %w", err)}
	}
	if directQPCMVDisabled(response) {
		return directQPCMVCleanupResult{confirmed: true}
	}
	if !directQPCMVUninitialized(response) && !directQPCMVUACEnabled(response) &&
		!directQPCMVQueryUnsupported(response) {
		return directQPCMVCleanupResult{err: errors.New("QPCMV pre-enable state was neither explicitly disabled nor enabled UAC option 2")}
	}
	result := cleanupDirectQPCMVForModuleSetup(command)
	result.required = true
	return result
}

func directQPCMVCleanupError(primary error, cleanup directQPCMVCleanupResult) error {
	if cleanup.err == nil {
		return primary
	}
	if primary == nil {
		return cleanup.err
	}
	return fmt.Errorf("%w; QPCMV cleanup was not confirmed: %v", primary, cleanup.err)
}

// preflightDirectQPCMV preserves the location-only API used by setup. New
// callers which already hold a module identity snapshot should use
// preflightDirectQPCMVExpected so CGSN is checked on the same exact handle.
func (a *app) preflightDirectQPCMV(locationID uint32) error {
	return a.preflightDirectQPCMVAtLocation(locationID, "")
}

func (a *app) preflightDirectQPCMVExpected(expected moduleSetupSnapshot) error {
	if expected.DeviceIdentityHash == "" {
		return errors.New("direct QPCMV preflight requires the expected module identity")
	}
	return a.preflightDirectQPCMVAtLocation(expected.USBLocationID, expected.DeviceIdentityHash)
}

// preflightDirectQPCMVAtLocation performs the reversible no-call sequence on
// one exact lifecycle-pinned usbAT handle:
//
//	[CGSN] -> USBCFG -> empty CLCC -> enable -> enabled query
//	       -> empty CLCC -> disable -> disabled query
//
// The caller may already hold moduleMutationMu and the exact-location guard;
// this function therefore neither acquires nor releases them.
func (a *app) preflightDirectQPCMVAtLocation(locationID uint32, expectedIdentityHash string) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if locationID == 0 {
		return errors.New("direct QPCMV preflight requires an exact USB location")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()

	if !a.moduleSetupCallIsIdle() {
		return errors.New("a voice call is cached; direct QPCMV preflight was not attempted")
	}
	state := a.directQPCMVRouteState()
	if state.ready || state.generation != 0 || state.phase == "armed" ||
		state.phase == "cleanup_pending" || state.phase == "cleanup_failed" {
		return fmt.Errorf("an existing or uncertain voice route blocks preflight (phase=%q generation=%d)", state.phase, state.generation)
	}

	var enableAttempted bool
	var staleCleanup directQPCMVCleanupResult
	var cleanup directQPCMVCleanupResult
	_, sessionErr := a.withExclusiveUSBATCommandsAtLocationExact(locationID, func(command usbATCommandFunc) error {
		if err := validateDirectQPCMVIdentity(command, expectedIdentityHash); err != nil {
			return err
		}
		if err := validateDirectQPCMVComposition(command); err != nil {
			return err
		}
		clccResponse, err := command("AT+CLCC", directQPCMVCommandTimeout)
		if err != nil {
			return fmt.Errorf("fresh CLCC before QPCMV preflight failed: %w", err)
		}
		if !directQPCMVExplicitOK(clccResponse) || validateCLCCVoiceIdleForModuleSetup(clccResponse) != nil || !a.moduleSetupCallIsIdle() {
			return errors.New("fresh CLCC did not confirm an idle modem before QPCMV preflight")
		}
		staleCleanup = ensureDirectQPCMVDisabledBeforeModuleSetupEnable(command)
		if staleCleanup.err != nil {
			return fmt.Errorf("QPCMV was not safely disabled before preflight enable: %w", staleCleanup.err)
		}
		// A stale-route cleanup contains another CLCC read, but revalidate after
		// its mutation so a call transition cannot authorize the new enable.
		if staleCleanup.attempted {
			clccResponse, err = command("AT+CLCC", directQPCMVCommandTimeout)
			if err != nil || !directQPCMVExplicitOK(clccResponse) || validateCLCCVoiceIdleForModuleSetup(clccResponse) != nil || !a.moduleSetupCallIsIdle() {
				return errors.New("idle call state changed after stale QPCMV cleanup")
			}
		}
		enableAttempted = true
		primaryErr := enableDirectQPCMV(command)
		cleanup = cleanupDirectQPCMVForModuleSetup(command)
		return directQPCMVCleanupError(primaryErr, cleanup)
	})

	if sessionErr != nil {
		phase := "unknown"
		detail := "direct QPCMV preflight failed before route state was proven"
		if staleCleanup.confirmed && !enableAttempted {
			phase = "stopped"
			detail = "direct QPCMV preflight failed; route confirmed off"
		}
		if staleCleanup.required && !staleCleanup.confirmed {
			phase = "cleanup_pending"
			if staleCleanup.attempted {
				phase = "cleanup_failed"
			}
			detail = "stale direct QPCMV route cleanup unconfirmed"
		}
		if enableAttempted {
			if cleanup.confirmed {
				phase = "stopped"
				detail = "direct QPCMV preflight failed; route confirmed off"
			} else {
				if cleanup.attempted {
					phase = "cleanup_failed"
				} else {
					phase = "cleanup_pending"
				}
				detail = "direct QPCMV preflight cleanup unconfirmed"
			}
		}
		a.setDirectQPCMVState(false, phase, detail, locationID, 0, sessionErr)
		return sessionErr
	}
	a.setDirectQPCMVState(false, "stopped", "direct QPCMV UAC option 2 preflight passed", locationID, 0, nil)
	return nil
}

// armDirectQPCMV enables UAC option 2 before a call becomes active. With
// requireNoActive=true it requires empty CLCC (outgoing/setup path); false
// requires exactly one cached-and-fresh incoming/waiting call (answer path).
// It never treats an active call as an opportunity to issue the first enable.
func (a *app) armDirectQPCMV(expectedLocation uint32, expectedIdentityHash string, requireNoActive bool) error {
	return a.armDirectQPCMVWithPostArm(expectedLocation, expectedIdentityHash, requireNoActive, nil)
}

// armDirectQPCMVAndExecuteCall keeps QPCMV enable and the call mutation on the
// same lifecycle-pinned USB handle. A transport-ambiguous ATD/ATA response does
// not authorize an automatic disable: the call may already have reached the
// network, so the armed route is preserved for the next fresh CLCC poll to
// either adopt or clean while idle.
func (a *app) armDirectQPCMVAndExecuteCall(expectedLocation uint32, expectedIdentityHash string, requireNoActive bool, callCommand string, timeout time.Duration) (string, error) {
	return a.armDirectQPCMVAndExecuteCallWithGate(
		expectedLocation, expectedIdentityHash, requireNoActive, callCommand, timeout, nil,
	)
}

// armDirectQPCMVAndExecuteCallWithGate adds a final, side-effect-free
// authorization check after the second fresh CLCC and enabled-state readback,
// but before ATD/ATA on the same lifecycle-pinned handle. The gate must not
// acquire callMu: the arm transaction deliberately holds callMu.RLock across
// this final interval so the cached topology cannot change underneath it.
func (a *app) armDirectQPCMVAndExecuteCallWithGate(expectedLocation uint32, expectedIdentityHash string, requireNoActive bool, callCommand string, timeout time.Duration, gate directQPCMVCallGate) (string, error) {
	if strings.TrimSpace(callCommand) == "" {
		return "", errors.New("direct QPCMV call command is empty")
	}
	var response string
	var callErr error
	err := a.armDirectQPCMVWithPostArm(expectedLocation, expectedIdentityHash, requireNoActive, func(device *usbAT, identity usbATPhysicalIdentity, command usbATCommandFunc) error {
		if gate != nil {
			if err := gate(device, identity); err != nil {
				return err
			}
		}
		response, callErr = command(callCommand, timeout)
		callErr = directCallCommandError(callCommand, response, callErr)
		return callErr
	})
	return response, err
}

// executeCallOnArmedDirectQPCMV is used by the answer path after the incoming
// call poller has already armed that exact call. It revalidates module identity,
// call intent, fresh ringing CLCC and QPCMV on one handle, then immediately
// issues ATA without sending a second QPCMV enable.
func (a *app) executeCallOnArmedDirectQPCMV(callCommand string, timeout time.Duration) (string, error) {
	return a.executeCallOnArmedDirectQPCMVWithGate(callCommand, timeout, nil)
}

// executeCallOnArmedDirectQPCMVWithGate applies the same immediate
// authorization gate to an already-armed incoming call. Unlike the arm path,
// this function releases its short cached-call read lock before invoking the
// gate, so the gate may inspect other in-memory ownership state but must remain
// mutation-free.
func (a *app) executeCallOnArmedDirectQPCMVWithGate(callCommand string, timeout time.Duration, gate directQPCMVCallGate) (string, error) {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return "", err
	}
	if strings.TrimSpace(callCommand) == "" {
		return "", errors.New("direct QPCMV call command is empty")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if state.phase != "armed" || state.ready || state.generation != 0 || state.locationID == 0 ||
		state.identityHash == "" || state.intent.direction != "incoming" || state.intent.callID == "" || state.intent.index < 0 {
		return "", errors.New("the exact incoming call is not safely armed")
	}
	var response string
	var commandErr error
	execution, sessionErr := a.withExclusiveUSBATCommandsAtLocationPinned(state.locationID, func(device *usbAT, identity usbATPhysicalIdentity, command usbATCommandFunc) error {
		if err := validateDirectQPCMVIdentity(command, state.identityHash); err != nil {
			return err
		}
		clccResponse, err := command("AT+CLCC", directQPCMVCommandTimeout)
		if err != nil || !directQPCMVExplicitOK(clccResponse) {
			return errors.New("fresh CLCC before answer was not explicitly acknowledged")
		}
		calls := parseCLCC(clccResponse)
		if len(calls) != 1 || calls[0].Direction != "incoming" || calls[0].Index != state.intent.index ||
			(calls[0].State != "incoming" && calls[0].State != "waiting") {
			return errors.New("fresh ringing call no longer matches the armed call")
		}
		a.callMu.RLock()
		cachedMatches := a.activeCall != nil && a.activeCall.ID == state.intent.callID &&
			a.activeCall.Index == state.intent.index && a.activeCall.Direction == "incoming" &&
			(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
		a.callMu.RUnlock()
		if !cachedMatches {
			return errors.New("cached ringing call no longer matches the armed call")
		}
		if err := queryDirectQPCMVEnabled(command); err != nil {
			return err
		}
		if gate != nil {
			if err := gate(device, identity); err != nil {
				return err
			}
		}
		response, commandErr = command(callCommand, timeout)
		commandErr = directCallCommandError(callCommand, response, commandErr)
		return commandErr
	})
	if commandErr != nil {
		phase := "armed"
		if errors.Is(commandErr, errDirectCallCommandAmbiguous) {
			phase = "call_command_ambiguous"
		}
		if execution.device != nil && !a.isCurrentUSBAT(execution.device, state.locationID) {
			phase = "unknown"
		}
		a.setDirectQPCMVStateForOwner(false, phase, "direct QPCMV armed; answer command requires fresh CLCC", state.locationID, state.identityHash, state.intent, 0, commandErr)
		return response, commandErr
	}
	if sessionErr != nil {
		return response, sessionErr
	}
	a.setDirectQPCMVStateForOwner(false, "armed", "direct QPCMV armed and answer command accepted", state.locationID, state.identityHash, state.intent, 0, nil)
	return response, nil
}

func (a *app) armDirectQPCMVWithPostArm(expectedLocation uint32, expectedIdentityHash string, requireNoActive bool, postArm func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if expectedLocation == 0 {
		return errors.New("direct QPCMV arm requires an exact USB location")
	}
	if expectedIdentityHash == "" {
		return errors.New("direct QPCMV arm requires an exact module identity")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	intent, intentErr := a.directQPCMVArmIntent(requireNoActive)
	if intentErr != nil {
		return intentErr
	}

	state := a.directQPCMVRouteState()
	if state.ready || state.generation != 0 ||
		(state.phase != "" && state.phase != "stopped" && state.phase != "unavailable" && state.phase != "failed") {
		return fmt.Errorf("an existing or uncertain voice route blocks arm (phase=%q generation=%d)", state.phase, state.generation)
	}
	a.setDirectQPCMVStateForOwner(false, "arming", "direct QPCMV UAC option 2 arm", expectedLocation, expectedIdentityHash, intent, 0, nil)

	var enableAttempted bool
	var routeEnabled bool
	var postArmAttempted bool
	var postArmErr error
	var staleCleanup directQPCMVCleanupResult
	var cleanup directQPCMVCleanupResult
	execution, sessionErr := a.withExclusiveUSBATCommandsAtLocationPinned(expectedLocation, func(device *usbAT, identity usbATPhysicalIdentity, command usbATCommandFunc) error {
		if err := validateDirectQPCMVIdentity(command, expectedIdentityHash); err != nil {
			return err
		}
		if err := validateDirectQPCMVComposition(command); err != nil {
			return err
		}
		clccResponse, err := command("AT+CLCC", directQPCMVCommandTimeout)
		if err != nil {
			return fmt.Errorf("fresh CLCC before direct QPCMV arm failed: %w", err)
		}
		if !directQPCMVExplicitOK(clccResponse) {
			return errors.New("fresh CLCC before direct QPCMV arm was not explicitly acknowledged")
		}
		calls := parseCLCC(clccResponse)

		a.callMu.RLock()
		defer a.callMu.RUnlock()
		if err := a.validateDirectQPCMVArmCallLocked(calls, requireNoActive); err != nil {
			return err
		}
		staleCleanup = ensureDirectQPCMVDisabledBeforeEnable(command, !requireNoActive)
		if staleCleanup.err != nil {
			return fmt.Errorf("QPCMV was not safely disabled before arm: %w", staleCleanup.err)
		}
		if staleCleanup.attempted {
			clccResponse, err = command("AT+CLCC", directQPCMVCommandTimeout)
			if err != nil || !directQPCMVExplicitOK(clccResponse) {
				return errors.New("fresh CLCC failed after stale QPCMV cleanup")
			}
			if err := a.validateDirectQPCMVArmCallLocked(parseCLCC(clccResponse), requireNoActive); err != nil {
				return fmt.Errorf("call state changed after stale QPCMV cleanup: %w", err)
			}
		}

		enableAttempted = true
		if primaryErr := enableDirectQPCMV(command); primaryErr != nil {
			cleanup = cleanupDirectQPCMVIdle(command)
			return directQPCMVCleanupError(primaryErr, cleanup)
		}
		routeEnabled = true
		if postArm != nil {
			clccResponse, err = command("AT+CLCC", directQPCMVCommandTimeout)
			if err != nil || !directQPCMVExplicitOK(clccResponse) {
				return errors.New("fresh CLCC immediately before call command was not explicitly acknowledged")
			}
			if err := a.validateDirectQPCMVArmCallLocked(parseCLCC(clccResponse), requireNoActive); err != nil {
				return fmt.Errorf("call state changed immediately before call command: %w", err)
			}
			postArmAttempted = true
			postArmErr = postArm(device, identity, command)
			return postArmErr
		}
		return nil
	})

	// The route itself was explicitly enabled and read back before the call
	// command ran. Explicit rejection is cleaned once in-session when possible;
	// an ambiguous outcome is retained through a grace interval because CLCC may
	// lag behind a call command which actually reached the network.
	if routeEnabled && postArmAttempted {
		detail := "direct QPCMV UAC option 2 armed and call command accepted"
		phase := "armed"
		if errors.Is(postArmErr, errDirectCallCommandAmbiguous) {
			phase = "call_command_ambiguous"
			detail = "direct QPCMV armed; call command outcome requires fresh CLCC"
		} else if errors.Is(postArmErr, errDirectCallCommandRejected) {
			phase = "cleanup_pending"
			detail = "call command rejected; direct QPCMV route awaits fresh idle cleanup"
		}
		// A transport loss may already have detached this exact lifecycle object.
		// Never republish armed ownership onto a replacement at the same location.
		if execution.device != nil && !a.isCurrentUSBAT(execution.device, expectedLocation) {
			err := fmt.Errorf("direct QPCMV call session detached before ownership could be published: %w", postArmErr)
			a.setDirectQPCMVStateForIdentity(false, "unknown", "direct QPCMV call session detached", expectedLocation, expectedIdentityHash, 0, err)
			return err
		}
		a.setDirectQPCMVStateForOwner(false, phase, detail, expectedLocation, expectedIdentityHash, intent, 0, postArmErr)
		return postArmErr
	}

	if sessionErr != nil {
		phase := "unknown"
		detail := "direct QPCMV arm failed before route state was proven"
		if staleCleanup.confirmed && !enableAttempted {
			phase = "stopped"
			detail = "direct QPCMV arm failed; route confirmed off"
		}
		if staleCleanup.required && !staleCleanup.confirmed {
			phase = "cleanup_pending"
			if staleCleanup.attempted {
				phase = "cleanup_failed"
			}
			detail = "stale direct QPCMV route cleanup unconfirmed"
		}
		if enableAttempted {
			if cleanup.confirmed {
				phase = "stopped"
				detail = "direct QPCMV arm failed; route confirmed off"
			} else {
				if cleanup.attempted {
					phase = "cleanup_failed"
				} else {
					phase = "cleanup_pending"
				}
				detail = "direct QPCMV arm cleanup unconfirmed"
			}
		}
		a.setDirectQPCMVStateForOwner(false, phase, detail, expectedLocation, expectedIdentityHash, intent, 0, sessionErr)
		return sessionErr
	}
	a.setDirectQPCMVStateForOwner(false, "armed", "direct QPCMV UAC option 2 armed before active call", expectedLocation, expectedIdentityHash, intent, 0, nil)
	return nil
}

func (a *app) directQPCMVArmIntent(requireNoActive bool) (directQPCMVCallIntent, error) {
	if requireNoActive {
		return directQPCMVCallIntent{direction: "outgoing", index: -1}, nil
	}
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if a.activeCall == nil || a.activeCall.ID == "" || a.activeCall.Direction != "incoming" ||
		(a.activeCall.State != "incoming" && a.activeCall.State != "waiting") {
		return directQPCMVCallIntent{}, errors.New("answer direct QPCMV arm requires one cached incoming call")
	}
	return directQPCMVCallIntent{direction: "incoming", callID: a.activeCall.ID, index: a.activeCall.Index}, nil
}

func (a *app) validateDirectQPCMVArmCallLocked(calls []parsedCall, requireNoActive bool) error {
	if requireNoActive {
		if len(calls) != 0 || a.activeCall != nil || a.callMediaEligible {
			return errors.New("outgoing direct QPCMV arm requires fresh and cached idle call state")
		}
		return nil
	}
	if len(calls) != 1 || (calls[0].State != "incoming" && calls[0].State != "waiting") {
		return errors.New("answer direct QPCMV arm requires exactly one incoming or waiting call")
	}
	if !a.callTopologyKnown || a.callGeneration == 0 || a.activeCall == nil ||
		a.activeCall.ID == "" || a.activeCall.Index != calls[0].Index ||
		a.activeCall.Direction != calls[0].Direction || a.activeCall.State != calls[0].State {
		return errors.New("fresh ringing call does not match the cached call generation")
	}
	return nil
}

// adoptArmedDirectQPCMVForCall is the only active-call transition. It performs
// fresh CLCC and QPCMV? on the exact armed handle, then assigns generation
// ownership. It never sends QPCMV=1,2 while the call is active.
func (a *app) adoptArmedDirectQPCMVForCall(ticket callMediaTicket) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 ||
		(ticket.Direction != "incoming" && ticket.Direction != "outgoing") {
		return errors.New("invalid call media ticket")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()

	state := a.directQPCMVRouteState()
	if state.ready && state.phase == "ready" && state.generation == ticket.Generation {
		if state.locationID == 0 || !a.callMediaTicketIsCurrent(ticket) {
			return errors.New("ready direct QPCMV route does not match the exact current call ticket")
		}
		return nil
	}
	if (state.phase != "armed" && state.phase != "call_command_ambiguous") || state.ready || state.generation != 0 || state.locationID == 0 {
		return errors.New("direct QPCMV was not safely armed before the call became active")
	}
	if state.identityHash == "" || !directQPCMVIntentMatchesTicket(state.intent, ticket) {
		return errors.New("direct QPCMV arm does not belong to this physical module and call intent")
	}
	if state.updatedAt.IsZero() || time.Since(state.updatedAt) > directQPCMVArmTTL {
		err := errors.New("direct QPCMV arm expired before active-call adoption")
		a.setDirectQPCMVStateForOwner(false, "cleanup_pending", "expired direct QPCMV arm awaits idle cleanup", state.locationID, state.identityHash, state.intent, ticket.Generation, err)
		return err
	}
	if !a.callMediaTicketIsCurrent(ticket) {
		return errors.New("call topology changed before direct QPCMV adoption")
	}

	_, sessionErr := a.withExclusiveUSBATCommandsAtLocationExact(state.locationID, func(command usbATCommandFunc) error {
		if err := validateDirectQPCMVIdentity(command, state.identityHash); err != nil {
			return err
		}
		clccResponse, err := command("AT+CLCC", directQPCMVCommandTimeout)
		if err != nil {
			return fmt.Errorf("fresh CLCC before direct QPCMV adoption failed: %w", err)
		}
		if !directQPCMVExplicitOK(clccResponse) || !singleActiveCallMatches(parseCLCC(clccResponse), ticket) {
			return errors.New("fresh CLCC did not confirm the same single active call")
		}

		a.callMu.RLock()
		defer a.callMu.RUnlock()
		if !a.callMediaTicketIsCurrentLocked(ticket) {
			return errors.New("call topology changed immediately before direct QPCMV adoption")
		}
		if err := queryDirectQPCMVEnabled(command); err != nil {
			return err
		}
		// Publish ownership before releasing callMu so a topology change cannot
		// race between validation and the generation assignment.
		a.setDirectQPCMVStateForOwner(true, "ready", "direct QPCMV UAC option 2 adopted by active call", state.locationID, state.identityHash, state.intent, ticket.Generation, nil)
		return nil
	})
	if sessionErr != nil {
		// No mutation is safe while this call may be active. Preserve ownership
		// for generation-scoped cleanup after CLCC becomes empty.
		a.setDirectQPCMVStateForOwner(false, "cleanup_pending", "direct QPCMV adoption unconfirmed; awaiting idle cleanup", state.locationID, state.identityHash, state.intent, ticket.Generation, sessionErr)
		return sessionErr
	}
	return nil
}

// confirmDirectQPCMVForCall is the final host-audio gate after CoreAudio has
// opened the exact UAC device. Unlike the idempotent poller adoption path, it
// always performs a new CLCC and QPCMV read on the owned USB location.
func (a *app) confirmDirectQPCMVForCall(ticket callMediaTicket) error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 ||
		(ticket.Direction != "incoming" && ticket.Direction != "outgoing") {
		return errors.New("invalid call media ticket")
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if !state.ready || state.phase != "ready" || state.generation != ticket.Generation || state.locationID == 0 {
		return errors.New("direct QPCMV route no longer belongs to the call ticket")
	}
	if !a.callMediaTicketIsCurrent(ticket) {
		return errors.New("call topology changed before direct QPCMV confirmation")
	}
	_, err := a.withExclusiveUSBATCommandsAtLocationExact(state.locationID, func(command usbATCommandFunc) error {
		if err := validateDirectQPCMVIdentity(command, state.identityHash); err != nil {
			return err
		}
		clccResponse, commandErr := command("AT+CLCC", directQPCMVCommandTimeout)
		if commandErr != nil {
			return fmt.Errorf("fresh CLCC before direct QPCMV confirmation failed: %w", commandErr)
		}
		if !directQPCMVExplicitOK(clccResponse) || !singleActiveCallMatches(parseCLCC(clccResponse), ticket) {
			return errors.New("fresh CLCC did not confirm the same single active call")
		}
		a.callMu.RLock()
		defer a.callMu.RUnlock()
		if !a.callMediaTicketIsCurrentLocked(ticket) {
			return errors.New("call topology changed during direct QPCMV confirmation")
		}
		return queryDirectQPCMVEnabled(command)
	})
	return err
}

func (a *app) callMediaTicketIsCurrentLocked(expected callMediaTicket) bool {
	if !a.callTopologyKnown || !a.callMediaEligible || a.activeCall == nil ||
		a.activeCall.State != "active" || a.callGeneration == 0 {
		return false
	}
	return callMediaTicket{
		Generation: a.callGeneration,
		CallID:     a.activeCall.ID,
		Index:      a.activeCall.Index,
		Direction:  a.activeCall.Direction,
	} == expected
}

// stopDirectQPCMVForCall is generation-scoped. It first proves empty CLCC, so
// a topology callback that runs while the call is still active only marks
// cleanup_pending and never disables module voice underneath live media.
func (a *app) stopDirectQPCMVForCall(expectedGeneration uint64) error {
	if expectedGeneration == 0 {
		return nil
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	return a.stopDirectQPCMVForCallLocked(expectedGeneration)
}

func (a *app) stopDirectQPCMVForCallLocked(expectedGeneration uint64) error {
	state := a.directQPCMVRouteState()
	if state.generation != expectedGeneration {
		return nil
	}
	return a.cleanupOwnedDirectQPCMVLocked(state)
}

// stopArmedDirectQPCMV handles an outgoing dial/answer attempt which never
// became active. It is location-scoped because an armed route has not yet been
// assigned a call generation.
func (a *app) stopArmedDirectQPCMV(expectedLocation uint32) error {
	if expectedLocation == 0 {
		return nil
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if state.locationID != expectedLocation || state.generation != 0 ||
		(state.phase != "armed" && state.phase != "cleanup_pending") {
		return nil
	}
	return a.cleanupOwnedDirectQPCMVLocked(state)
}

// cleanupOwnedDirectQPCMVLocked requires moduleVoiceOpMu. cleanup_failed is
// terminal for automatic control: a prior disable mutation was ambiguous, so
// repeating it would violate the no-duplicate-command invariant.
func (a *app) cleanupOwnedDirectQPCMVLocked(state directQPCMVRouteState) error {
	if state.locationID == 0 {
		return errors.New("direct QPCMV route has no exact USB location")
	}
	if state.phase == "cleanup_failed" {
		return errors.New("previous QPCMV disable was ambiguous; refusing to repeat it automatically")
	}

	var cleanup directQPCMVCleanupResult
	_, sessionErr := a.withExclusiveUSBATCommandsAtLocationExact(state.locationID, func(command usbATCommandFunc) error {
		if state.identityHash == "" {
			return errors.New("direct QPCMV cleanup has no physical module identity")
		}
		if err := validateDirectQPCMVIdentity(command, state.identityHash); err != nil {
			return err
		}
		cleanup = cleanupDirectQPCMVIdle(command)
		return cleanup.err
	})
	if sessionErr != nil {
		phase := "cleanup_pending"
		if cleanup.attempted {
			phase = "cleanup_failed"
		}
		a.setDirectQPCMVStateForOwner(false, phase, "direct QPCMV cleanup unconfirmed", state.locationID, state.identityHash, state.intent, state.generation, sessionErr)
		if errors.Is(sessionErr, errDirectQPCMVOwnerStillInCall) {
			// This is the expected first teardown observation when the cached
			// topology changes before the modem has actually cleared CLCC. Host
			// media is already stopped; leave QPCMV armed and let the next empty
			// CLCC observation perform the one physical disable without poller
			// error noise.
			return nil
		}
		return sessionErr
	}
	if !cleanup.confirmed {
		err := errors.New("direct QPCMV cleanup returned without disabled-state confirmation")
		a.setDirectQPCMVStateForOwner(false, "cleanup_failed", "direct QPCMV cleanup unconfirmed", state.locationID, state.identityHash, state.intent, state.generation, err)
		return err
	}
	a.setDirectQPCMVState(false, "stopped", "direct QPCMV route disabled", state.locationID, 0, nil)
	return nil
}

// cleanupDirectQPCMVIfIdle is called from an empty-CLCC observation. It also
// handles an armed ATD/ATA whose outcome was ambiguous and never became an
// active call. A call can pass through held/dialing generations before becoming
// empty, so this follows the published owner instead of guessing from the
// immediately preceding topology generation.
func (a *app) cleanupDirectQPCMVIfIdle() error {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	state := a.directQPCMVRouteState()
	if (state.phase != "armed" && state.phase != "call_command_ambiguous" && state.phase != "cleanup_pending") || state.locationID == 0 {
		return nil
	}
	if (state.phase == "armed" || state.phase == "call_command_ambiguous") &&
		(state.updatedAt.IsZero() || time.Since(state.updatedAt) < directQPCMVCallCommandGrace) {
		return nil
	}
	return a.cleanupOwnedDirectQPCMVLocked(state)
}

func (a *app) cleanupDirectQPCMVIfIdleWithMutationLock() error {
	if !a.moduleMutationMu.TryLock() {
		return errors.New("another module mutation is active")
	}
	defer a.moduleMutationMu.Unlock()
	return a.cleanupDirectQPCMVIfIdle()
}
