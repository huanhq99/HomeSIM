package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
)

const remoteRescueTestIMEI = "8675309" + "00000001"

func latchRemoteRescueTestGrant(
	t *testing.T,
	manager *remoteMediaManager,
) (remoteMediaOfferResponse, *remoteMediaLease, *usbAT, usbATPhysicalIdentity, callMediaTicket, string) {
	t.Helper()
	response, entry, device, physical := prepareRemoteMediaUnitLease(t, manager)
	ticket := callMediaTicket{
		Generation: entry.source.Call.CallGeneration + 1,
		CallID:     entry.source.Call.CallID, Index: entry.source.Call.CallIndex,
		Direction: entry.source.Call.CallDirection,
	}
	now := time.Now()
	identityHash := moduleIdentityDigest(remoteRescueTestIMEI)
	manager.mu.Lock()
	entry.activeTicket = ticket
	entry.activationEpoch = 1
	entry.activationAt = now.Add(-time.Second)
	entry.hostCaptureFrames = 160
	entry.hostPlaybackFrames = 160
	entry.hostCaptureAt = now
	entry.hostPlaybackAt = now
	entry.moduleVoiceIdentityHash = identityHash
	manager.mu.Unlock()
	manager.maybeLatchRescueProof(entry, remotevoice.MediaLeaseSnapshot{ActiveFresh: true}, now)
	return response, entry, device, physical, ticket, identityHash
}

func TestRemoteRescueGrantRequiresFreshExactIncomingOwner(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	response, entry, device, physical := prepareRemoteMediaUnitLease(t, manager)
	ticket := callMediaTicket{
		Generation: entry.source.Call.CallGeneration + 1,
		CallID:     entry.source.Call.CallID, Index: entry.source.Call.CallIndex,
		Direction: entry.source.Call.CallDirection,
	}
	if err := manager.authorizeRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, ticket,
	); err == nil {
		t.Fatal("prepared-only media lease authorized rescue hangup")
	}
	manager.mu.Lock()
	entry.activeTicket = ticket
	entry.activationEpoch = 1
	entry.activationAt = time.Now().Add(-time.Second)
	entry.hostCaptureFrames, entry.hostPlaybackFrames = 160, 160
	entry.hostCaptureAt, entry.hostPlaybackAt = time.Now(), time.Now()
	entry.moduleVoiceIdentityHash = moduleIdentityDigest(remoteRescueTestIMEI)
	manager.mu.Unlock()
	manager.maybeLatchRescueProof(entry, remotevoice.MediaLeaseSnapshot{ActiveFresh: false}, time.Now())
	if err := manager.authorizeRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, ticket,
	); err == nil {
		t.Fatal("non-fresh media snapshot authorized rescue hangup")
	}

	manager.maybeLatchRescueProof(entry, remotevoice.MediaLeaseSnapshot{ActiveFresh: true}, time.Now())
	if err := manager.authorizeRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, ticket,
	); err != nil {
		t.Fatalf("exact fresh media owner rejected: %v", err)
	}
	for _, test := range []struct {
		name       string
		identity   string
		sessionID  string
		generation uint64
		ticket     callMediaTicket
	}{
		{name: "login", identity: "other@example.com", sessionID: response.MediaSessionID, generation: response.LeaseGeneration, ticket: ticket},
		{name: "session", identity: "owner@example.com", sessionID: strings.Repeat("B", 43), generation: response.LeaseGeneration, ticket: ticket},
		{name: "generation", identity: "owner@example.com", sessionID: response.MediaSessionID, generation: response.LeaseGeneration + 1, ticket: ticket},
		{name: "ticket", identity: "owner@example.com", sessionID: response.MediaSessionID, generation: response.LeaseGeneration, ticket: callMediaTicket{Generation: ticket.Generation + 1, CallID: ticket.CallID, Index: ticket.Index, Direction: ticket.Direction}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := manager.authorizeRescueHangup(
				test.identity, test.sessionID, test.generation, test.ticket,
			); err == nil {
				t.Fatal("mismatched rescue owner was accepted")
			}
		})
	}

	manager.mu.Lock()
	grant := manager.rescue
	manager.mu.Unlock()
	if grant == nil {
		t.Fatal("fresh proof did not create a live grant")
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", grant), fmt.Sprintf("%#v", grant)} {
		if strings.Contains(rendered, "owner@example.com") || strings.Contains(rendered, response.MediaSessionID) ||
			strings.Contains(rendered, entry.moduleVoiceIdentityHash) {
			t.Fatalf("rescue grant formatting leaked ownership: %q", rendered)
		}
	}
	encoded, err := json.Marshal(grant)
	if err != nil || strings.Contains(string(encoded), response.MediaSessionID) {
		t.Fatalf("rescue grant JSON leaked ownership: %s err=%v", encoded, err)
	}
	if _, err := manager.beginRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		ticket, &usbAT{}, physical, entry.moduleVoiceIdentityHash,
	); err == nil {
		t.Fatal("same-location replacement USB lifecycle consumed rescue authority")
	}
	driftedPhysical := physical
	driftedPhysical.Location++
	if _, err := manager.beginRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		ticket, device, driftedPhysical, entry.moduleVoiceIdentityHash,
	); err == nil {
		t.Fatal("drifted physical USB identity consumed rescue authority")
	}
	if _, err := manager.beginRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		ticket, device, physical, moduleIdentityDigest("8675309"+"00000002"),
	); err == nil {
		t.Fatal("drifted modem identity consumed rescue authority")
	}
	if _, err := manager.beginRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		ticket, device, physical, entry.moduleVoiceIdentityHash,
	); err != nil {
		t.Fatalf("exact pinned rescue owner rejected: %v", err)
	}
}

