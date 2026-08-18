package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	smsStoreVersion       = 1
	smsStoreCursorVersion = 1
	smsStoreMaxPeerBytes  = 256
	smsStoreMaxCodeBytes  = 256
	smsStoreMaxBodyBytes  = 64 << 10
	smsStoreMaxFileBytes  = 256 << 10
	smsStoreDefaultLimit  = 100
	smsStoreMaximumLimit  = 500
	smsCursorKindDelta    = 0
	smsCursorKindSnapshot = 1
)

var (
	ErrSMSCursorInvalid        = errors.New("invalid SMS cursor")
	ErrSMSCursorResetRequired  = errors.New("SMS cursor belongs to a different store epoch; reset required")
	ErrSMSStoreClosed          = errors.New("SMS store is closed")
	ErrSMSStoreDegraded        = errors.New("SMS store is degraded and must be reopened")
	ErrSMSStoreCommitUncertain = errors.New("SMS event rename completed but directory durability is uncertain")

	smsEventNamePattern = regexp.MustCompile(`^([0-9]{20})\.evt$`)
)

type StoreMessage struct {
	ID        string    `json:"id"`
	EventID   string    `json:"event_id"`
	Direction string    `json:"direction"`
	Status    string    `json:"status"`
	Peer      string    `json:"peer"`
	Content   string    `json:"content"`
	Code      string    `json:"code,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Segments  int       `json:"segments,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SMSSyncResult struct {
	Messages  []StoreMessage `json:"messages"`
	Cursor    string         `json:"cursor"`
	Bootstrap bool           `json:"bootstrap"`
	HasMore   bool           `json:"has_more"`
}

type smsCursorState struct {
	Kind      byte
	HighWater uint64
	Position  uint64
}

type smsStoreMeta struct {
	Version   int       `json:"version"`
	Epoch     string    `json:"epoch"`
	CursorKey string    `json:"cursor_key"`
	DedupeKey string    `json:"dedupe_key"`
	CreatedAt time.Time `json:"created_at"`
}

type smsStoreEvent struct {
	Version   int          `json:"version"`
	Sequence  uint64       `json:"sequence"`
	EventID   string       `json:"event_id"`
	Message   StoreMessage `json:"message"`
	DedupeMAC string       `json:"dedupe_mac,omitempty"`
	MAC       string       `json:"mac"`
}

type smsStoredState struct {
	Message  StoreMessage
	Sequence uint64
}

type smsStore struct {
	mu        sync.RWMutex
	root      string
	eventsDir string
	lockFile  *os.File
	closed    bool
	degraded  bool
	syncDir   func(string) error
	epoch     [16]byte
	cursorKey [32]byte
	dedupeKey [32]byte
	nextSeq   uint64
	messages  map[string]smsStoredState
	dedupe    map[string]string
	events    []smsStoreEvent
}

func openSMSStore(root string) (*smsStore, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" {
		return nil, errors.New("SMS store root is required")
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, fmt.Errorf("prepare SMS store root: %w", err)
	}
	eventsDir := filepath.Join(root, "events")
	if err := ensurePrivateDirectory(eventsDir); err != nil {
		return nil, fmt.Errorf("prepare SMS event directory: %w", err)
	}

	lockPath := filepath.Join(root, "store.lock")
	if info, err := os.Lstat(lockPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SMS store lock must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect SMS store lock: %w", err)
	}
	lockFile, err := openSMSStoreLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("lock SMS store: %w", err)
	}

	s := &smsStore{
		root:      root,
		eventsDir: eventsDir,
		lockFile:  lockFile,
		syncDir:   syncSMSStoreDirectory,
		nextSeq:   1,
		messages:  make(map[string]smsStoredState),
		dedupe:    make(map[string]string),
	}
	if err := validateSMSStoreTemps(root); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("validate SMS store temporary files: %w", err)
	}
	if err := s.loadOrCreateMeta(); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.rebuild(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *smsStore) IngestIncoming(peer, content, code string, timestamp time.Time) (StoreMessage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return StoreMessage{}, false, err
	}
	peer, content, code, timestamp, err := validateSMSInput(peer, content, code, timestamp)
	if err != nil {
		return StoreMessage{}, false, err
	}
	dedupeMAC := s.incomingDedupeMAC(peer, content, code, timestamp)
	if id, ok := s.dedupe[dedupeMAC]; ok {
		state, exists := s.messages[id]
		if !exists {
			return StoreMessage{}, false, errors.New("SMS dedupe index is inconsistent")
		}
		return state.Message, false, nil
	}
	now := time.Now().UTC()
	messageID, err := randomOpaqueID("msg_", 16)
	if err != nil {
		return StoreMessage{}, false, err
	}
	message := StoreMessage{
		ID:        messageID,
		Direction: "incoming",
		Status:    "received",
		Peer:      peer,
		Content:   content,
		Code:      code,
		Timestamp: timestamp,
		UpdatedAt: now,
	}
	stored, err := s.appendLocked(message, dedupeMAC)
	return stored, err == nil, err
}

