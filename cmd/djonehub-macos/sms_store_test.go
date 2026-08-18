package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSMSStoreLifecycleRestartAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 8, 14, 8, 9, 10, 123, time.FixedZone("test", 8*60*60))
	incoming, created, err := store.IngestIncoming("synthetic-peer-a", "synthetic-content-a", "synthetic-code-a", timestamp)
	if err != nil || !created {
		t.Fatalf("ingest: created=%v err=%v", created, err)
	}
	if !strings.HasPrefix(incoming.ID, "msg_") || !strings.HasPrefix(incoming.EventID, "evt_") {
		t.Fatalf("IDs are not opaque: %+v", incoming)
	}
	duplicate, created, err := store.IngestIncoming("synthetic-peer-a", "synthetic-content-a", "synthetic-code-a", timestamp)
	if err != nil || created || duplicate.ID != incoming.ID {
		t.Fatalf("dedupe failed: created=%v duplicate=%+v err=%v", created, duplicate, err)
	}
	outgoing, err := store.BeginOutgoing("synthetic-peer-b", "synthetic-content-b", timestamp.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if outgoing.Status != "pending" || outgoing.Direction != "outgoing" {
		t.Fatalf("unexpected outgoing state: %+v", outgoing)
	}
	submitted, err := store.UpdateStatus(outgoing.ID, "submitted", 2)
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Status != "submitted" || submitted.Segments != 2 || submitted.EventID == outgoing.EventID {
		t.Fatalf("unexpected submitted state: %+v", submitted)
	}
	if _, err := store.UpdateStatus(outgoing.ID, "failed", 2); err == nil {
		t.Fatal("terminal status transition unexpectedly succeeded")
	}
	assertMode(t, root, 0o700)
	assertMode(t, filepath.Join(root, "events"), 0o700)
	assertMode(t, filepath.Join(root, "meta.json"), 0o600)
	entries, err := os.ReadDir(filepath.Join(root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("event count=%d, want 3", len(entries))
	}
	for _, entry := range entries {
		assertMode(t, filepath.Join(root, "events", entry.Name()), 0o600)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, err := reopened.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != outgoing.ID || items[0].Status != "submitted" || items[1].ID != incoming.ID {
		t.Fatalf("unexpected rebuilt state: %+v", items)
	}
	duplicate, created, err = reopened.IngestIncoming("synthetic-peer-a", "synthetic-content-a", "synthetic-code-a", timestamp)
	if err != nil || created || duplicate.ID != incoming.ID {
		t.Fatalf("restart dedupe failed: created=%v duplicate=%+v err=%v", created, duplicate, err)
	}
}

func TestSMSStoreSyncCursorTamperAndEpochReset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	first, _, err := store.IngestIncoming("peer-a", "first", "", base)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := store.Sync("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrap.Bootstrap || len(bootstrap.Messages) != 1 || bootstrap.Messages[0].ID != first.ID || bootstrap.Cursor == "" {
		t.Fatalf("unexpected bootstrap: %+v", bootstrap)
	}
	second, err := store.BeginOutgoing("peer-b", "second", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateStatus(second.ID, "unknown", 0); err != nil {
		t.Fatal(err)
	}
	delta1, err := store.Sync(bootstrap.Cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if delta1.Bootstrap || !delta1.HasMore || len(delta1.Messages) != 1 || delta1.Messages[0].Status != "pending" {
		t.Fatalf("unexpected first delta: %+v", delta1)
	}
	delta2, err := store.Sync(delta1.Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if delta2.HasMore || len(delta2.Messages) != 1 || delta2.Messages[0].Status != "unknown" {
		t.Fatalf("unexpected second delta: %+v", delta2)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(bootstrap.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	decoded[len(decoded)-1] ^= 0xff
	tampered := base64.RawURLEncoding.EncodeToString(decoded)
	if _, err := store.Sync(tampered, 10); !errors.Is(err, ErrSMSCursorInvalid) {
		t.Fatalf("tampered cursor error=%v", err)
	}
	epochTampered, err := base64.RawURLEncoding.DecodeString(bootstrap.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	epochTampered[1] ^= 0xff
	if _, err := store.Sync(base64.RawURLEncoding.EncodeToString(epochTampered), 10); !errors.Is(err, ErrSMSCursorInvalid) {
		t.Fatalf("unsigned epoch tamper error=%v, want invalid", err)
	}
	authenticatedEpochMismatch := append([]byte(nil), epochTampered...)
	mac := hmac.New(sha256.New, store.cursorKey[:])
	_, _ = mac.Write(authenticatedEpochMismatch[:34])
	copy(authenticatedEpochMismatch[34:], mac.Sum(nil))
	if _, err := store.Sync(base64.RawURLEncoding.EncodeToString(authenticatedEpochMismatch), 10); !errors.Is(err, ErrSMSCursorResetRequired) {
		t.Fatalf("authenticated epoch mismatch error=%v, want reset required", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if _, err := rebuilt.Sync(bootstrap.Cursor, 10); !errors.Is(err, ErrSMSCursorInvalid) {
		t.Fatalf("old-store cursor error=%v, want invalid", err)
	}
}

func TestSMSStoreBootstrapPagesWithoutSkippingHistory(t *testing.T) {
	store, err := openSMSStore(filepath.Join(t.TempDir(), "sms"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 3; i++ {
		if _, _, err := store.IngestIncoming(fmt.Sprintf("peer-%d", i), fmt.Sprintf("message-%d", i), "", time.Unix(int64(i+1), 0)); err != nil {
			t.Fatal(err)
		}
	}
	firstPage, err := store.Sync("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !firstPage.Bootstrap || !firstPage.HasMore || len(firstPage.Messages) != 2 {
		t.Fatalf("first bootstrap page is invalid: %+v", firstPage)
	}
	if firstPage.Messages[0].Content != "message-2" || firstPage.Messages[1].Content != "message-1" {
		t.Fatalf("first bootstrap page order is invalid: %+v", firstPage.Messages)
	}
	if _, _, err := store.IngestIncoming("new-peer", "post-high-water", "", time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	secondPage, err := store.Sync(firstPage.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !secondPage.Bootstrap || secondPage.HasMore || len(secondPage.Messages) != 1 || secondPage.Messages[0].Content != "message-0" {
		t.Fatalf("second bootstrap page skipped or mixed history: %+v", secondPage)
	}
	delta, err := store.Sync(secondPage.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Bootstrap || delta.HasMore || len(delta.Messages) != 1 || delta.Messages[0].Content != "post-high-water" {
		t.Fatalf("post-bootstrap delta is invalid: %+v", delta)
	}
}

func TestSMSStoreDirectorySyncFailureIsFailClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	store.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	if _, _, err := store.IngestIncoming("peer", "body", "", time.Now()); !errors.Is(err, ErrSMSStoreCommitUncertain) {
		t.Fatalf("first write error=%v, want commit uncertain", err)
	}
	if _, _, err := store.IngestIncoming("peer-2", "body-2", "", time.Now()); !errors.Is(err, ErrSMSStoreDegraded) {
		t.Fatalf("second write error=%v, want degraded", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "00000000000000000001.evt" {
		t.Fatalf("uncertain commit files=%v", entries)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, err := reopened.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "body" {
		t.Fatalf("uncertain committed event was not recovered: %+v", items)
	}
	if _, _, err := reopened.IngestIncoming("peer-2", "body-2", "", time.Now()); err != nil {
		t.Fatalf("reopened store did not advance past committed sequence: %v", err)
	}
}

func TestSMSStoreRecoverPendingAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	firstPending, err := store.BeginOutgoing("peer-1", "pending-1", base)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := store.BeginOutgoing("peer-2", "submitted", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateStatus(submitted.ID, "submitted", 1); err != nil {
		t.Fatal(err)
	}
	secondPending, err := store.BeginOutgoing("peer-3", "pending-2", base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.RecoverPending(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.events) != 6 {
		t.Fatalf("event count=%d, want 6", len(reopened.events))
	}
	if reopened.events[4].Message.ID != firstPending.ID || reopened.events[5].Message.ID != secondPending.ID {
		t.Fatalf("pending recovery order is not stable: %+v %+v", reopened.events[4].Message, reopened.events[5].Message)
	}
	items, err := reopened.List(10)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(items))
	for _, item := range items {
		statuses[item.ID] = item.Status
	}
	if statuses[firstPending.ID] != "unknown" || statuses[secondPending.ID] != "unknown" {
		t.Fatalf("pending messages were guessed instead of marked unknown: %+v", statuses)
	}
	if statuses[submitted.ID] != "submitted" {
		t.Fatalf("submitted message changed during recovery: %+v", statuses)
	}
	eventCount := len(reopened.events)
	if err := reopened.RecoverPending(); err != nil {
		t.Fatal(err)
	}
	if len(reopened.events) != eventCount {
		t.Fatalf("repeated recovery appended events: got %d, want %d", len(reopened.events), eventCount)
	}
}

func TestSMSStoreRecoverPendingFailureIsFailClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginOutgoing("peer", "pending", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	if err := reopened.RecoverPending(); !errors.Is(err, ErrSMSStoreCommitUncertain) {
		t.Fatalf("recovery error=%v, want commit uncertain", err)
	}
	if err := reopened.RecoverPending(); !errors.Is(err, ErrSMSStoreDegraded) {
		t.Fatalf("second recovery error=%v, want degraded", err)
	}
}

func TestSMSStoreConcurrentIngest(t *testing.T) {
	store, err := openSMSStore(filepath.Join(t.TempDir(), "sms"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const count = 64
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, created, err := store.IngestIncoming(fmt.Sprintf("peer-%d", i), fmt.Sprintf("body-%d", i), "", time.Unix(int64(i+1), 0))
			if err != nil {
				errs <- err
				return
			}
			if !created {
				errs <- fmt.Errorf("message %d was unexpectedly deduplicated", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	items, err := store.List(count)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != count {
		t.Fatalf("stored %d messages, want %d", len(items), count)
	}
}

func TestSMSStoreRejectsSymlinksAndSecondOpen(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(parent, "root-link")
	if err := os.Symlink(target, rootLink); err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(rootLink); err == nil {
		t.Fatal("symlink root unexpectedly accepted")
	}

	root := filepath.Join(parent, "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(root); err == nil {
		t.Fatal("second concurrent open unexpectedly succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(root, "events")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "events")); err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(root); err == nil {
		t.Fatal("symlink event directory unexpectedly accepted")
	}
}

func TestSMSStoreIgnoresPartialTempAndRejectsSymlinkEvent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestIncoming("peer", "body", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "events", ".tmp-partial"), []byte(`{"partial":`), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSMSStore(root)
	if err != nil {
		t.Fatalf("partial temp blocked recovery: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	eventPath := filepath.Join(root, "events", "00000000000000000001.evt")
	if err := os.Remove(eventPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "meta.json"), eventPath); err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(root); err == nil {
		t.Fatal("symlink event unexpectedly accepted")
	}
}

func TestSMSStoreRejectsUnsafeTemporaryArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	tempLink := filepath.Join(root, "events", ".tmp-link")
	if err := os.Symlink(target, tempLink); err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(root); err == nil {
		t.Fatal("temporary symlink unexpectedly accepted")
	}
	if err := os.Remove(tempLink); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(root, "events", ".tmp-directory")
	if err := os.Mkdir(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(root); err == nil {
		t.Fatal("temporary directory unexpectedly accepted")
	}
}

func TestSMSStoreRejectsTamperedEvent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sms")
	store, err := openSMSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestIncoming("synthetic-peer", "synthetic-private-content", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "events", "00000000000000000001.evt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "synthetic-private-content", "synthetic-changed-content", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSMSStore(root); err == nil || !strings.Contains(err.Error(), "MAC") {
		t.Fatalf("tampered event error=%v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%#o, want %#o", path, got, want)
	}
}
