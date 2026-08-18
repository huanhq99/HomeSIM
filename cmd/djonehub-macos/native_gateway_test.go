package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newNativeGatewayTestApp(t *testing.T, enrollmentOnce bool) (*app, http.Handler) {
	t.Helper()
	instance, _, _ := newRemoteTestApp(t, false, "status.read")
	if err := instance.configureNativeGateway(filepath.Join(t.TempDir(), "native-devices"), enrollmentOnce); err != nil {
		t.Fatalf("configure native gateway: %v", err)
	}
	t.Cleanup(instance.closeNativeGateway)
	return instance, instance.remoteRoutes()
}

func nativeTestKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, spki
}

func nativeTestSignature(t *testing.T, key *ecdsa.PrivateKey, signingInput string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(signature)
}

func nativeTestRemotePOST(t *testing.T, handler http.Handler, path string, payload any, actions ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := remoteTestRequest(http.MethodPost, path, string(body), actions...)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func nativeTestTicket(t *testing.T, instance *app) nativeEnrollmentTicketResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:7576/api/local/v1/native/enrollment/tickets",
		strings.NewReader(`{"confirm":true}`))
	request.Host = "127.0.0.1:7576"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	instance.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("issue enrollment ticket: status=%d body=%s", response.Code, response.Body.String())
	}
	var ticket nativeEnrollmentTicketResponse
	if err := json.Unmarshal(response.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.TicketID == "" || ticket.TicketSecret == "" || ticket.Host != testRemoteHost {
		t.Fatalf("invalid enrollment ticket response: host=%q scopes=%v", ticket.Host, ticket.Scopes)
	}
	return ticket
}

func nativeTestEnroll(t *testing.T, instance *app, handler http.Handler) (*ecdsa.PrivateKey, nativeEnrollmentCompleteResponse) {
	t.Helper()
	ticket := nativeTestTicket(t, instance)
	privateKey, spki := nativeTestKey(t)
	prepareResponse := nativeTestRemotePOST(t, handler, "/api/native/v1/enrollments", nativeEnrollmentPrepareRequest{
		Step:          "prepare",
		TicketID:      ticket.TicketID,
		TicketSecret:  ticket.TicketSecret,
		Algorithm:     "ES256",
		PublicKeySPKI: base64.RawURLEncoding.EncodeToString(spki),
	}, "devices.enroll")
	if prepareResponse.Code != http.StatusOK {
		t.Fatalf("prepare enrollment: status=%d body=%s", prepareResponse.Code, prepareResponse.Body.String())
	}
	var prepared nativeEnrollmentPrepareResponse
	if err := json.Unmarshal(prepareResponse.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	completeResponse := nativeTestRemotePOST(t, handler, "/api/native/v1/enrollments", nativeEnrollmentCompleteRequest{
		Step:         "complete",
		EnrollmentID: prepared.EnrollmentID,
		Signature:    nativeTestSignature(t, privateKey, prepared.SigningInput),
	}, "devices.enroll")
	if completeResponse.Code != http.StatusCreated {
		t.Fatalf("complete enrollment: status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	var completed nativeEnrollmentCompleteResponse
	if err := json.Unmarshal(completeResponse.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if !validNativeDeviceID(completed.DeviceID) || completed.Algorithm != "ES256" {
		t.Fatalf("invalid completed enrollment: %+v", completed)
	}
	return privateKey, completed
}

func nativeTestChallenge(t *testing.T, handler http.Handler, key *ecdsa.PrivateKey, deviceID, target string, actions ...string) (nativeAuthChallengeResponse, string) {
	t.Helper()
	response := nativeTestRemotePOST(t, handler, "/api/native/v1/auth/challenges", nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      deviceID,
		Method:        http.MethodGet,
		RequestTarget: target,
		BodySHA256:    nativeEmptyBodySHA256,
	}, actions...)
	if response.Code != http.StatusOK {
		t.Fatalf("issue native challenge: status=%d body=%s", response.Code, response.Body.String())
	}
	var challenge nativeAuthChallengeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	return challenge, nativeTestSignature(t, key, challenge.SigningInput)
}

func nativeTestSignedGET(target, deviceID string, challenge nativeAuthChallengeResponse, signature string, actions ...string) *http.Request {
	request := remoteTestRequest(http.MethodGet, target, "", actions...)
	request.Header.Set("X-MacCellular-Device-ID", deviceID)
	request.Header.Set("X-MacCellular-Challenge-ID", challenge.ChallengeID)
	request.Header.Set("X-MacCellular-Signature", signature)
	return request
}

func newNativeSMSSendTestApp(t *testing.T) (*app, http.Handler, *ecdsa.PrivateKey, nativeDeviceRecord) {
	t.Helper()
	instance, _, _ := newRemoteTestApp(t, true, "status.read", "sms.read", "sms.send")
	if err := instance.configureNativeGateway(filepath.Join(t.TempDir(), "native-devices"), false); err != nil {
		t.Fatalf("configure native gateway: %v", err)
	}
	t.Cleanup(instance.closeNativeGateway)
	privateKey, spki := nativeTestKey(t)
	record, _, err := instance.nativeDeviceStore.Enroll(
		testRemoteLogin, spki, []string{"sms.read", "sms.send", "status.read"},
	)
	if err != nil {
		t.Fatalf("enroll native SMS device: %v", err)
	}
	instance.demo = false
	return instance, instance.remoteRoutes(), privateKey, record
}

func nativeTestRequestChallenge(
	t *testing.T,
	handler http.Handler,
	key *ecdsa.PrivateKey,
	deviceID, method, target string,
	body []byte,
	actions ...string,
) (nativeAuthChallengeResponse, string) {
	t.Helper()
	digest := sha256.Sum256(body)
	response := nativeTestRemotePOST(t, handler, "/api/native/v1/auth/challenges", nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      deviceID,
		Method:        method,
		RequestTarget: target,
		BodySHA256:    fmt.Sprintf("%x", digest[:]),
	}, actions...)
	if response.Code != http.StatusOK {
		t.Fatalf("issue native request challenge: status=%d body=%s", response.Code, response.Body.String())
	}
	var challenge nativeAuthChallengeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
		t.Fatal(err)
	}
	return challenge, nativeTestSignature(t, key, challenge.SigningInput)
}

func nativeTestSignedRequest(
	method, target string,
	body []byte,
	deviceID string,
	challenge nativeAuthChallengeResponse,
	signature, operationID string,
	actions ...string,
) *http.Request {
	request := remoteTestRequest(method, target, string(body), actions...)
	request.Header.Set("X-MacCellular-Device-ID", deviceID)
	request.Header.Set("X-MacCellular-Challenge-ID", challenge.ChallengeID)
	request.Header.Set("X-MacCellular-Signature", signature)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", operationID)
	}
	return request
}

