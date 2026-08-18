package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	nativeDeviceStoreVersion  = 1
	nativeDeviceEventMaxBytes = 64 << 10
	nativeDeviceMetaMaxBytes  = 8 << 10
	nativeDeviceHeadMaxBytes  = 8 << 10
)

var (
	ErrNativeDeviceStoreClosed   = errors.New("native device store is closed")
	ErrNativeDeviceStoreDegraded = errors.New("native device store is degraded and must be reopened")
)

type nativeDeviceRecord struct {
	DeviceID      string    `json:"device_id"`
	Algorithm     string    `json:"algorithm"`
	PublicKeySPKI string    `json:"public_key_spki"`
	OwnerMAC      string    `json:"owner_hmac"`
	Scopes        []string  `json:"scopes"`
	EnrolledAt    time.Time `json:"enrolled_at"`
	RevokedAt     time.Time `json:"revoked_at,omitempty"`
}

func (r nativeDeviceRecord) active() bool {
	return r.DeviceID != "" && r.RevokedAt.IsZero()
}

func (r nativeDeviceRecord) publicKey() (*ecdsa.PublicKey, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(r.PublicKeySPKI)
	if err != nil {
		return nil, errors.New("native device public key encoding is invalid")
	}
	return parseNativeP256PublicKey(encoded)
}

