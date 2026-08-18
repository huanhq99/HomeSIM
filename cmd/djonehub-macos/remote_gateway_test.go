package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRemoteHost       = "gateway.example-tailnet.ts.net"
	testRemoteLogin      = "owner@example.com"
	testRemoteCapability = "owner.example/cap/cellular-gateway"
)

func newRemoteTestApp(t *testing.T, control bool, actions ...string) (*app, http.Handler, string) {
	t.Helper()
	instance := newDemoApp()
	if err := instance.initializeSMSStore(filepath.Join(t.TempDir(), "sms")); err != nil {
		t.Fatalf("initialize test SMS store: %v", err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "remote", "operations.jsonl")
	instance.configureRemoteAccess(testRemoteLogin, control, testRemoteCapability, testRemoteHost, "127.0.0.1:7577", ledgerPath)
	if instance.remoteLedger == nil {
		t.Fatal("remote operation ledger was not configured")
	}
	t.Cleanup(func() {
		_ = instance.remoteLedger.Close()
		instance.closeSMSStore()
	})
	handler := instance.remoteRoutes()
	request := remoteTestRequest(http.MethodGet, "/api/remote/v1/session", "", actions...)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("session status = %d, body=%s", response.Code, response.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil || session.CSRF == "" {
		t.Fatalf("decode session CSRF: %v body=%s", err, response.Body.String())
	}
	return instance, handler, session.CSRF
}

func TestRemoteSMSSyncUsesExactReadCapabilityAndNoStore(t *testing.T) {
	instance, handler, _ := newRemoteTestApp(t, false, "status.read", "sms.read")
	message, _, err := instance.smsStore.IngestIncoming("synthetic-peer-a", "synthetic-content-a", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sessionRequest := remoteTestRequest(http.MethodGet, "/api/remote/v1/session", "", "status.read", "sms.read")
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var session struct {
		Version string `json:"version"`
		SMSSync bool   `json:"sms_sync"`
	}
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode sync session: %v body=%s", err, sessionResponse.Body.String())
	}
	if session.Version != "1.4.0" || !session.SMSSync {
		t.Fatalf("session did not advertise durable SMS sync: status=%d session=%+v body=%s", sessionResponse.Code, session, sessionResponse.Body.String())
	}

	request := remoteTestRequest(http.MethodGet, "/api/remote/v1/sms/sync?limit=1", "", "sms.read")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("sync status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
	var result SMSSyncResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Bootstrap || len(result.Messages) != 1 || result.Messages[0].ID != message.ID || result.Cursor == "" {
		t.Fatalf("unexpected sync result: %+v", result)
	}

	denied := remoteTestRequest(http.MethodGet, "/api/remote/v1/sms/sync", "", "status.read")
	deniedResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("sync without sms.read=%d, want 403", deniedResponse.Code)
	}
}

func TestRemoteSMSSyncStrictQueryAndCursorErrors(t *testing.T) {
	instance, handler, _ := newRemoteTestApp(t, true, "status.read", "sms.read")
	if _, _, err := instance.smsStore.IngestIncoming("peer", "body", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		code string
	}{
		{name: "unknown", path: "/api/remote/v1/sms/sync?extra=1", code: "invalid_query"},
		{name: "duplicate limit", path: "/api/remote/v1/sms/sync?limit=1&limit=2", code: "invalid_query"},
		{name: "zero limit", path: "/api/remote/v1/sms/sync?limit=0", code: "invalid_limit"},
		{name: "large limit", path: "/api/remote/v1/sms/sync?limit=501", code: "invalid_limit"},
		{name: "signed number", path: "/api/remote/v1/sms/sync?limit=%2B1", code: "invalid_limit"},
		{name: "invalid cursor", path: "/api/remote/v1/sms/sync?cursor=not-a-cursor", code: "invalid_cursor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := remoteTestRequest(http.MethodGet, test.path, "", "sms.read")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response=%d %s, want 400 %s", response.Code, response.Body.String(), test.code)
			}
		})
	}

	bootstrap, err := instance.syncSMSHistory("", 10)
	if err != nil {
		t.Fatal(err)
	}
	instance.smsStore.mu.Lock()
	instance.smsStore.epoch[0] ^= 0xff
	instance.smsStore.mu.Unlock()
	request := remoteTestRequest(http.MethodGet, "/api/remote/v1/sms/sync?cursor="+bootstrap.Cursor, "", "sms.read")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), `"code":"reset_required"`) {
		t.Fatalf("old epoch response=%d %s, want 410 reset_required", response.Code, response.Body.String())
	}
}