func TestNativeGatewayEnrollsAndServesSignedReadOnlySMS(t *testing.T) {
	instance, handler := newNativeGatewayTestApp(t, true)
	privateKey, device := nativeTestEnroll(t, instance, handler)
	message, _, err := instance.smsStore.IngestIncoming(
		"synthetic-native-peer", "synthetic-native-content", "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	sessionTarget := "/api/native/v1/session"
	sessionChallenge, sessionSignature := nativeTestChallenge(
		t, handler, privateKey, device.DeviceID, sessionTarget, "status.read", "sms.read")
	sessionRequest := nativeTestSignedGET(
		sessionTarget, device.DeviceID, sessionChallenge, sessionSignature, "status.read", "sms.read")
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK || sessionResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("signed session: status=%d cache=%q body=%s",
			sessionResponse.Code, sessionResponse.Header().Get("Cache-Control"), sessionResponse.Body.String())
	}
	var session struct {
		Version  int      `json:"version"`
		DeviceID string   `json:"device_id"`
		Actions  []string `json:"actions"`
		SMSSync  bool     `json:"sms_sync"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Version != 1 || session.DeviceID != device.DeviceID || !session.SMSSync ||
		strings.Join(session.Actions, ",") != "sms.read,status.read" {
		t.Fatalf("unexpected native session: %+v", session)
	}

	replay := nativeTestSignedGET(sessionTarget, device.DeviceID, sessionChallenge, sessionSignature, "status.read")
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusForbidden {
		t.Fatalf("replayed native proof status=%d, want 403", replayResponse.Code)
	}

	syncTarget := "/api/native/v1/sms/sync?limit=10"
	syncChallenge, syncSignature := nativeTestChallenge(
		t, handler, privateKey, device.DeviceID, syncTarget, "sms.read")
	syncRequest := nativeTestSignedGET(syncTarget, device.DeviceID, syncChallenge, syncSignature, "sms.read")
	syncResponse := httptest.NewRecorder()
	handler.ServeHTTP(syncResponse, syncRequest)
	if syncResponse.Code != http.StatusOK {
		t.Fatalf("signed SMS sync: status=%d body=%s", syncResponse.Code, syncResponse.Body.String())
	}
	var syncResult SMSSyncResult
	if err := json.Unmarshal(syncResponse.Body.Bytes(), &syncResult); err != nil {
		t.Fatal(err)
	}
	if len(syncResult.Messages) != 1 || syncResult.Messages[0].ID != message.ID || syncResult.Cursor == "" {
		t.Fatalf("unexpected signed SMS sync: %+v", syncResult)
	}
}

func TestNativeSignedSMSSendIsDurableReplayableAndQueryable(t *testing.T) {
	instance, handler, privateKey, device := newNativeSMSSendTestApp(t)
	operationID := "nsm_AAAAAAAAAAAAAAAAAAAAAA"
	body := []byte(`{"version":1,"operation_id":"` + operationID + `","phone":"synthetic-native-recipient","message":"synthetic-native-message"}`)
	var hardwareCalls atomic.Int32
	instance.sendTextSMSOverride = func(phone, message string) (int, error) {
		if phone != "synthetic-native-recipient" || message != "synthetic-native-message" {
			t.Fatalf("unexpected SMS payload: phone=%q message=%q", phone, message)
		}
		hardwareCalls.Add(1)
		return 1, nil
	}

	send := func() *httptest.ResponseRecorder {
		challenge, signature := nativeTestRequestChallenge(
			t, handler, privateKey, device.DeviceID, http.MethodPost,
			"/api/native/v1/sms/send", body, "sms.send",
		)
		request := nativeTestSignedRequest(
			http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
			challenge, signature, operationID, "sms.send",
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	first := send()
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"code":"completed"`) {
		t.Fatalf("signed native SMS send=%d %s", first.Code, first.Body.String())
	}
	replay := send()
	if replay.Code != http.StatusOK || hardwareCalls.Load() != 1 {
		t.Fatalf("native SMS replay=%d calls=%d body=%s", replay.Code, hardwareCalls.Load(), replay.Body.String())
	}

	queryTarget := "/api/native/v1/sms/operations?operation_id=" + operationID
	queryChallenge, querySignature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodGet, queryTarget, nil, "sms.send",
	)
	query := nativeTestSignedRequest(
		http.MethodGet, queryTarget, nil, device.DeviceID,
		queryChallenge, querySignature, "", "sms.send",
	)
	queryResponse := httptest.NewRecorder()
	handler.ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK ||
		!strings.Contains(queryResponse.Body.String(), `"state":"completed"`) ||
		!strings.Contains(queryResponse.Body.String(), `"http_status":200`) ||
		!strings.Contains(queryResponse.Body.String(), `"code":"completed"`) {
		t.Fatalf("native SMS operation query=%d %s", queryResponse.Code, queryResponse.Body.String())
	}
	otherKey, otherSPKI := nativeTestKey(t)
	otherDevice, _, err := instance.nativeDeviceStore.Enroll(
		testRemoteLogin, otherSPKI, []string{"sms.send", "status.read"},
	)
	if err != nil {
		t.Fatal(err)
	}
	otherChallenge, otherSignature := nativeTestRequestChallenge(
		t, handler, otherKey, otherDevice.DeviceID, http.MethodGet, queryTarget, nil, "sms.send",
	)
	otherQuery := nativeTestSignedRequest(
		http.MethodGet, queryTarget, nil, otherDevice.DeviceID,
		otherChallenge, otherSignature, "", "sms.send",
	)
	otherResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherResponse, otherQuery)
	if otherResponse.Code != http.StatusOK || !strings.Contains(otherResponse.Body.String(), `"state":"not_found"`) {
		t.Fatalf("cross-device operation query=%d %s", otherResponse.Code, otherResponse.Body.String())
	}
	result, err := instance.syncSMSHistory("", 10)
	if err != nil || len(result.Messages) != 1 || result.Messages[0].Direction != "outgoing" ||
		result.Messages[0].Status != "submitted" {
		t.Fatalf("durable native outgoing=%+v err=%v", result.Messages, err)
	}
}