func (s *smsStore) BeginOutgoing(peer, content string, timestamp time.Time) (StoreMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return StoreMessage{}, err
	}
	peer, content, _, timestamp, err := validateSMSInput(peer, content, "", timestamp)
	if err != nil {
		return StoreMessage{}, err
	}
	messageID, err := randomOpaqueID("msg_", 16)
	if err != nil {
		return StoreMessage{}, err
	}
	message := StoreMessage{
		ID:        messageID,
		Direction: "outgoing",
		Status:    "pending",
		Peer:      peer,
		Content:   content,
		Timestamp: timestamp,
		UpdatedAt: time.Now().UTC(),
	}
	return s.appendLocked(message, "")
}

func (s *smsStore) UpdateStatus(id, status string, segments int) (StoreMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return StoreMessage{}, err
	}
	state, ok := s.messages[id]
	if !ok {
		return StoreMessage{}, errors.New("SMS message not found")
	}
	if state.Message.Direction != "outgoing" {
		return StoreMessage{}, errors.New("only outgoing SMS status can be updated")
	}
	status = strings.TrimSpace(status)
	if !validOutgoingStatus(status) {
		return StoreMessage{}, errors.New("invalid outgoing SMS status")
	}
	if segments < 0 || segments > 255 {
		return StoreMessage{}, errors.New("SMS segment count is out of range")
	}
	if status == "submitted" && segments == 0 {
		return StoreMessage{}, errors.New("submitted SMS requires a positive segment count")
	}
	if state.Message.Status == status && state.Message.Segments == segments {
		return state.Message, nil
	}
	if !validStatusTransition(state.Message.Status, status) {
		return StoreMessage{}, fmt.Errorf("invalid SMS status transition from %s to %s", state.Message.Status, status)
	}
	message := state.Message
	message.Status = status
	message.Segments = segments
	message.UpdatedAt = time.Now().UTC()
	return s.appendLocked(message, "")
}

// RecoverPending resolves send attempts which were durably recorded before a
// restart but have no durable completion result. It intentionally records
// unknown rather than guessing whether the modem submitted or rejected them.
func (s *smsStore) RecoverPending() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usableLocked(); err != nil {
		return err
	}
	pending := make([]smsStoredState, 0)
	for _, state := range s.messages {
		if state.Message.Direction == "outgoing" && state.Message.Status == "pending" {
			pending = append(pending, state)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Sequence < pending[j].Sequence })
	recoveredAt := time.Now().UTC()
	for _, state := range pending {
		message := state.Message
		message.Status = "unknown"
		message.UpdatedAt = recoveredAt
		if _, err := s.appendLocked(message, ""); err != nil {
			s.degraded = true
			return fmt.Errorf("recover pending SMS %s: %w", message.ID, err)
		}
	}
	return nil
}

func (s *smsStore) List(limit int) ([]StoreMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usableLocked(); err != nil {
		return nil, err
	}
	limit, err := normalizeSMSLimit(limit)
	if err != nil {
		return nil, err
	}
	states := make([]smsStoredState, 0, len(s.messages))
	for _, state := range s.messages {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Sequence > states[j].Sequence })
	if len(states) > limit {
		states = states[:limit]
	}
	result := make([]StoreMessage, len(states))
	for i := range states {
		result[i] = states[i].Message
	}
	return result, nil
}

