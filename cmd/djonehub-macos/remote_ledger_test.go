package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testRemotePath = "/api/remote/v1/sms/send"

func openTestRemoteLedger(t *testing.T) (*remoteOperationLedger, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote")
	path := filepath.Join(dir, "operations.jsonl")
	ledger, err := openRemoteOperationLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger, path
}

func completeTestRemoteOperation(t *testing.T, ledger *remoteOperationLedger, operation *remoteOperation, response remoteOperationResponse) remoteOperationCompletionResult {
	t.Helper()
	result, err := ledger.Complete(operation, response)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || !result.AuditDurable || result.Response != response {
		t.Fatalf("Complete = %#v, want committed audited response %#v", result, response)
	}
	return result
}

func TestRemoteOperationLedgerPersistsAndReplaysMinimalResponse(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("request with secret phone and message"))
	begin, err := ledger.Begin("Operator@Example.COM", testRemotePath, "request-0001", hash)
	if err != nil || begin.Disposition != remoteOperationExecute {
		t.Fatalf("Begin = %#v, %v", begin, err)
	}
	want := remoteOperationResponse{Status: 202, Code: "accepted"}
	completeTestRemoteOperation(t, ledger, begin.Operation, want)
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openRemoteOperationLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := reopened.Begin("operator@example.com", testRemotePath, "request-0001", hash)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Disposition != remoteOperationReplay || replay.Response != want {
		t.Fatalf("replay = %#v, want %#v", replay, want)
	}
	if got := string(replay.Response.JSON()); got != "{\"ok\":true,\"code\":\"accepted\"}\n" {
		t.Fatalf("minimal JSON = %q", got)
	}

	for _, candidate := range []string{path, path + ".audit.jsonl"} {
		payload, err := os.ReadFile(candidate)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"Operator@Example.COM", "operator@example.com", "request-0001", "secret phone", "message"} {
			if strings.Contains(string(payload), secret) {
				t.Fatalf("%s leaked %q", candidate, secret)
			}
		}
	}
}