func TestNativeSignedSMSSendConcurrentProofsExecuteHardwareOnce(t *testing.T) {
	instance, handler, privateKey, device := newNativeSMSSendTestApp(t)
	operationID := "nsm_EEEEEEEEEEEEEEEEEEEEEE"
	body := []byte(`{"version":1,"operation_id":"` + operationID + `","phone":"synthetic-native-recipient","message":"synthetic-concurrent"}`)
	started := make(chan struct{})
	release := make(chan struct{})
	var hardwareCalls atomic.Int32
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		if hardwareCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return 1, nil
	}
	requests := make([]*http.Request, 2)
	for index := range requests {
		challenge, signature := nativeTestRequestChallenge(
			t, handler, privateKey, device.DeviceID, http.MethodPost,
			"/api/native/v1/sms/send", body, "sms.send",
		)
		requests[index] = nativeTestSignedRequest(
			http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
			challenge, signature, operationID, "sms.send",
		)
	}
	responses := make(chan *httptest.ResponseRecorder, len(requests))
	for _, request := range requests {
		go func(request *http.Request) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses <- response
		}(request)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("native SMS hardware execution did not start")
	}
	time.Sleep(20 * time.Millisecond)
	if hardwareCalls.Load() != 1 {
		t.Fatalf("concurrent native SMS hardware calls=%d, want 1", hardwareCalls.Load())
	}
	queryTarget := "/api/native/v1/sms/operations?operation_id=" + operationID
	queryChallenge, querySignature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodGet, queryTarget, nil, "sms.send",
	)
	query := nativeTestSignedRequest(
		http.MethodGet, queryTarget, nil, device.DeviceID,
		queryChallenge, querySignature, "", "sms.send",
	)
	queryResponse := httptest.NewRecorder()
	handler.ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK ||
		!strings.Contains(queryResponse.Body.String(), `"state":"in_flight"`) {
		t.Fatalf("in-flight native SMS query=%d body=%s",
			queryResponse.Code, queryResponse.Body.String())
	}
	close(release)
	for range requests {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":"completed"`) {
				t.Fatalf("concurrent native SMS response=%d %s", response.Code, response.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent native SMS response timed out")
		}
	}
	if hardwareCalls.Load() != 1 {
		t.Fatalf("concurrent native SMS final hardware calls=%d, want 1", hardwareCalls.Load())
	}
}

