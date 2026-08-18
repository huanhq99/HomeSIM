package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	externalVoiceRecoveryStoreVersion     = 1
	externalVoiceRecoveryStoreMaxBytes    = 16 << 10
	externalVoiceRecoveryProviderTokenMax = 2 << 10
	externalVoiceRecoveryOpaqueMaximum    = 128
	externalVoiceRecoveryRootSecretBytes  = 32
	externalVoiceRecoveryStoreIDBytes     = 24
	externalVoiceRecoveryMACDomain        = "external-voice-recovery-slot-v1\x00"
	externalVoiceRecoveryCipherDomain     = "external-voice-recovery-provider-token-aes256gcm-v1\x00"
)

var (
	errExternalVoiceRecoveryStoreClosed  = errors.New("external voice recovery store is closed")
	errExternalVoiceRecoveryStoreCorrupt = errors.New("external voice recovery store is corrupt")
	errExternalVoiceRecoveryStoreBusy    = errors.New("external voice recovery is already armed")
	errExternalVoiceRecoveryStoreStale   = errors.New("external voice recovery identifier does not match")
	errExternalVoiceRecoveryStoreFailed  = errors.New("external voice recovery store is unavailable")

	externalVoiceRecoveryIDPattern = regexp.MustCompile(`^svr_[A-Za-z0-9_-]{32,128}$`)
)

type externalVoiceRecoveryState string

const (
	externalVoiceRecoveryClear    externalVoiceRecoveryState = "clear"
	externalVoiceRecoveryArmed    externalVoiceRecoveryState = "armed"
	externalVoiceRecoveryResolved externalVoiceRecoveryState = "resolved"
)

type externalVoiceRecoveryKind string

const (
	externalVoiceRecoveryAnswerIncoming externalVoiceRecoveryKind = "answer_incoming"
	externalVoiceRecoveryEndActive      externalVoiceRecoveryKind = "end_active"
)

type externalVoiceRecoveryResolution string

const (
	externalVoiceRecoveryApplied       externalVoiceRecoveryResolution = "applied"
	externalVoiceRecoveryRejected      externalVoiceRecoveryResolution = "rejected"
	externalVoiceRecoveryProviderEnded externalVoiceRecoveryResolution = "provider_ended"
)

// externalVoiceRecoveryArmRequest contains only opaque identifiers and hashes.
// ProviderToken is adapter-private correlation and is encrypted before storage.
type externalVoiceRecoveryArmRequest struct {
	RecoveryID       string
	Kind             externalVoiceRecoveryKind
	OwnerHash        string
	CommandKeyHash   string
	RequestHash      string
	PublicCallID     string
	Generation       uint64
	ExpectedRevision uint64
	PublicMediaLease string
	Adapter          string
	GatewayID        string
	ProviderToken    []byte
}

func (externalVoiceRecoveryArmRequest) String() string {
	return "externalVoiceRecoveryArmRequest{redacted}"
}

func (externalVoiceRecoveryArmRequest) GoString() string {
	return "externalVoiceRecoveryArmRequest{redacted}"
}

func (externalVoiceRecoveryArmRequest) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"externalVoiceRecoveryArmRequest","redacted":true}`), nil
}

type externalVoiceRecoverySnapshot struct {
	State            externalVoiceRecoveryState
	StoreGeneration  uint64
	RecoveryID       string
	Kind             externalVoiceRecoveryKind
	OwnerHash        string
	CommandKeyHash   string
	RequestHash      string
	PublicCallID     string
	Generation       uint64
	ExpectedRevision uint64
	PublicMediaLease string
	Adapter          string
	GatewayID        string
	ProviderToken    []byte
	ArmedAt          time.Time
	ResolvedAt       time.Time
	RecoveryIDHash   string
	Resolution       externalVoiceRecoveryResolution
}

func (externalVoiceRecoverySnapshot) String() string {
	return "externalVoiceRecoverySnapshot{redacted}"
}

func (externalVoiceRecoverySnapshot) GoString() string {
	return "externalVoiceRecoverySnapshot{redacted}"
}

func (externalVoiceRecoverySnapshot) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"externalVoiceRecoverySnapshot","redacted":true}`), nil
}

type externalVoiceRecoveryDiskRecord struct {
	Version          int                             `json:"version"`
	State            externalVoiceRecoveryState      `json:"state"`
	StoreID          string                          `json:"store_id"`
	SlotGeneration   uint64                          `json:"slot_generation"`
	ArmedAt          string                          `json:"armed_at,omitempty"`
	ResolvedAt       string                          `json:"resolved_at,omitempty"`
	RecoveryID       string                          `json:"recovery_id,omitempty"`
	RecoveryIDHash   string                          `json:"recovery_id_hash,omitempty"`
	Kind             externalVoiceRecoveryKind       `json:"kind,omitempty"`
	OwnerHash        string                          `json:"owner_hash,omitempty"`
	CommandKeyHash   string                          `json:"command_key_hash,omitempty"`
	RequestHash      string                          `json:"request_hash,omitempty"`
	PublicCallID     string                          `json:"public_call_id,omitempty"`
	Generation       uint64                          `json:"generation,omitempty"`
	ExpectedRevision uint64                          `json:"expected_revision,omitempty"`
	PublicMediaLease string                          `json:"public_media_lease_id,omitempty"`
	Adapter          string                          `json:"adapter,omitempty"`
	GatewayID        string                          `json:"gateway_id,omitempty"`
	ProviderToken    string                          `json:"provider_token,omitempty"`
	Resolution       externalVoiceRecoveryResolution `json:"resolution,omitempty"`
	MAC              string                          `json:"mac"`
}

