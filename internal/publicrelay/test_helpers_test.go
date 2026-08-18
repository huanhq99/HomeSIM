package publicrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"math/big"
	"sync"
	"testing"
)

const testIssuedAt int64 = 1_800_000_000

func testIdentifier(fill byte) string {
	value := make([]byte, identifierBytes)
	for index := range value {
		value[index] = fill
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func testRootKey(t testing.TB) RootKey {
	t.Helper()
	material := make([]byte, rootKeyBytes)
	for index := range material {
		material[index] = byte(index + 1)
	}
	root, err := NewRootKey(material)
	if err != nil {
		t.Fatalf("NewRootKey: %v", err)
	}
	return root
}

func testKeySet(t testing.TB) KeySet {
	t.Helper()
	keys, err := DeriveKeySet(testRootKey(t), testIdentifier(0x11), 7)
	if err != nil {
		t.Fatalf("DeriveKeySet: %v", err)
	}
	return keys
}

func testPrivateKey(t testing.TB, scalar string) *ecdsa.PrivateKey {
	t.Helper()
	d, ok := new(big.Int).SetString(scalar, 16)
	if !ok || d.Sign() <= 0 || d.Cmp(elliptic.P256().Params().N) >= 0 {
		t.Fatalf("invalid test scalar")
	}
	x, y := elliptic.P256().ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, D: d}
}

func testDeviceKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	return testPrivateKey(t, "1c3a1d5f7b9e2468ace02468bdf1357913579bdf2468ace013579bdf02468ace")
}

func testGatewayKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	return testPrivateKey(t, "2d4b2e608caf3579bdf13579ce02468a2468ace013579bdf02468ace13579bdf")
}

func testIntent(t testing.TB, operationFill byte) SMSSendIntent {
	t.Helper()
	return SMSSendIntent{
		OperationID: testIdentifier(operationFill),
		Destination: "+15551234567",
		Message:     "synthetic relay test 你好",
	}
}

func testSendMessage(t testing.TB, operationFill byte) Message {
	t.Helper()
	message, err := NewSMSSendMessage(testIntent(t, operationFill))
	if err != nil {
		t.Fatalf("NewSMSSendMessage: %v", err)
	}
	return message
}

func testMeta(direction Direction, sequence uint64, messageFill byte) EnvelopeMeta {
	return EnvelopeMeta{
		RouteID: testIdentifier(0x11), KeyEpoch: 7, Direction: direction,
		Sequence: sequence, MessageID: testIdentifier(messageFill), IssuedAt: testIssuedAt,
	}
}

type memoryCommitter struct {
	mu          sync.Mutex
	generation  uint64
	stateID     string
	encoded     []byte
	failure     error
	writeOnFail bool
	calls       int
}

func newMemoryCommitter() *memoryCommitter {
	return &memoryCommitter{stateID: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
}

func (committer *memoryCommitter) CommitReceiverState(
	ctx context.Context,
	commit ReceiverStateCommit,
) error {
	committer.mu.Lock()
	defer committer.mu.Unlock()
	committer.calls++
	if ctx == nil || ctx.Err() != nil {
		return errors.New("synthetic canceled commit")
	}
	if commit.ExpectedGeneration() != committer.generation ||
		commit.ExpectedStateID() != committer.stateID ||
		commit.NextGeneration() != committer.generation+1 {
		return ErrStateCAS
	}
	if committer.failure != nil {
		if committer.writeOnFail {
			committer.generation = commit.NextGeneration()
			committer.stateID = commit.NextStateID()
			committer.encoded = commit.EncodedState()
		}
		return committer.failure
	}
	committer.generation = commit.NextGeneration()
	committer.stateID = commit.NextStateID()
	committer.encoded = commit.EncodedState()
	return nil
}

func (committer *memoryCommitter) snapshot() (uint64, string, []byte, int) {
	committer.mu.Lock()
	defer committer.mu.Unlock()
	return committer.generation, committer.stateID,
		append([]byte(nil), committer.encoded...), committer.calls
}

func newTestDeviceReceiver(t testing.TB, keys KeySet, window uint64, committer ReceiverStateCommitter) *Receiver {
	t.Helper()
	receiver, err := NewDeviceReceiver(keys, window, &testDeviceKey(t).PublicKey, committer)
	if err != nil {
		t.Fatalf("NewDeviceReceiver: %v", err)
	}
	return receiver
}

func newTestGatewayReceiver(t testing.TB, keys KeySet, window uint64, committer ReceiverStateCommitter) *Receiver {
	t.Helper()
	receiver, err := NewGatewayReceiver(keys, window, &testGatewayKey(t).PublicKey, committer)
	if err != nil {
		t.Fatalf("NewGatewayReceiver: %v", err)
	}
	return receiver
}