func TestNativeSMSSendBindsBodyOperationAndNeverRetriesUnknown(t *testing.T) {
	instance, handler, privateKey, device := newNativeSMSSendTestApp(t)
	operationID := "nsm_BBBBBBBBBBBBBBBBBBBBBB"
	body := []byte(`{"version":1,"operation_id":"` + operationID + `","phone":"synthetic-native-recipient","message":"synthetic-native-unknown"}`)
	var hardwareCalls atomic.Int32
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		hardwareCalls.Add(1)
		return 2, errors.New("injected ambiguous modem response")
	}

	challenge, signature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodPost,
		"/api/native/v1/sms/send", body, "sms.send",
	)
	mismatched := nativeTestSignedRequest(
		http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
		challenge, signature, "nsm_CCCCCCCCCCCCCCCCCCCCCC", "sms.send",
	)
	mismatchResponse := httptest.NewRecorder()
	handler.ServeHTTP(mismatchResponse, mismatched)
	if mismatchResponse.Code != http.StatusBadRequest || hardwareCalls.Load() != 0 {
		t.Fatalf("mismatched operation id=%d calls=%d body=%s",
			mismatchResponse.Code, hardwareCalls.Load(), mismatchResponse.Body.String())
	}

	challenge, signature = nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodPost,
		"/api/native/v1/sms/send", body, "sms.send",
	)
	request := nativeTestSignedRequest(
		http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
		challenge, signature, operationID, "sms.send",
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), `"code":"unknown_outcome"`) || hardwareCalls.Load() != 1 {
		t.Fatalf("ambiguous native SMS=%d calls=%d body=%s", response.Code, hardwareCalls.Load(), response.Body.String())
	}

	// A fresh device proof can retrieve the same durable ledger response, but
	// cannot cause a second modem call.
	replayChallenge, replaySignature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodPost,
		"/api/native/v1/sms/send", body, "sms.send",
	)
	replay := nativeTestSignedRequest(
		http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
		replayChallenge, replaySignature, operationID, "sms.send",
	)
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusConflict || hardwareCalls.Load() != 1 ||
		!strings.Contains(replayResponse.Body.String(), `"code":"unknown_outcome"`) {
		t.Fatalf("unknown replay=%d calls=%d body=%s", replayResponse.Code, hardwareCalls.Load(), replayResponse.Body.String())
	}

	queryTarget := "/api/native/v1/sms/operations?operation_id=" + operationID
	queryChallenge, querySignature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodGet, queryTarget, nil, "sms.send",
	)
	query := nativeTestSignedRequest(
		http.MethodGet, queryTarget, nil, device.DeviceID,
		queryChallenge, querySignature, "", "sms.send",
	)
	queryResponse := httptest.NewRecorder()
	handler.ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK ||
		!strings.Contains(queryResponse.Body.String(), `"state":"completed"`) ||
		!strings.Contains(queryResponse.Body.String(), `"http_status":409`) ||
		!strings.Contains(queryResponse.Body.String(), `"code":"unknown_outcome"`) {
		t.Fatalf("unknown operation query=%d %s", queryResponse.Code, queryResponse.Body.String())
	}
}

func TestNativeSMSSendRejectsBodyTamperCapabilityRevocationAndDisabledControl(t *testing.T) {
	instance, handler, privateKey, device := newNativeSMSSendTestApp(t)
	operationID := "nsm_DDDDDDDDDDDDDDDDDDDDDD"
	body := []byte(`{"version":1,"operation_id":"` + operationID + `","phone":"synthetic-native-recipient","message":"synthetic-original"}`)
	var hardwareCalls atomic.Int32
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		hardwareCalls.Add(1)
		return 1, nil
	}
	challenge, signature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodPost,
		"/api/native/v1/sms/send", body, "sms.send",
	)
	tamperedBody := []byte(strings.Replace(string(body), "synthetic-original", "synthetic-tampered", 1))
	tampered := nativeTestSignedRequest(
		http.MethodPost, "/api/native/v1/sms/send", tamperedBody, device.DeviceID,
		challenge, signature, operationID, "sms.send",
	)
	tamperedResponse := httptest.NewRecorder()
	handler.ServeHTTP(tamperedResponse, tampered)
	if tamperedResponse.Code != http.StatusForbidden || hardwareCalls.Load() != 0 {
		t.Fatalf("tampered native SMS=%d calls=%d", tamperedResponse.Code, hardwareCalls.Load())
	}

	denied := nativeTestRemotePOST(t, handler, "/api/native/v1/auth/challenges", nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      device.DeviceID,
		Method:        http.MethodPost,
		RequestTarget: "/api/native/v1/sms/send",
		BodySHA256:    fmt.Sprintf("%x", sha256.Sum256(body)),
	}, "sms.read")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("send challenge without sms.send=%d", denied.Code)
	}

	revokedChallenge, revokedSignature := nativeTestRequestChallenge(
		t, handler, privateKey, device.DeviceID, http.MethodPost,
		"/api/native/v1/sms/send", body, "sms.send",
	)
	if _, err := instance.nativeDeviceStore.Revoke(device.DeviceID); err != nil {
		t.Fatal(err)
	}
	revoked := nativeTestSignedRequest(
		http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
		revokedChallenge, revokedSignature, operationID, "sms.send",
	)
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revoked)
	if revokedResponse.Code != http.StatusForbidden || hardwareCalls.Load() != 0 {
		t.Fatalf("revoked native SMS=%d calls=%d", revokedResponse.Code, hardwareCalls.Load())
	}

	other, otherHandler, _, otherDevice := newNativeSMSSendTestApp(t)
	other.remoteAccess.Control = false
	bodyDigest := sha256.Sum256(body)
	disabledResponse := nativeTestRemotePOST(t, otherHandler, "/api/native/v1/auth/challenges", nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      otherDevice.DeviceID,
		Method:        http.MethodPost,
		RequestTarget: "/api/native/v1/sms/send",
		BodySHA256:    fmt.Sprintf("%x", bodyDigest[:]),
	}, "sms.send")
	if disabledResponse.Code != http.StatusForbidden {
		t.Fatalf("native SMS challenge with remote control disabled=%d", disabledResponse.Code)
	}
}