type externalVoiceRecoveryDiskRecordJSON externalVoiceRecoveryDiskRecord

func (externalVoiceRecoveryDiskRecord) String() string {
	return "externalVoiceRecoveryDiskRecord{redacted}"
}

func (externalVoiceRecoveryDiskRecord) GoString() string {
	return "externalVoiceRecoveryDiskRecord{redacted}"
}

func (externalVoiceRecoveryDiskRecord) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"externalVoiceRecoveryDiskRecord","redacted":true}`), nil
}

type externalVoiceRecoveryMetaRecord struct {
	Version    int    `json:"version"`
	StoreID    string `json:"store_id"`
	CreatedAt  string `json:"created_at"`
	RootSecret string `json:"root_secret"`
}

type externalVoiceRecoveryMetaRecordJSON externalVoiceRecoveryMetaRecord

func (externalVoiceRecoveryMetaRecord) String() string {
	return "externalVoiceRecoveryMetaRecord{redacted}"
}

func (externalVoiceRecoveryMetaRecord) GoString() string {
	return "externalVoiceRecoveryMetaRecord{redacted}"
}

func (externalVoiceRecoveryMetaRecord) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"externalVoiceRecoveryMetaRecord","redacted":true}`), nil
}

type externalVoiceRecoveryStoreIO struct {
	createTemp func(string) (*os.File, error)
	write      func(*os.File, []byte) (int, error)
	syncFile   func(*os.File) error
	closeFile  func(*os.File) error
	rename     func(string, string) error
	syncDir    func(string) error
}

type externalVoiceRecoveryStore struct {
	mu sync.Mutex

	path       string
	dir        string
	tempPath   string
	metaPath   string
	metaTemp   string
	lock       *os.File
	current    externalVoiceRecoverySnapshot
	createdAt  time.Time
	storeID    string
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte
	closed     bool
	failure    error
	now        func() time.Time
	io         externalVoiceRecoveryStoreIO
}

func (*externalVoiceRecoveryStore) String() string {
	return "externalVoiceRecoveryStore{redacted}"
}

func (*externalVoiceRecoveryStore) GoString() string {
	return "externalVoiceRecoveryStore{redacted}"
}

