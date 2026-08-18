package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSMSHistoryInitializeRestoresIncomingAndRecoversPending(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	seed, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	incoming, _, err := seed.IngestIncoming("synthetic-incoming-peer", "synthetic-content-with-code", "synthetic-code", timestamp)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := seed.BeginOutgoing("synthetic-outgoing-peer", "synthetic-send-attempt", timestamp.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	instance := &app{}
	if err := instance.initializeSMSStore(root); err != nil {
		t.Fatal(err)
	}
	defer instance.closeSMSStore()
	if len(instance.sms) != 1 {
		t.Fatalf("legacy cache contains %d messages, want only the incoming message", len(instance.sms))
	}
	if instance.sms[0].Sender != incoming.Peer || instance.sms[0].Content != incoming.Content || instance.sms[0].Code != incoming.Code {
		t.Fatalf("incoming legacy projection is invalid: %+v", instance.sms[0])
	}
	for _, item := range instance.sms {
		if item.Sender == pending.Peer || item.Content == pending.Content {
			t.Fatalf("outgoing message leaked into incoming legacy cache: %+v", item)
		}
	}
	stored, err := instance.smsStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(stored))
	for _, item := range stored {
		statuses[item.ID] = item.Status
	}
	if statuses[pending.ID] != "unknown" {
		t.Fatalf("restart recovery status=%q, want unknown", statuses[pending.ID])
	}
	if statuses[incoming.ID] != "received" {
		t.Fatalf("incoming status=%q, want received", statuses[incoming.ID])
	}
}