func TestNativeSMSSendRejectsUnsafeTransportFramingAndDuplicateKey(t *testing.T) {
	instance, handler, privateKey, device := newNativeSMSSendTestApp(t)
	operationID := "nsm_FFFFFFFFFFFFFFFFFFFFFF"
	body := []byte(`{"version":1,"operation_id":"` + operationID + `","phone":"synthetic-native-recipient","message":"synthetic-framing"}`)
	var hardwareCalls atomic.Int32
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		hardwareCalls.Add(1)
		return 1, nil
	}

	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{
			name: "unknown_content_length",
			mutate: func(request *http.Request) {
				request.ContentLength = -1
			},
		},
		{
			name: "incorrect_content_length",
			mutate: func(request *http.Request) {
				request.ContentLength++
			},
		},
		{
			name: "transfer_encoding",
			mutate: func(request *http.Request) {
				request.TransferEncoding = []string{"chunked"}
			},
		},
		{
			name: "duplicate_idempotency_key",
			mutate: func(request *http.Request) {
				request.Header.Add("Idempotency-Key", operationID)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			challenge, signature := nativeTestRequestChallenge(
				t, handler, privateKey, device.DeviceID, http.MethodPost,
				"/api/native/v1/sms/send", body, "sms.send",
			)
			request := nativeTestSignedRequest(
				http.MethodPost, "/api/native/v1/sms/send", body, device.DeviceID,
				challenge, signature, operationID, "sms.send",
			)
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || hardwareCalls.Load() != 0 {
				t.Fatalf("unsafe native SMS framing=%d calls=%d body=%s",
					response.Code, hardwareCalls.Load(), response.Body.String())
			}
		})
	}
}

func TestNativeAndBrowserTransportsRemainDisjoint(t *testing.T) {
	instance, handler := newNativeGatewayTestApp(t, true)
	privateKey, device := nativeTestEnroll(t, instance, handler)
	challenge, signature := nativeTestChallenge(
		t, handler, privateKey, device.DeviceID, "/api/native/v1/session", "status.read")

	withOrigin := nativeTestSignedGET(
		"/api/native/v1/session", device.DeviceID, challenge, signature, "status.read")
	withOrigin.Header.Set("Origin", "https://"+testRemoteHost)
	withOrigin.Header.Set("Sec-Fetch-Site", "same-origin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, withOrigin)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "native_transport_required") {
		t.Fatalf("browser metadata on native route: status=%d body=%s", response.Code, response.Body.String())
	}

	for _, headerName := range []string{"Origin", "Sec-Fetch-Site", "X-MacCellular-CSRF"} {
		t.Run("duplicate_"+strings.ToLower(headerName), func(t *testing.T) {
			challenge, signature := nativeTestChallenge(
				t, handler, privateKey, device.DeviceID, "/api/native/v1/session", "status.read")
			request := nativeTestSignedGET(
				"/api/native/v1/session", device.DeviceID, challenge, signature, "status.read")
			// MIMEHeader.Get returns only the first value. An empty first value
			// must not hide a later browser-security header from the native gate.
			request.Header.Add(headerName, "")
			request.Header.Add(headerName, "synthetic-browser-metadata")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden ||
				!strings.Contains(response.Body.String(), "native_transport_required") {
				t.Fatalf("duplicate %s on native route: status=%d body=%s",
					headerName, response.Code, response.Body.String())
			}
		})
	}

	localNative := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7576/api/native/v1/session", nil)
	localNative.Host = "127.0.0.1:7576"
	localNativeResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(localNativeResponse, localNative)
	if localNativeResponse.Code != http.StatusNotFound {
		t.Fatalf("native remote route on local listener=%d, want 404", localNativeResponse.Code)
	}

	remoteLocal := remoteTestRequest(http.MethodPost,
		"/api/local/v1/native/enrollment/tickets", `{"confirm":true}`, "devices.enroll")
	remoteLocal.Header.Set("Content-Type", "application/json")
	remoteLocalResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteLocalResponse, remoteLocal)
	if remoteLocalResponse.Code != http.StatusForbidden {
		t.Fatalf("local admin route on remote listener=%d, want 403", remoteLocalResponse.Code)
	}

	callRoute := remoteTestRequest(http.MethodPost, "/api/native/v1/calls/answer", `{}`, "calls.control")
	callRoute.Header.Set("Content-Type", "application/json")
	callRouteResponse := httptest.NewRecorder()
	handler.ServeHTTP(callRouteResponse, callRoute)
	if callRouteResponse.Code != http.StatusNotFound {
		t.Fatalf("native call route=%d, want 404", callRouteResponse.Code)
	}
}