func (*externalVoiceRecoveryStore) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"externalVoiceRecoveryStore","redacted":true}`), nil
}

func openExternalVoiceRecoveryStore(path string) (*externalVoiceRecoveryStore, error) {
	return openExternalVoiceRecoveryStoreWithIO(path, externalVoiceRecoveryStoreIO{})
}

func openExternalVoiceRecoveryStoreWithIO(
	path string,
	overrides externalVoiceRecoveryStoreIO,
) (*externalVoiceRecoveryStore, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." {
		return nil, errors.New("external voice recovery store requires a clean absolute path")
	}
	dir := filepath.Dir(path)
	directoryCreated, err := ensureExternalVoiceRecoveryDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("secure external voice recovery directory: %w", err)
	}
	lock, err := openExternalVoiceRecoveryLock(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock external voice recovery store: %w", err)
	}
	store := &externalVoiceRecoveryStore{
		path: path, dir: dir, tempPath: path + ".tmp", metaPath: path + ".meta",
		metaTemp: path + ".meta.tmp", lock: lock, now: time.Now,
		io:      defaultExternalVoiceRecoveryStoreIO(),
		current: externalVoiceRecoverySnapshot{State: externalVoiceRecoveryClear},
	}
	store.applyIOOverrides(overrides)
	for _, tempPath := range []string{store.tempPath, store.metaTemp} {
		if removed, cleanupErr := removeExternalVoiceRecoveryTemp(tempPath); cleanupErr != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("inspect external voice recovery temporary file: %w", cleanupErr)
		} else if removed {
			if syncErr := store.io.syncDir(dir); syncErr != nil {
				_ = lock.Close()
				return nil, fmt.Errorf("persist external voice recovery temporary cleanup: %w", syncErr)
			}
		}
	}
	metaData, metaErr := readExternalVoiceRecoveryFile(store.metaPath, externalVoiceRecoveryStoreMaxBytes)
	slotData, slotErr := readExternalVoiceRecoveryFile(path, externalVoiceRecoveryStoreMaxBytes)
	metaMissing := errors.Is(metaErr, os.ErrNotExist)
	slotMissing := errors.Is(slotErr, os.ErrNotExist)
	if metaMissing && slotMissing {
		if !directoryCreated {
			_ = lock.Close()
			return nil, fmt.Errorf("%w: recovery metadata and slot are missing from an existing directory", errExternalVoiceRecoveryStoreCorrupt)
		}
		if _, randomErr := io.ReadFull(rand.Reader, store.rootSecret[:]); randomErr != nil {
			zeroExternalVoiceRecoverySecret(&store.rootSecret)
			_ = lock.Close()
			return nil, errors.New("generate external voice recovery root secret")
		}
		store.createdAt = store.now().UTC()
		storeIDBytes := make([]byte, externalVoiceRecoveryStoreIDBytes)
		if _, randomErr := io.ReadFull(rand.Reader, storeIDBytes); randomErr != nil {
			zeroExternalVoiceRecoverySecret(&store.rootSecret)
			_ = lock.Close()
			return nil, errors.New("generate external voice recovery store identifier")
		}
		store.storeID = "svs_" + base64.RawURLEncoding.EncodeToString(storeIDBytes)
		for index := range storeIDBytes {
			storeIDBytes[index] = 0
		}
		meta := externalVoiceRecoveryMetaRecord{
			Version: externalVoiceRecoveryStoreVersion, StoreID: store.storeID,
			CreatedAt:  store.createdAt.Format(time.RFC3339Nano),
			RootSecret: base64.RawURLEncoding.EncodeToString(store.rootSecret[:]),
		}
		if persistErr := store.persistMetaLocked(meta); persistErr != nil {
			zeroExternalVoiceRecoverySecret(&store.rootSecret)
			_ = lock.Close()
			return nil, fmt.Errorf("initialize external voice recovery metadata: %w", persistErr)
		}
		initial := externalVoiceRecoveryDiskRecord{
			Version: externalVoiceRecoveryStoreVersion, State: externalVoiceRecoveryClear,
			StoreID: store.storeID, SlotGeneration: 1,
		}
		if persistErr := store.persistLocked(initial); persistErr != nil {
			zeroExternalVoiceRecoverySecret(&store.rootSecret)
			_ = lock.Close()
			return nil, fmt.Errorf("initialize external voice recovery slot: %w", persistErr)
		}
		store.current.StoreGeneration = 1
		return store, nil
	}
	if metaMissing != slotMissing {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: recovery metadata and slot must both exist", errExternalVoiceRecoveryStoreCorrupt)
	}
	if metaErr != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("read external voice recovery metadata: %w", metaErr)
	}
	if slotErr != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("read external voice recovery slot: %w", slotErr)
	}
	meta, err := decodeExternalVoiceRecoveryMeta(metaData)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: metadata: %v", errExternalVoiceRecoveryStoreCorrupt, err)
	}
	rootSecret, _ := base64.RawURLEncoding.DecodeString(meta.RootSecret)
	copy(store.rootSecret[:], rootSecret)
	for index := range rootSecret {
		rootSecret[index] = 0
	}
	store.storeID = meta.StoreID
	store.createdAt, _ = time.Parse(time.RFC3339Nano, meta.CreatedAt)
	record, err := decodeExternalVoiceRecoveryRecord(slotData, store.storeID, store.rootSecret)
	if err != nil {
		zeroExternalVoiceRecoverySecret(&store.rootSecret)
		_ = lock.Close()
		return nil, fmt.Errorf("%w: slot: %v", errExternalVoiceRecoveryStoreCorrupt, err)
	}
	snapshot, err := externalVoiceRecoveryRecordSnapshot(record, store.rootSecret)
	if err != nil {
		zeroExternalVoiceRecoverySecret(&store.rootSecret)
		_ = lock.Close()
		return nil, fmt.Errorf("%w: %v", errExternalVoiceRecoveryStoreCorrupt, err)
	}
	store.current = snapshot
	return store, nil
}

func defaultExternalVoiceRecoveryStoreIO() externalVoiceRecoveryStoreIO {
	return externalVoiceRecoveryStoreIO{
		createTemp: createExternalVoiceRecoveryTemp,
		write:      func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		syncFile:   func(file *os.File) error { return file.Sync() },
		closeFile:  func(file *os.File) error { return file.Close() },
		rename:     renameExternalVoiceRecoveryFile,
		syncDir:    syncExternalVoiceRecoveryDirectory,
	}
}

func (s *externalVoiceRecoveryStore) applyIOOverrides(overrides externalVoiceRecoveryStoreIO) {
	if overrides.createTemp != nil {
		s.io.createTemp = overrides.createTemp
	}
	if overrides.write != nil {
		s.io.write = overrides.write
	}
	if overrides.syncFile != nil {
		s.io.syncFile = overrides.syncFile
	}
	if overrides.closeFile != nil {
		s.io.closeFile = overrides.closeFile
	}
	if overrides.rename != nil {
		s.io.rename = overrides.rename
	}
	if overrides.syncDir != nil {
		s.io.syncDir = overrides.syncDir
	}
}

func ensureExternalVoiceRecoveryDirectory(path string) (bool, error) {
	created := false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return false, err
		}
		created = true
		if err := os.Chmod(path, 0o700); err != nil {
			return false, err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("external voice recovery path must be a real directory")
	}
	if err := validateExternalVoiceRecoveryDirectory(path); err != nil {
		return false, err
	}
	return created, nil
}

// Arm durably publishes one possible provider mutation. It must return success
// before any provider mutation bytes are allowed to leave the process.
func (s *externalVoiceRecoveryStore) Arm(request externalVoiceRecoveryArmRequest) (externalVoiceRecoverySnapshot, error) {
	if err := validateExternalVoiceRecoveryArmRequest(request); err != nil {
		return externalVoiceRecoverySnapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return externalVoiceRecoverySnapshot{}, err
	}
	if s.current.State == externalVoiceRecoveryArmed {
		if externalVoiceRecoverySnapshotMatchesRequest(s.current, request) {
			return cloneExternalVoiceRecoverySnapshot(s.current), nil
		}
		return externalVoiceRecoverySnapshot{}, errExternalVoiceRecoveryStoreBusy
	}
	if s.current.State == externalVoiceRecoveryResolved &&
		s.current.RecoveryIDHash == externalVoiceRecoveryHash(request.RecoveryID) {
		return externalVoiceRecoverySnapshot{}, errExternalVoiceRecoveryStoreStale
	}
	armedAt := s.now().UTC()
	record := externalVoiceRecoveryDiskRecord{
		Version: externalVoiceRecoveryStoreVersion, State: externalVoiceRecoveryArmed,
		StoreID: s.storeID, SlotGeneration: s.current.StoreGeneration + 1,
		ArmedAt: armedAt.Format(time.RFC3339Nano), RecoveryID: request.RecoveryID,
		Kind: request.Kind, OwnerHash: request.OwnerHash, CommandKeyHash: request.CommandKeyHash,
		RequestHash: request.RequestHash, PublicCallID: request.PublicCallID,
		Generation: request.Generation, ExpectedRevision: request.ExpectedRevision,
		PublicMediaLease: request.PublicMediaLease, Adapter: request.Adapter,
		GatewayID: request.GatewayID,
	}
	sealedToken, err := sealExternalVoiceRecoveryProviderToken(s.rootSecret, record, request.ProviderToken)
	if err != nil {
		s.failure = err
		return externalVoiceRecoverySnapshot{}, fmt.Errorf("%w: seal provider token: %v", errExternalVoiceRecoveryStoreFailed, err)
	}
	record.ProviderToken = sealedToken
	if err := s.persistLocked(record); err != nil {
		s.failure = err
		return externalVoiceRecoverySnapshot{}, fmt.Errorf("%w: arm: %v", errExternalVoiceRecoveryStoreFailed, err)
	}
	s.current = externalVoiceRecoverySnapshot{
		State: externalVoiceRecoveryArmed, StoreGeneration: record.SlotGeneration,
		RecoveryID: request.RecoveryID, Kind: request.Kind,
		OwnerHash: request.OwnerHash, CommandKeyHash: request.CommandKeyHash,
		RequestHash: request.RequestHash, PublicCallID: request.PublicCallID,
		Generation: request.Generation, ExpectedRevision: request.ExpectedRevision,
		PublicMediaLease: request.PublicMediaLease, Adapter: request.Adapter,
		GatewayID: request.GatewayID, ProviderToken: append([]byte(nil), request.ProviderToken...),
		ArmedAt: armedAt,
	}
	return cloneExternalVoiceRecoverySnapshot(s.current), nil
}

// Resolve atomically replaces the armed record with a redacted tombstone. A
// failed resolution never changes the in-memory armed authority.
func (s *externalVoiceRecoveryStore) Resolve(
	recoveryID string,
	resolution externalVoiceRecoveryResolution,
) (externalVoiceRecoverySnapshot, error) {
	if !externalVoiceRecoveryIDPattern.MatchString(recoveryID) || !validExternalVoiceRecoveryResolution(resolution) {
		return externalVoiceRecoverySnapshot{}, errors.New("invalid external voice recovery resolution")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return externalVoiceRecoverySnapshot{}, err
	}
	recoveryIDHash := externalVoiceRecoveryHash(recoveryID)
	if s.current.State == externalVoiceRecoveryResolved {
		if s.current.RecoveryIDHash == recoveryIDHash && s.current.Resolution == resolution {
			return cloneExternalVoiceRecoverySnapshot(s.current), nil
		}
		return externalVoiceRecoverySnapshot{}, errExternalVoiceRecoveryStoreStale
	}
	if s.current.State != externalVoiceRecoveryArmed || s.current.RecoveryID != recoveryID {
		return externalVoiceRecoverySnapshot{}, errExternalVoiceRecoveryStoreStale
	}
	resolvedAt := s.now().UTC()
	record := externalVoiceRecoveryDiskRecord{
		Version: externalVoiceRecoveryStoreVersion, State: externalVoiceRecoveryResolved,
		StoreID: s.storeID, SlotGeneration: s.current.StoreGeneration + 1,
		ResolvedAt: resolvedAt.Format(time.RFC3339Nano), RecoveryIDHash: recoveryIDHash,
		Resolution: resolution,
	}
	if err := s.persistLocked(record); err != nil {
		s.failure = err
		return externalVoiceRecoverySnapshot{}, fmt.Errorf("%w: resolve: %v", errExternalVoiceRecoveryStoreFailed, err)
	}
	zeroExternalVoiceRecoverySnapshot(&s.current)
	s.current = externalVoiceRecoverySnapshot{
		State: externalVoiceRecoveryResolved, StoreGeneration: record.SlotGeneration,
		ResolvedAt:     resolvedAt,
		RecoveryIDHash: recoveryIDHash, Resolution: resolution,
	}
	return cloneExternalVoiceRecoverySnapshot(s.current), nil
}

func (s *externalVoiceRecoveryStore) Snapshot() (externalVoiceRecoverySnapshot, error) {
	if s == nil {
		return externalVoiceRecoverySnapshot{}, errExternalVoiceRecoveryStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return externalVoiceRecoverySnapshot{}, err
	}
	return cloneExternalVoiceRecoverySnapshot(s.current), nil
}

// ProviderSecret returns a value copy of the stable private root. Callers must
// domain-separate every derived key; the store never exposes this value through
// JSON, formatting, or Snapshot.
func (s *externalVoiceRecoveryStore) ProviderSecret() ([externalVoiceRecoveryRootSecretBytes]byte, error) {
	if s == nil {
		return [externalVoiceRecoveryRootSecretBytes]byte{}, errExternalVoiceRecoveryStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.availableLocked(); err != nil {
		return [externalVoiceRecoveryRootSecretBytes]byte{}, err
	}
	return s.rootSecret, nil
}

func (s *externalVoiceRecoveryStore) availableLocked() error {
	if s == nil || s.closed {
		return errExternalVoiceRecoveryStoreClosed
	}
	if s.failure != nil {
		return fmt.Errorf("%w: %v", errExternalVoiceRecoveryStoreFailed, s.failure)
	}
	return nil
}

func (s *externalVoiceRecoveryStore) persistLocked(record externalVoiceRecoveryDiskRecord) error {
	record.MAC = ""
	if err := validateExternalVoiceRecoveryDiskRecord(record, false); err != nil {
		return err
	}
	record.MAC = externalVoiceRecoveryRecordMAC(s.rootSecret, record)
	if err := validateExternalVoiceRecoveryDiskRecord(record, true); err != nil {
		return err
	}
	payload, err := json.Marshal(externalVoiceRecoveryDiskRecordJSON(record))
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > externalVoiceRecoveryStoreMaxBytes {
		return errors.New("external voice recovery record is too large")
	}
	return s.persistPayloadLocked(s.tempPath, s.path, payload)
}

func (s *externalVoiceRecoveryStore) persistMetaLocked(record externalVoiceRecoveryMetaRecord) error {
	if err := validateExternalVoiceRecoveryMeta(record); err != nil {
		return err
	}
	payload, err := json.Marshal(externalVoiceRecoveryMetaRecordJSON(record))
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > externalVoiceRecoveryStoreMaxBytes {
		return errors.New("external voice recovery metadata is too large")
	}
	return s.persistPayloadLocked(s.metaTemp, s.metaPath, payload)
}

func (s *externalVoiceRecoveryStore) persistPayloadLocked(tempPath, finalPath string, payload []byte) error {
	file, err := s.io.createTemp(tempPath)
	if err != nil {
		return err
	}
	closed := false
	renamed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !renamed {
			_ = os.Remove(tempPath)
		}
	}()
	written, err := s.io.write(file, payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	if err := s.io.syncFile(file); err != nil {
		return err
	}
	if err := s.io.closeFile(file); err != nil {
		return err
	}
	closed = true
	if err := s.io.rename(tempPath, finalPath); err != nil {
		return err
	}
	renamed = true
	if err := s.io.syncDir(s.dir); err != nil {
		return err
	}
	return nil
}

func (s *externalVoiceRecoveryStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	zeroExternalVoiceRecoverySnapshot(&s.current)
	zeroExternalVoiceRecoverySecret(&s.rootSecret)
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func validateExternalVoiceRecoveryArmRequest(request externalVoiceRecoveryArmRequest) error {
	if !externalVoiceRecoveryIDPattern.MatchString(request.RecoveryID) ||
		!validExternalVoiceRecoveryKind(request.Kind) ||
		!validExternalVoiceRecoveryHash(request.OwnerHash) ||
		!validExternalVoiceRecoveryHash(request.CommandKeyHash) ||
		!validExternalVoiceRecoveryHash(request.RequestHash) ||
		!validExternalVoiceRecoveryOpaque(request.PublicCallID, 8, externalVoiceRecoveryOpaqueMaximum) ||
		request.Generation == 0 || request.ExpectedRevision == 0 ||
		!validExternalVoiceRecoveryOpaque(request.PublicMediaLease, 8, externalVoiceRecoveryOpaqueMaximum) ||
		!validExternalVoiceRecoveryToken(request.Adapter, 1, 32) ||
		!validExternalVoiceRecoveryToken(request.GatewayID, 1, 64) ||
		len(request.ProviderToken) == 0 || len(request.ProviderToken) > externalVoiceRecoveryProviderTokenMax {
		return errors.New("invalid external voice recovery arm request")
	}
	return nil
}

func validateExternalVoiceRecoveryDiskRecord(record externalVoiceRecoveryDiskRecord, requireMAC bool) error {
	if record.Version != externalVoiceRecoveryStoreVersion {
		return errors.New("unsupported external voice recovery record version")
	}
	if !validExternalVoiceRecoveryToken(record.StoreID, 8, 64) || record.SlotGeneration == 0 {
		return errors.New("invalid external voice recovery slot metadata")
	}
	if requireMAC {
		if !validExternalVoiceRecoveryHash(record.MAC) {
			return errors.New("invalid external voice recovery slot MAC")
		}
	} else if record.MAC != "" {
		return errors.New("external voice recovery slot MAC must be empty before signing")
	}
	switch record.State {
	case externalVoiceRecoveryClear:
		if record.ArmedAt != "" || record.ResolvedAt != "" || record.RecoveryID != "" ||
			record.RecoveryIDHash != "" || record.Kind != "" || record.OwnerHash != "" ||
			record.CommandKeyHash != "" || record.RequestHash != "" || record.PublicCallID != "" ||
			record.Generation != 0 || record.ExpectedRevision != 0 || record.PublicMediaLease != "" ||
			record.Adapter != "" || record.GatewayID != "" || record.ProviderToken != "" ||
			record.Resolution != "" {
			return errors.New("invalid clear external voice recovery record")
		}
	case externalVoiceRecoveryArmed:
		sealedToken, err := base64.RawURLEncoding.DecodeString(record.ProviderToken)
		minimumSealedSize := 12 + 16 + 1
		maximumSealedSize := 12 + 16 + externalVoiceRecoveryProviderTokenMax
		if err != nil || len(sealedToken) < minimumSealedSize || len(sealedToken) > maximumSealedSize {
			return errors.New("invalid sealed external voice recovery provider token")
		}
		request := externalVoiceRecoveryArmRequest{
			RecoveryID: record.RecoveryID, Kind: record.Kind, OwnerHash: record.OwnerHash,
			CommandKeyHash: record.CommandKeyHash, RequestHash: record.RequestHash,
			PublicCallID: record.PublicCallID, Generation: record.Generation,
			ExpectedRevision: record.ExpectedRevision, PublicMediaLease: record.PublicMediaLease,
			Adapter: record.Adapter, GatewayID: record.GatewayID, ProviderToken: []byte{1},
		}
		if err := validateExternalVoiceRecoveryArmRequest(request); err != nil {
			return err
		}
		if !validExternalVoiceRecoveryTime(record.ArmedAt) || record.ResolvedAt != "" ||
			record.RecoveryIDHash != "" || record.Resolution != "" {
			return errors.New("invalid armed external voice recovery record")
		}
	case externalVoiceRecoveryResolved:
		if !validExternalVoiceRecoveryTime(record.ResolvedAt) ||
			!validExternalVoiceRecoveryHash(record.RecoveryIDHash) ||
			!validExternalVoiceRecoveryResolution(record.Resolution) ||
			record.ArmedAt != "" || record.RecoveryID != "" || record.Kind != "" ||
			record.OwnerHash != "" || record.CommandKeyHash != "" || record.RequestHash != "" ||
			record.PublicCallID != "" || record.Generation != 0 || record.ExpectedRevision != 0 ||
			record.PublicMediaLease != "" || record.Adapter != "" || record.GatewayID != "" ||
			record.ProviderToken != "" {
			return errors.New("invalid resolved external voice recovery tombstone")
		}
	default:
		return errors.New("invalid external voice recovery state")
	}
	return nil
}

func externalVoiceRecoveryRecordSnapshot(
	record externalVoiceRecoveryDiskRecord,
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte,
) (externalVoiceRecoverySnapshot, error) {
	if err := validateExternalVoiceRecoveryDiskRecord(record, true); err != nil {
		return externalVoiceRecoverySnapshot{}, err
	}
	if record.State == externalVoiceRecoveryResolved {
		resolvedAt, _ := time.Parse(time.RFC3339Nano, record.ResolvedAt)
		return externalVoiceRecoverySnapshot{
			State: record.State, StoreGeneration: record.SlotGeneration,
			ResolvedAt: resolvedAt, RecoveryIDHash: record.RecoveryIDHash,
			Resolution: record.Resolution,
		}, nil
	}
	if record.State == externalVoiceRecoveryClear {
		return externalVoiceRecoverySnapshot{
			State: externalVoiceRecoveryClear, StoreGeneration: record.SlotGeneration,
		}, nil
	}
	armedAt, _ := time.Parse(time.RFC3339Nano, record.ArmedAt)
	providerToken, err := openExternalVoiceRecoveryProviderToken(rootSecret, record)
	if err != nil {
		return externalVoiceRecoverySnapshot{}, err
	}
	return externalVoiceRecoverySnapshot{
		State: record.State, StoreGeneration: record.SlotGeneration,
		RecoveryID: record.RecoveryID, Kind: record.Kind,
		OwnerHash: record.OwnerHash, CommandKeyHash: record.CommandKeyHash,
		RequestHash: record.RequestHash, PublicCallID: record.PublicCallID,
		Generation: record.Generation, ExpectedRevision: record.ExpectedRevision,
		PublicMediaLease: record.PublicMediaLease, Adapter: record.Adapter,
		GatewayID: record.GatewayID, ProviderToken: providerToken, ArmedAt: armedAt,
	}, nil
}

type externalVoiceRecoveryProviderAAD struct {
	Version        int                       `json:"version"`
	StoreID        string                    `json:"store_id"`
	SlotGeneration uint64                    `json:"slot_generation"`
	Kind           externalVoiceRecoveryKind `json:"kind"`
	RecoveryID     string                    `json:"recovery_id"`
}

func sealExternalVoiceRecoveryProviderToken(
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte,
	record externalVoiceRecoveryDiskRecord,
	providerToken []byte,
) (string, error) {
	block, key, err := externalVoiceRecoveryProviderCipher(rootSecret)
	if err != nil {
		return "", err
	}
	defer zeroExternalVoiceRecoverySecret(&key)
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", errors.New("generate external voice recovery provider nonce")
	}
	sealed := gcm.Seal(nonce, nonce, providerToken, externalVoiceRecoveryProviderAssociatedData(record))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openExternalVoiceRecoveryProviderToken(
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte,
	record externalVoiceRecoveryDiskRecord,
) ([]byte, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(record.ProviderToken)
	if err != nil {
		return nil, errors.New("decode sealed external voice recovery provider token")
	}
	block, key, err := externalVoiceRecoveryProviderCipher(rootSecret)
	if err != nil {
		return nil, err
	}
	defer zeroExternalVoiceRecoverySecret(&key)
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(sealed) < gcm.NonceSize()+gcm.Overhead()+1 {
		return nil, errors.New("invalid sealed external voice recovery provider token")
	}
	nonce := sealed[:gcm.NonceSize()]
	ciphertext := sealed[gcm.NonceSize():]
	providerToken, err := gcm.Open(nil, nonce, ciphertext, externalVoiceRecoveryProviderAssociatedData(record))
	if err != nil || len(providerToken) == 0 || len(providerToken) > externalVoiceRecoveryProviderTokenMax {
		return nil, errors.New("authenticate sealed external voice recovery provider token")
	}
	return providerToken, nil
}

func externalVoiceRecoveryProviderCipher(
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte,
) (cipher.Block, [externalVoiceRecoveryRootSecretBytes]byte, error) {
	deriver := hmac.New(sha256.New, rootSecret[:])
	_, _ = deriver.Write([]byte(externalVoiceRecoveryCipherDomain))
	var key [externalVoiceRecoveryRootSecretBytes]byte
	copy(key[:], deriver.Sum(nil))
	block, err := aes.NewCipher(key[:])
	return block, key, err
}

func externalVoiceRecoveryProviderAssociatedData(record externalVoiceRecoveryDiskRecord) []byte {
	payload, _ := json.Marshal(externalVoiceRecoveryProviderAAD{
		Version: record.Version, StoreID: record.StoreID, SlotGeneration: record.SlotGeneration,
		Kind: record.Kind, RecoveryID: record.RecoveryID,
	})
	return payload
}

func decodeExternalVoiceRecoveryRecord(
	data []byte,
	storeID string,
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte,
) (externalVoiceRecoveryDiskRecord, error) {
	if len(data) == 0 || len(data) > externalVoiceRecoveryStoreMaxBytes || data[len(data)-1] != '\n' {
		return externalVoiceRecoveryDiskRecord{}, errors.New("external voice recovery record must be one bounded newline-terminated JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return externalVoiceRecoveryDiskRecord{}, errors.New("external voice recovery record must be a JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		nameToken, err := decoder.Token()
		name, ok := nameToken.(string)
		if err != nil || !ok {
			return externalVoiceRecoveryDiskRecord{}, errors.New("invalid external voice recovery field")
		}
		if _, duplicate := seen[name]; duplicate {
			return externalVoiceRecoveryDiskRecord{}, errors.New("duplicate external voice recovery field")
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return externalVoiceRecoveryDiskRecord{}, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return externalVoiceRecoveryDiskRecord{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return externalVoiceRecoveryDiskRecord{}, errors.New("trailing external voice recovery data")
	}
	var record externalVoiceRecoveryDiskRecord
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&record); err != nil || strict.Decode(new(any)) != io.EOF {
		return externalVoiceRecoveryDiskRecord{}, errors.New("invalid external voice recovery schema")
	}
	if err := validateExternalVoiceRecoveryDiskRecord(record, true); err != nil {
		return externalVoiceRecoveryDiskRecord{}, err
	}
	if record.StoreID != storeID || !hmac.Equal(
		[]byte(record.MAC),
		[]byte(externalVoiceRecoveryRecordMAC(rootSecret, record)),
	) {
		return externalVoiceRecoveryDiskRecord{}, errors.New("external voice recovery slot authentication failed")
	}
	canonical, err := json.Marshal(externalVoiceRecoveryDiskRecordJSON(record))
	if err != nil {
		return externalVoiceRecoveryDiskRecord{}, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return externalVoiceRecoveryDiskRecord{}, errors.New("external voice recovery record is not canonical")
	}
	return record, nil
}

func validateExternalVoiceRecoveryMeta(record externalVoiceRecoveryMetaRecord) error {
	rootSecret, err := base64.RawURLEncoding.DecodeString(record.RootSecret)
	if record.Version != externalVoiceRecoveryStoreVersion ||
		!validExternalVoiceRecoveryToken(record.StoreID, 8, 64) ||
		!validExternalVoiceRecoveryTime(record.CreatedAt) || err != nil ||
		len(rootSecret) != externalVoiceRecoveryRootSecretBytes {
		return errors.New("invalid external voice recovery metadata")
	}
	for index := range rootSecret {
		rootSecret[index] = 0
	}
	return nil
}

func decodeExternalVoiceRecoveryMeta(data []byte) (externalVoiceRecoveryMetaRecord, error) {
	if len(data) == 0 || len(data) > externalVoiceRecoveryStoreMaxBytes || data[len(data)-1] != '\n' {
		return externalVoiceRecoveryMetaRecord{}, errors.New("external voice recovery metadata must be one bounded newline-terminated JSON object")
	}
	if err := rejectDuplicateExternalVoiceRecoveryFields(data); err != nil {
		return externalVoiceRecoveryMetaRecord{}, err
	}
	var record externalVoiceRecoveryMetaRecord
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&record); err != nil || strict.Decode(new(any)) != io.EOF {
		return externalVoiceRecoveryMetaRecord{}, errors.New("invalid external voice recovery metadata schema")
	}
	if err := validateExternalVoiceRecoveryMeta(record); err != nil {
		return externalVoiceRecoveryMetaRecord{}, err
	}
	canonical, err := json.Marshal(externalVoiceRecoveryMetaRecordJSON(record))
	if err != nil {
		return externalVoiceRecoveryMetaRecord{}, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return externalVoiceRecoveryMetaRecord{}, errors.New("external voice recovery metadata is not canonical")
	}
	return record, nil
}

func rejectDuplicateExternalVoiceRecoveryFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("external voice recovery value must be a JSON object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		nameToken, err := decoder.Token()
		name, ok := nameToken.(string)
		if err != nil || !ok {
			return errors.New("invalid external voice recovery field")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("duplicate external voice recovery field")
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing external voice recovery data")
	}
	return nil
}

func externalVoiceRecoveryRecordMAC(
	rootSecret [externalVoiceRecoveryRootSecretBytes]byte,
	record externalVoiceRecoveryDiskRecord,
) string {
	record.MAC = ""
	payload, _ := json.Marshal(externalVoiceRecoveryDiskRecordJSON(record))
	mac := hmac.New(sha256.New, rootSecret[:])
	_, _ = mac.Write([]byte(externalVoiceRecoveryMACDomain))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func validExternalVoiceRecoveryKind(kind externalVoiceRecoveryKind) bool {
	return kind == externalVoiceRecoveryAnswerIncoming || kind == externalVoiceRecoveryEndActive
}

func validExternalVoiceRecoveryResolution(resolution externalVoiceRecoveryResolution) bool {
	switch resolution {
	case externalVoiceRecoveryApplied, externalVoiceRecoveryRejected, externalVoiceRecoveryProviderEnded:
		return true
	default:
		return false
	}
}

func validExternalVoiceRecoveryHash(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validExternalVoiceRecoveryOpaque(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character == 0x7f {
			return false
		}
	}
	return true
}

func validExternalVoiceRecoveryToken(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func validExternalVoiceRecoveryTime(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !parsed.IsZero() && value == parsed.UTC().Format(time.RFC3339Nano)
}

func externalVoiceRecoveryHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func externalVoiceRecoverySnapshotMatchesRequest(
	snapshot externalVoiceRecoverySnapshot,
	request externalVoiceRecoveryArmRequest,
) bool {
	return snapshot.State == externalVoiceRecoveryArmed && snapshot.RecoveryID == request.RecoveryID &&
		snapshot.Kind == request.Kind && snapshot.OwnerHash == request.OwnerHash &&
		snapshot.CommandKeyHash == request.CommandKeyHash && snapshot.RequestHash == request.RequestHash &&
		snapshot.PublicCallID == request.PublicCallID && snapshot.Generation == request.Generation &&
		snapshot.ExpectedRevision == request.ExpectedRevision &&
		snapshot.PublicMediaLease == request.PublicMediaLease && snapshot.Adapter == request.Adapter &&
		snapshot.GatewayID == request.GatewayID && bytes.Equal(snapshot.ProviderToken, request.ProviderToken)
}

func cloneExternalVoiceRecoverySnapshot(value externalVoiceRecoverySnapshot) externalVoiceRecoverySnapshot {
	copy := value
	copy.ProviderToken = append([]byte(nil), value.ProviderToken...)
	return copy
}

func zeroExternalVoiceRecoverySnapshot(value *externalVoiceRecoverySnapshot) {
	if value == nil {
		return
	}
	for index := range value.ProviderToken {
		value.ProviderToken[index] = 0
	}
	*value = externalVoiceRecoverySnapshot{}
}

func zeroExternalVoiceRecoverySecret(value *[externalVoiceRecoveryRootSecretBytes]byte) {
	if value == nil {
		return
	}
	for index := range value {
		value[index] = 0
	}
}
