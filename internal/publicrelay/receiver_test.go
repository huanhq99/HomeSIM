package publicrelay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gatedCommitter struct {
	backing *memoryCommitter
	entered chan<- struct{}
	release <-chan struct{}
}

func (committer *gatedCommitter) CommitReceiverState(
	ctx context.Context,
	commit ReceiverStateCommit,
) error {
	select {
	case committer.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-committer.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return committer.backing.CommitReceiverState(ctx, commit)
}

func sealSequence(t testing.TB, keys KeySet, sequence uint64, messageFill, operationFill byte) Envelope {
	t.Helper()
	envelope, err := SealClientToGateway(
		keys, testMeta(ClientToGateway, sequence, messageFill),
		testSendMessage(t, operationFill), testDeviceKey(t),
	)
	if err != nil {
		t.Fatalf("seal sequence %d: %v", sequence, err)
	}
	return envelope
}

func TestReceiverBoundedOutOfOrderDuplicateAndConflicts(t *testing.T) {
	keys := testKeySet(t)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, 4, committer)
	envelopes := map[uint64]Envelope{
		1: sealSequence(t, keys, 1, 0x41, 0x31),
		2: sealSequence(t, keys, 2, 0x42, 0x32),
		3: sealSequence(t, keys, 3, 0x43, 0x33),
	}
	for _, sequence := range []uint64{3, 1, 2} {
		verified, disposition, err := receiver.OpenClientToGateway(context.Background(), envelopes[sequence])
		if err != nil || disposition != ReceiveNew {
			t.Fatalf("sequence %d disposition=%q err=%v", sequence, disposition, err)
		}
		if _, ok := verified.Message(); !ok {
			t.Fatalf("sequence %d returned invalid verified wrapper", sequence)
		}
	}
	_, disposition, err := receiver.OpenClientToGateway(context.Background(), envelopes[2])
	if err != nil || disposition != ReceiveExactDuplicate {
		t.Fatalf("exact duplicate disposition=%q err=%v", disposition, err)
	}

	conflictingSequence := sealSequence(t, keys, 2, 0x52, 0x42)
	if _, _, err := receiver.OpenClientToGateway(context.Background(), conflictingSequence); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("same sequence conflict error=%v", err)
	}
	conflictingMessageID := sealSequence(t, keys, 4, 0x42, 0x44)
	if _, _, err := receiver.OpenClientToGateway(context.Background(), conflictingMessageID); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("same message ID conflict error=%v", err)
	}
	tooFar := sealSequence(t, keys, 8, 0x48, 0x38)
	if _, _, err := receiver.OpenClientToGateway(context.Background(), tooFar); !errors.Is(err, ErrSequenceTooFar) {
		t.Fatalf("too-far error=%v", err)
	}
	sequenceSeven := sealSequence(t, keys, 7, 0x47, 0x37)
	if _, disposition, err := receiver.OpenClientToGateway(context.Background(), sequenceSeven); err != nil || disposition != ReceiveNew {
		t.Fatalf("bounded advance disposition=%q err=%v", disposition, err)
	}
	if _, _, err := receiver.OpenClientToGateway(context.Background(), envelopes[3]); !errors.Is(err, ErrSequenceTooOld) {
		t.Fatalf("too-old error=%v", err)
	}
	generation, _, encoded, calls := committer.snapshot()
	state, err := DecodeReceiverState(keys, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 4 || calls != 4 || state.HighWater() != 7 || state.EntryCount() > 4 {
		t.Fatalf("generation=%d calls=%d high=%d entries=%d", generation, calls, state.HighWater(), state.EntryCount())
	}
}

func TestReceiverRejectsInitialUnboundedJump(t *testing.T) {
	keys := testKeySet(t)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, 4, committer)
	envelope := sealSequence(t, keys, 5, 0x45, 0x35)
	if _, _, err := receiver.OpenClientToGateway(context.Background(), envelope); !errors.Is(err, ErrSequenceTooFar) {
		t.Fatalf("initial jump error=%v", err)
	}
	if _, _, _, calls := committer.snapshot(); calls != 0 {
		t.Fatalf("rejected jump invoked committer %d times", calls)
	}
}