func TestRemoteRescueTombstoneOnlyFollowsTransportFailureAndExpires(t *testing.T) {
	for _, test := range []struct {
		name       string
		reason     remotevoice.MediaLeaseCloseReason
		wantRescue bool
	}{
		{name: "peer", reason: remotevoice.MediaLeaseClosePeerEnded, wantRescue: true},
		{name: "broker", reason: remotevoice.MediaLeaseCloseBrokerEnded, wantRescue: true},
		{name: "requested", reason: remotevoice.MediaLeaseCloseRequested},
		{name: "generation", reason: remotevoice.MediaLeaseCloseGenerationMismatch},
		{name: "answer", reason: remotevoice.MediaLeaseCloseAnswerFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newRemoteMediaUnitManager(t)
			response, _, _, _, ticket, _ := latchRemoteRescueTestGrant(t, manager)
			manager.closeAfterCoordinatorEnd(response.LeaseGeneration, test.reason)
			err := manager.authorizeRescueHangup(
				"owner@example.com", response.MediaSessionID, response.LeaseGeneration, ticket,
			)
			if (err == nil) != test.wantRescue {
				t.Fatalf("reason %q rescue err=%v, wantRescue=%v", test.reason, err, test.wantRescue)
			}
		})
	}

	manager := newRemoteMediaUnitManager(t)
	manager.rescueTTL = 15 * time.Millisecond
	response, _, _, _, ticket, _ := latchRemoteRescueTestGrant(t, manager)
	manager.closeAfterCoordinatorEnd(response.LeaseGeneration, remotevoice.MediaLeaseClosePeerEnded)
	deadline := time.Now().Add(time.Second)
	for manager.authorizeRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, ticket,
	) == nil {
		if time.Now().After(deadline) {
			t.Fatal("rescue tombstone outlived its fixed TTL")
		}
		time.Sleep(time.Millisecond)
	}

	manager = newRemoteMediaUnitManager(t)
	response, _, _, _, ticket, _ = latchRemoteRescueTestGrant(t, manager)
	manager.closeForCallGeneration(ticket.Generation)
	if err := manager.authorizeRescueHangup(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, ticket,
	); err == nil {
		t.Fatal("call topology teardown retained rescue authority")
	}

	manager = newRemoteMediaUnitManager(t)
	response, _, _, _, _, _ = latchRemoteRescueTestGrant(t, manager)
	manager.closeAfterCoordinatorEnd(response.LeaseGeneration, remotevoice.MediaLeaseClosePeerEnded)
	manager.mu.Lock()
	oldToken := manager.rescue.token
	manager.mu.Unlock()
	newResponse, _, _, _, newTicket, _ := latchRemoteRescueTestGrant(t, manager)
	manager.expireRescueGrant(oldToken)
	if err := manager.authorizeRescueHangup(
		"owner@example.com", newResponse.MediaSessionID, newResponse.LeaseGeneration, newTicket,
	); err != nil {
		t.Fatalf("old rescue timer invalidated a new generation: %v", err)
	}
}