func TestRemoteSMSSendAppearsAsSubmittedInDurableSync(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.read", "sms.send")
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", `{"phone":"synthetic-recipient-a","message":"synthetic-content-a"}`, "sms.send")
	authorizeRemoteMutation(request, csrf, "request-key-durable-sms")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("send response=%d %s", response.Code, response.Body.String())
	}
	result, err := instance.syncSMSHistory("", 10)
	if err != nil {
		t.Fatal(err)
	}
	var outgoing *StoreMessage
	for index := range result.Messages {
		if result.Messages[index].Direction == "outgoing" {
			outgoing = &result.Messages[index]
			break
		}
	}
	if outgoing == nil || outgoing.Peer != "synthetic-recipient-a" || outgoing.Status != "submitted" || outgoing.Segments != 1 {
		t.Fatalf("durable outgoing=%+v messages=%+v", outgoing, result.Messages)
	}
}

func TestRemoteSMSSendHardwareErrorPreservesSegmentsAsUnknown(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.read", "sms.send")
	instance.demo = false
	instance.sendTextSMSOverride = func(phone, message string) (int, error) {
		items, err := instance.smsStoreSnapshot().List(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Status != "pending" {
			t.Fatalf("hardware ran before pending was durable: %+v", items)
		}
		return 2, errors.New("injected timeout after multipart submission")
	}
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", `{"phone":"synthetic-recipient-b","message":"synthetic-multipart-content"}`, "sms.send")
	authorizeRemoteMutation(request, csrf, "request-key-unknown-segments")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"unknown_outcome"`) {
		t.Fatalf("send response=%d %s, want durable unknown_outcome", response.Code, response.Body.String())
	}
	items, err := instance.smsStoreSnapshot().List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "unknown" || items[0].Segments != 2 {
		t.Fatalf("hardware error did not preserve returned segments: %+v", items)
	}
}

func TestRemoteSMSSendSubmittedUpdateFailureFallsBackToUnknown(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.read", "sms.send")
	instance.demo = false
	// A successful transport with zero segments forces the submitted transition
	// to fail validation while leaving the store usable for the unknown fallback.
	instance.sendTextSMSOverride = func(string, string) (int, error) { return 0, nil }
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", `{"phone":"synthetic-recipient-c","message":"synthetic-content-c"}`, "sms.send")
	authorizeRemoteMutation(request, csrf, "request-key-submitted-update-failure")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"unknown_outcome"`) {
		t.Fatalf("send response=%d %s, want durable unknown_outcome", response.Code, response.Body.String())
	}
	items, err := instance.smsStoreSnapshot().List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "unknown" || items[0].Segments != 0 {
		t.Fatalf("submitted update failure did not fall back to unknown: %+v", items)
	}
	if available, code := instance.smsStoreStatus(); !available || code != "" {
		t.Fatalf("ordinary validation error disabled the store: available=%v code=%q", available, code)
	}
}

func remoteTestRequest(method, path, body string, actions ...string) *http.Request {
	request := httptest.NewRequest(method, "https://"+testRemoteHost+path, strings.NewReader(body))
	request.Host = testRemoteHost
	// Tailscale Serve forwards to loopback in production, but media ICE peer
	// binding must use an authenticated tailnet address. Tests model that exact
	// source explicitly instead of inheriting httptest's public example address.
	request.RemoteAddr = "100.64.0.2:42424"
	request.Header.Set("Tailscale-User-Login", testRemoteLogin)
	capabilityJSON, _ := json.Marshal(map[string]any{
		testRemoteCapability: []any{map[string]any{"actions": actions}},
	})
	request.Header.Set("Tailscale-App-Capabilities", string(capabilityJSON))
	return request
}

func authorizeRemoteMutation(request *http.Request, csrf, key string) {
	request.Header.Set("Origin", "https://"+testRemoteHost)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-MacCellular-CSRF", csrf)
	request.Header.Set("Idempotency-Key", key)
}

func TestRemoteGatewayRequiresExactHostIdentityAndCapability(t *testing.T) {
	_, handler, _ := newRemoteTestApp(t, true, "status.read")
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "wrong host", mutate: func(r *http.Request) { r.Host = "other.ts.net" }},
		{name: "missing identity", mutate: func(r *http.Request) { r.Header.Del("Tailscale-User-Login") }},
		{name: "wrong identity", mutate: func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "attacker@example.com") }},
		{name: "missing capability", mutate: func(r *http.Request) { r.Header.Del("Tailscale-App-Capabilities") }},
		{name: "wrong action", mutate: func(r *http.Request) {
			value := fmt.Sprintf(`{"%s":[{"actions":["sms.read"]}]}`, testRemoteCapability)
			r.Header.Set("Tailscale-App-Capabilities", value)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := remoteTestRequest(http.MethodGet, "/api/remote/v1/session", "", "status.read")
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.Code)
			}
		})
	}
}

