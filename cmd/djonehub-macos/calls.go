package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type callRecord struct {
	ID        string     `json:"id"`
	Index     int        `json:"index"`
	Direction string     `json:"direction"`
	State     string     `json:"state"`
	Number    string     `json:"number,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Missed    bool       `json:"missed"`
}

type parsedCall struct {
	Index     int
	Direction string
	State     string
	Number    string
}

// callMediaTicket identifies one exact, single-active-call topology. The
// generation changes whenever CLCC topology changes, so work queued for an old
// call can never authorize media for (or tear down) a newer call.
type callMediaTicket struct {
	Generation uint64 `json:"call_generation"`
	CallID     string `json:"call_id"`
	Index      int    `json:"call_index"`
	Direction  string `json:"call_direction"`
}

var (
	clccPattern = regexp.MustCompile(`\+CLCC:\s*(\d+),(\d+),(\d+),(\d+),(\d+)(?:,"([^"]*)",(\d+))?`)
	// The mutation parser deliberately accepts only the CLCC shape used by the
	// modem's voice-call query. Anchoring the whole line prevents a valid prefix
	// from hiding trailing fields or garbage that the UI parser would ignore.
	clccMutationLinePattern = regexp.MustCompile(`(?i)^\+CLCC:\s*([0-9]+)\s*,\s*([0-9]+)\s*,\s*([0-9]+)\s*,\s*([0-9]+)\s*,\s*([0-9]+)(?:\s*,\s*"([^"\r\n]*)"\s*,\s*([0-9]+))?\s*$`)
)

const directCallRedialCooldown = 5 * time.Second

// parseCLCC is the fail-closed compatibility entry point used by existing
// mutation gates outside this file. A malformed response becomes one invalid
// call instead of an empty snapshot, so neither an idle-only mutation nor an
// exact-single-call mutation can be authorized accidentally.
func parseCLCC(response string) []parsedCall {
	calls, err := parseCLCCForMutation(response)
	if err != nil {
		return []parsedCall{{Index: -1, Direction: "unknown", State: "unknown"}}
	}
	return calls
}

// parseCLCCForDisplay preserves the older tolerant behavior for callers that
// only render an already-untrusted response. It must never feed call topology
// state or authorize an AT mutation.
func parseCLCCForDisplay(response string) []parsedCall {
	matches := clccPattern.FindAllStringSubmatch(response, -1)
	out := make([]parsedCall, 0, len(matches))
	for _, match := range matches {
		// CLCC mode 0 is voice. Mode 1 is a data session and must not surface
		// as a phone call in the macOS UI.
		if match[4] != "0" {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		out = append(out, parsedCall{
			Index:     index,
			Direction: mapCallDirection(match[2]),
			State:     mapCallState(match[3]),
			Number:    strings.TrimSpace(match[6]),
		})
	}
	return out
}

type strictCLCCRecord struct {
	call       parsedCall
	mode       int
	lineNumber int
}

// parseCLCCForMutation validates the complete response before returning only
// mode-0 voice calls. QDC507 firmware exposes its attached packet-data
// contexts as mode-1 CLCC rows (often state=active with an empty number); those
// rows are not phone calls and must not permanently block ATD/ATA/ATH. Nothing
// is filtered until every row and the terminal OK have passed strict parsing,
// so malformed, duplicate, unknown-mode, or unsolicited input still fails.
func parseCLCCForMutation(response string) ([]parsedCall, error) {
	records, err := parseStrictCLCCRecords(response)
	if err != nil {
		return nil, err
	}
	out := make([]parsedCall, 0, len(records))
	for _, record := range records {
		if record.mode == 0 {
			out = append(out, record.call)
		}
	}
	return out, nil
}

// validateCLCCVoiceIdleForModuleSetup is deliberately isolated to the
// confirmed maintenance transaction. The QDC507 exposes its attached packet
// data contexts as mode-1 CLCC rows; a CFUN-backed USB composition change will
// interrupt those contexts. Every row is still parsed strictly, while any
// mode-0 voice call blocks the transaction. Mode 1/2 records are strictly
// validated but excluded from the voice topology by both maintenance and call
// mutation paths.
func validateCLCCVoiceIdleForModuleSetup(response string) error {
	records, err := parseStrictCLCCRecords(response)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.mode == 0 {
			return fmt.Errorf("CLCC response has a voice call at line %d", record.lineNumber)
		}
	}
	return nil
}