func TestRemoteRescueStatusIsScopedToExactLoginAndCurrentTicket(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	response, _, _, _, ticket, _ := latchRemoteRescueTestGrant(t, manager)
	manager.closeAfterCoordinatorEnd(response.LeaseGeneration, remotevoice.MediaLeaseClosePeerEnded)
	current := func(candidate callMediaTicket) bool { return candidate == ticket }
	owner := manager.snapshot("owner@example.com", nil, current)
	if !owner.RescueHangupReady || owner.Phase != "rescue_only" ||
		owner.LeaseGeneration != response.LeaseGeneration {
		t.Fatalf("owner rescue status=%+v", owner)
	}
	other := manager.snapshot("other@example.com", nil, current)
	if other.RescueHangupReady || other.LeaseGeneration != 0 {
		t.Fatalf("other login observed rescue authority: %+v", other)
	}
	stale := manager.snapshot("owner@example.com", nil, func(callMediaTicket) bool { return false })
	if stale.RescueHangupReady {
		t.Fatalf("stale call exposed rescue authority: %+v", stale)
	}
}

type remoteRescueExecutionFixture struct {
	app          *app
	manager      *remoteMediaManager
	response     remoteMediaOfferResponse
	entry        *remoteMediaLease
	device       *usbAT
	physical     usbATPhysicalIdentity
	ticket       callMediaTicket
	identityHash string
	commands     []string
	hangupReply  string
	hangupErr    error
}

func newRemoteRescueExecutionFixture(t *testing.T) *remoteRescueExecutionFixture {
	t.Helper()
	manager := newRemoteMediaUnitManager(t)
	response, entry, device, physical, ticket, identityHash := latchRemoteRescueTestGrant(t, manager)
	instance := newDemoApp()
	instance.remoteMedia = manager
	instance.callMu.Lock()
	instance.callTopologyKnown = true
	instance.callMediaEligible = true
	instance.callGeneration = ticket.Generation
	instance.activeCall = &callRecord{
		ID: ticket.CallID, Index: ticket.Index, Direction: ticket.Direction,
		State: "active", StartedAt: time.Now(), UpdatedAt: time.Now(),
	}
	instance.callMu.Unlock()
	instance.setDirectQPCMVStateForOwner(
		true, "ready", "test", physical.Location, identityHash,
		directQPCMVCallIntent{direction: ticket.Direction, callID: ticket.CallID, index: ticket.Index},
		ticket.Generation, nil,
	)
	fixture := &remoteRescueExecutionFixture{
		app: instance, manager: manager, response: response, entry: entry,
		device: device, physical: physical, ticket: ticket, identityHash: identityHash,
		hangupReply: "ATH\r\nOK\r\n",
	}
	instance.usbATPinnedSessionOverride = func(
		location uint32,
		operation func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error,
	) error {
		if location != physical.Location {
			return errors.New("wrong location")
		}
		return operation(device, physical, func(command string, _ time.Duration) (string, error) {
			fixture.commands = append(fixture.commands, command)
			switch command {
			case "AT+CGSN":
				return "AT+CGSN\r\n" + remoteRescueTestIMEI + "\r\nOK\r\n", nil
			case "AT+CLCC":
				return "+CLCC: 1,1,0,0,0,\"\",129\r\nOK\r\n", nil
			case directQPCMVQueryCommand:
				return "+QPCMV: 1,2\r\nOK\r\n", nil
			case "ATH":
				return fixture.hangupReply, fixture.hangupErr
			default:
				return "ERROR\r\n", nil
			}
		})
	}
	return fixture
}

func (f *remoteRescueExecutionFixture) execute() (remoteMediaRescueReservation, error) {
	return f.app.executeRemoteRescueHangup(
		"owner@example.com", f.response.MediaSessionID, f.response.LeaseGeneration, f.ticket,
	)
}