func TestLocalAndRemoteMuxesStayDisjoint(t *testing.T) {
	instance, remote, _ := newRemoteTestApp(t, true, "status.read")
	localRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7576/api/remote/v1/session", nil)
	localRequest.Host = "127.0.0.1:7576"
	localResponse := httptest.NewRecorder()
	instance.routes().ServeHTTP(localResponse, localRequest)
	if localResponse.Code != http.StatusNotFound {
		t.Fatalf("local listener remote path = %d, want 404", localResponse.Code)
	}

	remoteRequest := remoteTestRequest(http.MethodGet, "/api/status", "", "status.read")
	remoteResponse := httptest.NewRecorder()
	remote.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusForbidden {
		t.Fatalf("remote listener local path = %d, want 403", remoteResponse.Code)
	}
}

func TestRemoteVoiceV2SecurityAndPersistenceContractIsExact(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		path          string
		action        string
		additional    string
		explicit      bool
		mutation      bool
		requiresMedia bool
		ephemeral     bool
		persistent    bool
	}{
		{
			name: "snapshot", method: http.MethodGet, path: remoteVoiceV2SnapshotPath,
			action: "calls.read",
		},
		{
			name: "offer", method: http.MethodPost, path: remoteVoiceV2OfferPath,
			action: "calls.media", additional: "calls.read", explicit: true,
			mutation: true, ephemeral: true,
		},
		{
			name: "answer", method: http.MethodPost, path: remoteVoiceV2AnswerPath,
			action: "calls.control", additional: "calls.media", explicit: true,
			mutation: true, requiresMedia: true, persistent: true,
		},
		{
			name: "reject", method: http.MethodPost, path: remoteVoiceV2RejectPath,
			action: "calls.control", additional: "calls.read", explicit: true,
			mutation: true, persistent: true,
		},
		{
			name: "dtmf", method: http.MethodPost, path: remoteVoiceV2DTMFPath,
			action: "calls.control", additional: "calls.media", explicit: true,
			mutation: true, requiresMedia: true, persistent: true,
		},
		{
			name: "end", method: http.MethodPost, path: remoteVoiceV2EndPath,
			action: "calls.hangup", additional: "calls.media", explicit: true,
			mutation: true, requiresMedia: true, persistent: true,
		},
		{
			name: "reconcile", method: http.MethodPost, path: remoteVoiceV2ReconcilePath,
			action: "calls.media", explicit: true,
			mutation: true, requiresMedia: true, ephemeral: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "https://"+testRemoteHost+test.path, nil)
			if !isRemoteGatewayPath(test.path) {
				t.Fatal("fixed v2 voice path was not recognized by the remote gateway")
			}
			if got := remoteRequiredAction(test.path); got != test.action {
				t.Fatalf("required action=%q, want %q", got, test.action)
			}
			policy, ok := remoteVoiceV2Policy(test.path)
			if !ok || policy.additionalAction != test.additional || policy.requireExplicit != test.explicit {
				t.Fatalf("policy=%+v, want additional=%q explicit=%v", policy, test.additional, test.explicit)
			}
			if got := isRemoteGatewayMutation(request); got != test.mutation {
				t.Fatalf("mutation=%v, want %v", got, test.mutation)
			}
			if got := remoteRequiresMediaCapability(test.path); got != test.requiresMedia {
				t.Fatalf("requires media=%v, want %v", got, test.requiresMedia)
			}
			if got := isRemoteEphemeralMutation(request); got != test.ephemeral {
				t.Fatalf("ephemeral=%v, want %v", got, test.ephemeral)
			}
			if got := validRemoteLedgerPath(test.path); got != test.persistent {
				t.Fatalf("persistent ledger path=%v, want %v", got, test.persistent)
			}
		})
	}

	for _, path := range []string{
		"/api/remote/v2/voice",
		"/api/remote/v2/voice/",
		"/api/remote/v2/voice/calls",
		remoteVoiceV2AnswerPath + "/extra",
		remoteVoiceV2RejectPath + "/extra",
		"/api/remote/v3/voice/snapshot",
	} {
		request := httptest.NewRequest(http.MethodPost, "https://"+testRemoteHost+path, nil)
		if isRemoteGatewayPath(path) || isRemoteGatewayMutation(request) ||
			isRemoteEphemeralMutation(request) || remoteRequiresMediaCapability(path) ||
			validRemoteLedgerPath(path) {
			t.Fatalf("unknown v2-like path crossed a fixed security boundary: %q", path)
		}
	}
}