// parseStrictCLCCRecords validates the complete AT response framing and every
// non-empty line before applying a caller-specific mode policy. It accepts an
// optional exact command echo, zero or more complete records, and one terminal
// OK. Unsolicited lines, partial rows, unknown enums, multiparty calls and
// duplicate indexes all fail closed.
func parseStrictCLCCRecords(response string) ([]strictCLCCRecord, error) {
	normalized := strings.ReplaceAll(response, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	out := make([]strictCLCCRecord, 0)
	indexes := make(map[int]struct{})
	seenEcho := false
	seenRecord := false
	seenOK := false

	for lineNumber, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if seenOK {
			return nil, fmt.Errorf("CLCC response has content after terminal OK at line %d", lineNumber+1)
		}
		if strings.EqualFold(line, "AT+CLCC") {
			if seenEcho || seenRecord {
				return nil, fmt.Errorf("CLCC response has a misplaced or duplicate command echo at line %d", lineNumber+1)
			}
			seenEcho = true
			continue
		}
		if strings.EqualFold(line, "OK") {
			seenOK = true
			continue
		}

		match := clccMutationLinePattern.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("CLCC response has an unexpected or malformed line at line %d", lineNumber+1)
		}
		seenRecord = true

		values := make([]int, 5)
		for i := range values {
			value, err := strconv.Atoi(match[i+1])
			if err != nil {
				return nil, fmt.Errorf("CLCC response has an invalid numeric field at line %d", lineNumber+1)
			}
			values[i] = value
		}
		index, direction, state, mode, multiparty := values[0], values[1], values[2], values[3], values[4]
		if index <= 0 {
			return nil, fmt.Errorf("CLCC response has an invalid call index at line %d", lineNumber+1)
		}
		if _, duplicate := indexes[index]; duplicate {
			return nil, fmt.Errorf("CLCC response has a duplicate call index at line %d", lineNumber+1)
		}
		if direction != 0 && direction != 1 {
			return nil, fmt.Errorf("CLCC response has an unknown direction at line %d", lineNumber+1)
		}
		if state < 0 || state > 5 {
			return nil, fmt.Errorf("CLCC response has an unknown state at line %d", lineNumber+1)
		}
		if mode < 0 || mode > 2 {
			return nil, fmt.Errorf("CLCC response has an unknown mode at line %d", lineNumber+1)
		}
		if multiparty != 0 {
			return nil, fmt.Errorf("CLCC response has a multiparty call at line %d", lineNumber+1)
		}
		if match[7] != "" {
			numberType, err := strconv.Atoi(match[7])
			if err != nil || numberType < 0 || numberType > 255 {
				return nil, fmt.Errorf("CLCC response has an invalid number type at line %d", lineNumber+1)
			}
		}

		indexes[index] = struct{}{}
		out = append(out, strictCLCCRecord{
			call: parsedCall{
				Index:     index,
				Direction: mapCallDirection(strconv.Itoa(direction)),
				State:     mapCallState(strconv.Itoa(state)),
				Number:    strings.TrimSpace(match[6]),
			},
			mode:       mode,
			lineNumber: lineNumber + 1,
		})
	}
	if !seenOK {
		return nil, errors.New("CLCC response is missing a terminal OK")
	}
	return out, nil
}

func mapCallDirection(raw string) string {
	if raw == "1" {
		return "incoming"
	}
	return "outgoing"
}

func mapCallState(raw string) string {
	switch raw {
	case "0":
		return "active"
	case "1":
		return "held"
	case "2":
		return "dialing"
	case "3":
		return "alerting"
	case "4":
		return "incoming"
	case "5":
		return "waiting"
	default:
		return "unknown"
	}
}

func (a *app) startCallPoller(ctx context.Context) {
	interval := a.callPollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := a.pollCallOnce(); err != nil {
				log.Printf("call poll failed: %v", err)
			}
			timer.Reset(interval)
		}
	}
}

func (a *app) pollCallOnce() error {
	// Remote conditional controls use this same lock around fresh CLCC and the
	// resulting AT mutation. This keeps the cached call generation and the
	// physical command in one serialized control lane.
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if a.demo {
		return nil
	}
	if a.modem == nil && a.currentUSBDevice() == nil {
		a.invalidateCallMediaTopology()
		a.setCallPollStatus(fmt.Errorf("DJI USB device is not connected"))
		return nil
	}

	a.callMu.Lock()
	configured := a.callConfigured
	a.callMu.Unlock()
	if !configured {
		if _, err := a.runATCommand("AT+CLIP=1", 3*time.Second); err != nil {
			a.invalidateCallMediaTopology()
			a.setCallPollStatus(err)
			return err
		}
		a.logModuleVoiceConfig()
		a.callMu.Lock()
		a.callConfigured = true
		a.callMu.Unlock()
	}

	response, err := a.runATCommand("AT+CLCC", 3*time.Second)
	if err != nil {
		a.invalidateCallMediaTopology()
		a.setCallPollStatus(err)
		return err
	}
	if !atQuerySucceeded(response) || atResponseIsError(response) {
		err := errors.New("AT+CLCC did not return an explicit OK")
		a.invalidateCallMediaTopology()
		a.setCallPollStatus(err)
		return err
	}
	calls, parseErr := parseCLCCForMutation(response)
	if parseErr != nil {
		err := fmt.Errorf("AT+CLCC failed strict validation: %w", parseErr)
		a.invalidateCallMediaTopology()
		a.setCallPollStatus(err)
		return err
	}
	a.applyCallPoll(calls, time.Now())
	a.setCallPollStatus(nil)
	return nil
}

