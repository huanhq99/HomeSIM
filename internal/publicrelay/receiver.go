package publicrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
)

const (
	DefaultReceiveWindow uint64 = 64
	MaxReceiveWindow     uint64 = 256
)

// ReceiveDisposition distinguishes a newly durably accepted message from an
// exact authenticated duplicate. A duplicate must never re-execute a mutation;
// it must be reconciled against the separate durable operation journal.
type ReceiveDisposition string

const (
	ReceiveNew            ReceiveDisposition = "new"
	ReceiveExactDuplicate ReceiveDisposition = "exact_duplicate"
)

type replayEntry struct {
	sequence       uint64
	messageID      string
	ciphertextHash [sha256.Size]byte
}

// ReceiverState is an authenticated replay-window snapshot returned only by
// DecodeReceiverState. Generic JSON and formatting are redacted. New snapshots
// are emitted only through the mandatory ReceiverStateCommitter transaction.
type ReceiverState struct {
	version             uint64
	routeID             string
	keyEpoch            uint64
	direction           Direction
	verifierFingerprint [sha256.Size]byte
	window              uint64
	highWater           uint64
	generation          uint64
	previousStateID     [sha256.Size]byte
	stateID             [sha256.Size]byte
	entries             []replayEntry
}

func (state ReceiverState) Direction() Direction { return state.direction }
func (state ReceiverState) Window() uint64       { return state.window }
func (state ReceiverState) HighWater() uint64    { return state.highWater }
func (state ReceiverState) Generation() uint64   { return state.generation }
func (state ReceiverState) EntryCount() int      { return len(state.entries) }

// ReceiverStateCommitter is an integration boundary, not a storage
// implementation. A successful call MUST atomically compare both expected
// generation and state ID, durably replace the stored bytes, perform the
// platform's fsync-equivalent durability barrier, and only then return nil.
// It MUST return ErrStateCAS when the expected state is no longer current.
// Any other result is treated as an ambiguous commit failure and no plaintext
// is released by Receiver. This CAS prevents concurrent stale writes only while
// the authoritative store retains and atomically compares its latest generation
// and state ID. It cannot detect an offline rollback that restores both the
// state bytes and CAS metadata to one older, internally consistent pair.
type ReceiverStateCommitter interface {
	CommitReceiverState(context.Context, ReceiverStateCommit) error
}

// ReceiverStateCommit is an immutable CAS request. Its fields and generic
// formatting are private/redacted; explicit accessors return detached values
// to the storage adapter.
type ReceiverStateCommit struct {
	expectedGeneration uint64
	expectedStateID    [sha256.Size]byte
	nextGeneration     uint64
	nextStateID        [sha256.Size]byte
	encodedState       []byte
}

func (commit ReceiverStateCommit) ExpectedGeneration() uint64 { return commit.expectedGeneration }
func (commit ReceiverStateCommit) NextGeneration() uint64     { return commit.nextGeneration }
func (commit ReceiverStateCommit) ExpectedStateID() string {
	return base64.RawURLEncoding.EncodeToString(commit.expectedStateID[:])
}
func (commit ReceiverStateCommit) NextStateID() string {
	return base64.RawURLEncoding.EncodeToString(commit.nextStateID[:])
}
func (commit ReceiverStateCommit) EncodedState() []byte {
	return append([]byte(nil), commit.encodedState...)
}

// VerifiedMessage is a non-zero device-origin plaintext created only after the
// receiver's pinned device key, AEAD, schema, replay, and durable-CAS gates all
// succeed. Its fields are private.
type VerifiedMessage struct {
	message Message
	binding verifiedBinding
	valid   bool
}

// Message returns the typed c2g message only for a valid verified wrapper.
func (verified VerifiedMessage) Message() (Message, bool) {
	if !verified.valid || !verified.binding.valid || verified.binding.direction != ClientToGateway {
		return Message{}, false
	}
	return verified.message, true
}

// VerifiedGatewayMessage is a non-zero gateway-origin plaintext created only
// after the receiver's pinned gateway key, AEAD, schema, replay, and durable-
// CAS gates all succeed. Raw inspect results are intentionally not exposed.
type VerifiedGatewayMessage struct {
	message Message
	binding verifiedBinding
	valid   bool
}