func (s *smsStore) Sync(cursor string, limit int) (SMSSyncResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usableLocked(); err != nil {
		return SMSSyncResult{}, err
	}
	limit, err := normalizeSMSLimit(limit)
	if err != nil {
		return SMSSyncResult{}, err
	}
	if strings.TrimSpace(cursor) == "" {
		highWater := s.nextSeq - 1
		return s.snapshotPageLocked(highWater, 0, limit)
	}
	cursorState, err := s.decodeCursorLocked(cursor)
	if err != nil {
		return SMSSyncResult{}, err
	}
	highWater := s.nextSeq - 1
	if cursorState.HighWater > highWater {
		return SMSSyncResult{}, ErrSMSCursorInvalid
	}
	if cursorState.Kind == smsCursorKindSnapshot {
		return s.snapshotPageLocked(cursorState.HighWater, cursorState.Position, limit)
	}
	sequence := cursorState.HighWater
	start := sort.Search(len(s.events), func(i int) bool { return s.events[i].Sequence > sequence })
	end := start + limit
	if end > len(s.events) {
		end = len(s.events)
	}
	messages := make([]StoreMessage, 0, end-start)
	lastSequence := sequence
	for _, event := range s.events[start:end] {
		messages = append(messages, event.Message)
		lastSequence = event.Sequence
	}
	return SMSSyncResult{
		Messages: messages,
		Cursor:   s.encodeCursorLocked(lastSequence),
		HasMore:  end < len(s.events),
	}, nil
}

func (s *smsStore) snapshotPageLocked(highWater, position uint64, limit int) (SMSSyncResult, error) {
	statesByID := make(map[string]smsStoredState)
	for _, event := range s.events {
		if event.Sequence > highWater {
			break
		}
		statesByID[event.Message.ID] = smsStoredState{Message: event.Message, Sequence: event.Sequence}
	}
	states := make([]smsStoredState, 0, len(statesByID))
	for _, state := range statesByID {
		if position == 0 || state.Sequence < position {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Sequence > states[j].Sequence })
	hasMore := len(states) > limit
	if hasMore {
		states = states[:limit]
	}
	messages := make([]StoreMessage, len(states))
	for i := range states {
		messages[i] = states[i].Message
	}
	nextCursor := s.encodeCursorLocked(highWater)
	if hasMore {
		nextCursor = s.encodeSnapshotCursorLocked(highWater, states[len(states)-1].Sequence)
	}
	return SMSSyncResult{
		Messages:  messages,
		Cursor:    nextCursor,
		Bootstrap: true,
		HasMore:   hasMore,
	}, nil
}

func (s *smsStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.lockFile == nil {
		return nil
	}
	err := s.lockFile.Close()
	s.lockFile = nil
	return err
}