func TestRemoteOperationLedgerPersistsExternalDialSnapshot(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	body := []byte(`{"version":2,"number":"+8613800138000"}`)
	hash := sha256.Sum256(body)
	begin, err := ledger.Begin("dialer@example.invalid", remoteVoiceV2DialPath, "dial-restart-0001", hash)
	if err != nil || begin.Disposition != remoteOperationExecute {
		t.Fatalf("Begin = %#v, %v", begin, err)
	}
	fixture := newExternalVoiceDialRuntimeFixture(t)
	call, err := fixture.runtime.Dial(
		context.Background(), "dialer@example.invalid", "dial-restart-0001",
		externalVoiceDialRequest{Version: externalVoiceAPISchemaVersion, Number: "+8613800138000"}, hash,
	)
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes, err := json.Marshal(externalVoiceCallResponse{
		Version: externalVoiceAPISchemaVersion,
		Call:    call,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := string(append(payloadBytes, '\n'))
	want := remoteOperationResponse{Status: http.StatusOK, Code: "completed", Payload: payload}
	completeTestRemoteOperation(t, ledger, begin.Operation, want)
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openRemoteOperationLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := reopened.Begin("dialer@example.invalid", remoteVoiceV2DialPath, "dial-restart-0001", hash)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Disposition != remoteOperationReplay || replay.Response != want {
		t.Fatalf("replay = %#v, want %#v", replay, want)
	}
	if got := string(replay.Response.JSON()); got != payload {
		t.Fatalf("dial snapshot replay payload changed: got %q want %q", got, payload)
	}
}

func TestRemoteOperationLedgerRestartMarksStartedUnknown(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("same"))
	first, err := ledger.Begin("operator@example.com", testRemotePath, "request-0002", hash)
	if err != nil || first.Disposition != remoteOperationExecute {
		t.Fatalf("first Begin = %#v, %v", first, err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRemoteOperationLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Begin("operator@example.com", testRemotePath, "request-0002", hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != remoteOperationUnknownOutcome {
		t.Fatalf("Disposition = %v, want unknown outcome", got.Disposition)
	}
}

func TestRemoteOperationLedgerLookupIsReadOnlyAndPreservesRestartUnknown(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	identity := "native-v1:owner:" + remoteLedgerHash("case-sensitive-device")
	operationPath := "/api/native/v1/sms/send"
	key := "nsm_AAAAAAAAAAAAAAAAAAAAAA"
	missing, err := ledger.Lookup(identity, operationPath, key)
	if err != nil || missing.State != "not_found" {
		t.Fatalf("missing Lookup=%#v err=%v", missing, err)
	}
	recordsBefore := ledger.records
	hash := sha256.Sum256([]byte("signed native SMS body"))
	begin, err := ledger.Begin(identity, operationPath, key, hash)
	if err != nil || begin.Disposition != remoteOperationExecute {
		t.Fatalf("Begin=%#v err=%v", begin, err)
	}
	inFlight, err := ledger.Lookup(identity, operationPath, key)
	if err != nil || inFlight.State != "in_flight" || ledger.records != recordsBefore+1 {
		t.Fatalf("in-flight Lookup=%#v records=%d err=%v", inFlight, ledger.records, err)
	}
	want := remoteOperationResponse{Status: http.StatusOK, Code: "completed"}
	completeTestRemoteOperation(t, ledger, begin.Operation, want)
	completed, err := ledger.Lookup(identity, operationPath, key)
	if err != nil || completed.State != "completed" || completed.Response != want {
		t.Fatalf("completed Lookup=%#v err=%v", completed, err)
	}
	conflict, err := ledger.Lookup(identity, "/api/remote/v1/sms/send", key)
	if err != nil || conflict.State != "conflict" {
		t.Fatalf("cross-path Lookup=%#v err=%v", conflict, err)
	}

	unknownKey := "nsm_BBBBBBBBBBBBBBBBBBBBBB"
	unknownHash := sha256.Sum256([]byte("started without completion"))
	unknownBegin, err := ledger.Begin(identity, operationPath, unknownKey, unknownHash)
	if err != nil || unknownBegin.Disposition != remoteOperationExecute {
		t.Fatalf("unknown Begin=%#v err=%v", unknownBegin, err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRemoteOperationLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	unknown, err := reopened.Lookup(identity, operationPath, unknownKey)
	if err != nil || unknown.State != "unknown_outcome" {
		t.Fatalf("restart unknown Lookup=%#v err=%v", unknown, err)
	}
}

func TestRemoteOperationLedgerCompletionOperationsSyncFailureIsUnknown(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("operations sync failure"))
	begin, err := ledger.Begin("operator@example.com", testRemotePath, "request-ops-sync-fail", hash)
	if err != nil || begin.Disposition != remoteOperationExecute {
		t.Fatalf("Begin = %#v, %v", begin, err)
	}
	injected := errors.New("injected operations fsync failure")
	ledger.syncOperations = func(*os.File) error { return injected }

	result, err := ledger.Complete(begin.Operation, remoteOperationResponse{Status: 200, Code: "completed"})
	if !errors.Is(err, injected) {
		t.Fatalf("Complete error = %v, want injected fsync failure", err)
	}
	if result.Committed || result.AuditDurable {
		t.Fatalf("Complete = %#v, operations fsync failure must not be committed", result)
	}
	audit, readErr := os.ReadFile(path + ".audit.jsonl")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(audit), `"status":"completed"`) {
		t.Fatalf("audit claimed completed before authoritative fsync: %s", audit)
	}

	retry, err := ledger.Begin("operator@example.com", testRemotePath, "request-ops-sync-fail", hash)
	if err != nil || retry.Disposition != remoteOperationUnknownOutcome {
		t.Fatalf("same-key retry = %#v, %v; want unknown without execution", retry, err)
	}
	_, err = ledger.Begin("operator@example.com", testRemotePath, "request-after-ops-fail", sha256.Sum256([]byte("new")))
	if !errors.Is(err, errRemoteJournalUnavailable) {
		t.Fatalf("new key error = %v, want journal unavailable", err)
	}
}

func TestRemoteOperationLedgerAuditSyncFailureKeepsAuthorityAndRestartReplay(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("audit sync failure"))
	begin, err := ledger.Begin("operator@example.com", testRemotePath, "request-audit-sync-fail", hash)
	if err != nil || begin.Disposition != remoteOperationExecute {
		t.Fatalf("Begin = %#v, %v", begin, err)
	}

	var order []string
	ledger.syncOperations = func(file *os.File) error {
		order = append(order, "operations")
		return file.Sync()
	}
	injected := errors.New("injected audit fsync failure")
	ledger.syncAudit = func(*os.File) error {
		order = append(order, "audit")
		return injected
	}
	want := remoteOperationResponse{Status: 200, Code: "completed"}
	result, err := ledger.Complete(begin.Operation, want)
	if !errors.Is(err, errRemoteAuditUnavailable) || !errors.Is(err, injected) {
		t.Fatalf("Complete error = %v, want audit unavailable and injected failure", err)
	}
	if !result.Committed || result.AuditDurable || result.Response != want {
		t.Fatalf("Complete = %#v, want authoritative completion with failed audit", result)
	}
	if got := strings.Join(order, ","); got != "operations,audit" {
		t.Fatalf("durability order = %q, want operations,audit", got)
	}

	replay, err := ledger.Begin("operator@example.com", testRemotePath, "request-audit-sync-fail", hash)
	if err != nil || replay.Disposition != remoteOperationReplay || replay.Response != want {
		t.Fatalf("same-process replay = %#v, %v", replay, err)
	}
	_, err = ledger.Begin("operator@example.com", testRemotePath, "request-after-audit-fail", sha256.Sum256([]byte("new")))
	if !errors.Is(err, errRemoteAuditUnavailable) {
		t.Fatalf("new key error = %v, want audit unavailable", err)
	}

	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	// Model a crash in which the audit write reached the page cache but its
	// failed fsync means the completed line did not survive. The authoritative
	// operation completion did survive and must remain replayable.
	auditPath := path + ".audit.jsonl"
	auditBytes, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	var survivingAudit []string
	for _, line := range strings.Split(strings.TrimSpace(string(auditBytes)), "\n") {
		if line != "" && !strings.Contains(line, `"status":"completed"`) {
			survivingAudit = append(survivingAudit, line)
		}
	}
	if err := os.WriteFile(auditPath, []byte(strings.Join(survivingAudit, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := openRemoteOperationLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedReplay, err := reopened.Begin("operator@example.com", testRemotePath, "request-audit-sync-fail", hash)
	if err != nil || restartedReplay.Disposition != remoteOperationReplay || restartedReplay.Response != want {
		t.Fatalf("restart replay = %#v, %v", restartedReplay, err)
	}
	_, err = reopened.Begin("operator@example.com", testRemotePath, "request-after-restart-audit-fail", sha256.Sum256([]byte("new")))
	if !errors.Is(err, errRemoteAuditUnavailable) {
		t.Fatalf("restart new key error = %v, want audit unavailable", err)
	}
}

func TestRemoteOperationLedgerRejectsKeyReuseWithDifferentRequest(t *testing.T) {
	ledger, _ := openTestRemoteLedger(t)
	hash1 := sha256.Sum256([]byte("one"))
	hash2 := sha256.Sum256([]byte("two"))
	first, err := ledger.Begin("operator@example.com", testRemotePath, "request-0003", hash1)
	if err != nil {
		t.Fatal(err)
	}
	completeTestRemoteOperation(t, ledger, first.Operation, remoteOperationResponse{Status: 200, Code: "completed"})
	conflict, err := ledger.Begin("operator@example.com", testRemotePath, "request-0003", hash2)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Disposition != remoteOperationConflict {
		t.Fatalf("Disposition = %v, want conflict", conflict.Disposition)
	}
}

func TestRemoteOperationLedgerConcurrentDuplicatesWaitForOneOwner(t *testing.T) {
	ledger, _ := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("concurrent"))
	const workers = 24
	start := make(chan struct{})
	results := make(chan remoteOperationBeginResult, workers)
	errs := make(chan error, workers)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := ledger.Begin("operator@example.com", testRemotePath, "request-0004", hash)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var owner *remoteOperation
	var waiters []*remoteOperation
	for result := range results {
		switch result.Disposition {
		case remoteOperationExecute:
			calls.Add(1)
			owner = result.Operation
		case remoteOperationWait:
			waiters = append(waiters, result.Operation)
		default:
			t.Fatalf("unexpected disposition %v", result.Disposition)
		}
	}
	if calls.Load() != 1 || owner == nil || len(waiters) != workers-1 {
		t.Fatalf("owners=%d waiters=%d", calls.Load(), len(waiters))
	}
	want := remoteOperationResponse{Status: 200, Code: "completed"}
	completeTestRemoteOperation(t, ledger, owner, want)
	for _, waiter := range waiters {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got, err := ledger.Wait(ctx, waiter)
		cancel()
		if err != nil || got != want {
			t.Fatalf("Wait = %#v, %v", got, err)
		}
	}
}

func TestRemoteOperationLedgerPermissionsAndAuditSchema(t *testing.T) {
	ledger, path := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("safe"))
	begin, err := ledger.Begin("private@example.com", testRemotePath, "request-0005", hash)
	if err != nil {
		t.Fatal(err)
	}
	completeTestRemoteOperation(t, ledger, begin.Operation, remoteOperationResponse{Status: 503, Code: "modem_unavailable"})
	for _, candidate := range []string{filepath.Dir(path), path, path + ".audit.jsonl"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", candidate, got, want)
		}
	}
	audit, err := os.ReadFile(path + ".audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(audit)), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		wantFields := map[string]bool{"time": true, "login_hash": true, "path": true, "status": true, "key_hash": true, "request_hash": true}
		if len(fields) != len(wantFields) {
			t.Fatalf("audit fields = %#v", fields)
		}
		for field := range fields {
			if !wantFields[field] {
				t.Fatalf("unexpected audit field %q", field)
			}
		}
	}
	if strings.Contains(string(audit), "private@example.com") || strings.Contains(string(audit), "request-0005") {
		t.Fatal("audit leaked login or idempotency key")
	}
}

func TestRemoteOperationLedgerFailsClosedOnBadTailAndBadRecord(t *testing.T) {
	t.Run("partial tail", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "remote")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "operations.jsonl")
		if err := os.WriteFile(path, []byte(`{"version":1`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := openRemoteOperationLedger(path)
		if !errors.Is(err, errRemoteLedgerCorrupt) {
			t.Fatalf("error = %v, want corrupt", err)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "remote")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "operations.jsonl")
		if err := os.WriteFile(path, []byte("{\"version\":1,\"unexpected\":true}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := openRemoteOperationLedger(path)
		if !errors.Is(err, errRemoteLedgerCorrupt) {
			t.Fatalf("error = %v, want corrupt", err)
		}
	})
}

func TestRemoteOperationResponseRejectsSensitiveFreeFormCode(t *testing.T) {
	ledger, _ := openTestRemoteLedger(t)
	hash := sha256.Sum256([]byte("request"))
	begin, err := ledger.Begin("operator@example.com", testRemotePath, "request-0006", hash)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ledger.Complete(begin.Operation, remoteOperationResponse{Status: 200, Code: "phone_15551234567"})
	if err == nil {
		t.Fatal("sensitive free-form result code was accepted")
	}
}

func TestWriteRemoteOperationResponseIsOnlyMinimalJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	err := writeRemoteOperationResponse(recorder, remoteOperationResponse{Status: http.StatusAccepted, Code: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Body.String(); got != "{\"ok\":true,\"code\":\"accepted\"}\n" {
		t.Fatalf("body = %q", got)
	}
}