func TestRemoteRescueHangupUsesOnePinnedATHAndConsumesEveryOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		reply      string
		commandErr error
		wantErr    error
	}{
		{name: "accepted", reply: "ATH\r\nOK\r\n"},
		{name: "rejected", reply: "ATH\r\nERROR\r\n", wantErr: errRemoteRescueHangupRejected},
		{name: "ambiguous", commandErr: errors.New("timeout"), wantErr: errRemoteRescueHangupAmbiguous},
		{name: "incomplete", reply: "ATH\r\n", wantErr: errRemoteRescueHangupAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRemoteRescueExecutionFixture(t)
			fixture.hangupReply = test.reply
			fixture.hangupErr = test.commandErr
			reservation, err := fixture.execute()
			if !reservation.valid() || !errors.Is(err, test.wantErr) {
				t.Fatalf("reservation=%+v err=%v want=%v", reservation, err, test.wantErr)
			}
			want := []string{"AT+CGSN", "AT+CLCC", directQPCMVQueryCommand, "ATH"}
			if strings.Join(fixture.commands, "|") != strings.Join(want, "|") {
				t.Fatalf("commands=%v want=%v", fixture.commands, want)
			}
			for _, command := range fixture.commands {
				if command == "AT+CHUP" {
					t.Fatal("rescue hangup used forbidden fallback")
				}
			}
			if test.wantErr != nil {
				fixture.manager.maybeLatchRescueProof(
					fixture.entry, remotevoice.MediaLeaseSnapshot{ActiveFresh: true}, time.Now(),
				)
			}
			before := len(fixture.commands)
			if _, secondErr := fixture.execute(); secondErr == nil {
				t.Fatal("consumed rescue proof authorized a second operation")
			}
			for _, command := range fixture.commands[before:] {
				if command == "ATH" || command == "AT+CHUP" {
					t.Fatalf("second operation emitted call mutation %q", command)
				}
			}
		})
	}
}

func TestRemoteRescueHangupFreshCallGateFailsBeforeATH(t *testing.T) {
	fixture := newRemoteRescueExecutionFixture(t)
	fixture.app.usbATPinnedSessionOverride = func(
		_ uint32,
		operation func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error,
	) error {
		return operation(fixture.device, fixture.physical, func(command string, _ time.Duration) (string, error) {
			fixture.commands = append(fixture.commands, command)
			switch command {
			case "AT+CGSN":
				return remoteRescueTestIMEI + "\r\nOK\r\n", nil
			case "AT+CLCC":
				return "+CLCC: 2,1,0,0,0,\"\",129\r\nOK\r\n", nil
			default:
				return "ERROR\r\n", nil
			}
		})
	}
	reservation, err := fixture.execute()
	if err == nil || reservation.valid() {
		t.Fatalf("mismatched fresh call crossed final gate: reservation=%+v err=%v", reservation, err)
	}
	for _, command := range fixture.commands {
		if command == "ATH" || command == "AT+CHUP" {
			t.Fatalf("fresh-call failure emitted mutation %q", command)
		}
	}
}