func (s *smsStore) loadOrCreateMeta() error {
	path := filepath.Join(s.root, "meta.json")
	data, err := readPrivateRegular(path, smsStoreMaxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		var epoch [16]byte
		var cursorKey, dedupeKey [32]byte
		if _, err := rand.Read(epoch[:]); err != nil {
			return fmt.Errorf("generate SMS store epoch: %w", err)
		}
		if _, err := rand.Read(cursorKey[:]); err != nil {
			return fmt.Errorf("generate SMS cursor key: %w", err)
		}
		if _, err := rand.Read(dedupeKey[:]); err != nil {
			return fmt.Errorf("generate SMS dedupe key: %w", err)
		}
		meta := smsStoreMeta{
			Version:   smsStoreVersion,
			Epoch:     base64.RawURLEncoding.EncodeToString(epoch[:]),
			CursorKey: base64.RawURLEncoding.EncodeToString(cursorKey[:]),
			DedupeKey: base64.RawURLEncoding.EncodeToString(dedupeKey[:]),
			CreatedAt: time.Now().UTC(),
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		encoded = append(encoded, '\n')
		if err := atomicPrivateWrite(s.root, path, encoded, syncSMSStoreDirectory); err != nil {
			return fmt.Errorf("create SMS store metadata: %w", err)
		}
		data = encoded
	} else if err != nil {
		return fmt.Errorf("read SMS store metadata: %w", err)
	}
	var meta smsStoreMeta
	if err := decodeStrictJSON(data, &meta); err != nil {
		return fmt.Errorf("decode SMS store metadata: %w", err)
	}
	if meta.Version != smsStoreVersion || meta.CreatedAt.IsZero() {
		return errors.New("unsupported or invalid SMS store metadata")
	}
	if err := decodeFixedBase64(meta.Epoch, s.epoch[:]); err != nil {
		return fmt.Errorf("decode SMS store epoch: %w", err)
	}
	if err := decodeFixedBase64(meta.CursorKey, s.cursorKey[:]); err != nil {
		return fmt.Errorf("decode SMS cursor key: %w", err)
	}
	if err := decodeFixedBase64(meta.DedupeKey, s.dedupeKey[:]); err != nil {
		return fmt.Errorf("decode SMS dedupe key: %w", err)
	}
	return nil
}

func (s *smsStore) rebuild() error {
	entries, err := os.ReadDir(s.eventsDir)
	if err != nil {
		return fmt.Errorf("read SMS event directory: %w", err)
	}
	type namedEvent struct {
		sequence uint64
		name     string
	}
	names := make([]namedEvent, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp-") {
			if err := validatePrivateTemp(filepath.Join(s.eventsDir, name)); err != nil {
				return fmt.Errorf("unsafe SMS temporary event %q: %w", name, err)
			}
			continue
		}
		match := smsEventNamePattern.FindStringSubmatch(name)
		if match == nil {
			return fmt.Errorf("unexpected file in SMS event directory: %q", name)
		}
		var sequence uint64
		if _, err := fmt.Sscanf(match[1], "%d", &sequence); err != nil || sequence == 0 {
			return fmt.Errorf("invalid SMS event filename: %q", name)
		}
		names = append(names, namedEvent{sequence: sequence, name: name})
	}
	sort.Slice(names, func(i, j int) bool { return names[i].sequence < names[j].sequence })
	for i, named := range names {
		expected := uint64(i + 1)
		if named.sequence != expected {
			return fmt.Errorf("SMS event sequence gap: expected %d, found %d", expected, named.sequence)
		}
		data, err := readPrivateRegular(filepath.Join(s.eventsDir, named.name), smsStoreMaxFileBytes)
		if err != nil {
			return fmt.Errorf("read SMS event %d: %w", named.sequence, err)
		}
		var event smsStoreEvent
		if err := decodeStrictJSON(data, &event); err != nil {
			return fmt.Errorf("decode SMS event %d: %w", named.sequence, err)
		}
		if err := s.validateEvent(event, named.sequence); err != nil {
			return fmt.Errorf("validate SMS event %d: %w", named.sequence, err)
		}
		s.applyEvent(event)
		s.events = append(s.events, event)
		s.nextSeq = event.Sequence + 1
	}
	return nil
}

