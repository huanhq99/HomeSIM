package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newExternalVoiceRecoveryTestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "dedicated-recovery", "slot.json")
}

func openExternalVoiceRecoveryTestStore(t *testing.T) (*externalVoiceRecoveryStore, string) {
	t.Helper()
	path := newExternalVoiceRecoveryTestPath(t)
	store, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func externalVoiceRecoveryTestArm(seed string) externalVoiceRecoveryArmRequest {
	return externalVoiceRecoveryArmRequest{
		RecoveryID:       "svr_" + strings.Repeat(seed, 32),
		Kind:             externalVoiceRecoveryAnswerIncoming,
		OwnerHash:        externalVoiceRecoveryHash("owner-" + seed),
		CommandKeyHash:   externalVoiceRecoveryHash("command-" + seed),
		RequestHash:      externalVoiceRecoveryHash("request-" + seed),
		PublicCallID:     "public-call-" + seed,
		Generation:       7,
		ExpectedRevision: 11,
		PublicMediaLease: "public-media-lease-" + seed,
		Adapter:          "asterisk",
		GatewayID:        "synthetic-home-gateway",
		ProviderToken:    []byte("opaque-provider-correlation-" + seed),
	}
}

func TestExternalVoiceRecoveryStoreArmResolveReopenAndStableSecret(t *testing.T) {
	store, path := openExternalVoiceRecoveryTestStore(t)
	initial, err := store.Snapshot()
	if err != nil || initial.State != externalVoiceRecoveryClear || initial.StoreGeneration != 1 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	secret, err := store.ProviderSecret()
	if err != nil || secret == [externalVoiceRecoveryRootSecretBytes]byte{} {
		t.Fatalf("provider secret is unavailable: zero=%t err=%v", secret == [externalVoiceRecoveryRootSecretBytes]byte{}, err)
	}
	request := externalVoiceRecoveryTestArm("A")
	armed, err := store.Arm(request)
	if err != nil || armed.State != externalVoiceRecoveryArmed || armed.StoreGeneration != 2 ||
		!externalVoiceRecoverySnapshotMatchesRequest(armed, request) {
		t.Fatalf("armed=%+v err=%v", armed, err)
	}
	armed.ProviderToken[0] ^= 0xff
	snapshot, err := store.Snapshot()
	if err != nil || !bytes.Equal(snapshot.ProviderToken, request.ProviderToken) {
		t.Fatalf("snapshot alias or error: %+v err=%v", snapshot, err)
	}
	resolved, err := store.Resolve(request.RecoveryID, externalVoiceRecoveryApplied)
	if err != nil || resolved.State != externalVoiceRecoveryResolved || resolved.StoreGeneration != 3 ||
		resolved.RecoveryIDHash != externalVoiceRecoveryHash(request.RecoveryID) ||
		len(resolved.ProviderToken) != 0 {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	if _, err := store.Arm(request); !errors.Is(err, errExternalVoiceRecoveryStoreStale) {
		t.Fatalf("resolved recovery ID was reusable: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedSecret, err := reopened.ProviderSecret()
	if err != nil || reopenedSecret != secret {
		t.Fatalf("provider secret changed across reopen: err=%v", err)
	}
	reopenedSnapshot, err := reopened.Snapshot()
	if err != nil || reopenedSnapshot.State != externalVoiceRecoveryResolved ||
		reopenedSnapshot.StoreGeneration != 3 || reopenedSnapshot.Resolution != externalVoiceRecoveryApplied {
		t.Fatalf("reopened=%+v err=%v", reopenedSnapshot, err)
	}
	next := externalVoiceRecoveryTestArm("B")
	next.Kind = externalVoiceRecoveryEndActive
	nextArmed, err := reopened.Arm(next)
	if err != nil || nextArmed.StoreGeneration != 4 || nextArmed.Kind != externalVoiceRecoveryEndActive {
		t.Fatalf("next armed=%+v err=%v", nextArmed, err)
	}
}

func TestExternalVoiceRecoveryStorePermissionsLockAndDedicatedInitialization(t *testing.T) {
	path := newExternalVoiceRecoveryTestPath(t)
	store, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(path), 0o700},
		{path, 0o600},
		{path + ".meta", 0o600},
		{path + ".lock", 0o600},
	} {
		info, statErr := os.Stat(candidate.path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != candidate.mode {
			t.Fatalf("%s mode=%o want=%o", candidate.path, info.Mode().Perm(), candidate.mode)
		}
	}
	if _, err := openExternalVoiceRecoveryStore(path); err == nil {
		t.Fatal("second process-local recovery store lock was accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatalf("lock did not release: %v", err)
	}
	_ = reopened.Close()

	existingDir := filepath.Join(t.TempDir(), "existing-empty")
	if err := os.Mkdir(existingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openExternalVoiceRecoveryStore(filepath.Join(existingDir, "slot.json")); !errors.Is(err, errExternalVoiceRecoveryStoreCorrupt) {
		t.Fatalf("existing empty directory initialized: %v", err)
	}
}

func TestExternalVoiceRecoveryStoreRejectsMissingTamperedAndUnsafeFiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "missing slot", mutate: func(t *testing.T, path string) { t.Helper(); mustRemoveRecoveryTestFile(t, path) }},
		{name: "missing meta", mutate: func(t *testing.T, path string) { t.Helper(); mustRemoveRecoveryTestFile(t, path+".meta") }},
		{name: "truncated slot", mutate: func(t *testing.T, path string) {
			t.Helper()
			mustWriteRecoveryTestFile(t, path, []byte(`{"version":1}`))
		}},
		{name: "oversized slot", mutate: func(t *testing.T, path string) {
			t.Helper()
			mustWriteRecoveryTestFile(t, path, bytes.Repeat([]byte{'x'}, externalVoiceRecoveryStoreMaxBytes+1))
		}},
		{name: "tampered slot", mutate: func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`"slot_generation":1`), []byte(`"slot_generation":9`), 1)
			mustWriteRecoveryTestFile(t, path, data)
		}},
		{name: "tampered meta", mutate: func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path + ".meta")
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`"store_id":"svs_`), []byte(`"store_id":"svs_X`), 1)
			mustWriteRecoveryTestFile(t, path+".meta", data)
		}},
		{name: "unknown field", mutate: func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`{"version":1`), []byte(`{"version":1,"unexpected":true`), 1)
			mustWriteRecoveryTestFile(t, path, data)
		}},
		{name: "duplicate field", mutate: func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`{"version":1`), []byte(`{"version":1,"version":1`), 1)
			mustWriteRecoveryTestFile(t, path, data)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := openExternalVoiceRecoveryTestStore(t)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path)
			if _, err := openExternalVoiceRecoveryStore(path); err == nil {
				t.Fatal("unsafe or corrupt recovery store was accepted")
			}
		})
	}
}