func (verified VerifiedGatewayMessage) Kind() (MessageKind, bool) {
	if !verified.valid || !verified.binding.valid || verified.binding.direction != GatewayToClient {
		return "", false
	}
	return verified.binding.kind, true
}

type verifiedBinding struct {
	valid               bool
	routeID             string
	keyEpoch            uint64
	direction           Direction
	verifierFingerprint [sha256.Size]byte
	keyContextID        [sha256.Size]byte
	kind                MessageKind
	operationID         string
	commitment          Commitment
	outcome             InspectAndFenceOutcome
}

// FencedAbsentExpectation binds a recovery request to one exact g2c receiver
// route, epoch, derived key context, and pinned gateway verifier.
type FencedAbsentExpectation struct {
	valid               bool
	routeID             string
	keyEpoch            uint64
	verifierFingerprint [sha256.Size]byte
	keyContextID        [sha256.Size]byte
	operationID         string
	commitment          Commitment
}

// ConfirmsFencedAbsent is the only public absent-unlock predicate. A zero
// wrapper/expectation, a different route/epoch/key/kind/operation/commitment,
// or any outcome other than fenced_absent returns false.
func (verified VerifiedGatewayMessage) ConfirmsFencedAbsent(
	expectation FencedAbsentExpectation,
) bool {
	if !verified.valid || !verified.binding.valid || !expectation.valid ||
		verified.binding.direction != GatewayToClient ||
		verified.binding.kind != MessageSMSInspectAndFenceResult ||
		verified.binding.outcome != OperationFencedAbsent ||
		verified.binding.routeID != expectation.routeID ||
		verified.binding.keyEpoch != expectation.keyEpoch ||
		subtle.ConstantTimeCompare(
			verified.binding.verifierFingerprint[:], expectation.verifierFingerprint[:],
		) != 1 ||
		subtle.ConstantTimeCompare(verified.binding.keyContextID[:], expectation.keyContextID[:]) != 1 ||
		subtle.ConstantTimeCompare([]byte(verified.binding.operationID), []byte(expectation.operationID)) != 1 {
		return false
	}
	return verified.binding.commitment.Equal(expectation.commitment)
}

// Receiver pins one exact deep-copied P-256 verifier key, traffic direction,
// route, epoch, replay window, and mandatory durable committer.
type Receiver struct {
	mu                  sync.Mutex
	keys                KeySet
	direction           Direction
	verifier            *ecdsa.PublicKey
	verifierFingerprint [sha256.Size]byte
	keyContextID        [sha256.Size]byte
	committer           ReceiverStateCommitter
	state               ReceiverState
}

// NewDeviceReceiver constructs a c2g receiver pinned to one device verifier.
func NewDeviceReceiver(
	keys KeySet,
	window uint64,
	deviceVerifier *ecdsa.PublicKey,
	committer ReceiverStateCommitter,
) (*Receiver, error) {
	return newReceiver(keys, ClientToGateway, window, deviceVerifier, committer, ReceiverState{}, false)
}

// NewGatewayReceiver constructs a g2c receiver pinned to one gateway verifier.
func NewGatewayReceiver(
	keys KeySet,
	window uint64,
	gatewayVerifier *ecdsa.PublicKey,
	committer ReceiverStateCommitter,
) (*Receiver, error) {
	return newReceiver(keys, GatewayToClient, window, gatewayVerifier, committer, ReceiverState{}, false)
}

// NewDeviceReceiverFromState restores a c2g receiver from authenticated state.
func NewDeviceReceiverFromState(
	keys KeySet,
	state ReceiverState,
	deviceVerifier *ecdsa.PublicKey,
	committer ReceiverStateCommitter,
) (*Receiver, error) {
	return newReceiver(keys, ClientToGateway, state.window, deviceVerifier, committer, state, true)
}

// NewGatewayReceiverFromState restores a g2c receiver from authenticated state.
func NewGatewayReceiverFromState(
	keys KeySet,
	state ReceiverState,
	gatewayVerifier *ecdsa.PublicKey,
	committer ReceiverStateCommitter,
) (*Receiver, error) {
	return newReceiver(keys, GatewayToClient, state.window, gatewayVerifier, committer, state, true)
}