// ATA changes the network call state immediately, but the normal three-second
// poll cadence used to leave the audio helper waiting on a stale ringing
// snapshot. Run a short post-answer burst so the helper can open the already
// prepared UAC route without adding an arbitrary polling interval to callers.
func (a *app) refreshCallStateAfterAcceptedAnswer() {
	go func() {
		time.Sleep(100 * time.Millisecond)
		for attempt := 0; attempt < 5; attempt++ {
			if err := a.pollCallOnce(); err != nil {
				return
			}
			a.callMu.RLock()
			active := a.activeCall != nil && a.activeCall.State == "active"
			callEnded := a.activeCall == nil
			a.callMu.RUnlock()
			if active || callEnded {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// logModuleVoiceConfig 一次性记录模块的 USB 音频配置，便于排查通话无声。
func (a *app) logModuleVoiceConfig() {
	for _, cmd := range []string{`AT+QCFG="USBCFG"?`, "AT+QPCMV?", "AT+QDAI?"} {
		resp, err := a.runATCommand(cmd, 3*time.Second)
		if err != nil {
			log.Printf("voice usb diag: %s -> error: %v", cmd, err)
			continue
		}
		log.Printf("voice usb diag: %s -> %s", cmd, strings.TrimSpace(resp))
	}
}

func (a *app) applyCallPoll(calls []parsedCall, now time.Time) {
	historyChanged := false
	var selected *parsedCall
	for i := range calls {
		candidate := &calls[i]
		if selected == nil || callStatePriority(candidate.State) > callStatePriority(selected.State) {
			selected = candidate
		}
	}
	a.callMu.Lock()
	topologyKey := callTopologyFingerprint(calls)
	topologyChanged := !a.callTopologyKnown || a.callTopologyKey != topologyKey
	previousGeneration := a.callGeneration
	if topologyChanged {
		a.callGeneration++
		a.callTopologyKey = topologyKey
		a.callTopologyKnown = true
	}
	exactlyOneActive := len(calls) == 1 && calls[0].State == "active"
	// Keep the new generation unavailable until teardown for the prior
	// generation has completed. This closes the gap where a concurrent API call
	// could otherwise retag an old route before the old stop acquired opMu.
	a.callMediaEligible = exactlyOneActive && !topologyChanged
	var notify *callRecord
	var notifyGeneration uint64
	if selected == nil {
		if a.activeCall != nil {
			ended := now
			a.activeCall.EndedAt = &ended
			a.activeCall.UpdatedAt = now
			a.activeCall.Missed = a.activeCall.Direction == "incoming" &&
				(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
			log.Printf("call ended: number_redacted=true state=%q direction=%q duration=%s",
				a.activeCall.State, a.activeCall.Direction,
				now.Sub(a.activeCall.StartedAt).Round(time.Second))
			a.callHistory = append([]callRecord{*a.activeCall}, a.callHistory...)
			if len(a.callHistory) > 100 {
				a.callHistory = a.callHistory[:100]
			}
			historyChanged = true
			a.activeCall = nil
		}
		a.callMu.Unlock()
		if historyChanged {
			if err := a.persistCallHistory(); err != nil {
				log.Printf("persist call history: %v", err)
			}
		}
		if topologyChanged {
			a.stopCallMediaForTopologyChange(previousGeneration)
		}
		if err := a.cleanupDirectQPCMVIfIdleWithMutationLock(); err != nil {
			log.Printf("direct QPCMV idle cleanup failed: %v", err)
		}
		a.callMu.RLock()
		swiftAudioHost := a.swiftAudioHost
		a.callMu.RUnlock()
		if a.audio != nil && !swiftAudioHost && !a.audioManualSet && a.audio.isRunning() {
			a.audio.stop()
			a.lastAudioHealthLog = time.Time{}
			log.Printf("voice audio routing stopped")
		}
		return
	}

	numberChanged := a.activeCall != nil && a.activeCall.Number != "" && selected.Number != "" && a.activeCall.Number != selected.Number
	if a.activeCall == nil || a.activeCall.Index != selected.Index || a.activeCall.Direction != selected.Direction || numberChanged {
		if numberChanged {
			ended := now
			a.activeCall.EndedAt = &ended
			a.activeCall.UpdatedAt = now
			a.activeCall.Missed = a.activeCall.Direction == "incoming" &&
				(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
			a.callHistory = append([]callRecord{*a.activeCall}, a.callHistory...)
			if len(a.callHistory) > 100 {
				a.callHistory = a.callHistory[:100]
			}
			historyChanged = true
		}
		record := &callRecord{
			ID:        fmt.Sprintf("%d-%d", now.UnixMilli(), selected.Index),
			Index:     selected.Index,
			Direction: selected.Direction,
			State:     selected.State,
			Number:    selected.Number,
			StartedAt: now,
			UpdatedAt: now,
		}
		a.activeCall = record
		log.Printf("call started: number_redacted=true state=%q direction=%q", selected.State, selected.Direction)
		if selected.Direction == "incoming" && (selected.State == "incoming" || selected.State == "waiting") {
			copy := *record
			notify = &copy
		}
	} else {
		wasRinging := a.activeCall.State == "incoming" || a.activeCall.State == "waiting"
		prevState := a.activeCall.State
		a.activeCall.State = selected.State
		if prevState != selected.State {
			log.Printf("call state %q -> %q (number_redacted=true)", prevState, selected.State)
		}
		a.activeCall.UpdatedAt = now
		if selected.Number != "" {
			a.activeCall.Number = selected.Number
		}
		if selected.Direction == "incoming" && !wasRinging &&
			(selected.State == "incoming" || selected.State == "waiting") {
			copy := *a.activeCall
			notify = &copy
		}
	}
	if notify != nil {
		notifyGeneration = a.callGeneration
	}
	a.callMu.Unlock()
	if historyChanged {
		if err := a.persistCallHistory(); err != nil {
			log.Printf("persist call history: %v", err)
		}
	}
	if topologyChanged {
		a.stopCallMediaForTopologyChange(previousGeneration)
		a.finishCallTopologyTransition(topologyKey, exactlyOneActive)
	}

	if notify != nil {
		a.kickIncomingModuleVoicePrewarm(*notify, notifyGeneration)
		if a.callNotifier != nil {
			a.callNotifier(*notify)
		}
		// Web Push receives only a fixed incoming-call event and is keyed to the
		// exact CLCC topology generation. The bounded dispatcher never blocks
		// this modem poll path.
		a.notifyPublicWebQDCIncoming(notifyGeneration)
	}
	if selected.Direction == "incoming" && (selected.State == "incoming" || selected.State == "waiting") {
		if err := a.autoArmIncomingDirectQPCMV(); err != nil {
			log.Printf("incoming direct QPCMV arm deferred: %v", err)
		}
	}
	// Match MaVo's verified lifecycle: the call is established first and media
	// starts only after CLCC reports one active voice call.
	a.callMu.RLock()
	swiftAudioHost := a.swiftAudioHost
	a.callMu.RUnlock()
	if selected.State == "active" && !exactlyOneActive {
		log.Printf("voice audio route refused: expected exactly one voice call, got %d", len(calls))
	} else if selected.State == "active" && swiftAudioHost {
		ticket, valid := a.currentCallMediaTicket()
		if !valid {
			log.Printf("module voice route start refused: active call topology changed")
		} else if err := a.ensureModuleVoiceRouteForCall(ticket); err != nil {
			log.Printf("module voice route start for native MaVo host failed: %v", err)
		} else {
			if diagnostics, ok := a.remoteMedia.activeDiagnostics(); ok {
				stats := diagnostics.FrameQueueStats
				log.Printf("module voice route ready for native MaVo audio host: uplink_overflow=%d uplink_underrun=%d downlink_overflow=%d downlink_underrun=%d phone_mic_samples=%d phone_mic_signal=%d phone_mic_peak=%d",
					stats.UplinkOverflow, stats.UplinkUnderrun,
					stats.DownlinkOverflow, stats.DownlinkUnderrun,
					diagnostics.RemoteRTPSamples, diagnostics.RemoteRTPSignalSamples,
					diagnostics.RemoteRTPPeakPCM16)
			} else {
				log.Printf("module voice route ready for native MaVo audio host")
			}
		}
	} else if selected.State == "active" && a.audio != nil && !a.audioManualSet && !a.audio.isRunning() {
		ticket, valid := a.currentCallMediaTicket()
		if !valid {
			log.Printf("module voice route start refused: active call topology changed")
		} else if err := a.ensureModuleVoiceRouteForCall(ticket); err != nil {
			log.Printf("module voice route start after active CLCC failed: %v", err)
		} else if err := a.audio.start(); err != nil {
			log.Printf("voice audio start failed: %v", err)
		} else {
			log.Printf("voice audio routing started")
			if devs := a.audio.audioDevices(); len(devs) > 0 {
				log.Printf("voice audio devices: mod_in=%q mod_out=%q mac_in=%q mac_out=%q formats=%q",
					devs["mod_in"], devs["mod_out"], devs["mac_in"], devs["mac_out"],
					a.audio.formats())
			}
		}
	}
	// Throttled audio health snapshot while a call is up: whether the module's
	// USB audio is actually delivering voice (mod_in/far) and the Mac side is
	// consuming it (mac_out), so a silent path is easy to trace.
	if a.audio != nil && a.audio.isRunning() && time.Since(a.lastAudioHealthLog) >= 6*time.Second {
		a.lastAudioHealthLog = time.Now()
		_, farPeak, nearPeak, _ := a.audio.state()
		stats := a.audio.audioStats()
		farLive, nearLive, farOutLive, nearOutLive := a.audio.live()
		log.Printf("audio health: far_peak=%.4f near_peak=%.4f far_live=%.4f near_live=%.4f far_out=%.4f near_out=%.4f mod_in=%d mod_out=%d mac_in=%d mac_out=%d far_ring=%d fmt_chg=%q",
			farPeak, nearPeak, farLive, nearLive, farOutLive, nearOutLive,
			stats["mod_in_calls"], stats["mod_out_calls"], stats["mac_in_calls"], stats["mac_out_calls"],
			stats["far_ring_used"], a.audio.formatChanges())
	}
}

func (a *app) finishCallTopologyTransition(expectedKey string, eligible bool) {
	a.callMu.Lock()
	if a.callTopologyKnown && a.callTopologyKey == expectedKey {
		a.callMediaEligible = eligible
	}
	a.callMu.Unlock()
}

func callTopologyFingerprint(calls []parsedCall) string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		numberToken := "hidden"
		if call.Number != "" {
			digest := sha256.Sum256([]byte(call.Number))
			numberToken = fmt.Sprintf("%x", digest[:8])
		}
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%s", call.Index, call.Direction, call.State, numberToken))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func singleActiveCallMatches(calls []parsedCall, expected callMediaTicket) bool {
	return len(calls) == 1 && calls[0].State == "active" &&
		calls[0].Index == expected.Index && calls[0].Direction == expected.Direction
}

func (a *app) currentCallMediaTicket() (callMediaTicket, bool) {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if !a.callTopologyKnown || !a.callMediaEligible || a.activeCall == nil ||
		a.activeCall.State != "active" || a.callGeneration == 0 {
		return callMediaTicket{}, false
	}
	return callMediaTicket{
		Generation: a.callGeneration,
		CallID:     a.activeCall.ID,
		Index:      a.activeCall.Index,
		Direction:  a.activeCall.Direction,
	}, true
}

func (a *app) callMediaTicketIsCurrent(expected callMediaTicket) bool {
	current, ok := a.currentCallMediaTicket()
	return ok && current == expected
}

func (a *app) confirmActiveCallForMedia(expected callMediaTicket) error {
	if !a.callMediaTicketIsCurrent(expected) {
		return errors.New("cached call topology no longer matches the media ticket")
	}
	response, err := a.runATCommand("AT+CLCC", 3*time.Second)
	if err != nil {
		return fmt.Errorf("fresh CLCC failed: %w", err)
	}
	calls, parseErr := parseCLCCForMutation(response)
	if parseErr != nil {
		return fmt.Errorf("fresh CLCC failed strict validation: %w", parseErr)
	}
	if !singleActiveCallMatches(calls, expected) {
		return errors.New("fresh CLCC no longer confirms the same single active call")
	}
	if !a.callMediaTicketIsCurrent(expected) {
		return errors.New("call topology changed while confirming media")
	}
	return nil
}

func (a *app) stopCallMediaForTopologyChange(previousGeneration uint64) {
	if a.audio != nil && a.audio.isRunning() {
		a.audio.stop()
		a.lastAudioHealthLog = time.Time{}
	}
	if previousGeneration != 0 {
		stopRoute := a.stopModuleVoiceRouteForCall
		if a.callMediaRouteStop != nil {
			stopRoute = a.callMediaRouteStop
		}
		if err := stopRoute(previousGeneration); err != nil {
			log.Printf("module voice route teardown for call generation %d failed: %v", previousGeneration, err)
		}
		if a.remoteMedia != nil {
			a.remoteMedia.closeForCallGeneration(previousGeneration)
		}
	}
}

func (a *app) autoArmIncomingDirectQPCMV() error {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		return err
	}
	if a.implicitUACVoice {
		return nil
	}
	state := a.directQPCMVRouteState()
	if state.phase == "armed" && state.intent.direction == "incoming" {
		a.callMu.RLock()
		matches := a.activeCall != nil && a.activeCall.ID == state.intent.callID &&
			a.activeCall.Index == state.intent.index && a.activeCall.Direction == "incoming"
		a.callMu.RUnlock()
		if matches {
			return nil
		}
		return errors.New("an armed route belongs to a different incoming call observation")
	}
	if !a.moduleMutationMu.TryLock() {
		return errors.New("another module mutation is active")
	}
	defer a.moduleMutationMu.Unlock()
	locationID, identityHash, err := a.readModuleIdentity()
	if err != nil {
		return err
	}
	return a.armDirectQPCMV(locationID, identityHash, false)
}

// stopModuleVoiceRouteForCallWith serializes the generation check with the
// physical stop. Keeping this small wrapper platform-neutral also makes the
// old-stop/new-start race directly testable without USB hardware.
func (a *app) stopModuleVoiceRouteForCallWith(expectedGeneration uint64, stopLocked func() error) error {
	if expectedGeneration == 0 {
		return nil
	}
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	a.moduleVoiceMu.Lock()
	ownedGeneration := a.moduleVoiceCallGeneration
	a.moduleVoiceMu.Unlock()
	if ownedGeneration != expectedGeneration {
		return nil
	}
	return stopLocked()
}

func (a *app) invalidateCallMediaTopology() {
	a.callMu.Lock()
	if !a.callTopologyKnown {
		a.callMu.Unlock()
		return
	}
	previousGeneration := a.callGeneration
	a.callGeneration++
	a.callTopologyKnown = false
	a.callMediaEligible = false
	a.callMu.Unlock()
	a.stopCallMediaForTopologyChange(previousGeneration)
}

func callStatePriority(state string) int {
	switch state {
	case "incoming", "waiting":
		return 5
	case "active":
		return 4
	case "alerting":
		return 3
	case "dialing":
		return 2
	case "held":
		return 1
	default:
		return 0
	}
}

func (a *app) setCallPollStatus(err error) {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	a.callLastPoll = time.Now()
	if err != nil {
		a.callLastPollError = err.Error()
		return
	}
	a.callLastPollError = ""
}

func (a *app) callStatus(w http.ResponseWriter, _ *http.Request) {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	var active *callRecord
	if a.activeCall != nil {
		copy := *a.activeCall
		active = &copy
	}
	history := append([]callRecord(nil), a.callHistory...)
	audioRunning := false
	var audioFarPeak, audioNearPeak float64
	audioError := ""
	var audioStats map[string]int64
	audioDevices := map[string]string{}
	audioLive := map[string]float64{"far": 0, "near": 0, "far_out": 0, "near_out": 0}
	audioFormats := ""
	audioFmtLog := ""
	if a.audio != nil {
		audioRunning, audioFarPeak, audioNearPeak, audioError = a.audio.state()
		audioStats = a.audio.audioStats()
		audioDevices = a.audio.audioDevices()
		audioLive["far"], audioLive["near"], audioLive["far_out"], audioLive["near_out"] = a.audio.live()
		audioFormats = a.audio.formats()
		audioFmtLog = a.audio.formatChanges()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":          active,
		"history":         history,
		"polling":         !a.demo && !a.smsOnlyRuntime,
		"poll_interval_s": int(a.callPollInterval.Seconds()),
		"last_poll":       a.callLastPoll,
		"last_poll_error": a.callLastPollError,
		"call_generation": a.callGeneration,
		"media_eligible":  a.callMediaEligible,
		"audio": map[string]any{
			"running":   audioRunning,
			"far_peak":  audioFarPeak,
			"near_peak": audioNearPeak,
			"error":     audioError,
			"stats":     audioStats,
			"devices":   audioDevices,
			"live":      audioLive,
			"formats":   audioFormats,
			"fmt_log":   audioFmtLog,
		},
	})
}

func (a *app) rejectCall(w http.ResponseWriter, _ *http.Request) {
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if a.demo {
		a.applyCallPoll(nil, time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"rejected": true})
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	response, err := a.runATCommand("AT+CHUP", 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rejected": true,
		"response": response,
	})
}

func (a *app) answerCall(w http.ResponseWriter, _ *http.Request) {
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"answered": true})
		return
	}
	// Debounce only a previously accepted ATA. Failed or ambiguous commands do
	// not get reported as answered on an immediate retry.
	a.callMu.RLock()
	if time.Since(a.lastAnswerAt) < 2*time.Second {
		a.callMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]bool{"answered": true})
		return
	}
	a.callMu.RUnlock()
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()

	var response string
	var err error
	state := a.directQPCMVRouteState()
	if a.implicitUACVoice {
		response, err = a.runATCommand("ATA", 5*time.Second)
		if err == nil {
			err = validateCallATResponse(response)
		}
	} else if state.phase == "armed" && state.intent.direction == "incoming" {
		response, err = a.executeCallOnArmedDirectQPCMV("ATA", 5*time.Second)
	} else {
		locationID, identityHash, identityErr := a.readModuleIdentity()
		if identityErr != nil {
			writeError(w, http.StatusBadGateway, identityErr.Error())
			return
		}
		response, err = a.armDirectQPCMVAndExecuteCall(locationID, identityHash, false, "ATA", 5*time.Second)
	}
	if err != nil {
		if errors.Is(err, errDirectCallCommandAmbiguous) {
			writeJSON(w, http.StatusAccepted, map[string]any{"answered": false, "pending_confirmation": true})
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.callMu.Lock()
	a.lastAnswerAt = time.Now()
	a.callMu.Unlock()
	a.refreshCallStateAfterAcceptedAnswer()
	log.Printf("answer call accepted: response_ack=ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"answered": true,
		"response": response,
	})
}