func TestExternalVoiceRecoveryStoreRejectsSymlinkNonRegularHardlinkAndUnsafeTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix link and mode semantics")
	}
	t.Run("slot symlink", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		target := path + ".meta"
		mustRemoveRecoveryTestFile(t, path)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("symlink slot was accepted")
		}
	})
	t.Run("slot directory", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		mustRemoveRecoveryTestFile(t, path)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("directory slot was accepted")
		}
	})
	t.Run("slot hardlink", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		if err := os.Link(path, path+".second-link"); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("multi-link slot was accepted")
		}
	})
	t.Run("metadata symlink", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		mustRemoveRecoveryTestFile(t, path+".meta")
		if err := os.Symlink(path, path+".meta"); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("symlink metadata was accepted")
		}
	})
	t.Run("unsafe slot mode", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("group-readable slot was accepted")
		}
	})
	t.Run("unsafe directory mode", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		if err := os.Chmod(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("group-accessible recovery directory was accepted")
		}
	})
	t.Run("lock hardlink", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		if err := os.Link(path+".lock", path+".lock.second-link"); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("multi-link lock was accepted")
		}
	})
	t.Run("unsafe temp", func(t *testing.T) {
		store, path := openExternalVoiceRecoveryTestStore(t)
		_ = store.Close()
		if err := os.Symlink(path, path+".tmp"); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(path); err == nil {
			t.Fatal("symlink temporary file was accepted")
		}
	})
	t.Run("directory symlink", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "linked")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		if _, err := openExternalVoiceRecoveryStore(filepath.Join(linkDir, "slot.json")); err == nil {
			t.Fatal("symlink directory was accepted")
		}
	})
}

func TestExternalVoiceRecoveryStoreCleansSafeBoundedTemp(t *testing.T) {
	store, path := openExternalVoiceRecoveryTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tempPath := range []string{path + ".tmp", path + ".meta.tmp"} {
		mustWriteRecoveryTestFile(t, tempPath, []byte("stale-but-bounded\n"))
	}
	reopened, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, tempPath := range []string{path + ".tmp", path + ".meta.tmp"} {
		if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale temp survived: %s err=%v", tempPath, err)
		}
	}
}