func newReceiver(
	keys KeySet,
	direction Direction,
	window uint64,
	verifier *ecdsa.PublicKey,
	committer ReceiverStateCommitter,
	restored ReceiverState,
	hasRestored bool,
) (*Receiver, error) {
	if err := validateKeySet(keys); err != nil || nilReceiverStateCommitter(committer) ||
		(direction != ClientToGateway && direction != GatewayToClient) ||
		window == 0 || window > MaxReceiveWindow {
		return nil, ErrMalformed
	}
	pinned, fingerprint, _, err := cloneVerifierKey(verifier)
	if err != nil {
		return nil, ErrMalformed
	}
	receiver := &Receiver{
		keys: keys, direction: direction, verifier: pinned, verifierFingerprint: fingerprint,
		keyContextID: keyContextFingerprint(keys), committer: committer,
	}
	if hasRestored {
		if restored.direction != direction || restored.window != window ||
			validateReceiverState(keys, restored) != nil ||
			subtle.ConstantTimeCompare(restored.verifierFingerprint[:], fingerprint[:]) != 1 {
			return nil, ErrStateMismatch
		}
		receiver.state = cloneReceiverState(restored)
		return receiver, nil
	}
	receiver.state = ReceiverState{
		version: ProtocolVersion, routeID: keys.routeID, keyEpoch: keys.keyEpoch,
		direction: direction, verifierFingerprint: fingerprint, window: window,
	}
	return receiver, nil
}