func TestSMSHistoryIngestPersistsBeforeCacheAndSurvivesRestart(t *testing.T) {
	const syntheticCode = "654321"
	root := filepath.Join(t.TempDir(), "sms")
	instance := &app{}
	if err := instance.initializeSMSStore(root); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 8, 14, 4, 5, 6, 0, time.UTC)
	newCount, total, err := instance.ingestIncomingSMS([]receivedSMS{{
		Sender: "synthetic-peer", Content: "synthetic verification code " + syntheticCode, Timestamp: timestamp,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if newCount != 1 || total != 1 || len(instance.sms) != 1 || instance.sms[0].Code != syntheticCode {
		t.Fatalf("unexpected cache result: new=%d total=%d cache=%+v", newCount, total, instance.sms)
	}
	persisted, err := instance.smsStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Peer != "synthetic-peer" || persisted[0].Code != syntheticCode {
		t.Fatalf("message was not durably stored before projection: %+v", persisted)
	}
	instance.closeSMSStore()

	restarted := &app{}
	if err := restarted.initializeSMSStore(root); err != nil {
		t.Fatal(err)
	}
	defer restarted.closeSMSStore()
	if len(restarted.sms) != 1 || restarted.sms[0].Sender != "synthetic-peer" || restarted.sms[0].Content != "synthetic verification code "+syntheticCode {
		t.Fatalf("durable incoming message was not restored: %+v", restarted.sms)
	}
}

func TestSMSHistoryUnavailableStoreNeverMutatesCache(t *testing.T) {
	timestamp := time.Date(2026, 8, 14, 7, 8, 9, 0, time.UTC)
	withoutStore := &app{}
	if _, _, err := withoutStore.ingestIncomingSMS([]receivedSMS{{Sender: "peer", Content: "body", Timestamp: timestamp}}); err == nil {
		t.Fatal("ingest without a durable store unexpectedly succeeded")
	}
	if len(withoutStore.sms) != 0 {
		t.Fatalf("unpersisted message entered cache: %+v", withoutStore.sms)
	}
	if available, code := withoutStore.smsStoreStatus(); available || code != "sms_store_unavailable" {
		t.Fatalf("store status=(%v,%q), want unavailable", available, code)
	}

	store, err := openSMSStore(filepath.Join(t.TempDir(), "sms"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.mu.Lock()
	store.degraded = true
	store.mu.Unlock()
	degraded := &app{smsStore: store}
	if _, _, err := degraded.ingestIncomingSMS([]receivedSMS{{Sender: "peer", Content: "body", Timestamp: timestamp}}); !errors.Is(err, ErrSMSStoreDegraded) {
		t.Fatalf("degraded ingest error=%v, want ErrSMSStoreDegraded", err)
	}
	if len(degraded.sms) != 0 {
		t.Fatalf("message entered cache after persistence failure: %+v", degraded.sms)
	}
}

func TestSMSHistoryRejectsRelativeStorePath(t *testing.T) {
	instance := &app{}
	if err := instance.initializeSMSStore("relative/sms"); err == nil {
		t.Fatal("relative SMS store path unexpectedly succeeded")
	}
	if available, code := instance.smsStoreStatus(); available || code != "sms_store_unavailable" {
		t.Fatalf("relative-path store status=(%v,%q), want unavailable", available, code)
	}
}

func TestSMSHistoryOutgoingBeginAndUpdate(t *testing.T) {
	instance := &app{}
	if err := instance.initializeSMSStore(filepath.Join(t.TempDir(), "sms")); err != nil {
		t.Fatal(err)
	}
	defer instance.closeSMSStore()
	timestamp := time.Date(2026, 8, 14, 10, 11, 12, 0, time.UTC)
	pending, err := instance.beginOutgoingSMS("peer", "outgoing body", timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Direction != "outgoing" || pending.Status != "pending" {
		t.Fatalf("unexpected begin state: %+v", pending)
	}
	if len(instance.sms) != 0 {
		t.Fatalf("outgoing message entered legacy incoming cache: %+v", instance.sms)
	}
	submitted, err := instance.updateOutgoingSMS(pending.ID, "submitted", 2)
	if err != nil {
		t.Fatal(err)
	}
	if submitted.ID != pending.ID || submitted.Status != "submitted" || submitted.Segments != 2 {
		t.Fatalf("unexpected submitted state: %+v", submitted)
	}
	items, err := instance.smsStore.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != pending.ID || items[0].Status != "submitted" || items[0].Segments != 2 {
		t.Fatalf("outgoing state was not persisted: %+v", items)
	}
}

func TestSMSLocalSendPersistsBeforeHardwareAndRecordsSubmitted(t *testing.T) {
	instance := &app{}
	if err := instance.initializeSMSStore(filepath.Join(t.TempDir(), "sms")); err != nil {
		t.Fatal(err)
	}
	defer instance.closeSMSStore()
	var calls atomic.Int32
	instance.sendTextSMSOverride = func(phone, message string) (int, error) {
		calls.Add(1)
		if phone != "synthetic-recipient-a" || message != "synthetic body" {
			t.Fatalf("unexpected hardware arguments phone=%q message=%q", phone, message)
		}
		items, err := instance.smsStoreSnapshot().List(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Direction != "outgoing" || items[0].Status != "pending" {
			t.Fatalf("hardware ran before durable pending state: %+v", items)
		}
		return 2, nil
	}

	request := httptest.NewRequest(http.MethodPost, "/api/sms/send", strings.NewReader(`{"phone":"synthetic-recipient-a","message":"synthetic body"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	instance.sendSMS(response, request)
	if response.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("send response=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
	items, err := instance.smsStoreSnapshot().List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "submitted" || items[0].Segments != 2 {
		t.Fatalf("submitted state was not persisted: %+v", items)
	}
}

func TestSMSLocalSendUnknownOutcomeIsDurableAndNeverStartsWithoutStore(t *testing.T) {
	withoutStore := &app{}
	var calls atomic.Int32
	withoutStore.sendTextSMSOverride = func(string, string) (int, error) {
		calls.Add(1)
		return 1, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sms/send", strings.NewReader(`{"phone":"synthetic-recipient-b","message":"synthetic body"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	withoutStore.sendSMS(response, request)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("missing-store response=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}

	root := filepath.Join(t.TempDir(), "sms")
	instance := &app{}
	if err := instance.initializeSMSStore(root); err != nil {
		t.Fatal(err)
	}
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		calls.Add(1)
		return 1, errors.New("synthetic transport timeout")
	}
	request = httptest.NewRequest(http.MethodPost, "/api/sms/send", strings.NewReader(`{"phone":"synthetic-recipient-b","message":"synthetic body"}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	instance.sendSMS(response, request)
	if response.Code != http.StatusConflict || calls.Load() != 1 || !strings.Contains(response.Body.String(), `"code":"unknown_outcome"`) {
		t.Fatalf("ambiguous send response=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
	items, err := instance.smsStoreSnapshot().List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "unknown" || items[0].Segments != 1 {
		t.Fatalf("unknown outcome was not durably recorded: %+v", items)
	}
	instance.closeSMSStore()

	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, err = reopened.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "unknown" {
		t.Fatalf("unknown outcome did not survive restart: %+v", items)
	}
}

func TestSMSLocalSendTerminalPersistenceFailureDisablesFurtherHardware(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	instance := &app{}
	if err := instance.initializeSMSStore(root); err != nil {
		t.Fatal(err)
	}
	store := instance.smsStoreSnapshot()
	if store == nil {
		t.Fatal("SMS store was not initialized")
	}
	var calls atomic.Int32
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		calls.Add(1)
		store.mu.Lock()
		store.eventsDir = filepath.Join(t.TempDir(), "missing-events")
		store.mu.Unlock()
		return 1, nil
	}

	request := httptest.NewRequest(http.MethodPost, "/api/sms/send", strings.NewReader(`{"phone":"synthetic-recipient-c","message":"synthetic body"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	instance.sendSMS(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"unknown_outcome"`) {
		t.Fatalf("terminal persistence failure response=%d body=%s", response.Code, response.Body.String())
	}
	if available, code := instance.smsStoreStatus(); available || code != "sms_store_unavailable" {
		t.Fatalf("store status=(%v,%q), want fail-closed", available, code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/sms/send", strings.NewReader(`{"phone":"synthetic-recipient-d","message":"synthetic body"}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	instance.sendSMS(response, request)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("second send response=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
	instance.closeSMSStore()

	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RecoverPending(); err != nil {
		t.Fatal(err)
	}
	items, err := reopened.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "unknown" {
		t.Fatalf("restart did not conservatively recover pending send: %+v", items)
	}
}

func TestSMSTransportSerializesConcurrentMultipartAttempts(t *testing.T) {
	instance := &app{}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	instance.sendTextSMSOverride = func(string, string) (int, error) {
		entered <- struct{}{}
		<-release
		return 1, nil
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = instance.sendTextSMS("peer-one", "body-one")
		done <- struct{}{}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first SMS attempt did not reach the transport")
	}
	go func() {
		_, _ = instance.sendTextSMS("peer-two", "body-two")
		done <- struct{}{}
	}()
	select {
	case <-entered:
		t.Fatal("second SMS attempt interleaved with the first multipart transport")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serialized SMS attempt did not complete")
		}
	}
}