func (s *smsStore) appendLocked(message StoreMessage, dedupeMAC string) (StoreMessage, error) {
	eventID, err := randomOpaqueID("evt_", 16)
	if err != nil {
		return StoreMessage{}, err
	}
	message.EventID = eventID
	event := smsStoreEvent{
		Version:   smsStoreVersion,
		Sequence:  s.nextSeq,
		EventID:   eventID,
		Message:   message,
		DedupeMAC: dedupeMAC,
	}
	mac, err := s.eventMAC(event)
	if err != nil {
		return StoreMessage{}, err
	}
	event.MAC = mac
	encoded, err := json.Marshal(event)
	if err != nil {
		return StoreMessage{}, fmt.Errorf("encode SMS event: %w", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(s.eventsDir, fmt.Sprintf("%020d.evt", event.Sequence))
	if err := atomicPrivateWrite(s.eventsDir, path, encoded, s.syncDir); err != nil {
		if errors.Is(err, ErrSMSStoreCommitUncertain) {
			s.degraded = true
		}
		return StoreMessage{}, fmt.Errorf("persist SMS event: %w", err)
	}
	s.applyEvent(event)
	s.events = append(s.events, event)
	s.nextSeq++
	return message, nil
}

func (s *smsStore) applyEvent(event smsStoreEvent) {
	s.messages[event.Message.ID] = smsStoredState{Message: event.Message, Sequence: event.Sequence}
	if event.DedupeMAC != "" {
		s.dedupe[event.DedupeMAC] = event.Message.ID
	}
}

func (s *smsStore) validateEvent(event smsStoreEvent, expectedSequence uint64) error {
	if event.Version != smsStoreVersion || event.Sequence != expectedSequence {
		return errors.New("event version or sequence is invalid")
	}
	if !validOpaqueID(event.EventID, "evt_") || event.Message.EventID != event.EventID || !validOpaqueID(event.Message.ID, "msg_") {
		return errors.New("event or message ID is invalid")
	}
	if event.Message.Timestamp.IsZero() || event.Message.UpdatedAt.IsZero() {
		return errors.New("message timestamp is invalid")
	}
	if _, _, _, _, err := validateSMSInput(event.Message.Peer, event.Message.Content, event.Message.Code, event.Message.Timestamp); err != nil {
		return err
	}
	if event.Message.Direction == "incoming" {
		if event.Message.Status != "received" || event.Message.Segments != 0 || event.DedupeMAC == "" {
			return errors.New("incoming SMS event state is invalid")
		}
		expected := s.incomingDedupeMAC(event.Message.Peer, event.Message.Content, event.Message.Code, event.Message.Timestamp)
		if !hmac.Equal([]byte(event.DedupeMAC), []byte(expected)) {
			return errors.New("incoming SMS dedupe MAC is invalid")
		}
	} else if event.Message.Direction == "outgoing" {
		if !validOutgoingStatus(event.Message.Status) || event.DedupeMAC != "" || event.Message.Segments < 0 || event.Message.Segments > 255 {
			return errors.New("outgoing SMS event state is invalid")
		}
		if event.Message.Status == "submitted" && event.Message.Segments == 0 {
			return errors.New("submitted SMS event has no segments")
		}
	} else {
		return errors.New("SMS direction is invalid")
	}
	if old, ok := s.messages[event.Message.ID]; ok {
		if old.Message.Direction != event.Message.Direction || old.Message.Peer != event.Message.Peer || old.Message.Content != event.Message.Content || old.Message.Code != event.Message.Code || !old.Message.Timestamp.Equal(event.Message.Timestamp) {
			return errors.New("SMS immutable fields changed")
		}
		if !validStatusTransition(old.Message.Status, event.Message.Status) && (old.Message.Status != event.Message.Status || old.Message.Segments != event.Message.Segments) {
			return errors.New("SMS status transition is invalid")
		}
	} else if event.Message.Direction == "outgoing" && event.Message.Status != "pending" {
		return errors.New("outgoing SMS must begin in pending state")
	}
	expectedMAC, err := s.eventMAC(event)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(event.MAC), []byte(expectedMAC)) {
		return errors.New("SMS event MAC is invalid")
	}
	return nil
}

func (s *smsStore) eventMAC(event smsStoreEvent) (string, error) {
	event.MAC = ""
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.cursorKey[:])
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *smsStore) incomingDedupeMAC(peer, content, code string, timestamp time.Time) string {
	mac := hmac.New(sha256.New, s.dedupeKey[:])
	writeMACField(mac, peer)
	writeMACField(mac, content)
	writeMACField(mac, code)
	var timestampBytes [8]byte
	binary.BigEndian.PutUint64(timestampBytes[:], uint64(timestamp.UnixNano()))
	_, _ = mac.Write(timestampBytes[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *smsStore) encodeCursorLocked(sequence uint64) string {
	return s.encodeCursorStateLocked(smsCursorState{Kind: smsCursorKindDelta, HighWater: sequence})
}

func (s *smsStore) encodeSnapshotCursorLocked(highWater, position uint64) string {
	return s.encodeCursorStateLocked(smsCursorState{Kind: smsCursorKindSnapshot, HighWater: highWater, Position: position})
}

func (s *smsStore) encodeCursorStateLocked(state smsCursorState) string {
	payload := make([]byte, 1+len(s.epoch)+1+8+8)
	payload[0] = smsStoreCursorVersion
	copy(payload[1:17], s.epoch[:])
	payload[17] = state.Kind
	binary.BigEndian.PutUint64(payload[18:26], state.HighWater)
	binary.BigEndian.PutUint64(payload[26:34], state.Position)
	mac := hmac.New(sha256.New, s.cursorKey[:])
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(append(payload, mac.Sum(nil)...))
}

func (s *smsStore) decodeCursorLocked(cursor string) (smsCursorState, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil || len(decoded) != 1+16+1+8+8+sha256.Size || decoded[0] != smsStoreCursorVersion {
		return smsCursorState{}, ErrSMSCursorInvalid
	}
	payload, suppliedMAC := decoded[:34], decoded[34:]
	mac := hmac.New(sha256.New, s.cursorKey[:])
	_, _ = mac.Write(payload)
	if !hmac.Equal(suppliedMAC, mac.Sum(nil)) {
		return smsCursorState{}, ErrSMSCursorInvalid
	}
	if !hmac.Equal(payload[1:17], s.epoch[:]) {
		return smsCursorState{}, ErrSMSCursorResetRequired
	}
	state := smsCursorState{
		Kind:      payload[17],
		HighWater: binary.BigEndian.Uint64(payload[18:26]),
		Position:  binary.BigEndian.Uint64(payload[26:34]),
	}
	if state.Kind == smsCursorKindDelta {
		if state.Position != 0 {
			return smsCursorState{}, ErrSMSCursorInvalid
		}
	} else if state.Kind == smsCursorKindSnapshot {
		if state.Position == 0 || state.Position > state.HighWater {
			return smsCursorState{}, ErrSMSCursorInvalid
		}
	} else {
		return smsCursorState{}, ErrSMSCursorInvalid
	}
	return state, nil
}

func (s *smsStore) usableLocked() error {
	if s.closed {
		return ErrSMSStoreClosed
	}
	if s.degraded {
		return ErrSMSStoreDegraded
	}
	return nil
}

func validateSMSInput(peer, content, code string, timestamp time.Time) (string, string, string, time.Time, error) {
	peer = strings.TrimSpace(peer)
	if peer == "" || len(peer) > smsStoreMaxPeerBytes || strings.ContainsRune(peer, 0) {
		return "", "", "", time.Time{}, errors.New("SMS peer is invalid")
	}
	if strings.TrimSpace(content) == "" || len(content) > smsStoreMaxBodyBytes || strings.ContainsRune(content, 0) {
		return "", "", "", time.Time{}, errors.New("SMS content is invalid")
	}
	code = strings.TrimSpace(code)
	if len(code) > smsStoreMaxCodeBytes || strings.ContainsRune(code, 0) {
		return "", "", "", time.Time{}, errors.New("SMS code is invalid")
	}
	if timestamp.IsZero() {
		return "", "", "", time.Time{}, errors.New("SMS timestamp is required")
	}
	return peer, content, code, timestamp.UTC(), nil
}

func validOutgoingStatus(status string) bool {
	switch status {
	case "pending", "submitted", "failed", "unknown":
		return true
	default:
		return false
	}
}

func validStatusTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case "pending":
		return to == "submitted" || to == "failed" || to == "unknown"
	case "unknown":
		return to == "submitted" || to == "failed"
	default:
		return false
	}
}