func TestExternalVoiceRecoveryStoreFaultInjectionPreservesFailClosedAuthority(t *testing.T) {
	injected := errors.New("injected persistence failure")
	tests := []struct {
		name      string
		override  func(*externalVoiceRecoveryStore)
		wantArmed bool
	}{
		{name: "temporary create", override: func(store *externalVoiceRecoveryStore) {
			store.io.createTemp = func(string) (*os.File, error) { return nil, injected }
		}},
		{name: "write", override: func(store *externalVoiceRecoveryStore) {
			store.io.write = func(*os.File, []byte) (int, error) { return 0, injected }
		}},
		{name: "short write", override: func(store *externalVoiceRecoveryStore) {
			store.io.write = func(file *os.File, data []byte) (int, error) {
				return file.Write(data[:len(data)-1])
			}
		}},
		{name: "file sync", override: func(store *externalVoiceRecoveryStore) {
			store.io.syncFile = func(*os.File) error { return injected }
		}},
		{name: "close", override: func(store *externalVoiceRecoveryStore) {
			store.io.closeFile = func(file *os.File) error {
				_ = file.Close()
				return injected
			}
		}},
		{name: "rename", override: func(store *externalVoiceRecoveryStore) {
			store.io.rename = func(string, string) error { return injected }
		}},
		{name: "directory sync", wantArmed: true, override: func(store *externalVoiceRecoveryStore) {
			store.io.syncDir = func(string) error { return injected }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, path := openExternalVoiceRecoveryTestStore(t)
			test.override(store)
			if _, err := store.Arm(externalVoiceRecoveryTestArm("F")); !errors.Is(err, errExternalVoiceRecoveryStoreFailed) {
				t.Fatalf("Arm error=%v", err)
			}
			if _, err := store.Snapshot(); !errors.Is(err, errExternalVoiceRecoveryStoreFailed) {
				t.Fatalf("degraded Snapshot error=%v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openExternalVoiceRecoveryStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			snapshot, err := reopened.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			want := externalVoiceRecoveryClear
			if test.wantArmed {
				want = externalVoiceRecoveryArmed
			}
			if snapshot.State != want {
				t.Fatalf("reopened state=%q want=%q", snapshot.State, want)
			}
		})
	}
}

func TestExternalVoiceRecoveryStoreInitializationFaultsFailClosed(t *testing.T) {
	injected := errors.New("injected initialization failure")
	for _, test := range []struct {
		name       string
		override   externalVoiceRecoveryStoreIO
		wantUsable bool
	}{
		{name: "metadata write", override: externalVoiceRecoveryStoreIO{
			write: func(*os.File, []byte) (int, error) { return 0, injected },
		}},
		{name: "slot write", override: func() externalVoiceRecoveryStoreIO {
			calls := 0
			return externalVoiceRecoveryStoreIO{write: func(file *os.File, data []byte) (int, error) {
				calls++
				if calls == 2 {
					return 0, injected
				}
				return file.Write(data)
			}}
		}()},
		{name: "metadata directory sync", override: func() externalVoiceRecoveryStoreIO {
			calls := 0
			return externalVoiceRecoveryStoreIO{syncDir: func(string) error {
				calls++
				if calls == 1 {
					return injected
				}
				return nil
			}}
		}()},
		{name: "slot directory sync", wantUsable: true, override: func() externalVoiceRecoveryStoreIO {
			calls := 0
			return externalVoiceRecoveryStoreIO{syncDir: func(string) error {
				calls++
				if calls == 2 {
					return injected
				}
				return nil
			}}
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := newExternalVoiceRecoveryTestPath(t)
			if store, err := openExternalVoiceRecoveryStoreWithIO(path, test.override); err == nil {
				_ = store.Close()
				t.Fatal("faulted initialization succeeded")
			}
			reopened, err := openExternalVoiceRecoveryStore(path)
			if test.wantUsable {
				if err != nil {
					t.Fatalf("fully renamed store did not reopen: %v", err)
				}
				defer reopened.Close()
				snapshot, snapshotErr := reopened.Snapshot()
				if snapshotErr != nil || snapshot.State != externalVoiceRecoveryClear {
					t.Fatalf("reopened=%+v err=%v", snapshot, snapshotErr)
				}
				return
			}
			if !errors.Is(err, errExternalVoiceRecoveryStoreCorrupt) {
				t.Fatalf("partial initialization did not fail corrupt: %v", err)
			}
		})
	}
}

func TestExternalVoiceRecoveryStoreResolveFailureLeavesArmed(t *testing.T) {
	store, path := openExternalVoiceRecoveryTestStore(t)
	request := externalVoiceRecoveryTestArm("R")
	if _, err := store.Arm(request); err != nil {
		t.Fatal(err)
	}
	store.io.syncFile = func(*os.File) error { return errors.New("injected resolve sync failure") }
	if _, err := store.Resolve(request.RecoveryID, externalVoiceRecoveryApplied); !errors.Is(err, errExternalVoiceRecoveryStoreFailed) {
		t.Fatalf("Resolve error=%v", err)
	}
	_ = store.Close()
	reopened, err := openExternalVoiceRecoveryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err := reopened.Snapshot()
	if err != nil || snapshot.State != externalVoiceRecoveryArmed || snapshot.RecoveryID != request.RecoveryID {
		t.Fatalf("reopened armed=%+v err=%v", snapshot, err)
	}
}

func TestExternalVoiceRecoveryStoreRejectsAuthenticatedProviderTokenTampering(t *testing.T) {
	for _, test := range []struct {
		name  string
		index func([]byte) int
	}{
		{name: "nonce", index: func([]byte) int { return 0 }},
		{name: "ciphertext", index: func(value []byte) int { return len(value) - 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path := openExternalVoiceRecoveryTestStore(t)
			request := externalVoiceRecoveryTestArm("T")
			request.ProviderToken = []byte(`{"channel_name":"PJSIP/+15555550123","sdp":"v=0 private"}`)
			if _, err := store.Arm(request); err != nil {
				t.Fatal(err)
			}
			secret, err := store.ProviderSecret()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record externalVoiceRecoveryDiskRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			sealed, err := base64.RawURLEncoding.DecodeString(record.ProviderToken)
			if err != nil {
				t.Fatal(err)
			}
			sealed[test.index(sealed)] ^= 0x80
			record.ProviderToken = base64.RawURLEncoding.EncodeToString(sealed)
			record.MAC = externalVoiceRecoveryRecordMAC(secret, record)
			payload, err := json.Marshal(externalVoiceRecoveryDiskRecordJSON(record))
			if err != nil {
				t.Fatal(err)
			}
			mustWriteRecoveryTestFile(t, path, append(payload, '\n'))
			if _, err := openExternalVoiceRecoveryStore(path); !errors.Is(err, errExternalVoiceRecoveryStoreCorrupt) {
				t.Fatalf("authenticated %s tampering accepted: %v", test.name, err)
			}
		})
	}
}

func TestExternalVoiceRecoveryStoreFormattingAndFilesAreRedacted(t *testing.T) {
	store, path := openExternalVoiceRecoveryTestStore(t)
	request := externalVoiceRecoveryTestArm("Z")
	ownerPlaintext := "owner@example.invalid"
	phonePlaintext := "+15555550123"
	sdpPlaintext := "v=0 secret-sdp"
	commandPlaintext := "plain-command-key"
	request.OwnerHash = externalVoiceRecoveryHash(ownerPlaintext)
	request.CommandKeyHash = externalVoiceRecoveryHash(commandPlaintext)
	request.RequestHash = externalVoiceRecoveryHash(phonePlaintext + sdpPlaintext)
	request.ProviderToken = []byte(`{"channel_name":"PJSIP/+15555550123","sdp":"v=0 secret-sdp"}`)
	if _, err := store.Arm(request); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := store.ProviderSecret()
	if err != nil {
		t.Fatal(err)
	}
	metaData, err := os.ReadFile(path + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	for _, formatted := range []string{
		fmt.Sprintf("%v", request), fmt.Sprintf("%+v", request), fmt.Sprintf("%#v", request),
		fmt.Sprintf("%v", snapshot), fmt.Sprintf("%+v", snapshot), fmt.Sprintf("%#v", snapshot),
		fmt.Sprintf("%v", store), fmt.Sprintf("%+v", store), fmt.Sprintf("%#v", store),
		fmt.Sprintf("%v", externalVoiceRecoveryMetaRecord{RootSecret: string(secret[:])}),
		fmt.Sprintf("%v", externalVoiceRecoveryDiskRecord{ProviderToken: phonePlaintext}),
	} {
		for _, forbidden := range []string{
			ownerPlaintext, phonePlaintext, sdpPlaintext, commandPlaintext,
			string(secret[:]), string(request.ProviderToken), path,
		} {
			if strings.Contains(formatted, forbidden) {
				t.Fatalf("formatting leaked %q: %q", forbidden, formatted)
			}
		}
	}
	for _, filePath := range []string{path, path + ".meta"} {
		data, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			ownerPlaintext, phonePlaintext, sdpPlaintext, commandPlaintext,
			string(request.ProviderToken), base64.RawURLEncoding.EncodeToString(request.ProviderToken),
		} {
			if bytes.Contains(data, []byte(forbidden)) {
				t.Fatalf("%s leaked %q", filePath, forbidden)
			}
		}
	}
	if bytes.Contains(metaData, secret[:]) {
		t.Fatal("metadata stored raw root-secret bytes")
	}
	for _, value := range []any{
		request, snapshot, store,
		externalVoiceRecoveryMetaRecord{RootSecret: string(secret[:])},
		externalVoiceRecoveryDiskRecord{ProviderToken: phonePlaintext},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			ownerPlaintext, phonePlaintext, sdpPlaintext, commandPlaintext,
			string(secret[:]), string(request.ProviderToken), path,
		} {
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Fatalf("JSON leaked %q: %s", forbidden, encoded)
			}
		}
	}
}

