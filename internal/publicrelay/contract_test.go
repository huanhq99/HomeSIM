package publicrelay

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

func testEncodedEnvelope(t *testing.T) (Envelope, []byte) {
	t.Helper()
	envelope, err := SealClientToGateway(
		testKeySet(t), testMeta(ClientToGateway, 1, 0x41),
		testSendMessage(t, 0x31), testDeviceKey(t),
	)
	if err != nil {
		t.Fatalf("seal test envelope: %v", err)
	}
	encoded, err := EncodeEnvelope(envelope)
	if err != nil {
		t.Fatalf("encode test envelope: %v", err)
	}
	return envelope, encoded
}

func TestEnvelopeStrictWireSchema(t *testing.T) {
	_, valid := testEncodedEnvelope(t)
	if !strings.HasPrefix(string(valid), `{"version":2,"route_id":`) {
		t.Fatalf("unexpected canonical envelope prefix: %.80s", valid)
	}
	decoded, err := DecodeEnvelope(valid)
	if err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}
	reencoded, err := EncodeEnvelope(decoded)
	if err != nil || string(reencoded) != string(valid) {
		t.Fatalf("canonical round trip err=%v equal=%v", err, string(reencoded) == string(valid))
	}

	var wire wireEnvelope
	if err := json.Unmarshal(valid, &wire); err != nil {
		t.Fatal(err)
	}
	mutateWire := func(mutate func(*wireEnvelope)) []byte {
		copy := wire
		mutate(&copy)
		encoded, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	tests := map[string][]byte{
		"duplicate key":   []byte(strings.Replace(string(valid), `{"version":2,`, `{"version":2,"version":2,`, 1)),
		"unknown key":     []byte(strings.Replace(string(valid), `{"version":2,`, `{"extra":0,"version":2,`, 1)),
		"missing key":     []byte(strings.Replace(string(valid), `"issued_at":1800000000,`, "", 1)),
		"trailing value":  append(append([]byte(nil), valid...), []byte(` {}`)...),
		"wrong version":   mutateWire(func(value *wireEnvelope) { value.Version = 1 }),
		"bad route":       mutateWire(func(value *wireEnvelope) { value.RouteID = "route" }),
		"zero route":      mutateWire(func(value *wireEnvelope) { value.RouteID = testIdentifier(0) }),
		"zero epoch":      mutateWire(func(value *wireEnvelope) { value.KeyEpoch = 0 }),
		"bad direction":   mutateWire(func(value *wireEnvelope) { value.Direction = "client" }),
		"zero sequence":   mutateWire(func(value *wireEnvelope) { value.Sequence = 0 }),
		"bad message id":  mutateWire(func(value *wireEnvelope) { value.MessageID = "message" }),
		"zero message id": mutateWire(func(value *wireEnvelope) { value.MessageID = testIdentifier(0) }),
		"negative time":   mutateWire(func(value *wireEnvelope) { value.IssuedAt = -1 }),
		"bad ciphertext":  mutateWire(func(value *wireEnvelope) { value.Ciphertext = "***" }),
		"wrong ciphertext bucket": mutateWire(func(value *wireEnvelope) {
			value.Ciphertext = base64.RawURLEncoding.EncodeToString(make([]byte, 100))
		}),
		"bad signature": mutateWire(func(value *wireEnvelope) { value.Signature = "***" }),
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEnvelope(encoded); !errors.Is(err, ErrMalformed) || err.Error() != ErrMalformed.Error() {
				t.Fatal("malformed envelope accepted")
			}
		})
	}
}