func TestReceiverStateAuthenticatedRoundTripAndPinnedVerifier(t *testing.T) {
	keys := testKeySet(t)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, 8, committer)
	first := sealSequence(t, keys, 1, 0x41, 0x31)
	third := sealSequence(t, keys, 3, 0x43, 0x33)
	for _, envelope := range []Envelope{third, first} {
		if _, _, err := receiver.OpenClientToGateway(context.Background(), envelope); err != nil {
			t.Fatal(err)
		}
	}
	generation, stateID, encoded, _ := committer.snapshot()
	state, err := DecodeReceiverState(keys, encoded)
	if err != nil {
		t.Fatalf("DecodeReceiverState: %v", err)
	}
	restoredCommitter := newMemoryCommitter()
	restoredCommitter.generation = generation
	restoredCommitter.stateID = stateID
	restoredCommitter.encoded = append([]byte(nil), encoded...)
	restored, err := NewDeviceReceiverFromState(
		keys, state, &testDeviceKey(t).PublicKey, restoredCommitter,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, disposition, err := restored.OpenClientToGateway(context.Background(), first)
	if err != nil || disposition != ReceiveExactDuplicate {
		t.Fatalf("restored duplicate disposition=%q err=%v", disposition, err)
	}

	wrongVerifier := testGatewayKey(t)
	if _, err := NewDeviceReceiverFromState(keys, state, &wrongVerifier.PublicKey, restoredCommitter); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("wrong verifier restored state error=%v", err)
	}
	var wire wireReceiverState
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	wire.VerifierSPKISHA256 = base64.RawURLEncoding.EncodeToString(bytesOf(0x99, sha256.Size))
	tampered, _ := json.Marshal(wire)
	if _, err := DecodeReceiverState(keys, tampered); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered verifier fingerprint error=%v", err)
	}
	otherKeys, err := DeriveKeySet(testRootKey(t), testIdentifier(0x12), 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceiverState(otherKeys, encoded); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-key state error=%v", err)
	}
}

func TestReceiverStateGenerationOneRequiresZeroPreviousStateID(t *testing.T) {
	keys := testKeySet(t)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	envelope := sealSequence(t, keys, 1, 0x41, 0x31)
	if _, _, err := receiver.OpenClientToGateway(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	_, _, encoded, _ := committer.snapshot()
	state, err := DecodeReceiverState(keys, encoded)
	if err != nil || state.Generation() != 1 || !allZero(state.previousStateID[:]) {
		t.Fatalf("unexpected generation-one seed state generation=%d err=%v", state.Generation(), err)
	}
	state.previousStateID = sha256.Sum256([]byte("invalid synthetic predecessor at generation one"))
	state.stateID = deriveReceiverStateID(keys, state)
	if _, err := encodeReceiverState(keys, state); err == nil {
		t.Fatal("encoder accepted a non-zero predecessor at generation one")
	}

	unsigned := receiverStateWire(state)
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	var wire wireReceiverState
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	wire.PreviousStateID = unsigned.PreviousStateID
	wire.StateID = unsigned.StateID
	wire.MAC = base64.RawURLEncoding.EncodeToString(stateMAC(keys, canonical))
	authenticatedInvalid, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceiverState(keys, authenticatedInvalid); !errors.Is(err, ErrMalformed) {
		t.Fatalf("authenticated generation-one predecessor error=%v", err)
	}
}

func TestReceiverDeepCopiesPinnedVerifier(t *testing.T) {
	keys := testKeySet(t)
	signingKey := testDeviceKey(t)
	verifierSource := testDeviceKey(t)
	receiver, err := NewDeviceReceiver(
		keys, DefaultReceiveWindow, &verifierSource.PublicKey, newMemoryCommitter(),
	)
	if err != nil {
		t.Fatal(err)
	}
	verifierSource.X.SetInt64(1)
	verifierSource.Y.SetInt64(1)
	envelope, err := SealClientToGateway(
		keys, testMeta(ClientToGateway, 1, 0x41), testSendMessage(t, 0x31), signingKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.OpenClientToGateway(context.Background(), envelope); err != nil {
		t.Fatalf("mutating source verifier changed pinned key: %v", err)
	}
}

func TestReceiverCommitFailureNeverReleasesPlaintext(t *testing.T) {
	keys := testKeySet(t)
	envelope := sealSequence(t, keys, 1, 0x41, 0x31)
	committer := newMemoryCommitter()
	committer.failure = errors.New("synthetic disk failure with sensitive marker")
	receiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	verified, disposition, err := receiver.OpenClientToGateway(context.Background(), envelope)
	if !errors.Is(err, ErrStateCommit) || disposition != "" {
		t.Fatalf("commit failure disposition=%q err=%v", disposition, err)
	}
	if _, ok := verified.Message(); ok {
		t.Fatal("commit failure released verified plaintext")
	}
	committer.failure = nil
	verified, disposition, err = receiver.OpenClientToGateway(context.Background(), envelope)
	if err != nil || disposition != ReceiveNew {
		t.Fatalf("retry after non-write failure disposition=%q err=%v", disposition, err)
	}
	if _, ok := verified.Message(); !ok {
		t.Fatal("successful durable retry did not release plaintext")
	}

	ambiguousCommitter := newMemoryCommitter()
	ambiguousCommitter.failure = errors.New("synthetic post-write durability ambiguity")
	ambiguousCommitter.writeOnFail = true
	ambiguousReceiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, ambiguousCommitter)
	verified, _, err = ambiguousReceiver.OpenClientToGateway(context.Background(), envelope)
	if !errors.Is(err, ErrStateCommit) {
		t.Fatalf("ambiguous commit error=%v", err)
	}
	if _, ok := verified.Message(); ok {
		t.Fatal("ambiguous commit released plaintext")
	}
	ambiguousCommitter.failure = nil
	verified, _, err = ambiguousReceiver.OpenClientToGateway(context.Background(), envelope)
	if !errors.Is(err, ErrStateCAS) {
		t.Fatalf("post-write retry error=%v", err)
	}
	if _, ok := verified.Message(); ok {
		t.Fatal("CAS failure released plaintext")
	}
}

func TestReceiverCASPreventsLateSnapshotOverwriteAcrossInstances(t *testing.T) {
	keys := testKeySet(t)
	shared := newMemoryCommitter()
	firstReceiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, shared)
	staleReceiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, shared)
	first := sealSequence(t, keys, 1, 0x41, 0x31)
	second := sealSequence(t, keys, 2, 0x42, 0x32)
	if _, disposition, err := firstReceiver.OpenClientToGateway(context.Background(), first); err != nil || disposition != ReceiveNew {
		t.Fatalf("first CAS disposition=%q err=%v", disposition, err)
	}
	verified, disposition, err := staleReceiver.OpenClientToGateway(context.Background(), second)
	if !errors.Is(err, ErrStateCAS) || disposition != "" {
		t.Fatalf("stale CAS disposition=%q err=%v", disposition, err)
	}
	if _, ok := verified.Message(); ok {
		t.Fatal("stale CAS released plaintext")
	}
	generation, _, encoded, _ := shared.snapshot()
	state, err := DecodeReceiverState(keys, encoded)
	if err != nil || generation != 1 || state.HighWater() != 1 {
		t.Fatalf("late snapshot overwrote state generation=%d high=%d err=%v", generation, state.HighWater(), err)
	}
}