func (r nativeDeviceRecord) allows(scope string) bool {
	for _, candidate := range r.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

type nativeDeviceStoreMeta struct {
	Version  int       `json:"version"`
	EventKey string    `json:"event_hmac_key"`
	OwnerKey string    `json:"owner_hmac_key"`
	Created  time.Time `json:"created_at"`
}

type nativeDeviceEvent struct {
	Version   int                `json:"version"`
	Sequence  uint64             `json:"sequence"`
	EventID   string             `json:"event_id"`
	Type      string             `json:"type"`
	Device    nativeDeviceRecord `json:"device"`
	CreatedAt time.Time          `json:"created_at"`
	MAC       string             `json:"mac"`
}

// nativeDeviceStoreHead is a durable commitment to the append-only tail. Event
// numbering catches holes in the middle; this independently authenticated head
// also makes loss or deletion of the final enrollment/revocation event fail
// closed on restart.
type nativeDeviceStoreHead struct {
	Version   int       `json:"version"`
	Sequence  uint64    `json:"sequence"`
	EventMAC  string    `json:"event_mac"`
	UpdatedAt time.Time `json:"updated_at"`
	MAC       string    `json:"mac"`
}

type nativeDeviceStore struct {
	mu        sync.RWMutex
	root      string
	eventsDir string
	lockFile  *os.File
	closed    bool
	degraded  bool
	eventKey  [32]byte
	ownerKey  [32]byte
	nextSeq   uint64
	devices   map[string]nativeDeviceRecord
	head      nativeDeviceStoreHead
}

func (*nativeDeviceStore) String() string   { return "nativeDeviceStore{redacted}" }
func (*nativeDeviceStore) GoString() string { return "nativeDeviceStore{redacted}" }

func defaultNativeDeviceStoreRoot() string {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(base, "MacCellular", "remote", "native-devices")
}

func openNativeDeviceStore(root string) (*nativeDeviceStore, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." || !filepath.IsAbs(root) {
		return nil, errors.New("native device store requires an absolute path")
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, fmt.Errorf("prepare native device store: %w", err)
	}
	eventsDir := filepath.Join(root, "events")
	if err := ensurePrivateDirectory(eventsDir); err != nil {
		return nil, fmt.Errorf("prepare native device event directory: %w", err)
	}
	lockFile, err := openSMSStoreLock(filepath.Join(root, "store.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock native device store: %w", err)
	}
	store := &nativeDeviceStore{
		root:      root,
		eventsDir: eventsDir,
		lockFile:  lockFile,
		nextSeq:   1,
		devices:   make(map[string]nativeDeviceRecord),
	}
	if err := store.loadOrCreateMeta(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.loadOrCreateHead(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.rebuild(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *nativeDeviceStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	lockFile := s.lockFile
	s.lockFile = nil
	s.mu.Unlock()
	if lockFile != nil {
		return lockFile.Close()
	}
	return nil
}

func (s *nativeDeviceStore) Enroll(owner string, spki []byte, scopes []string) (nativeDeviceRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return nativeDeviceRecord{}, false, err
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativeDeviceRecord{}, false, err
	}
	publicKey, err := parseNativeP256PublicKey(spki)
	if err != nil {
		return nativeDeviceRecord{}, false, err
	}
	canonicalSPKI, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil || !bytes.Equal(canonicalSPKI, spki) {
		return nativeDeviceRecord{}, false, errors.New("native device public key must use canonical P-256 SPKI")
	}
	scopes, err = normalizeNativeScopes(scopes)
	if err != nil {
		return nativeDeviceRecord{}, false, err
	}
	deviceID := nativeDeviceID(spki)
	record := nativeDeviceRecord{
		DeviceID:      deviceID,
		Algorithm:     "ES256",
		PublicKeySPKI: base64.RawURLEncoding.EncodeToString(spki),
		OwnerMAC:      s.ownerMAC(owner),
		Scopes:        scopes,
	}
	if existing, ok := s.devices[deviceID]; ok {
		// A new, operator-authorized proof with the same key, owner and scopes is
		// an idempotent recovery after a lost completion response. Preserve the
		// original enrollment timestamp and never resurrect a revoked key.
		if existing.active() && nativeDeviceEnrollmentMatches(existing, record) {
			return cloneNativeDeviceRecord(existing), false, nil
		}
		return nativeDeviceRecord{}, false, errors.New("native device identity already exists with different or revoked state")
	}
	record.EnrolledAt = time.Now().UTC()
	stored, err := s.appendLocked("enrolled", record)
	return stored, err == nil, err
}

func (s *nativeDeviceStore) Revoke(deviceID string) (nativeDeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return nativeDeviceRecord{}, err
	}
	record, ok := s.devices[deviceID]
	if !ok {
		return nativeDeviceRecord{}, errors.New("native device not found")
	}
	if !record.RevokedAt.IsZero() {
		return cloneNativeDeviceRecord(record), nil
	}
	record.RevokedAt = time.Now().UTC()
	return s.appendLocked("revoked", record)
}

func (s *nativeDeviceStore) Lookup(deviceID, owner string) (nativeDeviceRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usableLocked(); err != nil {
		return nativeDeviceRecord{}, false, err
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativeDeviceRecord{}, false, err
	}
	record, ok := s.devices[deviceID]
	if !ok || !record.active() || !hmac.Equal([]byte(record.OwnerMAC), []byte(s.ownerMAC(owner))) {
		return nativeDeviceRecord{}, false, nil
	}
	return cloneNativeDeviceRecord(record), true, nil
}

func (s *nativeDeviceStore) Count() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usableLocked(); err != nil {
		return 0, err
	}
	count := 0
	for _, record := range s.devices {
		if record.active() {
			count++
		}
	}
	return count, nil
}