func TestStrictJSONRejectsInvalidUTF8AndIsolatedSurrogates(t *testing.T) {
	type valueWire struct {
		Value string `json:"value"`
	}
	keys := exactKeySet("value")
	valid := map[string][]byte{
		"raw UTF-8":        []byte(`{"value":"😀你好"}`),
		"surrogate pair":   []byte(`{"value":"\uD83D\uDE00"}`),
		"literal sequence": []byte(`{"value":"\\uD800"}`),
	}
	for name, encoded := range valid {
		t.Run(name, func(t *testing.T) {
			var value valueWire
			if err := decodeExactJSON(encoded, &value, keys, MaxJSONDepth); err != nil {
				t.Fatalf("valid Unicode rejected: %v", err)
			}
		})
	}
	invalid := map[string][]byte{
		"raw invalid UTF-8": {'{', '"', 'v', 'a', 'l', 'u', 'e', '"', ':', '"', 0xff, '"', '}'},
		"isolated high":     []byte(`{"value":"\uD83D"}`),
		"isolated low":      []byte(`{"value":"\uDE00"}`),
		"high then non-low": []byte(`{"value":"\uD83D\u0041"}`),
	}
	for name, encoded := range invalid {
		t.Run(name, func(t *testing.T) {
			var value valueWire
			if err := decodeExactJSON(encoded, &value, keys, MaxJSONDepth); err == nil {
				t.Fatal("invalid Unicode accepted")
			}
		})
	}
}

func TestOuterEnvelopeDoesNotExposeBusinessFields(t *testing.T) {
	_, encoded := testEncodedEnvelope(t)
	for _, forbidden := range []string{
		`"action"`, `"operation_id"`, `"destination"`, `"message"`, `"cursor"`,
		"+15551234567", "synthetic relay test", testIdentifier(0x31),
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("outer envelope exposed %q", forbidden)
		}
	}
}