func TestRemoteVoiceV2PathsAreSecuredAndRegisteredFailClosed(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "calls.read", "calls.control")

	readDenied := remoteTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", "status.read")
	readDeniedResponse := httptest.NewRecorder()
	handler.ServeHTTP(readDeniedResponse, readDenied)
	if readDeniedResponse.Code != http.StatusForbidden {
		t.Fatalf("v2 snapshot without calls.read=%d, want 403", readDeniedResponse.Code)
	}

	readAuthorized := remoteTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", "calls.read")
	readAuthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(readAuthorizedResponse, readAuthorized)
	if readAuthorizedResponse.Code != http.StatusOK ||
		!strings.Contains(readAuthorizedResponse.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled v2 snapshot=%d %s, want redacted disabled status",
			readAuthorizedResponse.Code, readAuthorizedResponse.Body.String())
	}

	answer := remoteTestRequest(http.MethodPost, remoteVoiceV2AnswerPath, `{}`, "calls.control")
	authorizeRemoteMutation(answer, csrf, "request-key-v2-no-media")
	answerResponse := httptest.NewRecorder()
	handler.ServeHTTP(answerResponse, answer)
	if answerResponse.Code != http.StatusForbidden {
		t.Fatalf("v2 answer without calls.media=%d, want 403", answerResponse.Code)
	}
	instance.remoteLedger.mu.Lock()
	entries := len(instance.remoteLedger.entries)
	records := instance.remoteLedger.records
	instance.remoteLedger.mu.Unlock()
	if entries != 0 || records != 0 {
		t.Fatalf("v2 capability rejection reached ledger: entries=%d records=%d", entries, records)
	}
}

func TestRemoteCapabilityParserRejectsTrailingAndUnknownGrantFields(t *testing.T) {
	for _, raw := range []string{
		fmt.Sprintf(`{"%s":[{"actions":["status.read"]}]} {}`, testRemoteCapability),
		fmt.Sprintf(`{"%s":[{"actions":["status.read"],"admin":true}]}`, testRemoteCapability),
	} {
		if _, err := parseRemoteCapabilityActions(raw, testRemoteCapability); err == nil {
			t.Fatalf("capability parser accepted invalid value %q", raw)
		}
	}
}

func TestRemoteMutationRequiresExplicitOriginJSONCSRFAndControl(t *testing.T) {
	_, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.send")
	base := func() *http.Request {
		request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", `{"phone":"10086","message":"test"}`, "status.read", "sms.send")
		authorizeRemoteMutation(request, csrf, "request-key-0001")
		return request
	}
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "valid", mutate: func(*http.Request) {}, want: http.StatusOK},
		{name: "missing origin", mutate: func(r *http.Request) { r.Header.Del("Origin") }, want: http.StatusForbidden},
		{name: "foreign origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, want: http.StatusForbidden},
		{name: "duplicate origin", mutate: func(r *http.Request) { r.Header.Add("Origin", "https://"+testRemoteHost) }, want: http.StatusForbidden},
		{name: "missing fetch metadata", mutate: func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }, want: http.StatusForbidden},
		{name: "duplicate fetch metadata", mutate: func(r *http.Request) { r.Header.Add("Sec-Fetch-Site", "same-origin") }, want: http.StatusForbidden},
		{name: "missing content type", mutate: func(r *http.Request) { r.Header.Del("Content-Type") }, want: http.StatusUnsupportedMediaType},
		{name: "wrong content type", mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
		{name: "duplicate content type", mutate: func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }, want: http.StatusUnsupportedMediaType},
		{name: "missing csrf", mutate: func(r *http.Request) { r.Header.Del("X-MacCellular-CSRF") }, want: http.StatusForbidden},
		{name: "duplicate csrf", mutate: func(r *http.Request) { r.Header.Add("X-MacCellular-CSRF", csrf) }, want: http.StatusForbidden},
		{name: "missing idempotency key", mutate: func(r *http.Request) { r.Header.Del("Idempotency-Key") }, want: http.StatusBadRequest},
		{name: "duplicate idempotency key", mutate: func(r *http.Request) { r.Header.Add("Idempotency-Key", "request-key-0001") }, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base()
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}

	_, readOnly, readOnlyCSRF := newRemoteTestApp(t, false, "status.read", "sms.send")
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", `{"phone":"10086","message":"test"}`, "status.read", "sms.send")
	authorizeRemoteMutation(request, readOnlyCSRF, "request-key-0002")
	response := httptest.NewRecorder()
	readOnly.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("control kill switch status = %d, want 403", response.Code)
	}
}

func TestRemoteMutationRejectsTrailingJSON(t *testing.T) {
	_, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.send")
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send",
		`{"phone":"10086","message":"test"} {}`, "status.read", "sms.send")
	authorizeRemoteMutation(request, csrf, "request-key-trailing")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d, want 400", response.Code)
	}
}

