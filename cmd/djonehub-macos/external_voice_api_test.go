package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
	"github.com/iniwex5/vohive/internal/turnauth"
)

func TestRemoteVoiceV2OfferUsesAuthenticatedBrowserContextControl(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	instance := newDemoApp()
	instance.sipVoice = fixture.runtime
	if instance.remoteAccess.Control {
		t.Fatal("test requires the unrelated Tailscale control config to remain disabled")
	}
	body := mustExternalVoiceJSON(t, syntheticExternalVoiceOffer(incoming, "synthetic_context_nonce_0001"))
	authorization := remoteAuthorization{
		Identity: testRemoteLogin,
		Actions:  map[string]bool{"calls.read": true, "calls.media": true},
	}
	request := httptest.NewRequest(http.MethodPost, remoteVoiceV2OfferPath, strings.NewReader(body))
	ctx := context.WithValue(request.Context(), remoteAuthorizationContextKey{}, authorization)
	request = request.WithContext(context.WithValue(ctx, remoteBrowserContextKey{}, remoteBrowserContext{
		Control: true, Transport: "tailscale-serve",
	}))
	response := httptest.NewRecorder()
	instance.remoteVoiceV2Offer(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("browser-context offer=%d %s", response.Code, response.Body.String())
	}
}

func TestRemoteVoiceV2PublicOfferInjectsServerUDPAndTLSTCPFallback(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	issuer, err := turnauth.New(turnauth.Config{
		Host: "turn.example.com", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: []byte(strings.Repeat("S", 48)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = issuer.Close() })
	instance := newDemoApp()
	instance.sipVoice = fixture.runtime
	instance.publicWeb = &publicWebStartupConfig{
		Control: true, ExternalVoice: true, TURNIssuer: issuer,
	}
	body := mustExternalVoiceJSON(t, syntheticExternalVoiceOffer(incoming, "synthetic_public_fallback_nonce_0001"))
	authorization := remoteAuthorization{
		Identity: testRemoteLogin,
		Actions:  map[string]bool{"calls.read": true, "calls.media": true},
	}
	request := httptest.NewRequest(http.MethodPost, remoteVoiceV2OfferPath, strings.NewReader(body))
	ctx := context.WithValue(request.Context(), remoteAuthorizationContextKey{}, authorization)
	request = request.WithContext(context.WithValue(ctx, remoteBrowserContextKey{}, remoteBrowserContext{
		Control: true, Transport: "cloudflare-access",
	}))
	response := httptest.NewRecorder()
	instance.remoteVoiceV2Offer(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("public offer=%d %s", response.Code, response.Body.String())
	}
	fixture.mu.Lock()
	network := cloneExternalVoiceNetwork(fixture.answerNetwork)
	fixture.mu.Unlock()
	wantURLs := []string{
		"turn:turn.example.com:3478?transport=udp",
		"turns:turn.example.com:443?transport=tcp",
	}
	if network.TURNRelay == nil || len(network.TURNRelay.URLs) != len(wantURLs) ||
		network.TURNRelay.URLs[0] != wantURLs[0] || network.TURNRelay.URLs[1] != wantURLs[1] ||
		network.TURNRelay.Username == "" || network.TURNRelay.Password == "" ||
		len(network.AllowedInterfaces) != 0 || len(network.AllowedLocalCIDRs) != 0 ||
		len(network.AllowedRemoteCIDRs) != 0 {
		t.Fatal("public offer did not install the complete isolated server TURN fallback")
	}
}

func TestRemoteVoiceV2OfferAnswerEndUsesExactOwnerAndDurableCommands(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	instance, handler, csrf := newRemoteTestApp(t, true,
		"status.read", "calls.read", "calls.media", "calls.control", "calls.hangup")
	instance.sipVoice = fixture.runtime

	offerRequest := syntheticExternalVoiceOffer(incoming, "synthetic_api_nonce_0001")
	offerBody := mustExternalVoiceJSON(t, offerRequest)
	offerHTTP := remoteTestRequest(http.MethodPost, remoteVoiceV2OfferPath, offerBody,
		"calls.read", "calls.media")
	authorizeRemoteMutation(offerHTTP, csrf, "unused-ephemeral-offer-key")
	offerRecorder := httptest.NewRecorder()
	handler.ServeHTTP(offerRecorder, offerHTTP)
	if offerRecorder.Code != http.StatusOK {
		t.Fatalf("offer=%d %s", offerRecorder.Code, offerRecorder.Body.String())
	}
	var offer externalVoiceOfferResponse
	if err := json.Unmarshal(offerRecorder.Body.Bytes(), &offer); err != nil ||
		offer.Version != externalVoiceAPISchemaVersion || offer.AnswerSDP != "synthetic-sdp-answer" ||
		offer.Call.Media == nil || !offer.Call.Media.Prepared {
		t.Fatalf("decode offer=%+v err=%v body=%s", offer, err, offerRecorder.Body.String())
	}
	instance.remoteLedger.mu.Lock()
	ledgerEntriesAfterOffer := len(instance.remoteLedger.entries)
	instance.remoteLedger.mu.Unlock()
	if ledgerEntriesAfterOffer != 0 {
		t.Fatalf("ephemeral SDP offer entered durable ledger: %d", ledgerEntriesAfterOffer)
	}

	answerRequest := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	answerBody := mustExternalVoiceJSON(t, answerRequest)
	var evidenceKind sipgateway.CommandKind
	var evidenceOwner string
	var evidenceBodyHash [sha256.Size]byte
	fixture.runtime.evidence = func(
		kind sipgateway.CommandKind,
		owner string,
		bodyHash [sha256.Size]byte,
	) (sipgateway.MutationEvidence, error) {
		evidenceKind, evidenceOwner, evidenceBodyHash = kind, owner, bodyHash
		return syntheticExternalVoiceMutationEvidence(kind, owner, bodyHash)
	}
	for attempt := 0; attempt < 2; attempt++ {
		request := remoteTestRequest(http.MethodPost, remoteVoiceV2AnswerPath, answerBody,
			"calls.control", "calls.media")
		authorizeRemoteMutation(request, csrf, "voice-answer-command-key-0001")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":"completed"`) {
			t.Fatalf("answer attempt %d=%d %s", attempt, response.Code, response.Body.String())
		}
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 1 {
		t.Fatalf("same durable answer key executed %d times", got)
	}
	if evidenceKind != sipgateway.CommandAnswerIncoming || evidenceOwner != testRemoteLogin ||
		evidenceBodyHash != sha256.Sum256([]byte(answerBody)) {
		t.Fatalf("answer evidence kind=%q owner=%q hash=%x", evidenceKind, evidenceOwner, evidenceBodyHash)
	}

	snapshotRequest := remoteTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", "calls.read")
	snapshotRecorder := httptest.NewRecorder()
	handler.ServeHTTP(snapshotRecorder, snapshotRequest)
	var snapshot externalVoiceSnapshotResponse
	if snapshotRecorder.Code != http.StatusOK || json.Unmarshal(snapshotRecorder.Body.Bytes(), &snapshot) != nil ||
		snapshot.Voice.Call == nil || snapshot.Voice.Call.Media == nil || !snapshot.Voice.OwnedByRequester {
		t.Fatalf("active snapshot=%d %s", snapshotRecorder.Code, snapshotRecorder.Body.String())
	}
	endRequest := externalVoiceCallRequest{
		Version: 2, Call: snapshot.Voice.Call.Call, ExpectedRevision: snapshot.Voice.Call.Revision,
		MediaLeaseID: snapshot.Voice.Call.Media.LeaseID,
	}
	endBody := mustExternalVoiceJSON(t, endRequest)
	endHTTP := remoteTestRequest(http.MethodPost, remoteVoiceV2EndPath, endBody,
		"calls.hangup", "calls.media")
	authorizeRemoteMutation(endHTTP, csrf, "voice-end-command-key-0001")
	endRecorder := httptest.NewRecorder()
	handler.ServeHTTP(endRecorder, endHTTP)
	if endRecorder.Code != http.StatusOK || !strings.Contains(endRecorder.Body.String(), `"code":"completed"`) {
		t.Fatalf("end=%d %s", endRecorder.Code, endRecorder.Body.String())
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandEndActive); got != 1 {
		t.Fatalf("durable end executed %d times", got)
	}
	if evidenceKind != sipgateway.CommandEndActive || evidenceOwner != testRemoteLogin ||
		evidenceBodyHash != sha256.Sum256([]byte(endBody)) {
		t.Fatalf("end evidence kind=%q owner=%q hash=%x", evidenceKind, evidenceOwner, evidenceBodyHash)
	}
}

func TestRemoteVoiceV2UnknownAnswerNeverRetriesAndCanReconcileReadOnly(t *testing.T) {
	fixture := newExternalVoiceRuntimeFixture(t)
	incoming := fixture.incoming(t)
	instance, handler, csrf := newRemoteTestApp(t, true,
		"status.read", "calls.read", "calls.media", "calls.control")
	instance.sipVoice = fixture.runtime

	offerHTTP := remoteTestRequest(http.MethodPost, remoteVoiceV2OfferPath,
		mustExternalVoiceJSON(t, syntheticExternalVoiceOffer(incoming, "synthetic_api_nonce_1001")),
		"calls.read", "calls.media")
	authorizeRemoteMutation(offerHTTP, csrf, "unused-ephemeral-offer-key-2")
	offerRecorder := httptest.NewRecorder()
	handler.ServeHTTP(offerRecorder, offerHTTP)
	var offer externalVoiceOfferResponse
	if offerRecorder.Code != http.StatusOK || json.Unmarshal(offerRecorder.Body.Bytes(), &offer) != nil ||
		offer.Call.Media == nil {
		t.Fatalf("offer=%d %s", offerRecorder.Code, offerRecorder.Body.String())
	}
	if err := fixture.emulator.SetNextCommandFault(sipgateway.CommandAnswerIncoming, sipgateway.CommandFault{
		Outcome: sipgateway.CommandUnknown, Apply: false,
	}); err != nil {
		t.Fatal(err)
	}
	callRequest := externalVoiceCallRequest{
		Version: 2, Call: offer.Call.Call, ExpectedRevision: offer.Call.Revision,
		MediaLeaseID: offer.Call.Media.LeaseID,
	}
	callBody := mustExternalVoiceJSON(t, callRequest)
	for attempt := 0; attempt < 2; attempt++ {
		request := remoteTestRequest(http.MethodPost, remoteVoiceV2AnswerPath, callBody,
			"calls.control", "calls.media")
		authorizeRemoteMutation(request, csrf, "voice-answer-unknown-key-0001")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"unknown_outcome"`) {
			t.Fatalf("unknown answer attempt %d=%d %s", attempt, response.Code, response.Body.String())
		}
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 1 {
		t.Fatalf("unknown answer was retried: %d", got)
	}
	for attempt := 0; attempt < 2; attempt++ {
		request := remoteTestRequest(http.MethodPost, remoteVoiceV2ReconcilePath, callBody,
			"calls.control", "calls.media")
		authorizeRemoteMutation(request, csrf, "unused-reconcile-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"reconcile_required":true`) {
			t.Fatalf("reconcile attempt %d=%d %s", attempt, response.Code, response.Body.String())
		}
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 1 {
		t.Fatalf("read-only reconcile repeated answer: %d", got)
	}
}

func TestRemoteVoiceV2DangerousRoutesRejectWildcardAndPartialCapabilities(t *testing.T) {
	_, handler, csrf := newRemoteTestApp(t, true,
		"status.read", "calls.read", "calls.media", "calls.control", "calls.hangup")

	wildcardSnapshot := remoteTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", "*")
	wildcardSnapshotRecorder := httptest.NewRecorder()
	handler.ServeHTTP(wildcardSnapshotRecorder, wildcardSnapshot)
	if wildcardSnapshotRecorder.Code != http.StatusOK {
		t.Fatalf("read-only wildcard snapshot=%d, want 200", wildcardSnapshotRecorder.Code)
	}

	for _, test := range []struct {
		name    string
		path    string
		actions []string
	}{
		{name: "offer wildcard", path: remoteVoiceV2OfferPath, actions: []string{"*"}},
		{name: "offer missing read", path: remoteVoiceV2OfferPath, actions: []string{"calls.media"}},
		{name: "answer missing media", path: remoteVoiceV2AnswerPath, actions: []string{"calls.control"}},
		{name: "end missing media", path: remoteVoiceV2EndPath, actions: []string{"calls.hangup"}},
		{name: "reconcile wildcard", path: remoteVoiceV2ReconcilePath, actions: []string{"*"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := remoteTestRequest(http.MethodPost, test.path, `{}`, test.actions...)
			authorizeRemoteMutation(request, csrf, "voice-capability-test-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("response=%d %s, want 403", response.Code, response.Body.String())
			}
		})
	}
}

func mustExternalVoiceJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