func TestReceiverConcurrentInstancesCASBeforePlaintextRelease(t *testing.T) {
	keys := testKeySet(t)
	backing := newMemoryCommitter()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	committer := &gatedCommitter{backing: backing, entered: entered, release: release}
	firstReceiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	secondReceiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	envelopes := []Envelope{
		sealSequence(t, keys, 1, 0x41, 0x31),
		sealSequence(t, keys, 2, 0x42, 0x32),
	}
	type result struct {
		verified    bool
		disposition ReceiveDisposition
		err         error
	}
	results := make(chan result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for index, receiver := range []*Receiver{firstReceiver, secondReceiver} {
		index, receiver := index, receiver
		go func() {
			verified, disposition, err := receiver.OpenClientToGateway(ctx, envelopes[index])
			_, ok := verified.Message()
			results <- result{verified: ok, disposition: disposition, err: err}
		}()
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("both receiver instances did not reach the CAS gate")
		}
	}
	close(release)
	released = true
	successes := 0
	casFailures := 0
	for range 2 {
		select {
		case got := <-results:
			switch {
			case got.err == nil && got.disposition == ReceiveNew && got.verified:
				successes++
			case errors.Is(got.err, ErrStateCAS) && got.disposition == "" && !got.verified:
				casFailures++
			default:
				t.Fatalf(
					"unexpected concurrent result verified=%v disposition=%q err=%v",
					got.verified, got.disposition, got.err,
				)
			}
		case <-ctx.Done():
			t.Fatal("concurrent receiver results timed out")
		}
	}
	generation, _, encoded, calls := backing.snapshot()
	state, err := DecodeReceiverState(keys, encoded)
	if err != nil || successes != 1 || casFailures != 1 || calls != 2 ||
		generation != 1 || state.Generation() != 1 || state.EntryCount() != 1 {
		t.Fatalf(
			"success=%d CAS=%d calls=%d generation=%d state_generation=%d entries=%d err=%v",
			successes, casFailures, calls, generation, state.Generation(), state.EntryCount(), err,
		)
	}
}