func TestRemoteSMSRefreshRequiresAnExactEmptyObject(t *testing.T) {
	_, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.read")
	for index, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "empty object", body: `{}`, want: http.StatusAccepted},
		{name: "unknown field", body: `{"force":true}`, want: http.StatusBadRequest},
		{name: "empty body", body: ``, want: http.StatusBadRequest},
		{name: "trailing value", body: `{} {}`, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/refresh", test.body,
				"status.read", "sms.read")
			authorizeRemoteMutation(request, csrf, fmt.Sprintf("request-key-refresh-%d", index))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d, body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestRemoteGatewayExposesCommittedAuditFailureWithoutRetry(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "sms.send")
	instance.smsMu.RLock()
	beforeMessages := len(instance.sms)
	instance.smsMu.RUnlock()
	injected := errors.New("injected audit fsync failure")
	var auditSyncs atomic.Int32
	instance.remoteLedger.syncAudit = func(file *os.File) error {
		if auditSyncs.Add(1) == 2 {
			return injected
		}
		return file.Sync()
	}

	requestBody := `{"phone":"10086","message":"test"}`
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", requestBody, "status.read", "sms.send")
	authorizeRemoteMutation(request, csrf, "request-key-audit-failure")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"code":"completed_audit_unavailable"`) {
		t.Fatalf("audit failure response = %d %s, want terminal 202", response.Code, response.Body.String())
	}
	instance.smsMu.RLock()
	afterFirst := len(instance.sms)
	instance.smsMu.RUnlock()
	if afterFirst != beforeMessages+1 {
		t.Fatalf("first mutation added %d messages, want 1", afterFirst-beforeMessages)
	}

	// The same key is an authoritative replay even though replay auditing is
	// still unavailable. It must never become unknown or execute again.
	replayRequest := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", requestBody, "status.read", "sms.send")
	authorizeRemoteMutation(replayRequest, csrf, "request-key-audit-failure")
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replayRequest)
	if replayResponse.Code != http.StatusOK || !strings.Contains(replayResponse.Body.String(), `"code":"completed"`) {
		t.Fatalf("same-key replay = %d %s, want durable terminal result", replayResponse.Code, replayResponse.Body.String())
	}
	instance.smsMu.RLock()
	afterReplay := len(instance.sms)
	instance.smsMu.RUnlock()
	if afterReplay != afterFirst {
		t.Fatalf("same-key replay repeated hardware-side mutation: before=%d after=%d", afterFirst, afterReplay)
	}

	newRequest := remoteTestRequest(http.MethodPost, "/api/remote/v1/sms/send", requestBody, "status.read", "sms.send")
	authorizeRemoteMutation(newRequest, csrf, "request-key-after-audit-failure")
	newResponse := httptest.NewRecorder()
	handler.ServeHTTP(newResponse, newRequest)
	if newResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("new key after audit failure = %d, want 503 fail-closed", newResponse.Code)
	}
	instance.smsMu.RLock()
	afterNewKey := len(instance.sms)
	instance.smsMu.RUnlock()
	if afterNewKey != afterFirst {
		t.Fatalf("new key executed while audit was unavailable: before=%d after=%d", afterFirst, afterNewKey)
	}
}

func TestRemoteCallControlRejectsStaleGeneration(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "calls.read", "calls.control")
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10086"}}, time.Now())
	instance.callMu.RLock()
	active := *instance.activeCall
	generation := instance.callGeneration
	instance.callMu.RUnlock()

	body := fmt.Sprintf(`{"call_id":%q,"call_generation":%d,"call_index":1,"call_direction":"incoming"}`,
		active.ID, generation+1)
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/calls/reject", body,
		"status.read", "calls.read", "calls.control")
	authorizeRemoteMutation(request, csrf, "request-key-stale")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale call status = %d, want 409, body=%s", response.Code, response.Body.String())
	}
	instance.callMu.RLock()
	defer instance.callMu.RUnlock()
	if instance.activeCall == nil {
		t.Fatal("stale remote command changed the current call")
	}
}

func TestRemoteDangerousCallControlsRemainFailClosedForABA(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "calls.read", "calls.control")
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	instance.callMu.RLock()
	active := *instance.activeCall
	generation := instance.callGeneration
	instance.callMu.RUnlock()
	body := fmt.Sprintf(`{"call_id":%q,"call_generation":%d,"call_index":1,"call_direction":"incoming"}`,
		active.ID, generation)
	for index, endpoint := range []string{"reject", "hangup", "dtmf"} {
		requestBody := body
		actions := []string{"status.read", "calls.read", "calls.control"}
		if endpoint == "hangup" {
			requestBody = fmt.Sprintf(`{"call":%s,"media_session_id":%q,"lease_generation":17}`,
				body, strings.Repeat("A", 43))
			actions = append(actions, "calls.media", "calls.hangup")
		}
		request := remoteTestRequest(http.MethodPost, "/api/remote/v1/calls/"+endpoint, requestBody, actions...)
		authorizeRemoteMutation(request, csrf, fmt.Sprintf("request-key-aba-%d", index))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("%s status = %d, want 409", endpoint, response.Code)
		}
		instance.callMu.RLock()
		stillActive := instance.activeCall != nil
		instance.callMu.RUnlock()
		if !stillActive {
			t.Fatalf("%s changed the call despite ABA fail-closed policy", endpoint)
		}
	}
}

func TestRemoteDialAndAnswerRemainFailClosedWithoutMedia(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "calls.read", "calls.control")
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming", Number: "10086"}}, time.Now())
	instance.callMu.RLock()
	active := *instance.activeCall
	generation := instance.callGeneration
	instance.callMu.RUnlock()

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "dial",
			path: "/api/remote/v1/calls/dial",
			body: fmt.Sprintf(`{"number":"10010","expected_call_generation":%d}`, generation),
		},
		{
			name: "answer",
			path: "/api/remote/v1/calls/answer",
			body: fmt.Sprintf(`{"call":{"call_id":%q,"call_generation":%d,"call_index":1,"call_direction":"incoming"},"media_session_id":%q,"lease_generation":17}`,
				active.ID, generation, strings.Repeat("A", 43)),
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := remoteTestRequest(http.MethodPost, test.path, test.body,
				"status.read", "calls.read", "calls.control", "calls.media")
			authorizeRemoteMutation(request, csrf, fmt.Sprintf("request-key-media-gate-%d", index))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409, body=%s", response.Code, response.Body.String())
			}
			instance.callMu.RLock()
			defer instance.callMu.RUnlock()
			if instance.activeCall == nil || instance.activeCall.ID != active.ID || instance.callGeneration != generation {
				t.Fatal("fail-closed remote call request changed the active call")
			}
		})
	}
}

func TestRemoteCallEstablishmentRequiresMediaCapabilityBeforeLedger(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "calls.read", "calls.control")
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	instance.callMu.RLock()
	active := *instance.activeCall
	generation := instance.callGeneration
	instance.callMu.RUnlock()
	body := fmt.Sprintf(`{"call":{"call_id":%q,"call_generation":%d,"call_index":1,"call_direction":"incoming"},"media_session_id":%q,"lease_generation":17}`,
		active.ID, generation, strings.Repeat("A", 43))
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/calls/answer", body,
		"status.read", "calls.read", "calls.control")
	authorizeRemoteMutation(request, csrf, "request-key-no-media-cap")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 before ledger, body=%s", response.Code, response.Body.String())
	}
	instance.remoteLedger.mu.Lock()
	entries := len(instance.remoteLedger.entries)
	records := instance.remoteLedger.records
	instance.remoteLedger.mu.Unlock()
	if entries != 0 || records != 0 {
		t.Fatalf("capability rejection reached durable ledger: entries=%d records=%d", entries, records)
	}
}

func TestRemoteCSRFExpiresAndBindsIdentity(t *testing.T) {
	store := newRemoteCSRFStore()
	now := time.Unix(2_000_000_000, 0)
	token := store.issue("owner@example.com", now)
	if !store.valid("owner@example.com", token, now.Add(time.Hour)) {
		t.Fatal("valid CSRF token was rejected")
	}
	if store.valid("other@example.com", token, now.Add(time.Hour)) {
		t.Fatal("CSRF token was not bound to identity")
	}
	if store.valid("owner@example.com", token, now.Add(9*time.Hour)) {
		t.Fatal("expired CSRF token was accepted")
	}
}

func TestRemoteAuthorizationRejectsDuplicateIdentityAndCapabilityHeaders(t *testing.T) {
	_, handler, _ := newRemoteTestApp(t, false, "status.read")
	for _, test := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "identity", header: "Tailscale-User-Login", value: testRemoteLogin},
		{name: "app capability", header: "Tailscale-App-Capabilities", value: `{"` + testRemoteCapability + `":[{"actions":["status.read"]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := remoteTestRequest(http.MethodGet, "/api/remote/v1/session", "", "status.read")
			request.Header.Add(test.header, test.value)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("duplicate %s status=%d, want 403", test.header, response.Code)
			}
		})
	}
}