func (a *app) hangupCall(w http.ResponseWriter, _ *http.Request) {
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if a.demo {
		a.applyCallPoll(nil, time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"hung_up": true})
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	response, err := a.runATCommand("ATH", 5*time.Second)
	if err != nil || (!atResponseIsError(response) && !atQuerySucceeded(response)) {
		writeJSON(w, http.StatusAccepted, map[string]any{"hung_up": false, "pending_confirmation": true})
		return
	}
	if atResponseIsError(response) {
		// Some firmwares only accept AT+CHUP to end the current call.
		response, err = a.runATCommand("AT+CHUP", 5*time.Second)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("hangup call: -> %s", strings.TrimSpace(response))
	writeJSON(w, http.StatusOK, map[string]any{
		"hung_up":  true,
		"response": response,
	})
}

func (a *app) dtmfCall(w http.ResponseWriter, r *http.Request) {
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	var body struct {
		Digit string `json:"digit"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	digit := body.Digit
	if len(digit) != 1 || !strings.ContainsRune("0123456789*#", rune(digit[0])) {
		writeError(w, http.StatusBadRequest, "DTMF 仅支持 0-9 * #")
		return
	}
	if err := a.sendCallDTMF(digit); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("dtmf accepted: digit_redacted=true response_ack=ok")
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (a *app) sendCallDTMF(digit string) error {
	return sendCallDTMFSequenceWith(a.runATCommand, digit)
}

func sendCallDTMFWith(run func(string, time.Duration) (string, error), digit string) error {
	return sendCallDTMFSequenceWith(run, digit)
}

func sendCallDTMFSequenceWith(run func(string, time.Duration) (string, error), digits string) error {
	if len(digits) == 0 || len(digits) > 20 || strings.Trim(digits, "0123456789*#") != "" {
		return errors.New("DTMF 仅支持 0-9 * #")
	}
	tones := make([]string, 0, len(digits))
	for _, digit := range digits {
		tones = append(tones, string(digit))
	}
	// CLDTMF accepts a quoted, comma-separated sequence. Sending a whole burst
	// avoids one public HTTP + USB AT round trip per key and keeps * / # out of
	// the AT parser's syntax. One unit is the module's 100 ms default tone.
	response, err := run(fmt.Sprintf("AT+CLDTMF=1,\"%s\"", strings.Join(tones, ",")),
		1500*time.Millisecond+time.Duration(len(tones))*250*time.Millisecond)
	if err != nil {
		return err
	}
	if atResponseIsError(response) {
		// Older firmware may only accept VTS one key at a time. Preserve order
		// and stop on the first unacknowledged key.
		for _, digit := range digits {
			response, err = run(fmt.Sprintf("AT+VTS=\"%c\"", digit), 1500*time.Millisecond)
			if err != nil {
				return err
			}
			if err := validateCallATResponse(response); err != nil {
				return err
			}
		}
		return nil
	}
	if err := validateCallATResponse(response); err != nil {
		return err
	}
	return nil
}

func (a *app) dialCall(w http.ResponseWriter, r *http.Request) {
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	var body struct {
		Number string `json:"number"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	number := normalizeDialNumber(body.Number)
	if number == "" {
		writeError(w, http.StatusBadRequest, "号码为空或包含非法字符")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"dialing": true})
		return
	}
	if remaining := a.directCallRedialCooldownRemaining(time.Now()); remaining > 0 {
		writeError(w, http.StatusConflict, "上一通电话刚结束，请稍后重拨")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		writeError(w, http.StatusConflict, "另一项模块配置操作正在进行")
		return
	}
	defer a.moduleMutationMu.Unlock()
	var err error
	if a.implicitUACVoice {
		var response string
		response, err = a.runATCommand("ATD"+number+";", 8*time.Second)
		if err == nil {
			err = validateCallATResponse(response)
		}
	} else {
		locationID, identityHash, identityErr := a.readModuleIdentity()
		if identityErr != nil {
			writeError(w, http.StatusBadGateway, identityErr.Error())
			return
		}
		_, err = a.armDirectQPCMVAndExecuteCall(locationID, identityHash, true, "ATD"+number+";", 8*time.Second)
	}
	if err != nil {
		if errors.Is(err, errDirectCallCommandAmbiguous) {
			writeJSON(w, http.StatusAccepted, map[string]any{"dialing": false, "pending_confirmation": true})
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("dial call accepted: number_redacted=true response_ack=ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"dialing":  true,
		"response": "OK",
	})
}

// Some QDC507 firmware keeps reporting dialing for a short interval after ATH.
// Start the cooldown only after CLCC has actually become voice-idle and the
// ended record has been committed; this avoids guessing from the ATH response.
func (a *app) directCallRedialCooldownRemaining(now time.Time) time.Duration {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if len(a.callHistory) == 0 || a.callHistory[0].EndedAt == nil {
		return 0
	}
	remaining := directCallRedialCooldown - now.Sub(*a.callHistory[0].EndedAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// validateCallATResponse keeps command echoes and modem ERROR replies from
// being reported as successful button presses. usbAT returns a completed AT
// response for both OK and ERROR, so transport success alone is insufficient.
func validateCallATResponse(response string) error {
	if atResponseIsError(response) {
		return fmt.Errorf("模块拒绝通话命令（ERROR）")
	}
	if !atQuerySucceeded(response) {
		return errors.New("通话命令未返回明确 OK")
	}
	return nil
}

// normalizeDialNumber keeps digits plus + * # and strips common formatting
// characters. Any other character makes the number invalid.
func normalizeDialNumber(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == '*' || r == '#':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')':
			// formatting only
		default:
			return ""
		}
	}
	return b.String()
}

func (a *app) audioStart(w http.ResponseWriter, _ *http.Request) {
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if a.audio == nil {
		writeError(w, http.StatusBadGateway, "通话音频不可用")
		return
	}
	a.callMu.Lock()
	hasActiveCall := a.activeCall != nil
	if !hasActiveCall {
		a.callMu.Unlock()
		writeError(w, http.StatusConflict, "当前没有通话；来电或拨号后会自动启动音频")
		return
	}
	a.audioManualSet = true
	a.audioManualOn = true
	a.callMu.Unlock()
	if err := a.audio.start(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if devs := a.audio.audioDevices(); len(devs) > 0 {
		log.Printf("voice audio started: mod_in=%q mod_out=%q mac_in=%q mac_out=%q",
			devs["mod_in"], devs["mod_out"], devs["mac_in"], devs["mac_out"])
	}
	writeJSON(w, http.StatusOK, map[string]bool{"audio_running": true})
}

func (a *app) audioStop(w http.ResponseWriter, _ *http.Request) {
	if a.audio != nil {
		a.callMu.Lock()
		a.audioManualSet = false
		a.audioManualOn = false
		a.callMu.Unlock()
		a.audio.stop()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"audio_running": false})
}

func (a *app) audioMute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Muted bool `json:"muted"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if a.audio != nil {
		a.audio.setMuted(body.Muted)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"muted": body.Muted})
}

func (a *app) audioRecord(w http.ResponseWriter, r *http.Request) {
	if a.swiftAudioHost {
		writeError(w, http.StatusConflict, "MaVo 音频策略已接管媒体；录音观察器尚未接入，避免影响实时音频")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action == "start" {
		if err := a.requireLegacyModuleVoicePath(); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if a.audio == nil {
		writeError(w, http.StatusBadGateway, "通话音频不可用")
		return
	}
	switch action {
	case "start":
		if err := a.audio.start(); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		path, err := a.audio.startRecording()
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recording": true, "path": path})
	case "stop":
		path, err := a.audio.stopRecording()
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recording": false, "path": path})
	default:
		writeError(w, http.StatusBadRequest, "action must be start or stop")
	}
}

// audioHostRegister selects the exact MaVo-derived Swift host route.  It is
// opt-in so an older installed notifier continues to use the previous Go
// route until this app has registered successfully.
func (a *app) audioHostRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.callMu.Lock()
	a.swiftAudioHost = body.Enabled
	a.callMu.Unlock()
	if body.Enabled && a.audio != nil && a.audio.isRunning() {
		a.audio.stop()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": body.Enabled})
}

func (a *app) audioHostConfig(w http.ResponseWriter, _ *http.Request) {
	device, identity, identityOK := a.currentUSBATIdentitySnapshot()
	if !identityOK {
		writeError(w, http.StatusBadGateway, "当前模块 USB AT 尚未连接")
		return
	}
	locationID := identity.Location
	ticket, ticketValid := a.currentCallMediaTicket()
	a.moduleVoiceMu.Lock()
	routeReady := a.moduleVoiceReady && a.moduleVoicePhase == "ready" &&
		a.moduleVoiceLocation != 0 && a.moduleVoiceLocation == locationID &&
		ticketValid && a.moduleVoiceCallGeneration == ticket.Generation
	routeLocation := a.moduleVoiceLocation
	routeError := a.moduleVoiceErr
	a.moduleVoiceMu.Unlock()
	if locationID == 0 || routeLocation != locationID {
		writeError(w, http.StatusConflict, "当前 AT 与 UAC 不是同一 USB location")
		return
	}
	vendorID, productID := uint16(identity.VendorID), uint16(identity.ProductID)
	a.callMu.RLock()
	hostEnabled := a.swiftAudioHost
	a.callMu.RUnlock()
	response := map[string]any{
		"vendor_id":    vendorID,
		"product_id":   productID,
		"location_id":  locationID,
		"route_ready":  routeReady,
		"route_error":  routeError,
		"host_enabled": hostEnabled,
	}
	if ticketValid {
		response["call_generation"] = ticket.Generation
		response["call_id"] = ticket.CallID
		response["call_index"] = ticket.Index
		response["call_direction"] = ticket.Direction
	}
	// Route/call checks above intentionally do not hold usbATOpenMu. Confirm the
	// identity snapshot is still the current lifecycle object before exposing it
	// to the native audio host.
	if !a.isCurrentUSBAT(device, locationID) {
		writeError(w, http.StatusConflict, "读取音频配置期间模块 USB 已变更")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// audioHostConfirm is the final MaVo-style gate after CoreAudio has opened the
// UAC device. It performs a new CLCC query and verifies that the route still
// belongs to the exact ticket returned by audioHostConfig.
func (a *app) audioHostConfirm(w http.ResponseWriter, r *http.Request) {
	var ticket callMediaTicket
	if !decodeJSON(w, r, &ticket) {
		return
	}
	if ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 ||
		(ticket.Direction != "incoming" && ticket.Direction != "outgoing") {
		writeError(w, http.StatusBadRequest, "invalid call media ticket")
		return
	}
	a.callMu.RLock()
	hostEnabled := a.swiftAudioHost
	a.callMu.RUnlock()
	if !hostEnabled {
		writeError(w, http.StatusConflict, "MaVo audio host is not registered")
		return
	}
	if err := a.confirmModuleVoiceCall(ticket); err != nil {
		if cleanupErr := a.stopModuleVoiceRouteForCall(ticket.Generation); cleanupErr != nil {
			log.Printf("module voice route cleanup after host confirmation failure: %v", cleanupErr)
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"confirmed": true})
}