func TestReceiverConcurrentExactReplayHasOneNew(t *testing.T) {
	keys := testKeySet(t)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	envelope := sealSequence(t, keys, 1, 0x41, 0x31)
	var newCount atomic.Int64
	var duplicateCount atomic.Int64
	var failures atomic.Int64
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			verified, disposition, err := receiver.OpenClientToGateway(context.Background(), envelope)
			if err != nil {
				failures.Add(1)
				return
			}
			if _, ok := verified.Message(); !ok {
				failures.Add(1)
				return
			}
			switch disposition {
			case ReceiveNew:
				newCount.Add(1)
			case ReceiveExactDuplicate:
				duplicateCount.Add(1)
			default:
				failures.Add(1)
			}
		}()
	}
	wait.Wait()
	_, _, _, calls := committer.snapshot()
	if newCount.Load() != 1 || duplicateCount.Load() != 15 || failures.Load() != 0 || calls != 1 {
		t.Fatalf(
			"new=%d duplicate=%d failures=%d commits=%d",
			newCount.Load(), duplicateCount.Load(), failures.Load(), calls,
		)
	}
}

func TestReceiverRejectsWrongRoleProofWithoutCommit(t *testing.T) {
	keys := testKeySet(t)
	committer := newMemoryCommitter()
	wrongVerifier := testGatewayKey(t)
	receiver, err := NewDeviceReceiver(keys, DefaultReceiveWindow, &wrongVerifier.PublicKey, committer)
	if err != nil {
		t.Fatal(err)
	}
	envelope := sealSequence(t, keys, 1, 0x41, 0x31)
	verified, _, err := receiver.OpenClientToGateway(context.Background(), envelope)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong pinned proof error=%v", err)
	}
	if _, ok := verified.Message(); ok {
		t.Fatal("wrong proof released plaintext")
	}
	if _, _, _, calls := committer.snapshot(); calls != 0 {
		t.Fatalf("wrong proof committed state %d times", calls)
	}
	if _, _, err := receiver.OpenGatewayToClient(context.Background(), envelope); !errors.Is(err, ErrDirection) {
		t.Fatalf("wrong receiver method error=%v", err)
	}
}

func TestReceiverStateRejectsNullEntriesAndMaximumFitsBound(t *testing.T) {
	keys := testKeySet(t)
	_, verifierFingerprint, _, err := cloneVerifierKey(&testDeviceKey(t).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	state := ReceiverState{
		version: ProtocolVersion, routeID: keys.routeID, keyEpoch: keys.keyEpoch,
		direction: ClientToGateway, verifierFingerprint: verifierFingerprint,
		window: MaxReceiveWindow, highWater: MaxReceiveWindow,
		generation: MaxReceiveWindow, entries: make([]replayEntry, 0, MaxReceiveWindow),
	}
	state.previousStateID = sha256.Sum256([]byte("synthetic previous receiver state"))
	for sequence := uint64(1); sequence <= MaxReceiveWindow; sequence++ {
		var input [8]byte
		binary.BigEndian.PutUint64(input[:], sequence)
		var messageID [identifierBytes]byte
		binary.BigEndian.PutUint64(messageID[identifierBytes-8:], sequence)
		state.entries = append(state.entries, replayEntry{
			sequence: sequence, messageID: base64.RawURLEncoding.EncodeToString(messageID[:]),
			ciphertextHash: sha256.Sum256(input[:]),
		})
	}
	state.stateID = deriveReceiverStateID(keys, state)
	encoded, err := encodeReceiverState(keys, state)
	if err != nil {
		t.Fatalf("maximum receiver state: %v", err)
	}
	if len(encoded) > MaxStateBytes {
		t.Fatalf("maximum receiver state size=%d", len(encoded))
	}
	decoded, err := DecodeReceiverState(keys, encoded)
	if err != nil || decoded.EntryCount() != int(MaxReceiveWindow) {
		t.Fatalf("decode maximum state entries=%d err=%v", decoded.EntryCount(), err)
	}

	var wire wireReceiverState
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	wire.Entries = nil
	nullEntries, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReceiverState(keys, nullEntries); !errors.Is(err, ErrMalformed) {
		t.Fatalf("entries:null error=%v", err)
	}
}

func TestReceiverRequiresCommitter(t *testing.T) {
	keys := testKeySet(t)
	if _, err := NewDeviceReceiver(keys, DefaultReceiveWindow, &testDeviceKey(t).PublicKey, nil); err == nil {
		t.Fatal("receiver accepted nil durable committer")
	}
	var typedNil *memoryCommitter
	if _, err := NewDeviceReceiver(
		keys, DefaultReceiveWindow, &testDeviceKey(t).PublicKey, typedNil,
	); err == nil {
		t.Fatal("receiver accepted typed-nil durable committer")
	}
}

func bytesOf(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}