func TestEnvelopeReaderEnforcesActualBound(t *testing.T) {
	_, valid := testEncodedEnvelope(t)
	if _, err := DecodeEnvelopeReader(strings.NewReader(string(valid))); err != nil {
		t.Fatalf("bounded valid reader: %v", err)
	}
	oversized := strings.NewReader(strings.Repeat("x", MaxEnvelopeBytes+1))
	if _, err := DecodeEnvelopeReader(oversized); err == nil {
		t.Fatal("oversized stream accepted")
	}
	if _, err := DecodeEnvelopeReader(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
}

func TestPublicParsersReturnFixedRedactedErrors(t *testing.T) {
	const marker = "SENSITIVE_MARKER_KIND_DIRECTION_KEY"
	_, valid := testEncodedEnvelope(t)
	unknownKey := []byte(strings.Replace(
		string(valid), `{"version":2,`, `{"version":2,"`+marker+`":0,`, 1,
	))
	var wire wireEnvelope
	if err := json.Unmarshal(valid, &wire); err != nil {
		t.Fatal(err)
	}
	wire.Direction = Direction(marker)
	badDirection, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{
		"unknown key": unknownKey,
		"direction":   badDirection,
	} {
		t.Run("envelope "+name, func(t *testing.T) {
			_, err := DecodeEnvelope(encoded)
			if !errors.Is(err, ErrMalformed) || err.Error() != ErrMalformed.Error() ||
				strings.Contains(err.Error(), marker) {
				t.Fatalf("public envelope error was not fixed and redacted: %v", err)
			}
		})
	}
	if _, err := DecodeEnvelopeReader(strings.NewReader(string(unknownKey))); !errors.Is(err, ErrMalformed) || err.Error() != ErrMalformed.Error() ||
		strings.Contains(err.Error(), marker) {
		t.Fatalf("public reader error was not fixed and redacted: %v", err)
	}
	if _, err := ParseCommitment(marker); !errors.Is(err, ErrMalformed) ||
		err.Error() != ErrMalformed.Error() || strings.Contains(err.Error(), marker) {
		t.Fatalf("public commitment error was not fixed and redacted: %v", err)
	}

	keys := testKeySet(t)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	envelope, err := SealClientToGateway(
		keys, testMeta(ClientToGateway, 1, 0x41), testSendMessage(t, 0x31), testDeviceKey(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.OpenClientToGateway(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	_, _, encodedState, _ := committer.snapshot()
	stateWithUnknownKey := []byte(strings.Replace(
		string(encodedState), `{"version":2,`, `{"version":2,"`+marker+`":0,`, 1,
	))
	if _, err := DecodeReceiverState(keys, stateWithUnknownKey); !errors.Is(err, ErrMalformed) ||
		err.Error() != ErrMalformed.Error() || strings.Contains(err.Error(), marker) {
		t.Fatalf("public state error was not fixed and redacted: %v", err)
	}
}

func TestInnerMessageStrictSchemas(t *testing.T) {
	message := testSendMessage(t, 0x31)
	encoded, err := encodeInnerMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	tests := [][]byte{
		[]byte(strings.Replace(string(encoded), `{"version":2,`, `{"version":2,"version":2,`, 1)),
		[]byte(strings.Replace(string(encoded), `"message":`, `"extra":0,"message":`, 1)),
		[]byte(strings.Replace(string(encoded), `"destination":"+15551234567",`, "", 1)),
		[]byte(strings.Replace(string(encoded), `"sms.send"`, `"sms.unknown"`, 1)),
		append(append([]byte(nil), encoded...), []byte(` true`)...),
	}
	for index, malformed := range tests {
		if _, err := decodeInnerMessage(malformed); err == nil {
			t.Fatalf("malformed inner message %d accepted: %s", index, malformed)
		}
	}
}

func TestRedactedFormattingAndJSON(t *testing.T) {
	keys := testKeySet(t)
	intent := testIntent(t, 0x31)
	commitment, err := ComputeSMSSendCommitment(keys, intent)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewSMSInspectAndFenceRequest(intent.OperationID, commitment)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewSMSInspectAndFenceResult(intent.OperationID, commitment, OperationFencedAbsent)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := NewSMSSendMessage(intent)
	envelope, _ := SealClientToGateway(
		keys, testMeta(ClientToGateway, 1, 0x41), message, testDeviceKey(t),
	)
	committer := newMemoryCommitter()
	receiver := newTestDeviceReceiver(t, keys, DefaultReceiveWindow, committer)
	verified, _, err := receiver.OpenClientToGateway(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	gatewayReceiver := newTestGatewayReceiver(t, keys, DefaultReceiveWindow, newMemoryCommitter())
	expectation, err := gatewayReceiver.NewFencedAbsentExpectation(request)
	if err != nil {
		t.Fatal(err)
	}
	commit := ReceiverStateCommit{
		expectedGeneration: 1, nextGeneration: 2, encodedState: []byte(intent.Message),
	}
	values := []any{
		testRootKey(t), keys, intent, commitment, request, result, message,
		envelope.meta, envelope, ReceiverState{}, receiver, verified,
		VerifiedGatewayMessage{}, expectation, commit,
	}
	for _, value := range values {
		forms := []string{fmt.Sprint(value), fmt.Sprintf("%#v", value)}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %T: %v", value, err)
		}
		forms = append(forms, string(encoded))
		for _, form := range forms {
			for _, secret := range []string{
				intent.OperationID, intent.Destination, intent.Message,
				testIdentifier(0x11), testIdentifier(0x41), commitment.Encode(),
			} {
				if strings.Contains(form, secret) {
					t.Fatalf("%T leaked %q through %q", value, secret, form)
				}
			}
		}
	}
}

func TestFencedAbsentAuthorityTypesHavePrivateFieldsAndOnePredicate(t *testing.T) {
	privateTypes := []reflect.Type{
		reflect.TypeOf(SMSInspectAndFenceRequest{}),
		reflect.TypeOf(SMSInspectAndFenceResult{}),
		reflect.TypeOf(VerifiedMessage{}),
		reflect.TypeOf(VerifiedGatewayMessage{}),
		reflect.TypeOf(FencedAbsentExpectation{}),
	}
	for _, valueType := range privateTypes {
		for index := 0; index < valueType.NumField(); index++ {
			if valueType.Field(index).IsExported() {
				t.Fatalf("%s exposes authority-bearing field %s", valueType, valueType.Field(index).Name)
			}
		}
	}
	for _, valueType := range []reflect.Type{
		reflect.TypeOf(SMSInspectAndFenceRequest{}),
		reflect.TypeOf(SMSInspectAndFenceResult{}),
		reflect.TypeOf(Message{}),
		reflect.TypeOf(VerifiedMessage{}),
		reflect.TypeOf(FencedAbsentExpectation{}),
	} {
		if _, exists := valueType.MethodByName("ConfirmsFencedAbsent"); exists {
			t.Fatalf("%s exposes fenced_absent predicate", valueType)
		}
	}
	if _, exists := reflect.TypeOf(VerifiedGatewayMessage{}).MethodByName("ConfirmsFencedAbsent"); !exists {
		t.Fatal("verified gateway wrapper lacks fenced_absent predicate")
	}
	if _, err := NewSMSInspectAndFenceMessage(SMSInspectAndFenceRequest{}); err == nil {
		t.Fatal("zero inspect request became a message")
	}
	if _, err := NewSMSInspectAndFenceResultMessage(SMSInspectAndFenceResult{}); err == nil {
		t.Fatal("zero inspect result became a message")
	}
}

func TestProofRejectsHighSAndNonCanonicalDER(t *testing.T) {
	envelope, _ := testEncodedEnvelope(t)
	signature, ok := decodeSignature(envelope.signature)
	if !ok {
		t.Fatal("test signature invalid")
	}
	signature.S.Sub(elliptic.P256().Params().N, signature.S)
	der, err := asn1.Marshal(signature)
	if err != nil {
		t.Fatal(err)
	}
	high := envelope
	high.signature = base64.RawURLEncoding.EncodeToString(der)
	if err := verifyDeviceEnvelope(&testDeviceKey(t).PublicKey, high); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("high-S proof error=%v", err)
	}
	if _, err := EncodeEnvelope(high); err == nil {
		t.Fatal("high-S proof encoded")
	}

	der = append(der, 0)
	trailing := envelope
	trailing.signature = base64.RawURLEncoding.EncodeToString(der)
	if err := verifyDeviceEnvelope(&testDeviceKey(t).PublicKey, trailing); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("trailing DER proof error=%v", err)
	}
}

type highSSigner struct{ key *ecdsa.PrivateKey }

func (signer highSSigner) Public() crypto.PublicKey { return &signer.key.PublicKey }

func (signer highSSigner) Sign(
	random io.Reader,
	digest []byte,
	options crypto.SignerOpts,
) ([]byte, error) {
	der, err := signer.key.Sign(random, digest, options)
	if err != nil {
		return nil, err
	}
	signature, ok := decodeDERSignature(der, false)
	if !ok {
		return nil, errors.New("test signer produced malformed DER")
	}
	halfOrder := new(big.Int).Rsh(new(big.Int).Set(elliptic.P256().Params().N), 1)
	if signature.S.Cmp(halfOrder) <= 0 {
		signature.S.Sub(elliptic.P256().Params().N, signature.S)
	}
	return asn1.Marshal(signature)
}

func TestTypedSignerNormalizesExternalHighSProof(t *testing.T) {
	keys := testKeySet(t)
	unsigned, err := sealUnsigned(
		keys, testMeta(ClientToGateway, 1, 0x41), testSendMessage(t, 0x31),
	)
	if err != nil {
		t.Fatal(err)
	}
	device := testDeviceKey(t)
	signed, err := signDeviceEnvelope(highSSigner{key: device}, unsigned)
	if err != nil {
		t.Fatalf("typed signing with external signer: %v", err)
	}
	if !validEncodedSignature(signed.signature) {
		t.Fatal("external signer proof was not normalized to canonical low-S")
	}
	if err := verifyDeviceEnvelope(&device.PublicKey, signed); err != nil {
		t.Fatalf("normalized external proof did not verify: %v", err)
	}
}
