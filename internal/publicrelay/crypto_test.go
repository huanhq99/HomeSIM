package publicrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"
)

func TestDeriveKeySetSeparatesContextsAndDirections(t *testing.T) {
	root := testRootKey(t)
	first, err := DeriveKeySet(root, testIdentifier(0x11), 7)
	if err != nil {
		t.Fatalf("DeriveKeySet first: %v", err)
	}
	secondEpoch, err := DeriveKeySet(root, testIdentifier(0x11), 8)
	if err != nil {
		t.Fatalf("DeriveKeySet epoch: %v", err)
	}
	secondRoute, err := DeriveKeySet(root, testIdentifier(0x12), 7)
	if err != nil {
		t.Fatalf("DeriveKeySet route: %v", err)
	}
	if bytes.Equal(first.c2g[:], first.g2c[:]) || bytes.Equal(first.c2g[:], first.bind[:]) ||
		bytes.Equal(first.g2c[:], first.bind[:]) {
		t.Fatal("derived key labels collided")
	}
	if bytes.Equal(first.c2g[:], secondEpoch.c2g[:]) || bytes.Equal(first.c2g[:], secondRoute.c2g[:]) {
		t.Fatal("route or epoch did not bind derived traffic key")
	}
	if _, err := NewRootKey(make([]byte, rootKeyBytes)); err == nil {
		t.Fatal("all-zero root accepted")
	}
}

func TestSMSSendCommitmentBindsEveryFieldAndIsKeyed(t *testing.T) {
	keys := testKeySet(t)
	intent := testIntent(t, 0x31)
	original, err := ComputeSMSSendCommitment(keys, intent)
	if err != nil {
		t.Fatalf("ComputeSMSSendCommitment: %v", err)
	}
	mutations := []SMSSendIntent{
		{OperationID: testIdentifier(0x32), Destination: intent.Destination, Message: intent.Message},
		{OperationID: intent.OperationID, Destination: "+15557654321", Message: intent.Message},
		{OperationID: intent.OperationID, Destination: intent.Destination, Message: intent.Message + "!"},
	}
	for index, mutation := range mutations {
		changed, err := ComputeSMSSendCommitment(keys, mutation)
		if err != nil {
			t.Fatalf("mutation %d: %v", index, err)
		}
		if original.Equal(changed) {
			t.Fatalf("mutation %d did not change commitment", index)
		}
	}
	otherKeys, err := DeriveKeySet(testRootKey(t), testIdentifier(0x12), 7)
	if err != nil {
		t.Fatal(err)
	}
	otherIntent := intent
	otherIntent.OperationID = testIdentifier(0x31)
	other, err := ComputeSMSSendCommitment(otherKeys, otherIntent)
	if err != nil {
		t.Fatal(err)
	}
	if original.Equal(other) {
		t.Fatal("different bind key produced identical commitment")
	}
	parsed, err := ParseCommitment(original.Encode())
	if err != nil || !parsed.Equal(original) {
		t.Fatalf("commitment round trip err=%v", err)
	}
}

