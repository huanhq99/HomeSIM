package publicrelay

import (
	"bytes"
	"context"
	"testing"
)

func FuzzDecodeEnvelope(f *testing.F) {
	_, valid := testEncodedEnvelopeForFuzz(f)
	f.Add(valid)
	f.Add([]byte(`{}`))
	f.Add(bytes.Repeat([]byte{'x'}, MaxEnvelopeBytes+1))
	f.Fuzz(func(t *testing.T, encoded []byte) {
		envelope, err := DecodeEnvelope(encoded)
		if err != nil {
			return
		}
		reencoded, err := EncodeEnvelope(envelope)
		if err != nil {
			t.Fatalf("decoded envelope did not re-encode: %v", err)
		}
		if _, err := DecodeEnvelope(reencoded); err != nil {
			t.Fatalf("re-encoded envelope did not decode: %v", err)
		}
	})
}

func FuzzUnpadMessage(f *testing.F) {
	padded, err := padMessage(testSendMessage(f, 0x31))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(padded)
	f.Add([]byte("DJR2"))
	f.Fuzz(func(t *testing.T, candidate []byte) {
		message, err := unpadMessage(candidate)
		if err != nil {
			return
		}
		repadded, err := padMessage(message)
		if err != nil {
			t.Fatalf("accepted message did not repad: %v", err)
		}
		if _, err := unpadMessage(repadded); err != nil {
			t.Fatalf("repadded message did not unpad: %v", err)
		}
	})
}

func FuzzDecodeReceiverState(f *testing.F) {
	keys := testKeySet(f)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(f, keys, DefaultReceiveWindow, committer)
	envelope, _ := testEncodedEnvelopeForFuzz(f)
	if _, _, err := receiver.OpenClientToGateway(context.Background(), envelope); err != nil {
		f.Fatal(err)
	}
	_, _, valid, _ := committer.snapshot()
	f.Add(valid)
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, encoded []byte) {
		state, err := DecodeReceiverState(keys, encoded)
		if err != nil {
			return
		}
		reencoded, err := encodeReceiverState(keys, state)
		if err != nil {
			t.Fatalf("decoded state did not re-encode: %v", err)
		}
		if _, err := DecodeReceiverState(keys, reencoded); err != nil {
			t.Fatalf("re-encoded state did not decode: %v", err)
		}
	})
}

func testEncodedEnvelopeForFuzz(t testing.TB) (Envelope, []byte) {
	t.Helper()
	envelope, err := SealClientToGateway(
		testKeySet(t), testMeta(ClientToGateway, 1, 0x41),
		testSendMessage(t, 0x31), testDeviceKey(t),
	)
	if err != nil {
		t.Fatalf("seal fuzz seed: %v", err)
	}
	encoded, err := EncodeEnvelope(envelope)
	if err != nil {
		t.Fatalf("encode fuzz seed: %v", err)
	}
	return envelope, encoded
}