func TestNativeProofBindsTargetCapabilityBodyAndRevocation(t *testing.T) {
	instance, handler := newNativeGatewayTestApp(t, true)
	privateKey, device := nativeTestEnroll(t, instance, handler)
	oversizedPage := nativeTestRemotePOST(t, handler, "/api/native/v1/auth/challenges", nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      device.DeviceID,
		Method:        http.MethodGet,
		RequestTarget: "/api/native/v1/sms/sync?limit=11",
		BodySHA256:    nativeEmptyBodySHA256,
	}, "sms.read")
	if oversizedPage.Code != http.StatusForbidden {
		t.Fatalf("native SMS page above limit=%d, want 403", oversizedPage.Code)
	}

	deniedChallenge := nativeTestRemotePOST(t, handler, "/api/native/v1/auth/challenges", nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      device.DeviceID,
		Method:        http.MethodGet,
		RequestTarget: "/api/native/v1/session",
		BodySHA256:    nativeEmptyBodySHA256,
	}, "sms.read")
	if deniedChallenge.Code != http.StatusForbidden {
		t.Fatalf("challenge without target capability=%d, want 403", deniedChallenge.Code)
	}

	target := "/api/native/v1/sms/sync?limit=1"
	challenge, signature := nativeTestChallenge(t, handler, privateKey, device.DeviceID, target, "sms.read")
	tampered := nativeTestSignedGET(
		"/api/native/v1/sms/sync?limit=2", device.DeviceID, challenge, signature, "sms.read")
	tamperedResponse := httptest.NewRecorder()
	handler.ServeHTTP(tamperedResponse, tampered)
	if tamperedResponse.Code != http.StatusForbidden {
		t.Fatalf("tampered request target=%d, want 403", tamperedResponse.Code)
	}

	unknownLengthChallenge, unknownLengthSignature := nativeTestChallenge(
		t, handler, privateKey, device.DeviceID, "/api/native/v1/session", "status.read")
	unknownLength := nativeTestSignedGET(
		"/api/native/v1/session", device.DeviceID, unknownLengthChallenge, unknownLengthSignature, "status.read")
	unknownLength.ContentLength = -1
	unknownLengthResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownLengthResponse, unknownLength)
	if unknownLengthResponse.Code != http.StatusForbidden {
		t.Fatalf("unknown-length signed GET=%d, want 403", unknownLengthResponse.Code)
	}

	revokedChallenge, revokedSignature := nativeTestChallenge(
		t, handler, privateKey, device.DeviceID, "/api/native/v1/session", "status.read")
	if _, err := instance.nativeDeviceStore.Revoke(device.DeviceID); err != nil {
		t.Fatal(err)
	}
	revoked := nativeTestSignedGET(
		"/api/native/v1/session", device.DeviceID, revokedChallenge, revokedSignature, "status.read")
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revoked)
	if revokedResponse.Code != http.StatusForbidden {
		t.Fatalf("revoked device proof=%d, want 403", revokedResponse.Code)
	}
}