func TestSealAndOpenTypedMessagesBothDirections(t *testing.T) {
	keys := testKeySet(t)
	deviceKey := testDeviceKey(t)
	gatewayKey := testGatewayKey(t)

	intent := testIntent(t, 0x31)
	send, err := NewSMSSendMessage(intent)
	if err != nil {
		t.Fatal(err)
	}
	c2g, err := SealClientToGateway(keys, testMeta(ClientToGateway, 1, 0x41), send, deviceKey)
	if err != nil {
		t.Fatalf("SealClientToGateway: %v", err)
	}
	wire, err := EncodeEnvelope(c2g)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	decoded, err := DecodeEnvelope(wire)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	receiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, newMemoryCommitter())
	verified, disposition, err := receiver.OpenClientToGateway(context.Background(), decoded)
	if err != nil || disposition != ReceiveNew {
		t.Fatalf("open c2g disposition=%q err=%v", disposition, err)
	}
	opened, ok := verified.Message()
	if !ok {
		t.Fatal("verified c2g wrapper did not release message")
	}
	openedIntent, ok := opened.SMSSend()
	if !ok || openedIntent != intent {
		t.Fatalf("opened SMS intent mismatch: ok=%v", ok)
	}

	commitment, err := ComputeSMSSendCommitment(keys, intent)
	if err != nil {
		t.Fatal(err)
	}
	inspectValue, err := NewSMSInspectAndFenceRequest(intent.OperationID, commitment)
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := NewSMSInspectAndFenceMessage(inspectValue)
	if err != nil {
		t.Fatal(err)
	}
	inspectEnvelope, err := SealClientToGateway(
		keys, testMeta(ClientToGateway, 2, 0x42), inspect, deviceKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	verified, disposition, err = receiver.OpenClientToGateway(context.Background(), inspectEnvelope)
	if err != nil || disposition != ReceiveNew {
		t.Fatalf("open inspect request disposition=%q err=%v", disposition, err)
	}
	opened, ok = verified.Message()
	if !ok {
		t.Fatal("verified inspect wrapper did not release message")
	}
	openedInspect, ok := opened.SMSInspectAndFence()
	openedOperation, operationOK := openedInspect.OperationID()
	openedCommitment, commitmentOK := openedInspect.OperationCommitment()
	if !ok || !operationOK || !commitmentOK || openedOperation != intent.OperationID ||
		!openedCommitment.Equal(commitment) {
		t.Fatalf("opened inspect request mismatch: ok=%v", ok)
	}

	resultValue, err := NewSMSInspectAndFenceResult(
		intent.OperationID, commitment, OperationFencedAbsent,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewSMSInspectAndFenceResultMessage(resultValue)
	if err != nil {
		t.Fatal(err)
	}
	g2c, err := SealGatewayToClient(keys, testMeta(GatewayToClient, 1, 0x51), result, gatewayKey)
	if err != nil {
		t.Fatalf("SealGatewayToClient: %v", err)
	}
	g2cReceiver := newTestGatewayReceiver(t, keys, DefaultReceiveWindow, newMemoryCommitter())
	expectation, err := g2cReceiver.NewFencedAbsentExpectation(inspectValue)
	if err != nil {
		t.Fatal(err)
	}
	verifiedGateway, disposition, err := g2cReceiver.OpenGatewayToClient(context.Background(), g2c)
	if err != nil || disposition != ReceiveNew {
		t.Fatalf("open g2c disposition=%q err=%v", disposition, err)
	}
	if !verifiedGateway.ConfirmsFencedAbsent(expectation) {
		t.Fatal("verified matching result did not confirm fenced_absent")
	}
	if (VerifiedGatewayMessage{}).ConfirmsFencedAbsent(expectation) ||
		verifiedGateway.ConfirmsFencedAbsent(FencedAbsentExpectation{}) {
		t.Fatal("zero verified value or expectation confirmed fenced_absent")
	}
	otherIntent := intent
	otherIntent.Message += "!"
	otherCommitment, err := ComputeSMSSendCommitment(keys, otherIntent)
	if err != nil {
		t.Fatal(err)
	}
	expectationMutations := map[string]func(*FencedAbsentExpectation){
		"route":       func(value *FencedAbsentExpectation) { value.routeID = testIdentifier(0x12) },
		"epoch":       func(value *FencedAbsentExpectation) { value.keyEpoch++ },
		"verifier":    func(value *FencedAbsentExpectation) { value.verifierFingerprint[0] ^= 1 },
		"key context": func(value *FencedAbsentExpectation) { value.keyContextID[0] ^= 1 },
		"operation":   func(value *FencedAbsentExpectation) { value.operationID = testIdentifier(0x32) },
		"commitment":  func(value *FencedAbsentExpectation) { value.commitment = otherCommitment },
	}
	for name, mutate := range expectationMutations {
		t.Run("fence expectation "+name, func(t *testing.T) {
			changed := expectation
			mutate(&changed)
			if verifiedGateway.ConfirmsFencedAbsent(changed) {
				t.Fatal("mismatched expectation confirmed fenced_absent")
			}
		})
	}
	verifiedMutations := map[string]func(*VerifiedGatewayMessage){
		"route":     func(value *VerifiedGatewayMessage) { value.binding.routeID = testIdentifier(0x12) },
		"epoch":     func(value *VerifiedGatewayMessage) { value.binding.keyEpoch++ },
		"direction": func(value *VerifiedGatewayMessage) { value.binding.direction = ClientToGateway },
		"verifier":  func(value *VerifiedGatewayMessage) { value.binding.verifierFingerprint[0] ^= 1 },
		"key":       func(value *VerifiedGatewayMessage) { value.binding.keyContextID[0] ^= 1 },
		"kind":      func(value *VerifiedGatewayMessage) { value.binding.kind = MessageSMSSend },
		"operation": func(value *VerifiedGatewayMessage) { value.binding.operationID = testIdentifier(0x32) },
		"commitment": func(value *VerifiedGatewayMessage) {
			value.binding.commitment = otherCommitment
		},
		"outcome": func(value *VerifiedGatewayMessage) { value.binding.outcome = OperationApplied },
	}
	for name, mutate := range verifiedMutations {
		t.Run("verified fence "+name, func(t *testing.T) {
			changed := verifiedGateway
			mutate(&changed)
			if changed.ConfirmsFencedAbsent(expectation) {
				t.Fatal("mismatched verified binding confirmed fenced_absent")
			}
		})
	}
}

func TestAEADAndProofBindOuterMetadataAndCiphertext(t *testing.T) {
	keys := testKeySet(t)
	deviceKey := testDeviceKey(t)
	envelope, err := SealClientToGateway(
		keys, testMeta(ClientToGateway, 1, 0x41), testSendMessage(t, 0x31), deviceKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(Envelope) Envelope{
		"route":      func(value Envelope) Envelope { value.meta.RouteID = testIdentifier(0x12); return value },
		"epoch":      func(value Envelope) Envelope { value.meta.KeyEpoch++; return value },
		"sequence":   func(value Envelope) Envelope { value.meta.Sequence++; return value },
		"message id": func(value Envelope) Envelope { value.meta.MessageID = testIdentifier(0x42); return value },
		"issued at":  func(value Envelope) Envelope { value.meta.IssuedAt++; return value },
		"ciphertext": func(value Envelope) Envelope {
			value.ciphertext = append([]byte(nil), value.ciphertext...)
			value.ciphertext[0] ^= 1
			return value
		},
	}
	for name, mutate := range mutations {
		changed := mutate(envelope)
		if err := verifyDeviceEnvelope(&deviceKey.PublicKey, changed); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s proof error=%v, want unauthorized", name, err)
		}
	}
	wrongKey := testGatewayKey(t)
	if err := verifyDeviceEnvelope(&wrongKey.PublicKey, envelope); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong key error=%v", err)
	}
	if _, err := gatewayProofDigest(envelope); !errors.Is(err, ErrDirection) {
		t.Fatalf("gateway digest over c2g error=%v", err)
	}
}