func nilReceiverStateCommitter(committer ReceiverStateCommitter) bool {
	if committer == nil {
		return true
	}
	value := reflect.ValueOf(committer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// NewFencedAbsentExpectation pins one request to this receiver's exact gateway
// verification and key context. It is unavailable on a device receiver.
func (receiver *Receiver) NewFencedAbsentExpectation(
	request SMSInspectAndFenceRequest,
) (FencedAbsentExpectation, error) {
	if receiver == nil || receiver.direction != GatewayToClient || validateInspectRequest(request) != nil {
		return FencedAbsentExpectation{}, ErrMalformed
	}
	return FencedAbsentExpectation{
		valid: true, routeID: receiver.keys.routeID, keyEpoch: receiver.keys.keyEpoch,
		verifierFingerprint: receiver.verifierFingerprint, keyContextID: receiver.keyContextID,
		operationID: request.operationID, commitment: request.commitment,
	}, nil
}

// OpenClientToGateway verifies and durably accepts one c2g device envelope.
func (receiver *Receiver) OpenClientToGateway(
	ctx context.Context,
	envelope Envelope,
) (VerifiedMessage, ReceiveDisposition, error) {
	if receiver == nil || ctx == nil || receiver.direction != ClientToGateway {
		return VerifiedMessage{}, "", ErrDirection
	}
	if err := verifyDeviceEnvelope(receiver.verifier, envelope); err != nil {
		return VerifiedMessage{}, "", ErrUnauthorized
	}
	verified, disposition, err := receiver.openVerified(ctx, envelope)
	if err != nil {
		return VerifiedMessage{}, "", err
	}
	return VerifiedMessage{message: verified.message, binding: verified.binding, valid: true}, disposition, nil
}

// OpenGatewayToClient verifies and durably accepts one g2c gateway envelope.
func (receiver *Receiver) OpenGatewayToClient(
	ctx context.Context,
	envelope Envelope,
) (VerifiedGatewayMessage, ReceiveDisposition, error) {
	if receiver == nil || ctx == nil || receiver.direction != GatewayToClient {
		return VerifiedGatewayMessage{}, "", ErrDirection
	}
	if err := verifyGatewayEnvelope(receiver.verifier, envelope); err != nil {
		return VerifiedGatewayMessage{}, "", ErrUnauthorized
	}
	verified, disposition, err := receiver.openVerified(ctx, envelope)
	if err != nil {
		return VerifiedGatewayMessage{}, "", err
	}
	return VerifiedGatewayMessage{
		message: verified.message, binding: verified.binding, valid: true,
	}, disposition, nil
}

type verifiedInternal struct {
	message Message
	binding verifiedBinding
}

func (receiver *Receiver) openVerified(
	ctx context.Context,
	envelope Envelope,
) (verifiedInternal, ReceiveDisposition, error) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	disposition, err := classifyEnvelope(receiver.state, envelope)
	if err != nil {
		return verifiedInternal{}, "", err
	}
	message, err := openAuthenticated(receiver.keys, envelope)
	if err != nil {
		return verifiedInternal{}, "", ErrUnauthorized
	}
	verified := verifiedInternal{
		message: message,
		binding: bindingForMessage(
			receiver.keys, receiver.verifierFingerprint, receiver.keyContextID, envelope.meta, message,
		),
	}
	if !verified.binding.valid {
		return verifiedInternal{}, "", ErrUnauthorized
	}
	if disposition == ReceiveExactDuplicate {
		return verified, disposition, nil
	}
	if receiver.state.generation >= MaxWireCounter {
		return verifiedInternal{}, "", ErrStateCommit
	}
	candidate := cloneReceiverState(receiver.state)
	candidate.previousStateID = receiver.state.stateID
	candidate.generation++
	acceptEnvelope(&candidate, envelope)
	candidate.stateID = deriveReceiverStateID(receiver.keys, candidate)
	encoded, err := encodeReceiverState(receiver.keys, candidate)
	if err != nil {
		return verifiedInternal{}, "", ErrStateCommit
	}
	commit := ReceiverStateCommit{
		expectedGeneration: receiver.state.generation,
		expectedStateID:    receiver.state.stateID,
		nextGeneration:     candidate.generation,
		nextStateID:        candidate.stateID,
		encodedState:       encoded,
	}
	if err := receiver.committer.CommitReceiverState(ctx, commit); err != nil {
		if errors.Is(err, ErrStateCAS) {
			return verifiedInternal{}, "", ErrStateCAS
		}
		return verifiedInternal{}, "", ErrStateCommit
	}
	receiver.state = candidate
	return verified, disposition, nil
}

func bindingForMessage(
	keys KeySet,
	verifierFingerprint [sha256.Size]byte,
	keyContextID [sha256.Size]byte,
	meta EnvelopeMeta,
	message Message,
) verifiedBinding {
	binding := verifiedBinding{
		valid: true, routeID: meta.RouteID, keyEpoch: meta.KeyEpoch,
		direction: meta.Direction, verifierFingerprint: verifierFingerprint,
		keyContextID: keyContextID, kind: message.kind,
	}
	switch message.kind {
	case MessageSMSSend:
		if message.send == nil {
			return verifiedBinding{}
		}
		commitment, err := ComputeSMSSendCommitment(keys, *message.send)
		if err != nil {
			return verifiedBinding{}
		}
		binding.operationID = message.send.OperationID
		binding.commitment = commitment
	case MessageSMSInspectAndFence:
		if message.inspect == nil {
			return verifiedBinding{}
		}
		binding.operationID = message.inspect.operationID
		binding.commitment = message.inspect.commitment
	case MessageSMSInspectAndFenceResult:
		if message.result == nil {
			return verifiedBinding{}
		}
		binding.operationID = message.result.operationID
		binding.commitment = message.result.commitment
		binding.outcome = message.result.outcome
	default:
		return verifiedBinding{}
	}
	return binding
}

func keyContextFingerprint(keys KeySet) [sha256.Size]byte {
	mac := hmac.New(sha256.New, keys.bind[:])
	writeMACPart(mac, "DJONEHUB-PUBLIC-RELAY-KEY-CONTEXT-ID-V2")
	writeMACBytes(mac, keys.c2g[:])
	writeMACBytes(mac, keys.g2c[:])
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func classifyEnvelope(state ReceiverState, envelope Envelope) (ReceiveDisposition, error) {
	if state.routeID != envelope.meta.RouteID || state.keyEpoch != envelope.meta.KeyEpoch ||
		state.direction != envelope.meta.Direction {
		return "", ErrStateMismatch
	}
	digest := sha256.Sum256(envelope.ciphertext)
	for _, entry := range state.entries {
		if entry.sequence == envelope.meta.Sequence {
			if entry.messageID == envelope.meta.MessageID &&
				subtle.ConstantTimeCompare(entry.ciphertextHash[:], digest[:]) == 1 {
				return ReceiveExactDuplicate, nil
			}
			return "", ErrSequenceConflict
		}
		if entry.messageID == envelope.meta.MessageID {
			return "", ErrSequenceConflict
		}
	}
	sequence := envelope.meta.Sequence
	if state.highWater == 0 {
		if sequence > state.window {
			return "", ErrSequenceTooFar
		}
		return ReceiveNew, nil
	}
	if sequence > state.highWater {
		if sequence-state.highWater > state.window {
			return "", ErrSequenceTooFar
		}
		return ReceiveNew, nil
	}
	if state.highWater-sequence >= state.window {
		return "", ErrSequenceTooOld
	}
	return ReceiveNew, nil
}

func acceptEnvelope(state *ReceiverState, envelope Envelope) {
	digest := sha256.Sum256(envelope.ciphertext)
	state.entries = append(state.entries, replayEntry{
		sequence: envelope.meta.Sequence, messageID: envelope.meta.MessageID, ciphertextHash: digest,
	})
	if envelope.meta.Sequence > state.highWater {
		state.highWater = envelope.meta.Sequence
	}
	minimum := uint64(1)
	if state.highWater >= state.window {
		minimum = state.highWater - state.window + 1
	}
	kept := state.entries[:0]
	for _, entry := range state.entries {
		if entry.sequence >= minimum {
			kept = append(kept, entry)
		}
	}
	state.entries = kept
	sort.Slice(state.entries, func(i, j int) bool {
		return state.entries[i].sequence < state.entries[j].sequence
	})
}

type wireReplayEntry struct {
	Sequence         uint64 `json:"sequence"`
	MessageID        string `json:"message_id"`
	CiphertextSHA256 string `json:"ciphertext_sha256"`
}

type wireReceiverState struct {
	Version            uint64            `json:"version"`
	RouteID            string            `json:"route_id"`
	KeyEpoch           uint64            `json:"key_epoch"`
	Direction          Direction         `json:"direction"`
	VerifierSPKISHA256 string            `json:"verifier_spki_sha256"`
	Window             uint64            `json:"window"`
	HighWater          uint64            `json:"high_water"`
	Generation         uint64            `json:"generation"`
	PreviousStateID    string            `json:"previous_state_id"`
	StateID            string            `json:"state_id"`
	Entries            []wireReplayEntry `json:"entries"`
	MAC                string            `json:"mac"`
}

type wireReceiverStateUnsigned struct {
	Version            uint64            `json:"version"`
	RouteID            string            `json:"route_id"`
	KeyEpoch           uint64            `json:"key_epoch"`
	Direction          Direction         `json:"direction"`
	VerifierSPKISHA256 string            `json:"verifier_spki_sha256"`
	Window             uint64            `json:"window"`
	HighWater          uint64            `json:"high_water"`
	Generation         uint64            `json:"generation"`
	PreviousStateID    string            `json:"previous_state_id"`
	StateID            string            `json:"state_id"`
	Entries            []wireReplayEntry `json:"entries"`
}

type wireReceiverStateIdentity struct {
	Version            uint64            `json:"version"`
	RouteID            string            `json:"route_id"`
	KeyEpoch           uint64            `json:"key_epoch"`
	Direction          Direction         `json:"direction"`
	VerifierSPKISHA256 string            `json:"verifier_spki_sha256"`
	Window             uint64            `json:"window"`
	HighWater          uint64            `json:"high_water"`
	Generation         uint64            `json:"generation"`
	PreviousStateID    string            `json:"previous_state_id"`
	Entries            []wireReplayEntry `json:"entries"`
}

var receiverStateKeys = exactKeySet(
	"version", "route_id", "key_epoch", "direction", "verifier_spki_sha256",
	"window", "high_water", "generation", "previous_state_id", "state_id", "entries", "mac",
)

func encodeReceiverState(keys KeySet, state ReceiverState) ([]byte, error) {
	if err := validateReceiverState(keys, state); err != nil {
		return nil, err
	}
	unsigned := receiverStateWire(state)
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return nil, ErrMalformed
	}
	mac := stateMAC(keys, canonical)
	wire := wireReceiverState{
		Version: unsigned.Version, RouteID: unsigned.RouteID, KeyEpoch: unsigned.KeyEpoch,
		Direction: unsigned.Direction, VerifierSPKISHA256: unsigned.VerifierSPKISHA256,
		Window: unsigned.Window, HighWater: unsigned.HighWater, Generation: unsigned.Generation,
		PreviousStateID: unsigned.PreviousStateID, StateID: unsigned.StateID,
		Entries: unsigned.Entries, MAC: base64.RawURLEncoding.EncodeToString(mac),
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > MaxStateBytes {
		return nil, ErrMalformed
	}
	return encoded, nil
}

// DecodeReceiverState strictly parses and authenticates a durable snapshot.
// It does not itself read, write, fsync, or prevent replacement by an older
// otherwise valid snapshot; the committer's CAS generation/state ID does that.
func DecodeReceiverState(keys KeySet, encoded []byte) (ReceiverState, error) {
	if len(encoded) == 0 || len(encoded) > MaxStateBytes || validateKeySet(keys) != nil {
		return ReceiverState{}, ErrMalformed
	}
	var wire wireReceiverState
	if err := decodeExactJSON(encoded, &wire, receiverStateKeys, MaxJSONDepth); err != nil || wire.Entries == nil {
		return ReceiverState{}, ErrMalformed
	}
	providedMAC, ok := decodeFixedBase64(wire.MAC)
	if !ok {
		return ReceiverState{}, ErrMalformed
	}
	unsigned := wireReceiverStateUnsigned{
		Version: wire.Version, RouteID: wire.RouteID, KeyEpoch: wire.KeyEpoch,
		Direction: wire.Direction, VerifierSPKISHA256: wire.VerifierSPKISHA256,
		Window: wire.Window, HighWater: wire.HighWater, Generation: wire.Generation,
		PreviousStateID: wire.PreviousStateID, StateID: wire.StateID, Entries: wire.Entries,
	}
	canonical, err := json.Marshal(unsigned)
	if err != nil || subtle.ConstantTimeCompare(providedMAC[:], stateMAC(keys, canonical)) != 1 {
		return ReceiverState{}, ErrUnauthorized
	}
	verifierFingerprint, ok := decodeFixedBase64(wire.VerifierSPKISHA256)
	if !ok {
		return ReceiverState{}, ErrMalformed
	}
	previousStateID, ok := decodeFixedBase64(wire.PreviousStateID)
	if !ok {
		return ReceiverState{}, ErrMalformed
	}
	stateID, ok := decodeFixedBase64(wire.StateID)
	if !ok {
		return ReceiverState{}, ErrMalformed
	}
	state := ReceiverState{
		version: wire.Version, routeID: wire.RouteID, keyEpoch: wire.KeyEpoch,
		direction: wire.Direction, verifierFingerprint: verifierFingerprint,
		window: wire.Window, highWater: wire.HighWater, generation: wire.Generation,
		previousStateID: previousStateID, stateID: stateID,
		entries: make([]replayEntry, 0, len(wire.Entries)),
	}
	for _, entry := range wire.Entries {
		digest, ok := decodeFixedBase64(entry.CiphertextSHA256)
		if !ok {
			return ReceiverState{}, ErrMalformed
		}
		state.entries = append(state.entries, replayEntry{
			sequence: entry.Sequence, messageID: entry.MessageID, ciphertextHash: digest,
		})
	}
	if err := validateReceiverState(keys, state); err != nil {
		return ReceiverState{}, ErrMalformed
	}
	return state, nil
}

func receiverStateWire(state ReceiverState) wireReceiverStateUnsigned {
	return wireReceiverStateUnsigned{
		Version: state.version, RouteID: state.routeID, KeyEpoch: state.keyEpoch,
		Direction:          state.direction,
		VerifierSPKISHA256: base64.RawURLEncoding.EncodeToString(state.verifierFingerprint[:]),
		Window:             state.window, HighWater: state.highWater, Generation: state.generation,
		PreviousStateID: base64.RawURLEncoding.EncodeToString(state.previousStateID[:]),
		StateID:         base64.RawURLEncoding.EncodeToString(state.stateID[:]),
		Entries:         wireReplayEntries(state),
	}
}

func wireReplayEntries(state ReceiverState) []wireReplayEntry {
	entries := append([]replayEntry(nil), state.entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].sequence < entries[j].sequence })
	wireEntries := make([]wireReplayEntry, 0, len(entries))
	for _, entry := range entries {
		wireEntries = append(wireEntries, wireReplayEntry{
			Sequence: entry.sequence, MessageID: entry.messageID,
			CiphertextSHA256: base64.RawURLEncoding.EncodeToString(entry.ciphertextHash[:]),
		})
	}
	return wireEntries
}