func TestRemotePWAAssetsAndServiceWorkerPrivacyPolicy(t *testing.T) {
	for _, asset := range []string{
		"remote/index.html", "remote/app.js", "remote/media-client.js", "remote/call-recorder.js", "remote/external-voice-client.js", "remote/sms-state.js", "remote/session-presentation.js", "remote/incoming-call-push.js", "remote/style.css", "remote/manifest.webmanifest",
		"remote/service-worker.js", "remote/icon-192.png", "remote/icon-512.png", "remote/apple-touch-icon.png",
	} {
		if _, err := webAssets.ReadFile(asset); err != nil {
			t.Errorf("embedded PWA asset %s: %v", asset, err)
		}
	}
	worker, err := webAssets.ReadFile("remote/service-worker.js")
	if err != nil {
		t.Fatal(err)
	}
	policy := string(worker)
	for _, required := range []string{
		`url.pathname.startsWith("/api/")`, `response.ok`, `response.type === "basic"`,
		`maccellular-remote-v63`, `app.js?v=20260817-remote39`, `session-presentation.js?v=20260815-remote1`,
		`incoming-call-push.js?v=20260815-remote1`, `sms-state.js?v=20260814-remote1`, `public-voice-ui.js?v=20260816-remote9`,
		`media-client.js?v=20260817-remote27`, `call-recorder.js?v=20260817-remote2`, `external-voice-client.js?v=20260815-remote2`, `style.css?v=20260817-remote8`,
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("service worker is missing privacy condition %q", required)
		}
	}
	indexAsset, err := webAssets.ReadFile("remote/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`app.js?v=20260817-remote39`, `session-presentation.js?v=20260815-remote1`,
		`incoming-call-push.js?v=20260815-remote1`, `sms-state.js?v=20260814-remote1`, `public-voice-ui.js?v=20260816-remote9`,
		`media-client.js?v=20260817-remote27`, `call-recorder.js?v=20260817-remote2`, `external-voice-client.js?v=20260815-remote2`, `style.css?v=20260817-remote8`,
		`手机与家中 Mac 双副本`, `通话接通后自动录音`,
		`id="dial-number" type="text" inputmode="none"`, `readonly required`,
	} {
		if !strings.Contains(string(indexAsset), required) {
			t.Errorf("PWA index and service-worker cache versions diverged; missing %q", required)
		}
	}
	if strings.Contains(string(indexAsset), "仅通过 Tailscale") {
		t.Error("PWA initial connection copy must remain transport-neutral")
	}
	if strings.Contains(string(indexAsset), "QDC507 直连路径不会被网页拨号打开") ||
		!strings.Contains(string(indexAsset), `id="dial-help"`) {
		t.Error("PWA dial help must be capability-driven and must not retain the obsolete direct-voice denial")
	}
	manifest, err := webAssets.ReadFile("remote/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"id": "/remote/"`, `"192x192"`, `"512x512"`} {
		if !strings.Contains(string(manifest), required) {
			t.Errorf("manifest is missing %q", required)
		}
	}
	appScript, err := webAssets.ReadFile("remote/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`externalVoiceStatus?.dial_enabled === true`,
		`api("/api/remote/v2/voice/calls/dial"`,
		`$("#answer").disabled = true`,
		`row.disabled = !callable`,
		`$("#prepare-media").disabled`,
		`../api/remote/v1/media/offers`,
		`session?.incoming_answer`,
		`remoteMedia.call_action_ready === true`,
		`localMedia.lease_generation === remoteMedia.lease_generation`,
		`session?.rescue_hangup`,
		`can("calls.hangup")`,
		`rescueMediaReceipt = null`,
		`authenticatedHangupReady`,
		`directAnswerInFlight`,
		`timeoutMilliseconds: 25_000`,
		`directAnsweredCallID`,
		`正在自动准备并接听，通常约 5–12 秒，无需再次点击`,
		`点击“接听”后会自动完成通话连接`,
		`接听请求没有得到确认，请再点一次；不会重复接听`,
		`return call ? { call } : null;`,
		`../api/remote/v1/sms/sync`,
		`remoteSMSState.clear()`,
		`page.bootstrap !== true`,
		`candidate?.external_voice_api_version === 2`,
		`hasExplicitActionFor(candidate, "calls.read")`,
		`canExternalVoice("calls.media")`,
		`canExternalVoice("calls.control")`,
		`canExternalVoice("calls.hangup")`,
		`call.phase === "media_ready"`,
		`call.media?.prepared === true`,
		`externalVoiceRecoveryLocked()`,
		`client.snapshot().phase === "outcome_unknown"`,
		`await client.reconcile()`,
		`client.close("manual-recovery-required")`,
		`remoteMediaClient.close("external-voice-priority")`,
		`externalVoiceClient?.close("snapshot-unavailable")`,
		`callsTab.hidden = smsOnly`,
		`callsPanel.hidden = smsOnly`,
		`selectTab("messages")`,
		`externalVoiceClient?.close("sms-only-session")`,
		`remoteMediaClient?.close("sms-only-session")`,
		`sessionPresentation.transportLabel(session)`,
		`maybeStartAutomaticRecording(nextCall)`,
		`../api/remote/v1/recordings`,
		`recording.mac_saved = true`,
		`本次自动录音已停止`,
		`formatRecordingTitle(item)`,
	} {
		if !strings.Contains(string(appScript), required) {
			t.Errorf("PWA must keep remote call controls fail-closed; missing %q", required)
		}
	}
	if strings.Contains(string(appScript), "征得同意") || strings.Contains(string(appScript), "录音前请") {
		t.Error("PWA must not show a per-call recording consent prompt for this private installation")
	}
	if strings.Contains(string(appScript), `input.focus({ preventScroll: true })`) {
		t.Error("dial-pad backspace must not summon the native mobile keyboard")
	}
	sessionPresentationScript, err := webAssets.ReadFile("remote/session-presentation.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`session?.sms_only === true`,
		`transport.toLowerCase() === "cloudflare-access"`,
		`return "公网"`,
		`return transport || "Tailscale"`,
	} {
		if !strings.Contains(string(sessionPresentationScript), required) {
			t.Errorf("PWA session presentation is missing public SMS condition %q", required)
		}
	}
	if strings.Contains(string(appScript), `localStorage.setItem("rescue`) ||
		strings.Contains(string(appScript), `sessionStorage.setItem("rescue`) {
		t.Error("PWA persisted the in-memory-only rescue receipt")
	}
	for _, forbidden := range []string{
		`localStorage.setItem("external`, `sessionStorage.setItem("external`,
		`localStorage.setItem("voice`, `sessionStorage.setItem("voice`,
	} {
		if strings.Contains(string(appScript), forbidden) {
			t.Errorf("PWA persisted external voice state; found %q", forbidden)
		}
	}
	smsScript, err := webAssets.ReadFile("remote/sms-state.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`this.messages = new Map()`,
		`this.cursor = ""`,
		`message?.peer || message?.sender`,
		`clear()`,
	} {
		if !strings.Contains(string(smsScript), required) {
			t.Errorf("PWA SMS state is missing in-memory privacy condition %q", required)
		}
	}
	if strings.Contains(string(smsScript), "localStorage") || strings.Contains(string(smsScript), "sessionStorage") ||
		strings.Contains(string(smsScript), "indexedDB") {
		t.Error("PWA persisted SMS messages or sync cursor outside process memory")
	}
	for _, forbidden := range []string{
		`JSON.stringify(remoteSMSState`,
		`localStorage.setItem("sms`,
		`sessionStorage.setItem("sms`,
		`localStorage.setItem("cursor`,
		`sessionStorage.setItem("cursor`,
	} {
		if strings.Contains(string(appScript), forbidden) {
			t.Errorf("PWA persisted SMS messages or sync cursor; found %q", forbidden)
		}
	}
	mediaScript, err := webAssets.ReadFile("remote/media-client.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`iceServers: []`,
		`iceTransportPolicy: "relay"`,
		`cache: "no-store"`,
		`typ relay`,
		`direction: "sendrecv"`,
		`this.remoteTrackSeen`,
		`answer.call_generation !== this.callGeneration`,
	} {
		if !strings.Contains(string(mediaScript), required) {
			t.Errorf("PWA media client is missing fail-closed condition %q", required)
		}
	}
	externalVoiceScript, err := webAssets.ReadFile("remote/external-voice-client.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`/api/remote/v1/session`,
		`/api/remote/v2/voice/snapshot`,
		`/api/remote/v2/voice/media/ice-credentials`,
		`/api/remote/v2/voice/media/offers`,
		`/api/remote/v2/voice/calls/reconcile`,
		`iceServers: Object.freeze([])`,
		`iceTransportPolicy: "relay"`,
		`defaultTimeoutMilliseconds = 28_000`,
		`this.pagehideHandler`,
	} {
		if !strings.Contains(string(externalVoiceScript), required) {
			t.Errorf("external voice client is missing fail-closed condition %q", required)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "indexedDB",
		"/api/remote/v1/media/", "/api/remote/v1/calls/",
	} {
		if strings.Contains(string(externalVoiceScript), forbidden) {
			t.Errorf("external voice client persisted state or reused legacy API; found %q", forbidden)
		}
	}
}