func TestPaddingBucketsAndStrictUnpadding(t *testing.T) {
	for _, length := range []int{1, 200, 500, 1000, 2000, 4000} {
		intent := testIntent(t, byte(0x30+length%10))
		intent.Message = string(bytes.Repeat([]byte{'x'}, length))
		message, err := NewSMSSendMessage(intent)
		if err != nil {
			t.Fatalf("length %d constructor: %v", length, err)
		}
		padded, err := padMessage(message)
		if err != nil {
			t.Fatalf("length %d pad: %v", length, err)
		}
		if !isPaddingBucket(len(padded)) {
			t.Fatalf("length %d yielded non-bucket %d", length, len(padded))
		}
		opened, err := unpadMessage(padded)
		if err != nil || opened.Kind() != MessageSMSSend {
			t.Fatalf("length %d unpad kind=%q err=%v", length, opened.Kind(), err)
		}
	}
	padded, err := padMessage(testSendMessage(t, 0x31))
	if err != nil {
		t.Fatal(err)
	}
	nonMinimal := make([]byte, 512)
	copy(nonMinimal, padded)
	if _, err := unpadMessage(nonMinimal); err == nil {
		t.Fatal("non-minimal zero-padded bucket accepted")
	}
	padded[len(padded)-1] = 1
	if _, err := unpadMessage(padded); err == nil {
		t.Fatal("non-zero padding accepted")
	}
	worstCase := testIntent(t, 0x39)
	worstCase.Message = string(bytes.Repeat([]byte{1}, MaxSMSMessageBytes))
	worstMessage, err := NewSMSSendMessage(worstCase)
	if err != nil {
		t.Fatal(err)
	}
	padded, err = padMessage(worstMessage)
	if err != nil || len(padded) != paddingBuckets[len(paddingBuckets)-1] {
		t.Fatalf("worst-case JSON expansion bucket=%d err=%v", len(padded), err)
	}
}