func remoteRescueHTTPFixture(
	t *testing.T,
) (*remoteRescueExecutionFixture, http.Handler, string, string) {
	t.Helper()
	fixture := newRemoteRescueExecutionFixture(t)
	instance := fixture.app
	ledgerPath := filepath.Join(t.TempDir(), "remote", "operations.jsonl")
	instance.configureRemoteAccess(
		testRemoteLogin, true, testRemoteCapability, testRemoteHost, "127.0.0.1:7577", ledgerPath,
	)
	if instance.remoteLedger == nil {
		t.Fatal("remote ledger was not configured")
	}
	t.Cleanup(func() { _ = instance.remoteLedger.Close() })
	instance.remoteIncomingAnswer = true
	instance.remoteRescueHangup = true
	handler := instance.remoteRoutes()
	sessionRequest := remoteTestRequest(
		http.MethodGet, "/api/remote/v1/session", "",
		"status.read", "calls.read", "calls.control", "calls.media", "calls.hangup",
	)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK ||
		json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("remote rescue session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	bodyBytes, err := json.Marshal(remoteIncomingAnswerRequest{
		Call: remoteCallExpectation{
			CallID: fixture.ticket.CallID, CallGeneration: fixture.ticket.Generation,
			CallIndex: fixture.ticket.Index, CallDirection: fixture.ticket.Direction,
		},
		MediaSessionID: fixture.response.MediaSessionID, LeaseGeneration: fixture.response.LeaseGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, handler, session.CSRF, string(bodyBytes)
}

func sendRemoteRescueHTTP(
	handler http.Handler,
	csrf string,
	body string,
	key string,
) *httptest.ResponseRecorder {
	request := remoteTestRequest(
		http.MethodPost, "/api/remote/v1/calls/hangup", body,
		"status.read", "calls.read", "calls.control", "calls.media", "calls.hangup",
	)
	authorizeRemoteMutation(request, csrf, key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func countRemoteRescueATH(commands []string) int {
	count := 0
	for _, command := range commands {
		if command == "ATH" {
			count++
		}
		if command == "AT+CHUP" {
			panic("remote rescue emitted forbidden AT+CHUP fallback")
		}
	}
	return count
}

func TestRemoteRescueHangupPersistentLedgerExecutesATHOnce(t *testing.T) {
	fixture, handler, csrf, body := remoteRescueHTTPFixture(t)
	first := sendRemoteRescueHTTP(handler, csrf, body, "rescue-hangup-once")
	if first.Code != http.StatusOK || countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("first rescue status=%d body=%s commands=%v", first.Code, first.Body.String(), fixture.commands)
	}
	commandsAfterFirst := len(fixture.commands)
	replay := sendRemoteRescueHTTP(handler, csrf, body, "rescue-hangup-once")
	if replay.Code != http.StatusOK || len(fixture.commands) != commandsAfterFirst {
		t.Fatalf("ledger replay status=%d commands=%v", replay.Code, fixture.commands)
	}
	differentKey := sendRemoteRescueHTTP(handler, csrf, body, "rescue-hangup-second-key")
	if differentKey.Code != http.StatusConflict || countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("second key status=%d commands=%v", differentKey.Code, fixture.commands)
	}
}

func TestRemoteOutgoingHangupSurvivesBrowserReloadWithoutMediaReceipt(t *testing.T) {
	testRemoteAuthenticatedHangupWithoutMediaReceipt(t, "outgoing")
}

func TestRemoteIncomingHangupSurvivesBrowserReloadWithoutMediaReceipt(t *testing.T) {
	testRemoteAuthenticatedHangupWithoutMediaReceipt(t, "incoming")
}

func testRemoteAuthenticatedHangupWithoutMediaReceipt(t *testing.T, direction string) {
	t.Helper()
	fixture := newRemoteRescueExecutionFixture(t)
	fixture.ticket.Direction = direction
	fixture.app.callMu.Lock()
	fixture.app.activeCall.Direction = direction
	fixture.app.callMu.Unlock()
	fixture.app.setDirectQPCMVStateForOwner(
		true, "ready", "test", fixture.physical.Location, fixture.identityHash,
		directQPCMVCallIntent{direction: direction, callID: fixture.ticket.CallID, index: fixture.ticket.Index},
		fixture.ticket.Generation, nil,
	)
	fixture.app.usbATPinnedSessionOverride = func(
		location uint32,
		operation func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error,
	) error {
		if location != fixture.physical.Location {
			return errors.New("wrong location")
		}
		return operation(fixture.device, fixture.physical, func(command string, _ time.Duration) (string, error) {
			fixture.commands = append(fixture.commands, command)
			switch command {
			case "AT+CGSN":
				return "AT+CGSN\r\n" + remoteRescueTestIMEI + "\r\nOK\r\n", nil
			case "AT+CLCC":
				clccDirection := 0
				if direction == "incoming" {
					clccDirection = 1
				}
				return fmt.Sprintf("+CLCC: 1,%d,0,0,0,\"\",129\r\nOK\r\n", clccDirection), nil
			case directQPCMVQueryCommand:
				return "+QPCMV: 1,2\r\nOK\r\n", nil
			case "ATH":
				return fixture.hangupReply, fixture.hangupErr
			default:
				return "ERROR\r\n", nil
			}
		})
	}
	ledgerPath := filepath.Join(t.TempDir(), "remote", "operations.jsonl")
	fixture.app.configureRemoteAccess(
		testRemoteLogin, true, testRemoteCapability, testRemoteHost, "127.0.0.1:7577", ledgerPath,
	)
	if fixture.app.remoteLedger == nil {
		t.Fatal("remote ledger was not configured")
	}
	t.Cleanup(func() { _ = fixture.app.remoteLedger.Close() })
	fixture.app.remoteIncomingAnswer = true
	fixture.app.remoteRescueHangup = true
	handler := fixture.app.remoteRoutes()
	sessionRequest := remoteTestRequest(
		http.MethodGet, "/api/remote/v1/session", "",
		"status.read", "calls.read", "calls.control", "calls.media", "calls.hangup",
	)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK ||
		json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	bodyBytes, err := json.Marshal(map[string]any{"call": remoteCallExpectation{
		CallID: fixture.ticket.CallID, CallGeneration: fixture.ticket.Generation,
		CallIndex: fixture.ticket.Index, CallDirection: direction,
	}})
	if err != nil {
		t.Fatal(err)
	}
	key := direction + "-reload-hangup"
	response := sendRemoteRescueHTTP(handler, session.CSRF, string(bodyBytes), key)
	if response.Code != http.StatusOK || countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("status=%d body=%s commands=%v", response.Code, response.Body.String(), fixture.commands)
	}
	replay := sendRemoteRescueHTTP(handler, session.CSRF, string(bodyBytes), key)
	if replay.Code != http.StatusOK || countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("replay status=%d body=%s commands=%v", replay.Code, replay.Body.String(), fixture.commands)
	}
}