func TestNativeAuthChallengeIsSingleUseUnderConcurrency(t *testing.T) {
	store, err := openNativeDeviceStore(filepath.Join(t.TempDir(), "native-devices"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	privateKey, spki := nativeTestKey(t)
	record, _, err := store.Enroll(testRemoteLogin, spki, []string{"status.read"})
	if err != nil {
		t.Fatal(err)
	}
	auth := newNativeAuthManager(store)
	actions := map[string]bool{"status.read": true}
	challenge, err := auth.Issue(testRemoteLogin, testRemoteHost, actions, nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      record.DeviceID,
		Method:        http.MethodGet,
		RequestTarget: "/api/native/v1/session",
		BodySHA256:    nativeEmptyBodySHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := nativeTestSignature(t, privateKey, challenge.SigningInput)
	const contenders = 32
	start := make(chan struct{})
	var successes atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < contenders; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			request := httptest.NewRequest(http.MethodGet,
				"https://"+testRemoteHost+"/api/native/v1/session", nil)
			request.Host = testRemoteHost
			request.Header.Set("X-MacCellular-Device-ID", record.DeviceID)
			request.Header.Set("X-MacCellular-Challenge-ID", challenge.ChallengeID)
			request.Header.Set("X-MacCellular-Signature", signature)
			if _, err := auth.Verify(request, testRemoteLogin, testRemoteHost, actions, nil); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful concurrent proofs=%d, want exactly 1", successes.Load())
	}
}

func TestNativeSMSSendScopeRequiresExplicitEnrollmentAndAppCapability(t *testing.T) {
	scopes, err := normalizeNativeScopes([]string{"status.read", "sms.send", "sms.read"})
	if err != nil || strings.Join(scopes, ",") != "sms.read,sms.send,status.read" {
		t.Fatalf("native SMS scopes=%v err=%v", scopes, err)
	}
	if _, err := normalizeNativeScopes([]string{"status.read", "sms.read", "sms.send", "extra"}); err == nil {
		t.Fatal("oversized native scope set was accepted")
	}
	if nativeActionsAllow(map[string]bool{"*": true}, "sms.send") {
		t.Fatal("wildcard App Cap unexpectedly enabled native SMS sending")
	}
	if !nativeActionsAllow(map[string]bool{"sms.send": true}, "sms.send") {
		t.Fatal("explicit sms.send App Cap was rejected")
	}
}

func TestNativeEnrollmentIsOneShotAndExpiresAtBoundary(t *testing.T) {
	store, err := openNativeDeviceStore(filepath.Join(t.TempDir(), "native-devices"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := newNativeEnrollmentManager(store, false).Issue(
		testRemoteLogin, testRemoteHost, []string{"status.read"}); err != ErrNativeEnrollmentDisabled {
		t.Fatalf("disabled enrollment error=%v", err)
	}
	manager := newNativeEnrollmentManager(store, true)
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	ticket, err := manager.Issue(testRemoteLogin, testRemoteHost, []string{"status.read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Issue(testRemoteLogin, testRemoteHost, []string{"status.read"}); err != ErrNativeEnrollmentConsumed {
		t.Fatalf("second ticket error=%v, want consumed", err)
	}
	_, spki := nativeTestKey(t)
	manager.now = func() time.Time { return now.Add(nativeEnrollmentTicketTTL) }
	_, err = manager.Prepare(testRemoteLogin, testRemoteHost, nativeEnrollmentPrepareRequest{
		Step:          "prepare",
		TicketID:      ticket.TicketID,
		TicketSecret:  ticket.TicketSecret,
		Algorithm:     "ES256",
		PublicKeySPKI: base64.RawURLEncoding.EncodeToString(spki),
	})
	if err != ErrNativeEnrollmentInvalid {
		t.Fatalf("ticket accepted at exact expiry: %v", err)
	}
}

func TestNativeEnrollmentProofFailuresRemainFailClosed(t *testing.T) {
	store, err := openNativeDeviceStore(filepath.Join(t.TempDir(), "native-devices"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := newNativeEnrollmentManager(store, true)
	privateKey, spki := nativeTestKey(t)
	ticket, err := manager.Issue(testRemoteLogin, testRemoteHost, []string{"status.read"})
	if err != nil {
		t.Fatal(err)
	}
	validPrepare := nativeEnrollmentPrepareRequest{
		Step:          "prepare",
		TicketID:      ticket.TicketID,
		TicketSecret:  ticket.TicketSecret,
		Algorithm:     "ES256",
		PublicKeySPKI: base64.RawURLEncoding.EncodeToString(spki),
	}
	wrongOwner := validPrepare
	if _, err := manager.Prepare("other@example.invalid", testRemoteHost, wrongOwner); err != ErrNativeEnrollmentInvalid {
		t.Fatalf("wrong-owner prepare error=%v", err)
	}
	wrongSecret := validPrepare
	wrongSecret.TicketSecret = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err := manager.Prepare(testRemoteLogin, testRemoteHost, wrongSecret); err != ErrNativeEnrollmentInvalid {
		t.Fatalf("wrong-secret prepare error=%v", err)
	}
	prepared, err := manager.Prepare(testRemoteLogin, testRemoteHost, validPrepare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(testRemoteLogin, testRemoteHost, validPrepare); err != ErrNativeEnrollmentInvalid {
		t.Fatalf("consumed ticket prepare error=%v", err)
	}
	invalidComplete := nativeEnrollmentCompleteRequest{
		Step:         "complete",
		EnrollmentID: prepared.EnrollmentID,
		Signature:    base64.RawURLEncoding.EncodeToString([]byte("invalid-signature")),
	}
	if _, err := manager.Complete(testRemoteLogin, testRemoteHost, invalidComplete); err != ErrNativeEnrollmentInvalid {
		t.Fatalf("invalid complete signature error=%v", err)
	}
	validComplete := invalidComplete
	validComplete.Signature = nativeTestSignature(t, privateKey, prepared.SigningInput)
	if _, err := manager.Complete(testRemoteLogin, testRemoteHost, validComplete); err != ErrNativeEnrollmentInvalid {
		t.Fatalf("burned pending proof error=%v", err)
	}
	if count, err := store.Count(); err != nil || count != 0 {
		t.Fatalf("invalid proof enrolled a device: count=%d err=%v", count, err)
	}
}

func TestNativeAuthRejectsDuplicateHeadersAndExactExpiry(t *testing.T) {
	store, err := openNativeDeviceStore(filepath.Join(t.TempDir(), "native-devices"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	privateKey, spki := nativeTestKey(t)
	record, _, err := store.Enroll(testRemoteLogin, spki, []string{"status.read"})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{"status.read": true}
	makeRequest := func(challenge nativeAuthChallengeResponse, signature string) *http.Request {
		request := httptest.NewRequest(http.MethodGet,
			"https://"+testRemoteHost+"/api/native/v1/session", nil)
		request.Host = testRemoteHost
		request.Header.Set("X-MacCellular-Device-ID", record.DeviceID)
		request.Header.Set("X-MacCellular-Challenge-ID", challenge.ChallengeID)
		request.Header.Set("X-MacCellular-Signature", signature)
		return request
	}
	requestChallenge := nativeAuthChallengeRequest{
		Version:       1,
		DeviceID:      record.DeviceID,
		Method:        http.MethodGet,
		RequestTarget: "/api/native/v1/session",
		BodySHA256:    nativeEmptyBodySHA256,
	}

	auth := newNativeAuthManager(store)
	now := time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)
	auth.now = func() time.Time { return now }
	challenge, err := auth.Issue(testRemoteLogin, testRemoteHost, actions, requestChallenge)
	if err != nil {
		t.Fatal(err)
	}
	signature := nativeTestSignature(t, privateKey, challenge.SigningInput)
	duplicate := makeRequest(challenge, signature)
	duplicate.Header.Add("X-MacCellular-Device-ID", record.DeviceID)
	if _, err := auth.Verify(duplicate, testRemoteLogin, testRemoteHost, actions, nil); err != ErrNativeAuthentication {
		t.Fatalf("duplicate auth header error=%v", err)
	}
	if _, err := auth.Verify(makeRequest(challenge, signature), testRemoteLogin, testRemoteHost, actions, nil); err != nil {
		t.Fatalf("malformed envelope unexpectedly burned challenge: %v", err)
	}

	expiring, err := auth.Issue(testRemoteLogin, testRemoteHost, actions, requestChallenge)
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return now.Add(nativeAuthChallengeTTL) }
	if _, err := auth.Verify(
		makeRequest(expiring, nativeTestSignature(t, privateKey, expiring.SigningInput)),
		testRemoteLogin, testRemoteHost, actions, nil,
	); err != ErrNativeAuthentication {
		t.Fatalf("challenge accepted at exact expiry: %v", err)
	}
}

func TestNativeDeviceStoreIsPrivateDurableAndTamperEvident(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native-devices")
	store, err := openNativeDeviceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, spki := nativeTestKey(t)
	_ = privateKey
	record, created, err := store.Enroll(testRemoteLogin, spki, []string{"sms.read", "status.read"})
	if err != nil || !created {
		t.Fatalf("enroll device: created=%v err=%v", created, err)
	}
	if second, err := openNativeDeviceStore(root); err == nil {
		_ = second.Close()
		t.Fatal("second process lock unexpectedly succeeded")
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Mode().Perm() != 0o700 {
				return fmt.Errorf("directory %s mode=%o", filepath.Base(path), info.Mode().Perm())
			}
			return nil
		}
		if info.Mode().Perm() != 0o600 {
			return fmt.Errorf("file %s mode=%o", filepath.Base(path), info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), testRemoteLogin) {
			return fmt.Errorf("raw owner leaked into %s", filepath.Base(path))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openNativeDeviceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := reopened.Lookup(record.DeviceID, testRemoteLogin)
	if err != nil || !ok || stored.DeviceID != record.DeviceID {
		t.Fatalf("reopened lookup: ok=%v record=%+v err=%v", ok, stored, err)
	}
	idempotent, created, err := reopened.Enroll(
		testRemoteLogin, spki, []string{"status.read", "sms.read"})
	if err != nil || created || !idempotent.EnrolledAt.Equal(record.EnrolledAt) {
		t.Fatalf("idempotent reenrollment: created=%v record=%+v err=%v", created, idempotent, err)
	}
	if _, _, err := reopened.Enroll(
		"different-owner@example.invalid", spki, []string{"status.read", "sms.read"}); err == nil {
		t.Fatal("same key was rebound to a different owner")
	}
	entries, err := os.ReadDir(filepath.Join(root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("idempotent reenrollment appended %d events, want 1", len(entries))
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	eventPath := filepath.Join(root, "events", "00000000000000000001.ndev")
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(eventPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if compromised, err := openNativeDeviceStore(root); err == nil {
		_ = compromised.Close()
		t.Fatal("tampered native device event unexpectedly reopened")
	}
}

func TestNativeDeviceStoreRejectsMissingRevocationTail(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native-devices")
	store, err := openNativeDeviceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	_, spki := nativeTestKey(t)
	record, _, err := store.Enroll(testRemoteLogin, spki, []string{"status.read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(record.DeviceID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := openNativeDeviceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, active, err := verified.Lookup(record.DeviceID, testRemoteLogin); err != nil || active {
		t.Fatalf("durable revocation lookup: active=%v err=%v", active, err)
	}
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "events", "00000000000000000002.ndev")); err != nil {
		t.Fatal(err)
	}
	if rolledBack, err := openNativeDeviceStore(root); err == nil {
		_ = rolledBack.Close()
		t.Fatal("store accepted a deleted revocation tail event")
	}
}

func TestNativeCredentialFormattingIsRedacted(t *testing.T) {
	store, err := openNativeDeviceStore(filepath.Join(t.TempDir(), "native-devices"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret := strings.Repeat("sensitive-ticket-material", 2)
	ticket := nativeEnrollmentTicketResponse{TicketSecret: secret}
	prepare := nativeEnrollmentPrepareRequest{TicketSecret: secret, PublicKeySPKI: secret}
	manager := newNativeEnrollmentManager(store, true)
	auth := newNativeAuthManager(store)
	for name, value := range map[string]any{
		"store": store, "ticket": ticket, "prepare": prepare, "enrollment": manager, "auth": auth,
	} {
		formatted := fmt.Sprintf("%v %#v %+v", value, value, value)
		if strings.Contains(formatted, secret) || strings.Contains(formatted, "eventKey:") ||
			strings.Contains(formatted, "ownerKey:") {
			t.Fatalf("%s formatting exposed credential material: %s", name, formatted)
		}
	}
}

func TestNativeStoreRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permission semantics are platform-specific")
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "native-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if store, err := openNativeDeviceStore(link); err == nil {
		_ = store.Close()
		t.Fatal("symlink native device root unexpectedly opened")
	}
}