func TestNonceAndAADAreDeterministicAndContextBound(t *testing.T) {
	keys := testKeySet(t)
	meta := testMeta(ClientToGateway, 1, 0x41)
	aad, err := canonicalAAD(meta)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := deriveNonce(keys.c2g[:], meta)
	if err != nil {
		t.Fatal(err)
	}
	nonceAgain, _ := deriveNonce(keys.c2g[:], meta)
	if !bytes.Equal(nonce, nonceAgain) || len(nonce) != nonceBytes {
		t.Fatal("nonce is not deterministic 96-bit output")
	}
	sameSequence := meta
	sameSequence.MessageID = testIdentifier(0x42)
	sameSequence.IssuedAt++
	sameSequenceNonce, _ := deriveNonce(keys.c2g[:], sameSequence)
	if !bytes.Equal(nonce, sameSequenceNonce) {
		t.Fatal("message ID or issued_at changed the sequence-derived nonce")
	}
	changed := meta
	changed.Sequence++
	changedAAD, _ := canonicalAAD(changed)
	changedNonce, _ := deriveNonce(keys.c2g[:], changed)
	if bytes.Equal(nonce, changedNonce) {
		t.Fatal("sequence did not change nonce")
	}
	if !bytes.Equal(nonce[:4], changedNonce[:4]) ||
		binary.BigEndian.Uint64(nonce[4:]) != meta.Sequence ||
		binary.BigEndian.Uint64(changedNonce[4:]) != changed.Sequence {
		t.Fatal("nonce is not prefix || uint64be(sequence)")
	}
	g2cMeta := meta
	g2cMeta.Direction = GatewayToClient
	g2cNonce, _ := deriveNonce(keys.g2c[:], g2cMeta)
	if bytes.Equal(nonce, g2cNonce) {
		t.Fatal("direction did not change nonce prefix")
	}
	nextEpochKeys, err := DeriveKeySet(testRootKey(t), meta.RouteID, meta.KeyEpoch+1)
	if err != nil {
		t.Fatal(err)
	}
	nextEpochMeta := meta
	nextEpochMeta.KeyEpoch++
	nextEpochNonce, _ := deriveNonce(nextEpochKeys.c2g[:], nextEpochMeta)
	if bytes.Equal(nonce, nextEpochNonce) {
		t.Fatal("epoch did not change nonce prefix")
	}
	nextRouteKeys, err := DeriveKeySet(testRootKey(t), testIdentifier(0x12), meta.KeyEpoch)
	if err != nil {
		t.Fatal(err)
	}
	nextRouteMeta := meta
	nextRouteMeta.RouteID = testIdentifier(0x12)
	nextRouteNonce, _ := deriveNonce(nextRouteKeys.c2g[:], nextRouteMeta)
	if bytes.Equal(nonce, nextRouteNonce) {
		t.Fatal("route did not change nonce prefix")
	}
	if sha256.Sum256(aad) == sha256.Sum256(changedAAD) {
		t.Fatal("metadata did not bind AAD")
	}
	if base64.RawURLEncoding.EncodeToString(nonce) == "" {
		t.Fatal("empty nonce encoding")
	}
}

func TestMessageDirectionAndFenceOutcomeFailClosed(t *testing.T) {
	keys := testKeySet(t)
	commitment, err := ComputeSMSSendCommitment(keys, testIntent(t, 0x31))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSMSInspectAndFenceResult(
		testIdentifier(0x31), commitment, InspectAndFenceOutcome("not_found"),
	); err == nil {
		t.Fatal("ordinary not_found outcome accepted")
	}
	resultValue, err := NewSMSInspectAndFenceResult(
		testIdentifier(0x31), commitment, OperationFencedAbsent,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewSMSInspectAndFenceResultMessage(resultValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SealClientToGateway(
		keys, testMeta(ClientToGateway, 1, 0x41), result, testDeviceKey(t),
	); !errors.Is(err, ErrMessageKind) {
		t.Fatalf("g2c result in c2g direction error=%v", err)
	}
}

func TestSMSIntentBounds(t *testing.T) {
	valid := testIntent(t, 0x31)
	tests := map[string]SMSSendIntent{
		"bad operation":          {OperationID: "operation", Destination: valid.Destination, Message: valid.Message},
		"empty destination":      {OperationID: valid.OperationID, Message: valid.Message},
		"letters in destination": {OperationID: valid.OperationID, Destination: "+1ABC", Message: valid.Message},
		"long destination": {
			OperationID: valid.OperationID, Destination: "+" + string(bytes.Repeat([]byte{'1'}, 32)),
			Message: valid.Message,
		},
		"empty message": {OperationID: valid.OperationID, Destination: valid.Destination},
		"long message": {
			OperationID: valid.OperationID, Destination: valid.Destination,
			Message: string(bytes.Repeat([]byte{'x'}, MaxSMSMessageBytes+1)),
		},
		"NUL message": {
			OperationID: valid.OperationID, Destination: valid.Destination, Message: "before\x00after",
		},
		"invalid UTF-8": {
			OperationID: valid.OperationID, Destination: valid.Destination, Message: string([]byte{0xff}),
		},
	}
	for name, intent := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMSSendMessage(intent); err == nil {
				t.Fatal("invalid SMS intent accepted")
			}
		})
	}
}