func TestExternalVoiceRecoveryStoreConcurrentArmResolveAndSnapshot(t *testing.T) {
	store, _ := openExternalVoiceRecoveryTestStore(t)
	request := externalVoiceRecoveryTestArm("C")
	const workers = 32
	var wait sync.WaitGroup
	errorsSeen := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Arm(request)
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent idempotent Arm: %v", err)
		}
	}
	stop := make(chan struct{})
	readErrors := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, err := store.Snapshot()
					if err != nil {
						readErrors <- err
						return
					}
				}
			}
		}()
	}
	resolved, err := store.Resolve(request.RecoveryID, externalVoiceRecoveryProviderEnded)
	if err != nil || resolved.State != externalVoiceRecoveryResolved {
		t.Fatalf("Resolve=%+v err=%v", resolved, err)
	}
	close(stop)
	wait.Wait()
	close(readErrors)
	for err := range readErrors {
		t.Error(err)
	}
}

func TestExternalVoiceRecoveryStoreRejectsInvalidRequestsAndCloseUse(t *testing.T) {
	store, _ := openExternalVoiceRecoveryTestStore(t)
	valid := externalVoiceRecoveryTestArm("I")
	invalid := valid
	invalid.OwnerHash = "owner@example.invalid"
	if _, err := store.Arm(invalid); err == nil {
		t.Fatal("plaintext owner was accepted")
	}
	invalid = valid
	invalid.ProviderToken = bytes.Repeat([]byte{'x'}, externalVoiceRecoveryProviderTokenMax+1)
	if _, err := store.Arm(invalid); err == nil {
		t.Fatal("oversized provider token was accepted")
	}
	if _, err := store.Arm(valid); err != nil {
		t.Fatal(err)
	}
	conflict := externalVoiceRecoveryTestArm("J")
	if _, err := store.Arm(conflict); !errors.Is(err, errExternalVoiceRecoveryStoreBusy) {
		t.Fatalf("second armed operation error=%v", err)
	}
	if _, err := store.Resolve(conflict.RecoveryID, externalVoiceRecoveryApplied); !errors.Is(err, errExternalVoiceRecoveryStoreStale) {
		t.Fatalf("stale Resolve error=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(); !errors.Is(err, errExternalVoiceRecoveryStoreClosed) {
		t.Fatalf("Snapshot after Close error=%v", err)
	}
	if secret, err := store.ProviderSecret(); !errors.Is(err, errExternalVoiceRecoveryStoreClosed) ||
		secret != [externalVoiceRecoveryRootSecretBytes]byte{} {
		t.Fatalf("ProviderSecret after Close zero=%t err=%v", secret == [externalVoiceRecoveryRootSecretBytes]byte{}, err)
	}
}

func mustRemoveRecoveryTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func mustWriteRecoveryTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExternalVoiceRecoveryTimeCanonicalization(t *testing.T) {
	if !validExternalVoiceRecoveryTime(time.Date(2026, 8, 14, 12, 0, 0, 123, time.UTC).Format(time.RFC3339Nano)) {
		t.Fatal("canonical recovery time was rejected")
	}
	if validExternalVoiceRecoveryTime("2026-08-14T20:00:00+08:00") {
		t.Fatal("non-UTC recovery time was accepted")
	}
}