func normalizeSMSLimit(limit int) (int, error) {
	if limit == 0 {
		return smsStoreDefaultLimit, nil
	}
	if limit < 0 || limit > smsStoreMaximumLimit {
		return 0, fmt.Errorf("SMS limit must be between 1 and %d", smsStoreMaximumLimit)
	}
	return limit, nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path must be a real directory, not a symlink")
	}
	return os.Chmod(path, 0o700)
}

func atomicPrivateWrite(dir, finalPath string, data []byte, syncDir func(string) error) error {
	tempID, err := randomOpaqueID(".tmp-", 12)
	if err != nil {
		return err
	}
	tempPath := filepath.Join(dir, tempID)
	file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := renameSMSStoreFile(tempPath, finalPath); err != nil {
		return err
	}
	removeTemp = false
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %v", ErrSMSStoreCommitUncertain, err)
	}
	return nil
}

func validateSMSStoreTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		if err := validatePrivateTemp(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("unsafe temporary file %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func randomOpaqueID(prefix string, byteCount int) (string, error) {
	random := make([]byte, byteCount)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func validOpaqueID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == 16
}

func decodeFixedBase64(value string, destination []byte) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	if len(decoded) != len(destination) {
		return errors.New("decoded value has unexpected length")
	}
	copy(destination, decoded)
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func writeMACField(writer io.Writer, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = io.WriteString(writer, value)
}