func TestRemoteIncomingAnswerWaitsForMediaWithoutAnotherUserClick(t *testing.T) {
	var checks atomic.Int32
	err := waitForRemoteIncomingAnswerReadiness(
		context.Background(), 100*time.Millisecond, time.Millisecond,
		func() error { return nil },
		func() error {
			if checks.Add(1) < 3 {
				return errors.New("media is still preparing")
			}
			return nil
		},
	)
	if err != nil || checks.Load() != 3 {
		t.Fatalf("readiness wait err=%v checks=%d", err, checks.Load())
	}

	err = waitForRemoteIncomingAnswerReadiness(
		context.Background(), 100*time.Millisecond, time.Millisecond,
		func() error { return errors.New("call ended") },
		func() error { return nil },
	)
	if !errors.Is(err, errRemoteIncomingCallChanged) {
		t.Fatalf("changed call err=%v", err)
	}
}

func TestRemoteUnknownHardwareOutcomeOverridesTemporaryHTTPAcceptance(t *testing.T) {
	recorder := newRemoteResponseRecorder()
	markRemoteOperationUnknownOutcome(recorder)
	writeJSON(recorder, http.StatusAccepted, map[string]bool{"pending_confirmation": true})
	got := recorder.durableResponse()
	want := remoteOperationResponse{Status: http.StatusConflict, Code: "unknown_outcome"}
	if got != want {
		t.Fatalf("durable outcome=%+v, want %+v", got, want)
	}
	if recorder.statusCode() != http.StatusAccepted {
		t.Fatalf("handler status=%d, want temporary 202 before ledger serialization", recorder.statusCode())
	}
}