func (s *nativeDeviceStore) loadOrCreateMeta() error {
	path := filepath.Join(s.root, "meta.json")
	data, err := readPrivateRegular(path, nativeDeviceMetaMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := rand.Read(s.eventKey[:]); err != nil {
			return err
		}
		if _, err := rand.Read(s.ownerKey[:]); err != nil {
			return err
		}
		meta := nativeDeviceStoreMeta{
			Version:  nativeDeviceStoreVersion,
			EventKey: base64.RawURLEncoding.EncodeToString(s.eventKey[:]),
			OwnerKey: base64.RawURLEncoding.EncodeToString(s.ownerKey[:]),
			Created:  time.Now().UTC(),
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		if err := atomicPrivateWrite(s.root, path, encoded, syncSMSStoreDirectory); err != nil {
			return fmt.Errorf("create native device store metadata: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read native device store metadata: %w", err)
	}
	var meta nativeDeviceStoreMeta
	if err := decodeStrictJSON(data, &meta); err != nil || meta.Version != nativeDeviceStoreVersion || meta.Created.IsZero() {
		return errors.New("native device store metadata is invalid")
	}
	if err := decodeFixedBase64(meta.EventKey, s.eventKey[:]); err != nil {
		return errors.New("native device event key is invalid")
	}
	if err := decodeFixedBase64(meta.OwnerKey, s.ownerKey[:]); err != nil {
		return errors.New("native device owner key is invalid")
	}
	return nil
}

func (s *nativeDeviceStore) rebuild() error {
	entries, err := os.ReadDir(s.eventsDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp-") {
			if err := validatePrivateTemp(filepath.Join(s.eventsDir, name)); err != nil {
				return fmt.Errorf("unsafe native device temporary file: %w", err)
			}
			continue
		}
		if entry.IsDir() || len(name) != len("00000000000000000001.ndev") || !strings.HasSuffix(name, ".ndev") {
			return fmt.Errorf("unexpected native device store entry %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	lastEventMAC := ""
	for _, name := range names {
		data, err := readPrivateRegular(filepath.Join(s.eventsDir, name), nativeDeviceEventMaxBytes)
		if err != nil {
			return err
		}
		var event nativeDeviceEvent
		if err := decodeStrictJSON(data, &event); err != nil {
			return fmt.Errorf("parse native device event %q: %w", name, err)
		}
		if event.Version != nativeDeviceStoreVersion || event.Sequence != s.nextSeq ||
			name != fmt.Sprintf("%020d.ndev", event.Sequence) || !validOpaqueID(event.EventID, "nde_") ||
			event.CreatedAt.IsZero() || !hmac.Equal([]byte(event.MAC), []byte(s.eventMAC(event))) {
			return fmt.Errorf("native device event %q failed integrity validation", name)
		}
		if err := s.applyEventLocked(event); err != nil {
			return fmt.Errorf("apply native device event %q: %w", name, err)
		}
		lastEventMAC = event.MAC
		s.nextSeq++
	}
	lastSequence := s.nextSeq - 1
	if s.head.Sequence != lastSequence ||
		!hmac.Equal([]byte(s.head.EventMAC), []byte(lastEventMAC)) {
		return errors.New("native device event tail does not match durable head")
	}
	return nil
}

func (s *nativeDeviceStore) appendLocked(eventType string, record nativeDeviceRecord) (nativeDeviceRecord, error) {
	eventID, err := randomOpaqueID("nde_", 16)
	if err != nil {
		return nativeDeviceRecord{}, err
	}
	event := nativeDeviceEvent{
		Version:   nativeDeviceStoreVersion,
		Sequence:  s.nextSeq,
		EventID:   eventID,
		Type:      eventType,
		Device:    cloneNativeDeviceRecord(record),
		CreatedAt: time.Now().UTC(),
	}
	event.MAC = s.eventMAC(event)
	encoded, err := json.Marshal(event)
	if err != nil {
		return nativeDeviceRecord{}, err
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(s.eventsDir, fmt.Sprintf("%020d.ndev", event.Sequence))
	if err := atomicPrivateWrite(s.eventsDir, path, encoded, syncSMSStoreDirectory); err != nil {
		s.degraded = true
		return nativeDeviceRecord{}, fmt.Errorf("append native device event: %w", err)
	}
	head := nativeDeviceStoreHead{
		Version:   nativeDeviceStoreVersion,
		Sequence:  event.Sequence,
		EventMAC:  event.MAC,
		UpdatedAt: time.Now().UTC(),
	}
	head.MAC = s.headMAC(head)
	if err := s.writeHead(head); err != nil {
		s.degraded = true
		return nativeDeviceRecord{}, fmt.Errorf("commit native device event head: %w", err)
	}
	if err := s.applyEventLocked(event); err != nil {
		s.degraded = true
		return nativeDeviceRecord{}, err
	}
	s.head = head
	s.nextSeq++
	return cloneNativeDeviceRecord(record), nil
}

func (s *nativeDeviceStore) applyEventLocked(event nativeDeviceEvent) error {
	record := cloneNativeDeviceRecord(event.Device)
	if !validNativeDeviceID(record.DeviceID) || record.Algorithm != "ES256" || record.EnrolledAt.IsZero() {
		return errors.New("native device record has invalid identity metadata")
	}
	spki, err := base64.RawURLEncoding.DecodeString(record.PublicKeySPKI)
	if err != nil || nativeDeviceID(spki) != record.DeviceID {
		return errors.New("native device ID does not match its public key")
	}
	if _, err := parseNativeP256PublicKey(spki); err != nil {
		return err
	}
	if _, err := normalizeNativeScopes(record.Scopes); err != nil {
		return err
	}
	ownerMAC, err := base64.RawURLEncoding.DecodeString(record.OwnerMAC)
	if err != nil || len(ownerMAC) != sha256.Size {
		return errors.New("native device owner binding is invalid")
	}
	switch event.Type {
	case "enrolled":
		if !record.RevokedAt.IsZero() {
			return errors.New("enrollment event cannot be revoked")
		}
		if _, exists := s.devices[record.DeviceID]; exists {
			return errors.New("duplicate native device enrollment")
		}
	case "revoked":
		existing, exists := s.devices[record.DeviceID]
		if !exists || !existing.active() || record.RevokedAt.IsZero() {
			return errors.New("native device revocation has no active owner")
		}
		if !nativeDeviceRecordsEqualIgnoringRevocation(existing, record) {
			return errors.New("native device revocation changed immutable fields")
		}
	default:
		return errors.New("native device event type is invalid")
	}
	s.devices[record.DeviceID] = record
	return nil
}

func (s *nativeDeviceStore) eventMAC(event nativeDeviceEvent) string {
	mac := hmac.New(sha256.New, s.eventKey[:])
	var sequence [8]byte
	binary.BigEndian.PutUint64(sequence[:], event.Sequence)
	_, _ = mac.Write(sequence[:])
	writeMACField(mac, fmt.Sprint(event.Version))
	writeMACField(mac, event.EventID)
	writeMACField(mac, event.Type)
	writeMACField(mac, event.Device.DeviceID)
	writeMACField(mac, event.Device.Algorithm)
	writeMACField(mac, event.Device.PublicKeySPKI)
	writeMACField(mac, event.Device.OwnerMAC)
	writeMACField(mac, strings.Join(event.Device.Scopes, "\x00"))
	writeMACField(mac, event.Device.EnrolledAt.UTC().Format(time.RFC3339Nano))
	writeMACField(mac, event.Device.RevokedAt.UTC().Format(time.RFC3339Nano))
	writeMACField(mac, event.CreatedAt.UTC().Format(time.RFC3339Nano))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *nativeDeviceStore) ownerMAC(owner string) string {
	mac := hmac.New(sha256.New, s.ownerKey[:])
	_, _ = io.WriteString(mac, owner)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *nativeDeviceStore) loadOrCreateHead() error {
	path := filepath.Join(s.root, "head.json")
	data, err := readPrivateRegular(path, nativeDeviceHeadMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(s.eventsDir)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".tmp-") {
				return errors.New("native device store head is missing while events exist")
			}
		}
		head := nativeDeviceStoreHead{
			Version:   nativeDeviceStoreVersion,
			UpdatedAt: time.Now().UTC(),
		}
		head.MAC = s.headMAC(head)
		if err := s.writeHead(head); err != nil {
			return fmt.Errorf("create native device store head: %w", err)
		}
		s.head = head
		return nil
	}
	if err != nil {
		return fmt.Errorf("read native device store head: %w", err)
	}
	var head nativeDeviceStoreHead
	if err := decodeStrictJSON(data, &head); err != nil ||
		head.Version != nativeDeviceStoreVersion || head.UpdatedAt.IsZero() ||
		!hmac.Equal([]byte(head.MAC), []byte(s.headMAC(head))) {
		return errors.New("native device store head is invalid")
	}
	if head.Sequence == 0 {
		if head.EventMAC != "" {
			return errors.New("empty native device store head has an event MAC")
		}
	} else {
		decoded, err := base64.RawURLEncoding.DecodeString(head.EventMAC)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("native device store head event MAC is invalid")
		}
	}
	s.head = head
	return nil
}

func (s *nativeDeviceStore) writeHead(head nativeDeviceStoreHead) error {
	encoded, err := json.Marshal(head)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return atomicPrivateWrite(
		s.root, filepath.Join(s.root, "head.json"), encoded, syncSMSStoreDirectory)
}

func (s *nativeDeviceStore) headMAC(head nativeDeviceStoreHead) string {
	mac := hmac.New(sha256.New, s.eventKey[:])
	writeMACField(mac, fmt.Sprint(head.Version))
	writeMACField(mac, fmt.Sprint(head.Sequence))
	writeMACField(mac, head.EventMAC)
	writeMACField(mac, head.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *nativeDeviceStore) usableLocked() error {
	if s == nil || s.closed {
		return ErrNativeDeviceStoreClosed
	}
	if s.degraded {
		return ErrNativeDeviceStoreDegraded
	}
	return nil
}

func parseNativeP256PublicKey(spki []byte) (*ecdsa.PublicKey, error) {
	if len(spki) == 0 || len(spki) > 1024 {
		return nil, errors.New("native device public key is missing or oversized")
	}
	parsed, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, errors.New("native device public key is not valid SPKI")
	}
	publicKey, ok := parsed.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve == nil || publicKey.Curve.Params().Name != "P-256" ||
		publicKey.X == nil || publicKey.Y == nil || !publicKey.Curve.IsOnCurve(publicKey.X, publicKey.Y) {
		return nil, errors.New("native device public key must be P-256")
	}
	return publicKey, nil
}

func nativeDeviceID(spki []byte) string {
	digest := sha256.Sum256(spki)
	return "dev_" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func validNativeDeviceID(value string) bool {
	if !strings.HasPrefix(value, "dev_") {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "dev_"))
	return err == nil && len(decoded) == sha256.Size
}

func normalizeNativeOwner(owner string) (string, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" || len(owner) > 320 || strings.ContainsRune(owner, 0) {
		return "", errors.New("native device owner is invalid")
	}
	return owner, nil
}

func normalizeNativeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > 3 {
		return nil, errors.New("native device scopes are missing or oversized")
	}
	seen := make(map[string]struct{}, len(scopes))
	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope != "status.read" && scope != "sms.read" && scope != "sms.send" {
			return nil, errors.New("native device scope is not allowed")
		}
		if _, exists := seen[scope]; exists {
			return nil, errors.New("native device scope is duplicated")
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	sort.Strings(result)
	return result, nil
}

func nativeDeviceEnrollmentMatches(existing, candidate nativeDeviceRecord) bool {
	return existing.DeviceID == candidate.DeviceID && existing.Algorithm == candidate.Algorithm &&
		existing.PublicKeySPKI == candidate.PublicKeySPKI &&
		hmac.Equal([]byte(existing.OwnerMAC), []byte(candidate.OwnerMAC)) &&
		strings.Join(existing.Scopes, "\x00") == strings.Join(candidate.Scopes, "\x00")
}

func nativeDeviceRecordsEqualIgnoringRevocation(existing, candidate nativeDeviceRecord) bool {
	return existing.DeviceID == candidate.DeviceID && existing.Algorithm == candidate.Algorithm &&
		existing.PublicKeySPKI == candidate.PublicKeySPKI &&
		hmac.Equal([]byte(existing.OwnerMAC), []byte(candidate.OwnerMAC)) &&
		strings.Join(existing.Scopes, "\x00") == strings.Join(candidate.Scopes, "\x00") &&
		existing.EnrolledAt.Equal(candidate.EnrolledAt)
}

func cloneNativeDeviceRecord(record nativeDeviceRecord) nativeDeviceRecord {
	copy := record
	copy.Scopes = append([]string(nil), record.Scopes...)
	return copy
}