func TestRemoteRescueHangupAmbiguousLedgerNeverRetries(t *testing.T) {
	fixture, handler, csrf, body := remoteRescueHTTPFixture(t)
	fixture.hangupErr = errors.New("injected transport timeout")
	first := sendRemoteRescueHTTP(handler, csrf, body, "rescue-hangup-unknown")
	if first.Code != http.StatusConflict || !strings.Contains(first.Body.String(), "unknown_outcome") ||
		countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("ambiguous rescue status=%d body=%s commands=%v", first.Code, first.Body.String(), fixture.commands)
	}
	fixture.manager.maybeLatchRescueProof(
		fixture.entry, remotevoice.MediaLeaseSnapshot{ActiveFresh: true}, time.Now(),
	)
	commandsAfterFirst := len(fixture.commands)
	for _, key := range []string{"rescue-hangup-unknown", "rescue-hangup-new-key"} {
		response := sendRemoteRescueHTTP(handler, csrf, body, key)
		if response.Code != http.StatusConflict {
			t.Fatalf("retry key=%q status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	if len(fixture.commands) != commandsAfterFirst || countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("ambiguous rescue was retried: %v", fixture.commands)
	}
}

func TestRemoteRescueHangupDifferentKeysConvergeToOneATH(t *testing.T) {
	fixture, handler, csrf, body := remoteRescueHTTPFixture(t)
	const workers = 50
	statuses := make(chan int, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			response := sendRemoteRescueHTTP(
				handler, csrf, body, fmt.Sprintf("rescue-concurrent-%03d", index),
			)
			statuses <- response.Code
		}(index)
	}
	group.Wait()
	close(statuses)
	accepted := 0
	for status := range statuses {
		if status == http.StatusOK {
			accepted++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected concurrent rescue status %d", status)
		}
	}
	if accepted != 1 || countRemoteRescueATH(fixture.commands) != 1 {
		t.Fatalf("accepted=%d commands=%v", accepted, fixture.commands)
	}
}

func TestRemoteRescueHangupLedgerBeginFailureSendsNoAT(t *testing.T) {
	fixture, handler, csrf, body := remoteRescueHTTPFixture(t)
	fixture.app.remoteLedger.syncOperations = func(*os.File) error {
		return errors.New("injected operation fsync failure")
	}
	response := sendRemoteRescueHTTP(handler, csrf, body, "rescue-ledger-fsync")
	if response.Code != http.StatusServiceUnavailable || countRemoteRescueATH(fixture.commands) != 0 {
		t.Fatalf("status=%d body=%s commands=%v", response.Code, response.Body.String(), fixture.commands)
	}
}

func TestRemoteRescueHangupRequiresDedicatedCapability(t *testing.T) {
	fixture, handler, csrf, body := remoteRescueHTTPFixture(t)
	request := remoteTestRequest(
		http.MethodPost, "/api/remote/v1/calls/hangup", body,
		"status.read", "calls.read", "calls.control", "calls.media",
	)
	authorizeRemoteMutation(request, csrf, "rescue-missing-capability")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || countRemoteRescueATH(fixture.commands) != 0 {
		t.Fatalf("status=%d body=%s commands=%v", response.Code, response.Body.String(), fixture.commands)
	}
}

func TestRemoteRescueGrantFormattingRedactsOwner(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	response, _, _, _, _, _ := latchRemoteRescueTestGrant(t, manager)
	for _, rendered := range []string{fmt.Sprintf("%+v", manager), fmt.Sprintf("%#v", manager)} {
		if strings.Contains(rendered, response.MediaSessionID) || strings.Contains(rendered, "owner@example.com") {
			t.Fatalf("manager formatting leaked rescue owner: %q", rendered)
		}
	}
}