func deriveReceiverStateID(keys KeySet, state ReceiverState) [sha256.Size]byte {
	identity := wireReceiverStateIdentity{
		Version: state.version, RouteID: state.routeID, KeyEpoch: state.keyEpoch,
		Direction:          state.direction,
		VerifierSPKISHA256: base64.RawURLEncoding.EncodeToString(state.verifierFingerprint[:]),
		Window:             state.window, HighWater: state.highWater, Generation: state.generation,
		PreviousStateID: base64.RawURLEncoding.EncodeToString(state.previousStateID[:]),
		Entries:         wireReplayEntries(state),
	}
	canonical, _ := json.Marshal(identity)
	mac := hmac.New(sha256.New, keys.bind[:])
	writeMACPart(mac, "DJONEHUB-PUBLIC-RELAY-RECEIVER-STATE-ID-V2")
	writeMACBytes(mac, canonical)
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func stateMAC(keys KeySet, canonical []byte) []byte {
	mac := hmac.New(sha256.New, keys.bind[:])
	writeMACPart(mac, "DJONEHUB-PUBLIC-RELAY-RECEIVER-STATE-V2")
	writeMACBytes(mac, canonical)
	return mac.Sum(nil)
}

func validateReceiverState(keys KeySet, state ReceiverState) error {
	if validateKeySet(keys) != nil || state.version != ProtocolVersion ||
		state.routeID != keys.routeID || state.keyEpoch != keys.keyEpoch ||
		(state.direction != ClientToGateway && state.direction != GatewayToClient) ||
		allZero(state.verifierFingerprint[:]) || state.window == 0 || state.window > MaxReceiveWindow ||
		state.highWater > MaxWireCounter || state.generation > MaxWireCounter ||
		uint64(len(state.entries)) > state.window {
		return ErrStateMismatch
	}
	if state.generation == 0 {
		if state.highWater != 0 || len(state.entries) != 0 ||
			!allZero(state.previousStateID[:]) || !allZero(state.stateID[:]) {
			return ErrMalformed
		}
		return nil
	}
	expectedStateID := deriveReceiverStateID(keys, state)
	if state.highWater == 0 || allZero(state.stateID[:]) ||
		(state.generation == 1 && !allZero(state.previousStateID[:])) ||
		(state.generation > 1 && allZero(state.previousStateID[:])) ||
		subtle.ConstantTimeCompare(state.stateID[:], expectedStateID[:]) != 1 {
		return ErrMalformed
	}
	seenMessages := make(map[string]struct{}, len(state.entries))
	lastSequence := uint64(0)
	hasHighWater := false
	minimum := uint64(1)
	if state.highWater >= state.window {
		minimum = state.highWater - state.window + 1
	}
	for _, entry := range state.entries {
		if entry.sequence == 0 || entry.sequence > state.highWater || entry.sequence < minimum ||
			entry.sequence <= lastSequence || !validIdentifier(entry.messageID) || allZero(entry.ciphertextHash[:]) {
			return ErrMalformed
		}
		if _, duplicate := seenMessages[entry.messageID]; duplicate {
			return ErrMalformed
		}
		seenMessages[entry.messageID] = struct{}{}
		lastSequence = entry.sequence
		hasHighWater = hasHighWater || entry.sequence == state.highWater
	}
	if !hasHighWater {
		return ErrMalformed
	}
	return nil
}

func cloneReceiverState(state ReceiverState) ReceiverState {
	copy := state
	copy.entries = append([]replayEntry(nil), state.entries...)
	return copy
}

func decodeFixedBase64(encoded string) ([sha256.Size]byte, bool) {
	decoded, err := decodeCanonicalBase64(encoded, sha256.Size)
	if err != nil || len(decoded) != sha256.Size {
		return [sha256.Size]byte{}, false
	}
	var fixed [sha256.Size]byte
	copy(fixed[:], decoded)
	return fixed, true
}
